// Copyright © 2021 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package image

import (
	"context"
	"fmt"
	"io"
	"os"

	dockerstreams "github.com/docker/cli/cli/streams"
	"github.com/docker/distribution/registry/api/errcode"
	"github.com/docker/docker/api/types"
	dockerioutils "github.com/docker/docker/pkg/ioutils"
	dockerjsonmessage "github.com/docker/docker/pkg/jsonmessage"
	dockerprogress "github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/streamformatter"

	"github.com/alibaba/sealer/common"
	"github.com/alibaba/sealer/logger"
	"github.com/alibaba/sealer/pkg/image/distributionutil"
	"github.com/alibaba/sealer/pkg/image/reference"
	"github.com/alibaba/sealer/pkg/image/store"
	imageUtils "github.com/alibaba/sealer/pkg/image/utils"
	v1 "github.com/alibaba/sealer/types/api/v1"
	"github.com/alibaba/sealer/utils"
)

const ManifestUnknown = "manifest unknown"

// DefaultImageService is the default service, which is used for image pull/push
type DefaultImageService struct {
	ForceDeleteImage bool // sealer rmi -f
	imageStore       store.ImageStore
}

// PullIfNotExist is used to pull image if not exists locally
func (d DefaultImageService) PullIfNotExist(imageName string) error {
	img, err := d.GetImageByName(imageName)
	if err != nil {
		return err
	}
	if img != nil {
		logger.Debug("image %s already exists", imageName)
		return nil
	}

	return d.Pull(imageName)
}

func (d DefaultImageService) GetImageByName(imageName string) (*v1.Image, error) {
	var img *v1.Image
	named, err := reference.ParseToNamed(imageName)
	if err != nil {
		return nil, err
	}
	img, err = d.imageStore.GetByName(named.Raw())
	if err == nil {
		logger.Debug("image %s already exists", named)
		return img, nil
	}
	return nil, nil
}

// Pull always do pull action
func (d DefaultImageService) Pull(imageName string) error {
	named, err := reference.ParseToNamed(imageName)
	if err != nil {
		return err
	}
	var (
		reader, writer  = io.Pipe()
		writeFlusher    = dockerioutils.NewWriteFlusher(writer)
		progressChanOut = streamformatter.NewJSONProgressOutput(writeFlusher, false)
		streamOut       = dockerstreams.NewOut(common.StdOut)
	)
	defer func() {
		_ = reader.Close()
		_ = writer.Close()
		_ = writeFlusher.Close()
	}()

	layerStore, err := store.NewDefaultLayerStore()
	if err != nil {
		return err
	}

	puller, err := distributionutil.NewPuller(named, distributionutil.Config{
		LayerStore:     layerStore,
		ProgressOutput: progressChanOut,
	})
	if err != nil {
		return err
	}

	go func() {
		err := dockerjsonmessage.DisplayJSONMessagesToStream(reader, streamOut, nil)
		if err != nil && err != io.ErrClosedPipe {
			logger.Warn("error occurs in display progressing, err: %s", err)
		}
	}()

	dockerprogress.Message(progressChanOut, "", fmt.Sprintf("Start to Pull Image %s", named.Raw()))
	image, err := puller.Pull(context.Background(), named)
	if err != nil {
		if err.(errcode.Errors)[0].(errcode.Error).Message == ManifestUnknown {
			err = fmt.Errorf("image %s does not exist, %v", imageName, err)
		}
		return err
	}
	// TODO use image store to do the job next
	err = d.imageStore.Save(*image, named.Raw())
	if err == nil {
		dockerprogress.Message(progressChanOut, "", fmt.Sprintf("Success to Pull Image %s", named.Raw()))
	}
	return err
}

// Push push local image to remote registry
func (d DefaultImageService) Push(imageName string) error {
	named, err := reference.ParseToNamed(imageName)
	if err != nil {
		return err
	}
	var (
		reader, writer  = io.Pipe()
		writeFlusher    = dockerioutils.NewWriteFlusher(writer)
		progressChanOut = streamformatter.NewJSONProgressOutput(writeFlusher, false)
		streamOut       = dockerstreams.NewOut(common.StdOut)
	)
	defer func() {
		_ = reader.Close()
		_ = writer.Close()
		_ = writeFlusher.Close()
	}()

	layerStore, err := store.NewDefaultLayerStore()
	if err != nil {
		return err
	}

	pusher, err := distributionutil.NewPusher(named,
		distributionutil.Config{
			LayerStore:     layerStore,
			ProgressOutput: progressChanOut,
		})
	if err != nil {
		return err
	}
	go func() {
		err := dockerjsonmessage.DisplayJSONMessagesToStream(reader, streamOut, nil)
		// reader may be closed in another goroutine
		// so do not log warn when err == io.ErrClosedPipe
		if err != nil && err != io.ErrClosedPipe {
			logger.Warn("error occurs in display progressing, err: %s", err)
		}
	}()

	dockerprogress.Message(progressChanOut, "", fmt.Sprintf("Start to Push Image %s", named.Raw()))
	err = pusher.Push(context.Background(), named)
	if err == nil {
		dockerprogress.Message(progressChanOut, "", fmt.Sprintf("Success to Push Image %s", named.CompleteName()))
	}
	return err
}

// Login login into a registry, for saving auth info in ~/.docker/config.json
func (d DefaultImageService) Login(RegistryURL, RegistryUsername, RegistryPasswd string) error {
	err := distributionutil.Login(context.Background(), &types.AuthConfig{ServerAddress: RegistryURL, Username: RegistryUsername, Password: RegistryPasswd})
	if err != nil {
		return fmt.Errorf("failed to authenticate %s: %v", RegistryURL, err)
	}
	if err := utils.SetDockerConfig(RegistryURL, RegistryUsername, RegistryPasswd); err != nil {
		return err
	}
	logger.Info("%s login %s success", RegistryUsername, RegistryURL)
	return nil
}

func (d DefaultImageService) Delete(imageArg string) error {
	var (
		images        []*v1.Image
		image         *v1.Image
		imageTagCount int
		imageID       string
		imageStore    = d.imageStore
		named         reference.Named
		err           error
	)

	imageMetadataMap, err := imageStore.GetImageMetadataMap()
	if err != nil {
		return err
	}
	// example ImageName : 7e2e51b85680d827fae08853dea32ad6:latest
	// example ImageID :   7e2e51b85680d827fae08853dea32ad6
	// https://github.com/alibaba/sealer/blob/f9d609c7fede47a7ac229bcd03d92dd0429b5038/image/reference/util.go#L59
	named, err = reference.ParseToNamed(imageArg)
	if err != nil {
		return err
	}
	img, err := imageStore.GetByName(named.Raw())
	//untag it if it is a name, then try to untag it as ID
	if err == nil {
		//1.untag image
		if err = imageStore.DeleteByName(named.Raw()); err != nil {
			return fmt.Errorf("failed to untag image %s, err: %v", imageArg, err)
		}
		imageID = img.Spec.ID
	} else {
		imageList, err := imageUtils.SimilarImageListByID(imageArg)
		if err != nil {
			return err
		}
		if len(imageList) == 0 || !d.ForceDeleteImage && len(imageList) > 1 {
			return fmt.Errorf("not to find image: %s", imageArg)
		}
		if err = imageStore.DeleteByID(imageList[0], d.ForceDeleteImage); err != nil {
			return err
		}
		imageID = imageList[0]
	}
	image, err = imageStore.GetByID(imageID)
	if err != nil {
		return fmt.Errorf("failed to get image metadata for image %s, err: %w", image.Spec.ID, err)
	}
	logger.Info("untag image %s succeeded", image.Spec.ID)

	for _, value := range imageMetadataMap {
		tmpImage, err := imageStore.GetByID(value.ID)
		if err != nil {
			continue
		}
		if value.ID == imageID {
			imageTagCount++
			if imageTagCount > 1 {
				continue
			}
		}
		images = append(images, tmpImage)
	}
	if imageTagCount != 1 && !d.ForceDeleteImage {
		return nil
	}

	err = store.DeleteImageLocal(image.Spec.ID)
	if err != nil {
		return err
	}

	layer2ImageNames := layer2ImageMap(images)
	// TODO: find a atomic way to delete layers and image
	layerStore, err := store.NewDefaultLayerStore()
	if err != nil {
		return err
	}

	for _, layer := range image.Spec.Layers {
		layerID := store.LayerID(layer.ID)
		if isLayerDeletable(layer2ImageNames, layerID) {
			err = layerStore.Delete(layerID)
			if err != nil {
				// print log and continue to delete other layers of the image
				logger.Error("Fail to delete image %s's layer %s", image.Spec.ID, layerID)
			}
		}
	}

	logger.Info("image %s delete success", image.Spec.ID)
	return nil
}

// Prune delete the unused Layer in the `DefaultLayerDir` directory
func (d DefaultImageService) Prune() error {
	imageMetadataMap, err := d.imageStore.GetImageMetadataMap()
	var allImageLayerDirs []string
	if err != nil {
		return err
	}

	for _, imageMetadata := range imageMetadataMap {
		image, err := d.imageStore.GetByID(imageMetadata.ID)
		if err != nil {
			return err
		}
		res, err := GetImageLayerDirs(image)
		if err != nil {
			return err
		}
		allImageLayerDirs = append(allImageLayerDirs, res...)
	}
	allImageLayerDirs = utils.RemoveDuplicate(allImageLayerDirs)
	dirs, err := store.GetDirListInDir(common.DefaultLayerDir)
	if err != nil {
		return err
	}
	dirs = utils.RemoveStrSlice(dirs, allImageLayerDirs)
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		_, err = common.StdOut.WriteString(fmt.Sprintf("%s layer deleted\n", dir))
		if err != nil {
			return err
		}
	}
	return nil
}

func isLayerDeletable(layer2ImageNames map[store.LayerID][]string, layerID store.LayerID) bool {
	return len(layer2ImageNames[layerID]) <= 1
}

// layer2ImageMap accepts a directory parameter which contains image metadata.
// It reads these metadata and saves the layer and image relationship in a map.
func layer2ImageMap(images []*v1.Image) map[store.LayerID][]string {
	var layer2ImageNames = make(map[store.LayerID][]string)
	for _, image := range images {
		for _, layer := range image.Spec.Layers {
			layerID := store.LayerID(layer.ID)
			layer2ImageNames[layerID] = append(layer2ImageNames[layerID], image.Spec.ID)
		}
	}
	return layer2ImageNames
}

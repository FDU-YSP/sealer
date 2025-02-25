/*
Copyright © 2022 Alibaba Group Holding Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gen

import (
	"fmt"
	"strconv"

	"github.com/alibaba/sealer/apply/processor"
	"github.com/alibaba/sealer/pkg/filesystem"
	"github.com/alibaba/sealer/pkg/filesystem/cloudfilesystem"
	"github.com/alibaba/sealer/pkg/filesystem/cloudimage"
	"github.com/alibaba/sealer/pkg/image"
	"github.com/alibaba/sealer/pkg/runtime"

	v1 "k8s.io/api/core/v1"

	"github.com/alibaba/sealer/common"
	"github.com/alibaba/sealer/pkg/client/k8s"
	v2 "github.com/alibaba/sealer/types/api/v2"
	"github.com/alibaba/sealer/utils"
)

const (
	masterLabel = "node-role.kubernetes.io/master"
)

type ParserArg struct {
	Name       string
	Passwd     string
	Image      string
	Port       uint16
	Pk         string
	PkPassword string
}

type GenerateProcessor struct {
	Runtime      *runtime.KubeadmRuntime
	ImageManager image.Service
	ImageMounter cloudimage.Interface
	Filesystem   cloudfilesystem.Interface
}

func NewGenerateProcessor() (processor.Interface, error) {
	imageMounter, err := filesystem.NewCloudImageMounter()
	if err != nil {
		return nil, err
	}
	imgSvc, err := image.NewImageService()
	if err != nil {
		return nil, err
	}
	return &GenerateProcessor{
		ImageManager: imgSvc,
		ImageMounter: imageMounter,
	}, nil
}

func (g *GenerateProcessor) Execute(cluster *v2.Cluster) error {
	fileName := fmt.Sprintf("%s/.sealer/%s/Clusterfile", common.GetHomeDir(), cluster.Name)
	err := utils.MarshalYamlToFile(fileName, cluster)
	if err != nil {
		return err
	}

	pipLine, err := g.GetPipeLine()
	if err != nil {
		return err
	}

	for _, f := range pipLine {
		if err = f(cluster); err != nil {
			return err
		}
	}
	return nil
}

func (g *GenerateProcessor) init(cluster *v2.Cluster) error {
	runt, err := runtime.NewDefaultRuntime(cluster, nil)
	if err != nil {
		return err
	}
	fs, err := filesystem.NewFilesystem(common.DefaultMountCloudImageDir(cluster.Name))
	if err != nil {
		return err
	}
	g.Runtime = runt.(*runtime.KubeadmRuntime)
	g.Filesystem = fs
	return nil
}

func (g *GenerateProcessor) GetPipeLine() ([]func(cluster *v2.Cluster) error, error) {
	var todoList []func(cluster *v2.Cluster) error
	todoList = append(todoList,
		g.init,
		g.MountImage,
		g.MountRootfs,
		g.ApplyRegistry,
		g.UnmountImage,
	)
	return todoList, nil
}

func GenerateCluster(arg *ParserArg) (*v2.Cluster, error) {
	var nodeip, masterip []string
	cluster := &v2.Cluster{}

	cluster.Kind = common.Kind
	cluster.APIVersion = common.APIVersion
	cluster.Name = arg.Name
	cluster.Spec.Image = arg.Image
	cluster.Spec.SSH.Passwd = arg.Passwd
	cluster.Spec.SSH.Port = strconv.Itoa(int(arg.Port))
	cluster.Spec.SSH.Pk = arg.Pk
	cluster.Spec.SSH.PkPasswd = arg.PkPassword

	c, err := k8s.Newk8sClient()
	if err != nil {
		return nil, fmt.Errorf("generate clusterfile failed, %s", err)
	}

	all, err := c.ListNodes()
	if err != nil {
		return nil, fmt.Errorf("generate clusterfile failed, %s", err)
	}
	for _, n := range all.Items {
		for _, v := range n.Status.Addresses {
			if _, ok := n.Labels[masterLabel]; ok {
				if v.Type == v1.NodeInternalIP {
					masterip = append(masterip, v.Address)
				}
			} else if v.Type == v1.NodeInternalIP {
				nodeip = append(nodeip, v.Address)
			}
		}
	}

	masterHosts := v2.Host{
		IPS:   masterip,
		Roles: []string{common.MASTER},
	}

	nodeHosts := v2.Host{
		IPS:   nodeip,
		Roles: []string{common.NODE},
	}

	cluster.Spec.Hosts = append(cluster.Spec.Hosts, masterHosts, nodeHosts)
	return cluster, nil
}

func (g *GenerateProcessor) MountRootfs(cluster *v2.Cluster) error {
	hosts := append(cluster.GetMasterIPList(), cluster.GetNodeIPList()...)
	regConfig := runtime.GetRegistryConfig(common.DefaultTheClusterRootfsDir(cluster.Name), cluster.GetMaster0IP())
	if utils.NotInIPList(regConfig.IP, hosts) {
		hosts = append(hosts, regConfig.IP)
	}
	return g.Filesystem.MountRootfs(cluster, hosts, false)
}

func (g *GenerateProcessor) MountImage(cluster *v2.Cluster) error {
	err := g.ImageManager.PullIfNotExist(cluster.Spec.Image)
	if err != nil {
		return err
	}
	return g.ImageMounter.MountImage(cluster)
}

func (g *GenerateProcessor) UnmountImage(cluster *v2.Cluster) error {
	return g.ImageMounter.UnMountImage(cluster)
}

func (g *GenerateProcessor) ApplyRegistry(cluster *v2.Cluster) error {
	runt, err := runtime.NewDefaultRuntime(cluster, nil)
	if err != nil {
		return err
	}
	err = runt.(*runtime.KubeadmRuntime).GenerateRegistryCert()
	if err != nil {
		return err
	}
	return g.Runtime.ApplyRegistry()
}

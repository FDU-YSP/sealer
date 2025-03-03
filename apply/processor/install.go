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

package processor

import (
	"github.com/alibaba/sealer/pkg/clusterfile"
	"github.com/alibaba/sealer/pkg/config"
	"github.com/alibaba/sealer/pkg/filesystem"
	"github.com/alibaba/sealer/pkg/filesystem/cloudfilesystem"
	"github.com/alibaba/sealer/pkg/guest"
	"github.com/alibaba/sealer/pkg/plugin"
	v2 "github.com/alibaba/sealer/types/api/v2"
)

type InstallProcessor struct {
	fileSystem  cloudfilesystem.Interface
	clusterFile clusterfile.Interface
	Guest       guest.Interface
	Config      config.Interface
	Plugins     plugin.Plugins
}

// Execute :according to the different of desired cluster to install app on cluster.
func (i InstallProcessor) Execute(cluster *v2.Cluster) error {
	i.Config = config.NewConfiguration(cluster.Name)
	i.Plugins = plugin.NewPlugins(cluster.Name)
	if err := i.initPlugin(); err != nil {
		return err
	}
	pipLine, err := i.GetPipeLine()
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

func (i InstallProcessor) initPlugin() error {
	return i.Plugins.Dump(i.clusterFile.GetPlugins())
}

func (i InstallProcessor) GetPipeLine() ([]func(cluster *v2.Cluster) error, error) {
	var todoList []func(cluster *v2.Cluster) error
	todoList = append(todoList,
		i.RunConfig,
		i.MountRootfs,
		i.GetPhasePluginFunc(plugin.PhasePreGuest),
		i.Install,
		i.GetPhasePluginFunc(plugin.PhasePostInstall),
	)
	return todoList, nil
}

func (i InstallProcessor) RunConfig(cluster *v2.Cluster) error {
	return i.Config.Dump(i.clusterFile.GetConfigs())
}

func (i InstallProcessor) MountRootfs(cluster *v2.Cluster) error {
	hosts := append(cluster.GetMasterIPList(), cluster.GetNodeIPList()...)
	//initFlag : no need to do init cmd like installing docker service and so on.
	return i.fileSystem.MountRootfs(cluster, hosts, false)
}

func (i InstallProcessor) Install(cluster *v2.Cluster) error {
	return i.Guest.Apply(cluster)
}

func (i InstallProcessor) GetPhasePluginFunc(phase plugin.Phase) func(cluster *v2.Cluster) error {
	return func(cluster *v2.Cluster) error {
		if phase == plugin.PhasePreGuest {
			if err := i.Plugins.Load(); err != nil {
				return err
			}
		}
		return i.Plugins.Run(cluster, phase)
	}
}

func NewInstallProcessor(rootfs string, clusterFile clusterfile.Interface) (Interface, error) {
	gs, err := guest.NewGuestManager()
	if err != nil {
		return nil, err
	}

	fs, err := filesystem.NewFilesystem(rootfs)
	if err != nil {
		return nil, err
	}

	return InstallProcessor{
		clusterFile: clusterFile,
		fileSystem:  fs,
		Guest:       gs,
	}, nil
}

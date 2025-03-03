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

package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/alibaba/sealer/utils"
	"golang.org/x/sync/errgroup"

	"github.com/pkg/errors"

	"github.com/alibaba/sealer/logger"
	"github.com/alibaba/sealer/pkg/ipvs"
)

const (
	RemoteAddIPVS                   = "seautil ipvs --vs %s:6443 %s --health-path /healthz --health-schem https --run-once"
	RemoteStaticPodMkdir            = "mkdir -p /etc/kubernetes/manifests"
	RemoteJoinConfig                = `echo "%s" > %s/etc/kubeadm.yml`
	LvscareDefaultStaticPodFileName = "/etc/kubernetes/manifests/kube-lvscare.yaml"
	RemoteAddIPVSEtcHosts           = "echo %s %s >> /etc/hosts"
	RemoteCheckRoute                = "seautil route check --host %s"
	RemoteAddRoute                  = "seautil route add --host %s --gateway %s"
	RemoteDelRoute                  = "if command -v seautil > /dev/null 2>&1; then seautil route del --host %s --gateway %s; fi"
	LvscareStaticPodCmd             = `echo "%s" > %s`
)

func (k *KubeadmRuntime) joinNodeConfig(nodeIP string) ([]byte, error) {
	// TODO get join config from config file
	k.setAPIServerEndpoint(fmt.Sprintf("%s:6443", k.getVIP()))
	cGroupDriver, err := k.getCgroupDriverFromShell(nodeIP)
	if err != nil {
		return nil, err
	}
	k.setCgroupDriver(cGroupDriver)
	return utils.MarshalYamlConfigs(k.JoinConfiguration, k.KubeletConfiguration)
}

func (k *KubeadmRuntime) joinNodes(nodes []string) error {
	if len(nodes) == 0 {
		return nil
	}
	if err := k.MergeKubeadmConfig(); err != nil {
		return err
	}
	if err := k.WaitSSHReady(6, nodes...); err != nil {
		return errors.Wrap(err, "join nodes wait for ssh ready time out")
	}
	if err := k.sendRegistryCert(nodes); err != nil {
		return err
	}
	if err := k.GetJoinTokenHashAndKey(); err != nil {
		return err
	}
	var masters string
	eg, _ := errgroup.WithContext(context.Background())
	for _, master := range k.GetMasterIPList() {
		masters += fmt.Sprintf(" --rs %s:6443", master)
	}
	ipvsCmd := fmt.Sprintf(RemoteAddIPVS, k.getVIP(), masters)

	k.setAPIServerEndpoint(fmt.Sprintf("%s:6443", k.getVIP()))
	k.cleanJoinLocalAPIEndPoint()

	registryHost := getRegistryHost(k.getImageMountDir(), k.GetMaster0IP())
	addRegistryHostsAndLogin := fmt.Sprintf(RemoteAddEtcHosts, registryHost, registryHost)
	cf := GetRegistryConfig(k.getImageMountDir(), k.GetMaster0IP())
	if cf.Username != "" && cf.Password != "" {
		addRegistryHostsAndLogin = fmt.Sprintf("%s && %s", addRegistryHostsAndLogin, fmt.Sprintf(DockerLoginCommand, cf.Domain+":"+cf.Port, cf.Username, cf.Password))
	}
	for _, node := range nodes {
		node := node
		eg.Go(func() error {
			logger.Info("Start to join %s as worker", node)
			err := k.checkMultiNetworkAddVIPRoute(node)
			if err != nil {
				return fmt.Errorf("failed to check multi network: %v", err)
			}
			// send join node config, get cgroup driver on every join nodes
			joinConfig, err := k.joinNodeConfig(node)
			if err != nil {
				return fmt.Errorf("failed to join node %s %v", node, err)
			}
			cmdWriteJoinConfig := fmt.Sprintf(RemoteJoinConfig, string(joinConfig), k.getRootfs())
			cmdHosts := fmt.Sprintf(RemoteAddIPVSEtcHosts, k.getVIP(), k.getAPIServerDomain())
			cmd := k.Command(k.getKubeVersion(), JoinNode)
			yaml := ipvs.LvsStaticPodYaml(k.getVIP(), k.GetMasterIPList(), "")
			lvscareStaticCmd := fmt.Sprintf(LvscareStaticPodCmd, yaml, LvscareDefaultStaticPodFileName)
			ssh, err := k.getHostSSHClient(node)
			if err != nil {
				return fmt.Errorf("failed to join node %s %v", node, err)
			}
			if err := ssh.CmdAsync(node, addRegistryHostsAndLogin, cmdWriteJoinConfig, cmdHosts, ipvsCmd, cmd, RemoteStaticPodMkdir, lvscareStaticCmd); err != nil {
				return fmt.Errorf("failed to join node %s %v", node, err)
			}
			logger.Info("Succeeded in joining %s as worker", node)
			return err
		})
	}
	return eg.Wait()
}

func (k *KubeadmRuntime) deleteNodes(nodes []string) error {
	if len(nodes) == 0 {
		return nil
	}
	eg, _ := errgroup.WithContext(context.Background())
	for _, node := range nodes {
		node := node
		eg.Go(func() error {
			logger.Info("Start to delete worker %s", node)
			if err := k.deleteNode(node); err != nil {
				return fmt.Errorf("delete node %s failed %v", node, err)
			}
			err := k.deleteVIPRouteIfExist(node)
			if err != nil {
				return fmt.Errorf("failed to delete %s route: %v", node, err)
			}
			logger.Info("Succeeded in deleting worker %s", node)
			return nil
		})
	}
	return eg.Wait()
}

func (k *KubeadmRuntime) deleteNode(node string) error {
	ssh, err := k.getHostSSHClient(node)
	if err != nil {
		return fmt.Errorf("failed to delete node: %v", err)
	}

	remoteCleanCmds := []string{fmt.Sprintf(RemoteCleanMasterOrNode, vlogToStr(k.Vlog)),
		fmt.Sprintf(RemoteRemoveAPIServerEtcHost, getRegistryHost(k.getRootfs(), k.GetMaster0IP())),
		fmt.Sprintf(RemoteRemoveAPIServerEtcHost, k.getAPIServerDomain())}
	address, err := utils.GetLocalHostAddresses()
	//if the node to be removed is the execution machine, kubelet, ~./kube and ApiServer host will be added
	if err != nil || !utils.IsLocalIP(node, address) {
		remoteCleanCmds = append(remoteCleanCmds, RemoveKubeConfig)
	} else {
		apiServerHost := getAPIServerHost(k.GetMaster0IP(), k.getAPIServerDomain())
		remoteCleanCmds = append(remoteCleanCmds, fmt.Sprintf(RemoteAddEtcHosts, apiServerHost, apiServerHost))
	}
	if err := ssh.CmdAsync(node, remoteCleanCmds...); err != nil {
		return err
	}
	//remove node
	if len(k.GetMasterIPList()) > 0 {
		hostname, err := k.isHostName(k.GetMaster0IP(), node)
		if err != nil {
			return err
		}
		ssh, err := k.getHostSSHClient(k.GetMaster0IP())
		if err != nil {
			return fmt.Errorf("failed to delete node on master0,%v", err)
		}
		if err := ssh.CmdAsync(k.GetMaster0IP(), fmt.Sprintf(KubeDeleteNode, strings.TrimSpace(hostname))); err != nil {
			return fmt.Errorf("delete node %s failed %v", hostname, err)
		}
	}

	return nil
}

func (k *KubeadmRuntime) checkMultiNetworkAddVIPRoute(node string) error {
	sshClient, err := k.getHostSSHClient(node)
	if err != nil {
		return err
	}
	result, err := sshClient.CmdToString(node, fmt.Sprintf(RemoteCheckRoute, node), "")
	if err != nil {
		return err
	}
	if result == utils.RouteOK {
		return nil
	}
	_, err = sshClient.Cmd(node, fmt.Sprintf(RemoteAddRoute, k.getVIP(), node))
	return err
}

func (k *KubeadmRuntime) deleteVIPRouteIfExist(node string) error {
	sshClient, err := k.getHostSSHClient(node)
	if err != nil {
		return err
	}
	_, err = sshClient.Cmd(node, fmt.Sprintf(RemoteDelRoute, k.getVIP(), node))
	return err
}

/*
Copyright 2019 The KubeOne Authors.

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

package tasks

import (
	"fmt"
	"time"

	"github.com/MakeNowJust/heredoc/v2"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	kubeoneapi "k8c.io/kubeone/pkg/apis/kubeone"
	"k8c.io/kubeone/pkg/runner"
	"k8c.io/kubeone/pkg/scripts"
	"k8c.io/kubeone/pkg/ssh"
	"k8c.io/kubeone/pkg/state"
	"k8c.io/kubeone/pkg/templates"
	"k8c.io/kubeone/pkg/templates/admissionconfig"
	encryptionproviders "k8c.io/kubeone/pkg/templates/encryptionproviders"

	"k8s.io/apimachinery/pkg/runtime"
)

func installPrerequisites(s *state.State) error {
	s.Logger.Infoln("Installing prerequisites...")

	if err := s.RunTaskOnAllNodes(installPrerequisitesOnNode, state.RunParallel); err != nil {
		return fmt.Errorf("failed to install prerequisites: %w", err)
	}

	return s.RunTaskOnAllNodes(func(ctx *state.State, node *kubeoneapi.HostConfig, conn ssh.Connection) error {
		ctx.Logger.Info("Pre-pull images")

		_, _, err := ctx.Runner.Run(
			heredoc.Doc(`
				sudo kubeadm config images pull --kubernetes-version {{ .KUBERNETES_VERSION }}
			`), runner.TemplateVariables{
				"KUBERNETES_VERSION": ctx.Cluster.Versions.Kubernetes,
			})

		return err
	}, state.RunParallel)
}

func generateConfigurationFiles(s *state.State) error {
	s.Configuration.AddFile("cfg/cloud-config", s.Cluster.CloudProvider.CloudConfig)

	if s.Cluster.Features.StaticAuditLog != nil && s.Cluster.Features.StaticAuditLog.Enable {
		if err := s.Configuration.AddFilePath("cfg/audit-policy.yaml", s.Cluster.Features.StaticAuditLog.Config.PolicyFilePath, s.ManifestFilePath); err != nil {
			return errors.Wrap(err, "unable to add policy file")
		}
	}
	if s.Cluster.Features.PodNodeSelector != nil && s.Cluster.Features.PodNodeSelector.Enable {
		admissionCfg, err := admissionconfig.NewAdmissionConfig(s.Cluster.Versions.Kubernetes, s.Cluster.Features.PodNodeSelector)
		if err != nil {
			return errors.Wrap(err, "failed to generate admissionconfiguration manifest")
		}
		s.Configuration.AddFile("cfg/admission-config.yaml", admissionCfg)

		if err := s.Configuration.AddFilePath("cfg/podnodeselector.yaml", s.Cluster.Features.PodNodeSelector.Config.ConfigFilePath, s.ManifestFilePath); err != nil {
			return errors.Wrap(err, "failed to add podnodeselector config file")
		}
	}

	if s.ShouldEnableEncryption() || s.EncryptionEnabled() {
		configFileName := s.GetEncryptionProviderConfigName()
		var config string
		// User provided custom config
		if s.Cluster.Features.EncryptionProviders.CustomEncryptionConfiguration != "" {
			config = s.Cluster.Features.EncryptionProviders.CustomEncryptionConfiguration
			s.Configuration.AddFile(fmt.Sprintf("cfg/%s", configFileName), config)
		} else if s.ShouldEnableEncryption() { // automatically generate config
			encryptionProvidersConfig, err := encryptionproviders.NewEncyrptionProvidersConfig(s)
			if err != nil {
				return err
			}
			config, err = templates.KubernetesToYAML([]runtime.Object{encryptionProvidersConfig})
			if err != nil {
				return err
			}
			s.Configuration.AddFile(fmt.Sprintf("cfg/%s", configFileName), config)
		}
	}

	return nil
}

func installPrerequisitesOnNode(s *state.State, node *kubeoneapi.HostConfig, _ ssh.Connection) error {
	logger := s.Logger.WithField("os", node.OperatingSystem)

	err := setupProxy(logger, s)
	if err != nil {
		return err
	}

	logger.Infoln("Installing kubeadm...")

	return errors.Wrap(installKubeadm(s, *node), "failed to install kubeadm")
}

func setupProxy(logger *logrus.Entry, s *state.State) error {
	logger.Infoln("Creating environment file...")
	if err := createEnvironmentFile(s); err != nil {
		return errors.Wrap(err, "failed to create environment file")
	}

	logger.Infoln("Configuring proxy...")
	if err := containerRuntimeEnvironment(s); err != nil {
		return errors.Wrap(err, "failed to configure proxy for docker daemon")
	}

	return nil
}

func createEnvironmentFile(s *state.State) error {
	cmd, err := scripts.EnvironmentFile(s.Cluster)
	if err != nil {
		return err
	}

	_, _, err = s.Runner.RunRaw(cmd)

	return err
}

func disableNMCloudSetup(s *state.State, node *kubeoneapi.HostConfig, _ ssh.Connection) error {
	if node.OperatingSystem != kubeoneapi.OperatingSystemNameRHEL {
		return nil
	}

	var allHosts = s.LiveCluster.ControlPlane
	allHosts = append(allHosts, s.LiveCluster.StaticWorkers...)
	for _, host := range allHosts {
		if node.ID == host.Config.ID && !host.Initialized() {
			cmd, err := scripts.DisableNMCloudSetup()
			if err != nil {
				return err
			}

			s.Logger.Infoln("Disable nm-cloud-setup... the node will be rebooted...")
			// Intentionally ignore error because restarting machines causes
			// the connection to error
			_, _, _ = s.Runner.RunRaw(cmd)

			timeout := 1 * time.Minute
			s.Logger.Infof("Waiting for %s before proceeding to give machines time to boot up...", timeout)
			time.Sleep(timeout)

			// NB: In some cases, KubeOne might not be able to re-use SSH connections
			// after rebooting nodes. Because of that, we close all connections here,
			// and then KubeOne will automatically reinitialize them on the next task.
			s.Runner.Conn.Close()
		}
	}

	return nil
}

func installKubeadm(s *state.State, node kubeoneapi.HostConfig) error {
	return runOnOS(s, node.OperatingSystem, map[kubeoneapi.OperatingSystemName]runOnOSFn{
		kubeoneapi.OperatingSystemNameAmazon:  installKubeadmAmazonLinux,
		kubeoneapi.OperatingSystemNameCentOS:  installKubeadmCentOS,
		kubeoneapi.OperatingSystemNameDebian:  installKubeadmDebian,
		kubeoneapi.OperatingSystemNameFlatcar: installKubeadmFlatcar,
		kubeoneapi.OperatingSystemNameRHEL:    installKubeadmCentOS,
		kubeoneapi.OperatingSystemNameUbuntu:  installKubeadmDebian,
	})
}

func installKubeadmDebian(s *state.State) error {
	cmd, err := scripts.KubeadmDebian(s.Cluster, s.ForceInstall)
	if err != nil {
		return err
	}

	_, _, err = s.Runner.RunRaw(cmd)

	return errors.WithStack(err)
}

func installKubeadmCentOS(s *state.State) error {
	cmd, err := scripts.KubeadmCentOS(s.Cluster, s.ForceInstall)
	if err != nil {
		return err
	}

	_, _, err = s.Runner.RunRaw(cmd)

	return errors.WithStack(err)
}

func installKubeadmAmazonLinux(s *state.State) error {
	cmd, err := scripts.KubeadmAmazonLinux(s.Cluster, s.ForceInstall)
	if err != nil {
		return err
	}

	_, _, err = s.Runner.RunRaw(cmd)

	return errors.WithStack(err)
}

func installKubeadmFlatcar(s *state.State) error {
	cmd, err := scripts.KubeadmFlatcar(s.Cluster)
	if err != nil {
		return err
	}

	_, _, err = s.Runner.RunRaw(cmd)

	return errors.WithStack(err)
}

func uploadConfigurationFiles(s *state.State) error {
	return s.RunTaskOnAllNodes(uploadConfigurationFilesToNode, state.RunParallel)
}

func uploadConfigurationFilesToNode(s *state.State, _ *kubeoneapi.HostConfig, conn ssh.Connection) error {
	s.Logger.Infoln("Uploading config files...")

	if err := s.Configuration.UploadTo(conn, s.WorkDir); err != nil {
		return errors.Wrap(err, "failed to upload")
	}

	cmd, err := scripts.SaveCloudConfig(s.WorkDir)
	if err != nil {
		return err
	}

	// move config files to their permanent locations
	_, _, err = s.Runner.RunRaw(cmd)
	if err != nil {
		return err
	}

	cmd, err = scripts.SaveAuditPolicyConfig(s.WorkDir)
	if err != nil {
		return err
	}
	_, _, err = s.Runner.RunRaw(cmd)
	if err != nil {
		return err
	}

	cmd, err = scripts.SavePodNodeSelectorConfig(s.WorkDir)
	if err != nil {
		return err
	}
	_, _, err = s.Runner.RunRaw(cmd)
	if err != nil {
		return err
	}

	cmd, err = scripts.SaveEncryptionProvidersConfig(s.WorkDir, s.GetEncryptionProviderConfigName())
	if err != nil {
		return err
	}
	_, _, err = s.Runner.RunRaw(cmd)
	if err != nil {
		return err
	}

	return nil
}

func containerRuntimeEnvironment(s *state.State) error {
	if s.Cluster.Proxy.HTTP == "" && s.Cluster.Proxy.HTTPS == "" && s.Cluster.Proxy.NoProxy == "" {
		return nil
	}

	s.Logger.Infoln("Configuring docker/containerd/kubelet environment...")
	cmd, err := scripts.DaemonsEnvironmentDropIn("docker", "containerd", "kubelet")
	if err != nil {
		return err
	}

	_, _, err = s.Runner.RunRaw(cmd)

	return err
}

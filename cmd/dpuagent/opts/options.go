/*
Copyright 2026 NVIDIA

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

package opts

import (
	"fmt"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
)

const (
	DPUAgentDir       = "/var/lib/dpf/dpuagent"
	DefaultCertDir    = DPUAgentDir + "/pki"
	DefaultKubeconfig = DPUAgentDir + "/kubeconfig"
)

type Options struct {
	ZeroTrustMode              bool
	ControlPlaneMTU            int32
	Kubeconfig                 string
	BootstrapKubeconfig        string
	CertDir                    string
	DPUName                    string
	DPUNamespace               string
	DPUUID                     string
	DPUFlavor                  string
	KubeadmSecretName          string
	KubeadmSecretNamespace     string
	SkipSysctl                 bool
	SkipNetworkConfig          bool
	SkipDNSConfig              bool
	SkipContainerdConfigration bool
	SkipSFConfig               bool
	SkipVFMac                  bool
	SkipOVSRawScript           bool
	SkipKernelCmdLine          bool
	SkipRemoveBuiltinKubelet   bool
	SkipConfigureKubelet       bool
	SkipStartKubelet           bool
	SkipRebootMethodDiscovery  bool

	// Granular ConfigureKubelet sub-step toggles. These let a node run the join
	// payload while skipping individual kubelet service side effects.
	SkipKubeletConfigCleanup    bool
	SkipKubeletStop             bool
	SkipKubeletSystemdDropIn    bool
	SkipKubeletCustomizedConfig bool
	SkipKubeletVersionCheck     bool
}

// ApplyFlavorSkips folds the DPUFlavor skip toggles into the options. The flavor already
// reaches the DPU as YAML, so this is the one place the two spellings have to agree.
func (o *Options) ApplyFlavorSkips(skip provisioningv1.DPUAgentSkipOperations) {
	for _, m := range []struct {
		option *bool
		flavor bool
	}{
		{&o.SkipSysctl, skip.Sysctl},
		{&o.SkipNetworkConfig, skip.NetworkConfig},
		{&o.SkipDNSConfig, skip.DNSConfig},
		{&o.SkipContainerdConfigration, skip.ContainerdConfig},
		{&o.SkipSFConfig, skip.SFConfig},
		{&o.SkipVFMac, skip.VFMac},
		{&o.SkipOVSRawScript, skip.OVSRawScript},
		{&o.SkipKernelCmdLine, skip.KernelCmdLine},
		{&o.SkipRemoveBuiltinKubelet, skip.RemoveBuiltinKubelet},
		{&o.SkipConfigureKubelet, skip.ConfigureKubelet},
		{&o.SkipStartKubelet, skip.StartKubelet},
		{&o.SkipRebootMethodDiscovery, skip.RebootMethodDiscovery},
		{&o.SkipKubeletConfigCleanup, skip.KubeletConfigCleanup},
		{&o.SkipKubeletStop, skip.KubeletStop},
		{&o.SkipKubeletSystemdDropIn, skip.KubeletSystemdDropIn},
		{&o.SkipKubeletCustomizedConfig, skip.KubeletCustomizedConfig},
		{&o.SkipKubeletVersionCheck, skip.KubeletVersionCheck},
	} {
		*m.option = *m.option || m.flavor
	}
}

func (o Options) Validate() error {
	if o.ZeroTrustMode {
		if o.Kubeconfig == "" && o.BootstrapKubeconfig == "" {
			return fmt.Errorf("kubeconfig or bootstrap-kubeconfig is required for zero trust mode")
		}
		if o.BootstrapKubeconfig != "" && o.CertDir == "" {
			return fmt.Errorf("cert-dir is required when bootstrap-kubeconfig is set")
		}
	}
	if o.ControlPlaneMTU < 0 {
		return fmt.Errorf("control plane MTU must be greater than 0: %d", o.ControlPlaneMTU)
	}
	if o.DPUName == "" {
		return fmt.Errorf("dpu name is required")
	}
	if o.DPUUID == "" {
		return fmt.Errorf("dpu uid is required")
	}
	if o.DPUFlavor == "" {
		return fmt.Errorf("dpu flavor is required")
	}
	if !o.SkipConfigureKubelet && o.KubeadmSecretName == "" {
		return fmt.Errorf("kubeadm secret name is required")
	}
	// The kubelet sub-step skips only take effect while ConfigureKubelet runs.
	// Setting them together with skip-configure-kubelet is contradictory.
	if o.SkipConfigureKubelet {
		subSteps := []struct {
			name string
			set  bool
		}{
			{"skip-kubelet-config-cleanup", o.SkipKubeletConfigCleanup},
			{"skip-kubelet-stop", o.SkipKubeletStop},
			{"skip-kubelet-systemd-drop-in", o.SkipKubeletSystemdDropIn},
			{"skip-kubelet-customized-config", o.SkipKubeletCustomizedConfig},
			{"skip-kubelet-version-check", o.SkipKubeletVersionCheck},
		}
		for _, s := range subSteps {
			if s.set {
				return fmt.Errorf("--%s has no effect when --skip-configure-kubelet is set", s.name)
			}
		}
	}
	return nil
}

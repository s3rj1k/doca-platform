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
	"reflect"
	"strings"
	"testing"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"

	. "github.com/onsi/gomega"
)

func validBaseOptions() Options {
	return Options{
		ControlPlaneMTU:   1500,
		DPUName:           "dpu",
		DPUUID:            "uid",
		DPUFlavor:         "/opt/dpf/dpuflavor.yaml",
		KubeadmSecretName: "kubeadm-join",
	}
}

// TestValidate_KubeletSubStepSkipRequiresConfigureKubelet sets one sub-step at a time so a
// row of Validate's table naming the wrong flag, or reading the wrong field, is visible.
func TestValidate_KubeletSubStepSkipRequiresConfigureKubelet(t *testing.T) {
	for _, tc := range []struct {
		flag string
		set  func(*Options)
	}{
		{"skip-kubelet-config-cleanup", func(o *Options) { o.SkipKubeletConfigCleanup = true }},
		{"skip-kubelet-stop", func(o *Options) { o.SkipKubeletStop = true }},
		{"skip-kubelet-systemd-drop-in", func(o *Options) { o.SkipKubeletSystemdDropIn = true }},
		{"skip-kubelet-customized-config", func(o *Options) { o.SkipKubeletCustomizedConfig = true }},
		{"skip-kubelet-version-check", func(o *Options) { o.SkipKubeletVersionCheck = true }},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			o := validBaseOptions()
			o.SkipConfigureKubelet = true
			o.KubeadmSecretName = "" // not required when configure-kubelet is skipped
			tc.set(&o)

			err := o.Validate()
			if err == nil {
				t.Fatalf("--%s alongside --skip-configure-kubelet was accepted", tc.flag)
			}
			if !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("error %q does not name --%s", err, tc.flag)
			}
		})
	}
}

func TestValidate_KubeletSubStepSkipsAllowedWhenConfiguring(t *testing.T) {
	o := validBaseOptions()
	o.SkipKubeletVersionCheck = true
	o.SkipKubeletStop = true
	o.SkipKubeletSystemdDropIn = true

	if err := o.Validate(); err != nil {
		t.Fatalf("unexpected error with sub-step skips while configuring kubelet: %v", err)
	}
}

func TestValidate_SkipConfigureKubeletAloneIsValid(t *testing.T) {
	o := validBaseOptions()
	o.SkipConfigureKubelet = true
	o.KubeadmSecretName = ""

	if err := o.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestApplyFlavorSkips covers the one place the flavor and the command line have to agree.
// The reflection walk means a toggle added to the API without a row here fails rather than
// being silently ignored on the card.
// optionForToggle spells out the mapping the test checks against, so it cannot be derived from
// the same code it is meant to verify. SkipContainerdConfigration is misspelled in Options.
var optionForToggle = map[string]string{
	"Sysctl":                  "SkipSysctl",
	"NetworkConfig":           "SkipNetworkConfig",
	"DNSConfig":               "SkipDNSConfig",
	"ContainerdConfig":        "SkipContainerdConfigration",
	"SFConfig":                "SkipSFConfig",
	"VFMac":                   "SkipVFMac",
	"OVSRawScript":            "SkipOVSRawScript",
	"KernelCmdLine":           "SkipKernelCmdLine",
	"RemoveBuiltinKubelet":    "SkipRemoveBuiltinKubelet",
	"ConfigureKubelet":        "SkipConfigureKubelet",
	"StartKubelet":            "SkipStartKubelet",
	"RebootMethodDiscovery":   "SkipRebootMethodDiscovery",
	"KubeletConfigCleanup":    "SkipKubeletConfigCleanup",
	"KubeletStop":             "SkipKubeletStop",
	"KubeletSystemdDropIn":    "SkipKubeletSystemdDropIn",
	"KubeletCustomizedConfig": "SkipKubeletCustomizedConfig",
	"KubeletVersionCheck":     "SkipKubeletVersionCheck",
}

func TestApplyFlavorSkips(t *testing.T) {
	allSet := func() provisioningv1.DPUAgentSkipOperations {
		skip := provisioningv1.DPUAgentSkipOperations{}
		v := reflect.ValueOf(&skip).Elem()
		for i := range v.NumField() {
			if v.Field(i).Kind() == reflect.Bool {
				v.Field(i).SetBool(true)
			}
		}
		return skip
	}

	t.Run("every toggle reaches an option", func(t *testing.T) {
		g := NewWithT(t)
		options := &Options{}
		options.ApplyFlavorSkips(allSet())

		v := reflect.ValueOf(options).Elem()
		typ := v.Type()
		set := 0
		for i := range v.NumField() {
			if !strings.HasPrefix(typ.Field(i).Name, "Skip") || v.Field(i).Kind() != reflect.Bool {
				continue
			}
			g.Expect(v.Field(i).Bool()).To(BeTrue(), "%s was not set from the flavor", typ.Field(i).Name)
			set++
		}

		skipFields := reflect.TypeOf(provisioningv1.DPUAgentSkipOperations{}).NumField()
		g.Expect(set).To(Equal(skipFields), "every API toggle should map to exactly one option")
	})

	// Setting them all cannot tell two swapped rows apart, so each is set on its own and has
	// to move exactly one option. This is what the flag name checking never covered.
	t.Run("each toggle reaches its own option and no other", func(t *testing.T) {
		skipType := reflect.TypeOf(provisioningv1.DPUAgentSkipOperations{})
		for i := range skipType.NumField() {
			g := NewWithT(t)

			skip := provisioningv1.DPUAgentSkipOperations{}
			reflect.ValueOf(&skip).Elem().Field(i).SetBool(true)

			options := &Options{}
			options.ApplyFlavorSkips(skip)

			v := reflect.ValueOf(options).Elem()
			typ := v.Type()
			var moved []string
			for j := range v.NumField() {
				if strings.HasPrefix(typ.Field(j).Name, "Skip") && v.Field(j).Kind() == reflect.Bool && v.Field(j).Bool() {
					moved = append(moved, typ.Field(j).Name)
				}
			}
			// Named, not just counted, or two swapped rows would still move one each.
			name := skipType.Field(i).Name
			want, ok := optionForToggle[name]
			g.Expect(ok).To(BeTrue(), "%s has no expected option, add it to optionForToggle", name)
			g.Expect(moved).To(ConsistOf(want), "%s should move %s", name, want)
		}
	})

	// A flag and a flavor asking for the same skip agree rather than one undoing the other.
	t.Run("a flag already set is not cleared by an unset toggle", func(t *testing.T) {
		g := NewWithT(t)
		options := &Options{SkipSysctl: true, SkipConfigureKubelet: true}
		options.ApplyFlavorSkips(provisioningv1.DPUAgentSkipOperations{})

		g.Expect(options.SkipSysctl).To(BeTrue())
		g.Expect(options.SkipConfigureKubelet).To(BeTrue())
	})

	t.Run("nothing set leaves the options alone", func(t *testing.T) {
		g := NewWithT(t)
		options := &Options{}
		options.ApplyFlavorSkips(provisioningv1.DPUAgentSkipOperations{})

		g.Expect(options.SkipSysctl).To(BeFalse())
		g.Expect(options.SkipKubeletVersionCheck).To(BeFalse())
	})
}

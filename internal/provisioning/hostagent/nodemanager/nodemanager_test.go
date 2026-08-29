//go:build linux

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

package nodemanager

import (
	"testing"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"

	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"
)

// TestSetPCIAddress covers the repair. A DPU that was treated as a host writes its own PCI
// numbering over the DPUDevice of the host that owns it, and nothing else ever corrects it.
func TestSetPCIAddress(t *testing.T) {
	for _, tc := range []struct {
		name        string
		existing    *string
		address     string
		wantChanged bool
		want        string
	}{
		{
			name:        "an unset address is recorded",
			address:     "0000:04:00.0",
			wantChanged: true,
			want:        "0000-04-00.0",
		},
		{
			name:        "the same address is left alone",
			existing:    ptr.To("0000-04-00.0"),
			address:     "0000:04:00.0",
			wantChanged: false,
			want:        "0000-04-00.0",
		},
		{
			// What a DPU wrote over its host, which used to persist until that host's
			// HostAgent Pod restarted.
			name:        "an address read from another machine is corrected",
			existing:    ptr.To("0000-08-00.0"),
			address:     "0000:04:00.0",
			wantChanged: true,
			want:        "0000-04-00.0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			dpuDevice := &provisioningv1.DPUDevice{}
			dpuDevice.Status.PCIAddress = tc.existing

			g.Expect(SetPCIAddress(dpuDevice, tc.address)).To(Equal(tc.wantChanged))
			g.Expect(dpuDevice.Status.PCIAddress).To(HaveValue(Equal(tc.want)))
		})
	}
}

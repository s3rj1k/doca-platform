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

package webhooks

import (
	"context"
	"fmt"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The dpu-agent refuses to start when a kubelet sub step skip is combined with skipping
// ConfigureKubelet, and DPUFlavor spec is immutable, so the API has to refuse it first.
var _ = Describe("DPUFlavor skipOperations", func() {
	var flavorWithSkips = func(name string, skips provisioningv1.DPUAgentSkipOperations) *provisioningv1.DPUFlavor {
		return &provisioningv1.DPUFlavor{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
			},
			Spec: provisioningv1.DPUFlavorSpec{
				DPUAgentConfig: provisioningv1.DPUAgentConfig{
					SkipOperations: skips,
				},
			},
		}
	}

	// The id is spelled out because a Kubernetes name cannot carry the field's capitals.
	subSteps := []struct {
		name string
		id   string
		set  func(*provisioningv1.DPUAgentSkipOperations)
	}{
		{"kubeletConfigCleanup", "config-cleanup", func(s *provisioningv1.DPUAgentSkipOperations) { s.KubeletConfigCleanup = true }},
		{"kubeletStop", "stop", func(s *provisioningv1.DPUAgentSkipOperations) { s.KubeletStop = true }},
		{"kubeletSystemdDropIn", "systemd-drop-in", func(s *provisioningv1.DPUAgentSkipOperations) { s.KubeletSystemdDropIn = true }},
		{"kubeletCustomizedConfig", "customized-config", func(s *provisioningv1.DPUAgentSkipOperations) { s.KubeletCustomizedConfig = true }},
		{"kubeletVersionCheck", "version-check", func(s *provisioningv1.DPUAgentSkipOperations) { s.KubeletVersionCheck = true }},
	}

	for _, subStep := range subSteps {
		It(fmt.Sprintf("rejects configureKubelet together with %s", subStep.name), func() {
			skips := provisioningv1.DPUAgentSkipOperations{ConfigureKubelet: true}
			subStep.set(&skips)

			err := k8sClient.Create(context.Background(), flavorWithSkips("flavor-reject-"+subStep.id, skips))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("kubelet sub step skips have no effect"))
		})

		It(fmt.Sprintf("accepts %s on its own", subStep.name), func() {
			skips := provisioningv1.DPUAgentSkipOperations{}
			subStep.set(&skips)

			obj := flavorWithSkips("flavor-accept-"+subStep.id, skips)
			Expect(k8sClient.Create(context.Background(), obj)).To(Succeed())
			DeferCleanup(k8sClient.Delete, context.Background(), obj)
		})
	}

	It("accepts configureKubelet on its own", func() {
		obj := flavorWithSkips("flavor-accept-configure-kubelet",
			provisioningv1.DPUAgentSkipOperations{ConfigureKubelet: true})
		Expect(k8sClient.Create(context.Background(), obj)).To(Succeed())
		DeferCleanup(k8sClient.Delete, context.Background(), obj)
	})

	// Every field is absent from the object when unset, so the rule has to tolerate that.
	It("accepts a flavor that skips nothing", func() {
		obj := flavorWithSkips("flavor-accept-empty", provisioningv1.DPUAgentSkipOperations{})
		Expect(k8sClient.Create(context.Background(), obj)).To(Succeed())
		DeferCleanup(k8sClient.Delete, context.Background(), obj)
	})
})

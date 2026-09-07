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

package joinpayload

import (
	"bytes"
	"fmt"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	"github.com/nvidia/doca-platform/cmd/dpuagent/opts"
	"github.com/nvidia/doca-platform/internal/provisioning/dpuagent/operations"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace  = "dpf-operator-system"
	testSecretName = "dpu-1-join"
)

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(provisioningv1.AddToScheme(s))
	return s
}

func joinSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testSecretName, Namespace: testNamespace},
		Data:       data,
	}
}

// contextFor builds a Context holding a fake client seeded with fakeObjects and a DPU
// carrying conditions, plus the options the operation reads.
func contextFor(fakeObjects []runtime.Object, conditions []metav1.Condition) *operations.Context {
	builder := fake.NewClientBuilder().WithScheme(newTestScheme())
	for _, o := range fakeObjects {
		builder = builder.WithRuntimeObjects(o)
	}
	dpu := &provisioningv1.DPU{ObjectMeta: metav1.ObjectMeta{Name: "dpu-1", Namespace: testNamespace}}
	if conditions != nil {
		dpu.Status.AgentStatus = &provisioningv1.AgentStatus{Conditions: conditions}
	}
	return &operations.Context{
		Client:    builder.Build(),
		LatestDPU: dpu,
		Options: opts.Options{
			RunJoinPayload:         true,
			KubeadmSecretName:      testSecretName,
			KubeadmSecretNamespace: testNamespace,
		},
	}
}

var _ = Describe("RunJoinPayload", func() {
	Context("ShouldSkip", func() {
		It("skips unless the cluster manager asked for it", func() {
			operation := &RunJoinPayload{}
			Expect(operation.ShouldSkip(&operations.Context{
				Options: opts.Options{RunJoinPayload: false},
			})).To(BeTrue())
			Expect(operation.ShouldSkip(&operations.Context{
				Options: opts.Options{RunJoinPayload: true},
			})).To(BeFalse())
		})
	})

	Context("Execute", func() {
		It("runs the payload from the secret", func() {
			var ran string
			operation := &RunJoinPayload{
				runBash: func(cmd string) (bytes.Buffer, bytes.Buffer, error) {
					ran = cmd
					return bytes.Buffer{}, bytes.Buffer{}, nil
				},
			}
			optCtx := contextFor(
				[]runtime.Object{joinSecret(map[string][]byte{"join": []byte("k0s install worker")})},
				nil,
			)

			Expect(operation.Execute(ctx, optCtx)).To(Succeed())
			Expect(ran).To(Equal("k0s install worker"))
		})

		It("does not run twice once the condition is True", func() {
			ran := false
			operation := &RunJoinPayload{
				runBash: func(string) (bytes.Buffer, bytes.Buffer, error) {
					ran = true
					return bytes.Buffer{}, bytes.Buffer{}, nil
				},
			}
			optCtx := contextFor(
				[]runtime.Object{joinSecret(map[string][]byte{"join": []byte("k0s install worker")})},
				[]metav1.Condition{{
					Type:               conditionType,
					Status:             metav1.ConditionTrue,
					Reason:             "Success",
					LastTransitionTime: metav1.Now(),
				}},
			)

			Expect(operation.Execute(ctx, optCtx)).To(Succeed())
			Expect(ran).To(BeFalse(), "a second run would re-register the node")
		})

		It("fails when the secret is missing", func() {
			operation := &RunJoinPayload{}
			optCtx := contextFor(nil, nil)

			err := operation.Execute(ctx, optCtx)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get join payload secret"))
		})

		It("fails when the secret has no payload key", func() {
			operation := &RunJoinPayload{}
			optCtx := contextFor(
				[]runtime.Object{joinSecret(map[string][]byte{"other": []byte("x")})},
				nil,
			)

			err := operation.Execute(ctx, optCtx)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`does not contain key "join"`))
		})

		It("fails when the payload is empty", func() {
			operation := &RunJoinPayload{}
			optCtx := contextFor(
				[]runtime.Object{joinSecret(map[string][]byte{"join": {}})},
				nil,
			)

			err := operation.Execute(ctx, optCtx)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`has an empty key "join"`))
		})

		It("surfaces the payload's own output when it fails", func() {
			operation := &RunJoinPayload{
				runBash: func(string) (bytes.Buffer, bytes.Buffer, error) {
					return *bytes.NewBufferString("some stdout"),
						*bytes.NewBufferString("token expired"),
						fmt.Errorf("exit status 1")
				},
			}
			optCtx := contextFor(
				[]runtime.Object{joinSecret(map[string][]byte{"join": []byte("k0s install worker")})},
				nil,
			)

			err := operation.Execute(ctx, optCtx)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("token expired"))
			Expect(err.Error()).To(ContainSubstring("some stdout"))
		})

		It("fails when the DPU was not retrieved", func() {
			operation := &RunJoinPayload{}
			err := operation.Execute(ctx, &operations.Context{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("latest DPU not retrieved"))
		})
	})
})

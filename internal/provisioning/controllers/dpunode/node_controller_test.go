/*
  COPYRIGHT 2026 NVIDIA
  Licensed under the Apache License, Version 2.0 (the License);
  you may not use this file except in compliance with the License.
  You may obtain a copy of the License at
      http://www.apache.org/licenses/LICENSE-2.0
  Unless required by applicable law or agreed to in writing, software
  distributed under the License is distributed on an AS IS BASIS,
  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
  See the License for the specific language governing permissions and
  limitations under the License.
*/

package dpunode

import (
	"context"
	"fmt"

	operatorv1 "github.com/nvidia/doca-platform/api/operator/v1alpha1"
	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	operatorcontroller "github.com/nvidia/doca-platform/internal/operator/controllers"
	dnutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/dpunode/util"
	cutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Helper function to create a DPU-enabled node
func createDPUNode() *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
			Labels: map[string]string{
				cutil.NodeSelectorLabel: "true",
			},
		},
	}

	return node
}

// Helper function to create a DPFOperatorConfig
func createDPFOperatorConfig() *operatorv1.DPFOperatorConfig {
	return &operatorv1.DPFOperatorConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      operatorcontroller.DefaultDPFOperatorConfigSingletonName,
			Namespace: operatorcontroller.DefaultDPFOperatorConfigSingletonNamespace,
		},
		Spec: operatorv1.DPFOperatorConfigSpec{DeploymentMode: operatorv1.DeploymentModeHostTrusted},
	}
}

// errorInjectingClient wraps a client and injects errors for specific operations
type errorInjectingClient struct {
	client.Client
	getError    error
	updateError error
	deleteError error
	getFunc     func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
	updateFunc  func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error
	deleteFunc  func(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error
}

func (c *errorInjectingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if c.getFunc != nil {
		return c.getFunc(ctx, key, obj, opts...)
	}
	if c.getError != nil {
		return c.getError
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *errorInjectingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.updateFunc != nil {
		return c.updateFunc(ctx, obj, opts...)
	}
	if c.updateError != nil {
		return c.updateError
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c *errorInjectingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if c.deleteFunc != nil {
		return c.deleteFunc(ctx, obj, opts...)
	}
	if c.deleteError != nil {
		return c.deleteError
	}
	return c.Client.Delete(ctx, obj, opts...)
}

var _ = Describe("NodeReconciler Reconcile - Node Deletion", func() {
	var (
		reconciler *NodeReconciler
		fakeClient client.Client
		ctx        context.Context
		scheme     *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = provisioningv1.AddToScheme(scheme)
		_ = operatorv1.AddToScheme(scheme)

		fakeClient = fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&corev1.Node{}, &provisioningv1.DPUNode{}).
			Build()

		reconciler = &NodeReconciler{
			Client: fakeClient,
		}
	})

	Context("when node does not exist", func() {
		It("should return without error", func() {
			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "non-existent-node"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("when node is not DPU-enabled", func() {
		It("should return without processing", func() {
			// Create a node without DPU label
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "non-dpu-node",
				},
			}
			Expect(fakeClient.Create(ctx, node)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "non-dpu-node"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	// A DPU carries the same PCI IDs as the card in its host, so NFD can label it as if it were
	// a host. DMS cannot reach a DPU from a DPU, so anything that landed there has to go.
	Context("when the node is itself a DPU", func() {
		var dpuNode *corev1.Node

		BeforeEach(func() {
			Expect(fakeClient.Create(ctx, createDPFOperatorConfig())).To(Succeed())

			dpuNode = createDPUNode()
			dpuNode.Labels[cutil.DPUNodeLabel] = cutil.DPUNodeLabelValue
			Expect(fakeClient.Create(ctx, dpuNode)).To(Succeed())
		})

		It("should not deploy a HostAgent Pod", func() {
			// Set, so the Pod's absence is the fix at work rather than CreateHostAgentPod
			// refusing for want of a registry address.
			reconciler.Options = dnutil.HostAgentPodOptions{BFBRegistryAddress: "registry.example.com:5000"}

			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: dpuNode.Name},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			pod := &corev1.Pod{}
			err = fakeClient.Get(ctx, types.NamespacedName{
				Name:      cutil.GenerateHostAgentPodName(dpuNode),
				Namespace: operatorcontroller.DefaultDPFOperatorConfigSingletonNamespace,
			}, pod)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		// The HostAgent Pod registers a DPUNode for whatever node it ran on. On a DPU that
		// object is spurious, and it stays behind after the Pod is gone.
		It("should delete the DPUNode the HostAgent Pod registered for itself", func() {
			spurious := &provisioningv1.DPUNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      dpuNode.Name,
					Namespace: operatorcontroller.DefaultDPFOperatorConfigSingletonNamespace,
				},
			}
			Expect(fakeClient.Create(ctx, spurious)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: dpuNode.Name},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			err = fakeClient.Get(ctx, client.ObjectKeyFromObject(spurious), &provisioningv1.DPUNode{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the spurious DPUNode should be deleted")
		})

		It("should ignore a node whose label is not true", func() {
			dpuNode.Labels[cutil.DPUNodeLabel] = "false"
			Expect(fakeClient.Update(ctx, dpuNode)).To(Succeed())
			reconciler.Options = dnutil.HostAgentPodOptions{BFBRegistryAddress: "registry.example.com:5000"}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: dpuNode.Name},
			})
			Expect(err).NotTo(HaveOccurred())

			pod := &corev1.Pod{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{
				Name:      cutil.GenerateHostAgentPodName(dpuNode),
				Namespace: operatorcontroller.DefaultDPFOperatorConfigSingletonNamespace,
			}, pod)).To(Succeed(), "a node opted out should be treated as a host again")
		})

		It("should delete a HostAgent Pod that was deployed before the label was applied", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cutil.GenerateHostAgentPodName(dpuNode),
					Namespace: operatorcontroller.DefaultDPFOperatorConfigSingletonNamespace,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "dms", Image: "dms:latest"}},
				},
			}
			Expect(fakeClient.Create(ctx, pod)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: dpuNode.Name},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			err = fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the HostAgent Pod should be deleted")
		})
	})

	Context("when DPU node is being deleted", func() {
		BeforeEach(func() {
			// Create DPFOperatorConfig for all deletion scenarios
			// This is required in case the code tries to call deployHostAgent
			dpfConfig := createDPFOperatorConfig()
			Expect(fakeClient.Create(ctx, dpfConfig)).To(Succeed())
		})

		It("should handle deletion when DPUNode does not exist", func() {
			// Create DPU-enabled node with finalizer
			node := createDPUNode()
			Expect(fakeClient.Create(ctx, node)).To(Succeed())

			// Delete the node (this sets DeletionTimestamp in real scenarios)
			Expect(fakeClient.Delete(ctx, node)).To(Succeed())

			// Reconcile should handle the case gracefully when DPUNode doesn't exist
			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "test-node"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("should call handleReconcileDelete when node has DeletionTimestamp", func() {
			// Create DPU-enabled node
			node := createDPUNode()
			// Add finalizer to prevent immediate deletion
			node.Finalizers = []string{"test-finalizer"}
			Expect(fakeClient.Create(ctx, node)).To(Succeed())

			// Create corresponding DPUNode
			dpuNode := &provisioningv1.DPUNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-node",
					Namespace: operatorcontroller.DefaultDPFOperatorConfigSingletonNamespace,
				},
			}
			Expect(fakeClient.Create(ctx, dpuNode)).To(Succeed())

			// Delete the node to set DeletionTimestamp (node still exists because of finalizer)
			Expect(fakeClient.Delete(ctx, node)).To(Succeed())

			// Verify node exists with DeletionTimestamp
			fetchedNode := &corev1.Node{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: "test-node"}, fetchedNode)).To(Succeed())
			Expect(fetchedNode.DeletionTimestamp.IsZero()).To(BeFalse(), "Node should have DeletionTimestamp set")

			// Reconcile should call handleReconcileDelete (lines 69-73)
			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "test-node"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			// Verify DPUNode has been deleted (no finalizers, so deleted immediately)
			updatedDPUNode := &provisioningv1.DPUNode{}
			err = fakeClient.Get(ctx, types.NamespacedName{
				Name:      "test-node",
				Namespace: operatorcontroller.DefaultDPFOperatorConfigSingletonNamespace,
			}, updatedDPUNode)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "DPUNode should be deleted")
		})

		It("should delete DPUNode when it exists", func() {
			// Create DPU-enabled node with finalizer
			node := createDPUNode()
			// Add finalizer to prevent immediate deletion when Delete is called
			node.Finalizers = []string{"test-finalizer"}
			Expect(fakeClient.Create(ctx, node)).To(Succeed())

			// Create corresponding DPUNode
			dpuNode := &provisioningv1.DPUNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-node",
					Namespace: operatorcontroller.DefaultDPFOperatorConfigSingletonNamespace,
				},
			}
			Expect(fakeClient.Create(ctx, dpuNode)).To(Succeed())

			// Delete the node (this sets DeletionTimestamp because node has finalizer)
			Expect(fakeClient.Delete(ctx, node)).To(Succeed())

			// Reconcile should delete the DPUNode
			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "test-node"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			// Verify DPUNode has been deleted (since it has no finalizers, it's removed immediately)
			updatedDPUNode := &provisioningv1.DPUNode{}
			err = fakeClient.Get(ctx, types.NamespacedName{
				Name:      "test-node",
				Namespace: operatorcontroller.DefaultDPFOperatorConfigSingletonNamespace,
			}, updatedDPUNode)

			// DPUNode should be deleted immediately since it has no finalizers
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("should return error when getting DPUNode fails during deletion", func() {
			// Create DPU-enabled node with finalizer
			node := createDPUNode()
			Expect(fakeClient.Create(ctx, node)).To(Succeed())

			// Delete the node (this sets DeletionTimestamp)
			Expect(fakeClient.Delete(ctx, node)).To(Succeed())

			// Create error injecting client that fails on DPUNode Get
			errorClient := &errorInjectingClient{
				Client: fakeClient,
				getFunc: func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					// Return error only for DPUNode Get operations during deletion
					if _, ok := obj.(*provisioningv1.DPUNode); ok {
						return fmt.Errorf("simulated DPUNode get error")
					}
					// Pass through for other Get operations (e.g., Node)
					return fakeClient.Get(ctx, key, obj, opts...)
				},
			}

			errorReconciler := &NodeReconciler{
				Client: errorClient,
			}

			// Reconcile should return error
			result, err := errorReconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "test-node"},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get DPUNode"))
			Expect(err.Error()).To(ContainSubstring("simulated DPUNode get error"))
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("should return error when deleting DPUNode fails during node deletion", func() {
			// Create DPU-enabled node with finalizer
			node := createDPUNode()
			Expect(fakeClient.Create(ctx, node)).To(Succeed())

			// Create corresponding DPUNode
			dpuNode := &provisioningv1.DPUNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-node",
					Namespace: operatorcontroller.DefaultDPFOperatorConfigSingletonNamespace,
				},
			}
			Expect(fakeClient.Create(ctx, dpuNode)).To(Succeed())

			// Delete the node (this sets DeletionTimestamp)
			Expect(fakeClient.Delete(ctx, node)).To(Succeed())

			// Create error injecting client that fails on DPUNode Delete
			errorClient := &errorInjectingClient{
				Client: fakeClient,
				deleteFunc: func(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
					// Return error only for DPUNode Delete operations
					if _, ok := obj.(*provisioningv1.DPUNode); ok {
						return fmt.Errorf("simulated DPUNode delete error")
					}
					// Pass through for other Delete operations
					return fakeClient.Delete(ctx, obj, opts...)
				},
			}

			errorReconciler := &NodeReconciler{
				Client: errorClient,
			}

			// Reconcile should return error
			result, err := errorReconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "test-node"},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to delete DPUNode"))
			Expect(err.Error()).To(ContainSubstring("simulated DPUNode delete error"))
			Expect(result).To(Equal(ctrl.Result{}))

			// Verify DPUNode still exists (not deleted due to error)
			updatedDPUNode := &provisioningv1.DPUNode{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{
				Name:      "test-node",
				Namespace: operatorcontroller.DefaultDPFOperatorConfigSingletonNamespace,
			}, updatedDPUNode)).To(Succeed())
		})
	})
})

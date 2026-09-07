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

package state

import (
	"context"
	"testing"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	dutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/dpu/util"
	k0smotronv1 "github.com/nvidia/doca-platform/third_party/forked/github.com/k0sproject/k0smotron/api/k0smotron.io/v1beta2"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestDeleteNodeJoinBootstrapTokens covers which side of the pair a token is revoked from. A
// k0smotron token goes with the request in this cluster, a kubeadm one from the child cluster.
func TestDeleteNodeJoinBootstrapTokens(t *testing.T) {
	const (
		namespace   = "dpf-operator-system"
		clusterName = "dpu-cplane"
		dpuName     = "dpu-1"
	)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(provisioningv1.AddToScheme(scheme))
	utilruntime.Must(k0smotronv1.AddToScheme(scheme))

	dpu := func() *provisioningv1.DPU {
		return &provisioningv1.DPU{
			ObjectMeta: metav1.ObjectMeta{Name: dpuName, Namespace: namespace},
			Spec: provisioningv1.DPUSpec{
				Cluster: provisioningv1.K8sCluster{Name: clusterName, Namespace: namespace},
			},
		}
	}
	dpuCluster := func(clusterType string) *provisioningv1.DPUCluster {
		return &provisioningv1.DPUCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: provisioningv1.DPUClusterSpec{
				Type:       clusterType,
				Kubeconfig: clusterName + "-admin-kubeconfig",
			},
		}
	}
	joinTokenRequest := func() *k0smotronv1.JoinTokenRequest {
		return &k0smotronv1.JoinTokenRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dutil.K0smotronJoinTokenRequestName(dpu()),
				Namespace: namespace,
			},
			Spec: k0smotronv1.JoinTokenRequestSpec{ClusterName: clusterName, Role: "worker"},
		}
	}
	build := func(objects ...client.Object) client.Client {
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	}

	t.Run("revokes a k0smotron token by deleting the request that minted it", func(t *testing.T) {
		g := NewWithT(t)
		request := joinTokenRequest()
		c := build(dpuCluster(dutil.K0smotronClusterType), request)

		g.Expect(deleteNodeJoinBootstrapTokens(ctx, c, dpu())).To(Succeed())

		err := c.Get(ctx, client.ObjectKeyFromObject(request), &k0smotronv1.JoinTokenRequest{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	t.Run("tolerates a k0smotron request that is already gone", func(t *testing.T) {
		g := NewWithT(t)
		c := build(dpuCluster(dutil.K0smotronClusterType))

		g.Expect(deleteNodeJoinBootstrapTokens(ctx, c, dpu())).To(Succeed())
	})

	t.Run("reaches into the child cluster for a kamaji node", func(t *testing.T) {
		g := NewWithT(t)
		// Guards the k0smotron branch from widening. There is no kubeconfig Secret here, so
		// only the child cluster path can fail, which is how the branch taken is visible.
		request := joinTokenRequest()
		c := build(dpuCluster(string(provisioningv1.KamajiCluster)), request)

		err := deleteNodeJoinBootstrapTokens(ctx, c, dpu())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("failed to create client for DPU cluster"))

		// The request is left alone, since a kamaji cluster does not own it.
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(request), &k0smotronv1.JoinTokenRequest{})).To(Succeed())
	})

	t.Run("skips an unassigned DPU", func(t *testing.T) {
		g := NewWithT(t)
		request := joinTokenRequest()
		c := build(request)

		unassigned := dpu()
		unassigned.Spec.Cluster = provisioningv1.K8sCluster{}
		g.Expect(deleteNodeJoinBootstrapTokens(ctx, c, unassigned)).To(Succeed())

		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(request), &k0smotronv1.JoinTokenRequest{})).To(Succeed())
	})

	t.Run("skips a DPUCluster that has already gone", func(t *testing.T) {
		g := NewWithT(t)
		c := build()

		g.Expect(deleteNodeJoinBootstrapTokens(ctx, c, dpu())).To(Succeed())
	})
}

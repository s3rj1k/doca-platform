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

package state_test

import (
	"context"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	"github.com/nvidia/doca-platform/internal/provisioning/controllers/allocator"
	"github.com/nvidia/doca-platform/internal/provisioning/controllers/dpu/state"
	dutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/dpu/util"
	cutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/util"
	"github.com/nvidia/doca-platform/pkg/dpucluster"
	testutils "github.com/nvidia/doca-platform/test/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var _ = Describe("DPU: deleting", func() {
	const (
		dpuName       = "dpu-deleting-finalizer-test"
		dpuDeviceName = "dpu-device-deleting-finalizer-test"
	)

	It("deleting state should remove finalizers from DpuDevice", func() {
		By("prepare DPUDevice CR with finalizer (as added by DPU controller when DPU uses it)")
		dpuDevice := dpuDeviceObj(dpuDeviceName)
		controllerutil.AddFinalizer(dpuDevice, provisioningv1.DPUDeviceFinalizer)
		createObject(dpuDevice)

		By("prepare DPU CR in OsInstalling state")
		dpu := dpuObj(dpuName)
		dpu.Spec.DPUDeviceName = dpuDevice.Name
		dpu.Spec.NodeEffect = provisioningv1.NodeEffect{Action: provisioningv1.Action{NoEffect: ptr.To(true)}}
		dpu.Spec.Cluster.Name = "" // skip deleteNode in Deleting
		dpu.Status.Phase = provisioningv1.DPUOSInstalling
		dpu.Status.DPUInstallInterface = ptr.To(string(provisioningv1.InstallViaGNOI))
		createObject(dpu)

		By("delete DpuDevice")
		Expect(k8sClient.Delete(ctx, dpuDevice)).To(Succeed())

		By("verify DpuDevice still exists (finalizer blocks deletion)")
		gotDevice := &provisioningv1.DPUDevice{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: dpuDevice.Namespace, Name: dpuDevice.Name}, gotDevice)).To(Succeed())
		Expect(gotDevice.DeletionTimestamp).NotTo(BeNil())
		Expect(controllerutil.ContainsFinalizer(gotDevice, provisioningv1.DPUDeviceFinalizer)).To(BeTrue())

		By("move to Deleting state and run cleanup")
		ctrlCtx := &dutil.ControllerContext{
			Client:               k8sClient,
			DPUInProvisioningMap: dutil.NewDPUInProvisioningMap(10),
			ClusterAllocator:     &noOpAllocator{},
		}
		_, err := state.Deleting(ctx, dpu, ctrlCtx)
		Expect(err).NotTo(HaveOccurred())

		By("verify DpuDevice is deleted after finalizer removal")
		Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: dpuDevice.Namespace, Name: dpuDevice.Name}, &provisioningv1.DPUDevice{})
			return apierrors.IsNotFound(err)
		}).WithTimeout(5 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())
	})
})

// noOpAllocator implements allocator.Allocator for tests; ClusterAllocator must be non-nil in Deleting().
type noOpAllocator struct{}

func (n *noOpAllocator) Allocate(context.Context, *provisioningv1.DPU) (allocator.AllocateResult, error) {
	return types.NamespacedName{}, nil
}
func (n *noOpAllocator) SaveAssignedDPU(*provisioningv1.DPU)      {}
func (n *noOpAllocator) SaveCluster(*provisioningv1.DPUCluster)   {}
func (n *noOpAllocator) ReleaseDPU(*provisioningv1.DPU)           {}
func (n *noOpAllocator) RemoveCluster(*provisioningv1.DPUCluster) {}
func (n *noOpAllocator) GetDPUsCount(*provisioningv1.DPUCluster) int {
	return 0
}

// The join token lives in the DPU cluster where nothing owns it, and the annotation on the join
// Secret is the only record of which one it is. Deleting had no test for this at all.
var _ = Describe("DPU: deleting revokes the join token", func() {
	var (
		dpu              *provisioningv1.DPU
		dpuCluster       *provisioningv1.DPUCluster
		dpuClusterClient client.Client
		joinSecret       *corev1.Secret
		tokenSecret      *corev1.Secret
	)

	const tokenID = "abc123"

	// Deleting releases the DPU from both of these before it reaches the token.
	revokeCtrlCtx := func() *dutil.ControllerContext {
		return &dutil.ControllerContext{
			Client:               k8sClient,
			DPUInProvisioningMap: dutil.NewDPUInProvisioningMap(10),
			ClusterAllocator:     &noOpAllocator{},
		}
	}

	BeforeEach(func() {
		dpuDevice := dpuDeviceObj("dpu-device-revoke-test")
		createObject(dpuDevice)

		dpuCluster = dpuClusterObj("dpu-cluster-revoke-test", "static")
		kamajiSecret, err := testutils.GetFakeKamajiClusterSecretFromEnvtest(*dpuCluster, cfg)
		Expect(err).ToNot(HaveOccurred())
		createObject(kamajiSecret)
		createObject(dpuCluster)
		dpuClusterClient, err = dpucluster.NewConfig(k8sClient, dpuCluster).Client(ctx)
		Expect(err).ToNot(HaveOccurred())

		// What the generator would have minted and what PrepareBFB would have stamped.
		tokenSecret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      dutil.BootstrapTokenSecretName(tokenID),
			Namespace: metav1.NamespaceSystem,
		}}
		Expect(dpuClusterClient.Create(ctx, tokenSecret)).To(Succeed())
		// The DPU cluster here is the same envtest apiserver, so a token a spec does not
		// revoke would otherwise collide with the next one.
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(dpuClusterClient.Delete(ctx, tokenSecret))).To(Succeed())
		})

		dpu = dpuObj("dpu-revoke-test")
		dpu.Spec.DPUDeviceName = dpuDevice.Name
		dpu.Spec.NodeEffect = provisioningv1.NodeEffect{Action: provisioningv1.Action{NoEffect: ptr.To(true)}}
		dpu.Spec.Cluster.Name = dpuCluster.Name
		dpu.Spec.Cluster.Namespace = dpuCluster.Namespace
		dpu.Status.Phase = provisioningv1.DPUDeleting
		dpu.Status.DPUInstallInterface = ptr.To(string(provisioningv1.InstallViaGNOI))
		createObject(dpu)

		joinSecret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      cutil.KubeadmJoinSecretName(dpu.Name),
			Namespace: dpu.Namespace,
			Annotations: map[string]string{
				cutil.JoinTokenIDAnnotation: tokenID,
			},
		}}
		createObject(joinSecret)
	})

	It("deletes the bootstrap token from the DPU cluster", func() {
		_, err := state.Deleting(ctx, dpu, revokeCtrlCtx())
		Expect(err).NotTo(HaveOccurred())

		err = dpuClusterClient.Get(ctx, client.ObjectKeyFromObject(tokenSecret), &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the token the DPU was given should be revoked")
	})

	// The annotation goes with the join Secret, so revoking has to happen first or the only
	// record of which token to revoke is destroyed.
	It("revokes before the join Secret that names the token is deleted", func() {
		_, err := state.Deleting(ctx, dpu, revokeCtrlCtx())
		Expect(err).NotTo(HaveOccurred())

		err = dpuClusterClient.Get(ctx, client.ObjectKeyFromObject(tokenSecret), &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(joinSecret), &corev1.Secret{}))
		}).WithTimeout(10 * time.Second).Should(BeTrue())
	})

	// A token nobody revoked is a live credential, so a failure has to stop the deletion
	// rather than let the Secret naming it go.
	It("stops the deletion when the DPU cluster cannot be reached", func() {
		// Its kubeconfig is immutable, so the Secret behind it goes instead.
		kubeconfigSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      dpuCluster.Spec.Kubeconfig,
			Namespace: dpuCluster.Namespace,
		}}
		Expect(k8sClient.Delete(ctx, kubeconfigSecret)).To(Succeed())

		_, err := state.Deleting(ctx, dpu, revokeCtrlCtx())
		Expect(err).To(HaveOccurred())

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(joinSecret), &corev1.Secret{})).To(Succeed(),
			"the Secret naming the token must survive so the next pass can retry")
	})

	// A token already gone is a revoked token, so a retry after a partial deletion has to
	// finish rather than block on the Secret it cannot find.
	It("finishes when the token is already gone", func() {
		Expect(dpuClusterClient.Delete(ctx, tokenSecret)).To(Succeed())

		_, err := state.Deleting(ctx, dpu, revokeCtrlCtx())
		Expect(err).ToNot(HaveOccurred())

		Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(joinSecret), &corev1.Secret{})
			return apierrors.IsNotFound(err)
		}).WithTimeout(10 * time.Second).Should(BeTrue())
	})
})

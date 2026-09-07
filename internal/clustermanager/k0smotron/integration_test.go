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

package k0smotron

import (
	"context"
	"testing"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	k0smotronv1 "github.com/nvidia/doca-platform/third_party/forked/github.com/k0sproject/k0smotron/api/k0smotron.io/v1beta2"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestHandlerAgainstAPIServer runs the handler's whole cycle against a real API server. The
// fake client validates nothing and tracks no ownership, so neither is covered elsewhere.
func TestHandlerAgainstAPIServer(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "handler-lifecycle"}}
	g.Expect(envtestClient.Create(ctx, namespace)).To(Succeed())

	handler := NewHandler(envtestClient)

	dc := testDPUCluster()
	dc.Namespace = namespace.Name
	dc.UID = ""
	g.Expect(envtestClient.Create(ctx, dc)).To(Succeed())

	key := client.ObjectKeyFromObject(dc)

	t.Run("creates the control plane and waits for its kubeconfig", func(t *testing.T) {
		g := NewWithT(t)

		// A missing kubeconfig is the normal first pass, reported as an error so the
		// caller comes back rather than treating the cluster as done.
		name, conds, err := handler.ReconcileCluster(ctx, dc)
		g.Expect(err).To(HaveOccurred())
		g.Expect(name).To(BeEmpty())
		created := conditionOfType(conds, string(provisioningv1.ConditionCreated))
		g.Expect(created.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(created.Reason).To(Equal("KubeconfigNotReady"))
		g.Expect(conditionOfType(conds, ConditionSpecApplied).Status).To(Equal(metav1.ConditionTrue))

		cluster := &k0smotronv1.Cluster{}
		g.Expect(envtestClient.Get(ctx, key, cluster)).To(Succeed())
		g.Expect(cluster.Spec.Version).To(Equal(DefaultK0sVersion))
		g.Expect(cluster.Spec.Service.Type).To(Equal(corev1.ServiceTypeNodePort))

		// The DPUCluster owns it, so tearing the DPUCluster down takes the control plane.
		owner := metav1.GetControllerOf(cluster)
		g.Expect(owner).NotTo(BeNil())
		g.Expect(owner.Kind).To(Equal("DPUCluster"))
		g.Expect(owner.Name).To(Equal(dc.Name))
		g.Expect(owner.UID).To(Equal(dc.UID))

		profiles, found, err := unstructured.NestedSlice(cluster.Spec.K0sConfig.Object, "spec", "workerProfiles")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue(), "k0sConfig was pruned by the CRD")
		g.Expect(profiles[0]).To(HaveKeyWithValue("name", workerProfile))
	})

	t.Run("publishes the kubeconfig once k0smotron serves it", func(t *testing.T) {
		g := NewWithT(t)

		// What k0smotron writes once a control plane replica is up.
		g.Expect(envtestClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: dc.Name + "-kubeconfig", Namespace: namespace.Name},
			Data:       map[string][]byte{k0smotronKubeconfigKey: []byte("a-kubeconfig")},
		})).To(Succeed())

		name, conds, err := handler.ReconcileCluster(ctx, dc)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(name).To(Equal(dc.Name + "-admin-kubeconfig"))
		g.Expect(conds[0].Status).To(Equal(metav1.ConditionTrue))

		// The published Secret carries the key pkg/dpucluster reads.
		published := &corev1.Secret{}
		g.Expect(envtestClient.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: name}, published)).To(Succeed())
		g.Expect(published.Data).To(HaveKeyWithValue(dpfKubeconfigKey, []byte("a-kubeconfig")))
		g.Expect(published.Type).To(Equal(corev1.SecretTypeOpaque))
		g.Expect(metav1.GetControllerOf(published)).NotTo(BeNil())
	})

	t.Run("republishes a kubeconfig k0smotron has rotated", func(t *testing.T) {
		g := NewWithT(t)

		source := &corev1.Secret{}
		sourceKey := client.ObjectKey{Namespace: namespace.Name, Name: dc.Name + "-kubeconfig"}
		g.Expect(envtestClient.Get(ctx, sourceKey, source)).To(Succeed())
		source.Data[k0smotronKubeconfigKey] = []byte("a-rotated-kubeconfig")
		g.Expect(envtestClient.Update(ctx, source)).To(Succeed())

		name, _, err := handler.ReconcileCluster(ctx, dc)
		g.Expect(err).NotTo(HaveOccurred())

		published := &corev1.Secret{}
		g.Expect(envtestClient.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: name}, published)).To(Succeed())
		g.Expect(published.Data).To(HaveKeyWithValue(dpfKubeconfigKey, []byte("a-rotated-kubeconfig")))
	})

	t.Run("is idempotent across passes", func(t *testing.T) {
		g := NewWithT(t)

		for range 3 {
			_, _, err := handler.ReconcileCluster(ctx, dc)
			g.Expect(err).NotTo(HaveOccurred())
		}

		clusters := &k0smotronv1.ClusterList{}
		g.Expect(envtestClient.List(ctx, clusters, client.InNamespace(namespace.Name))).To(Succeed())
		g.Expect(clusters.Items).To(HaveLen(1))
	})

	t.Run("brings a control plane the API server defaulted up to version", func(t *testing.T) {
		g := NewWithT(t)
		// Patched rather than updated, so whatever the CRD defaulted has to survive.
		upgrading := NewHandler(envtestClient, WithK0sVersion("v1.35.8+k0s.0"))

		_, _, err := upgrading.ReconcileCluster(ctx, dc)
		g.Expect(err).NotTo(HaveOccurred())

		cluster := &k0smotronv1.Cluster{}
		g.Expect(envtestClient.Get(ctx, key, cluster)).To(Succeed())
		g.Expect(cluster.Spec.Version).To(Equal("v1.35.8+k0s.0"))
		g.Expect(cluster.Spec.Service.Type).To(Equal(corev1.ServiceTypeNodePort), "the service was reset by the patch")
	})

	t.Run("tears the control plane down", func(t *testing.T) {
		g := NewWithT(t)

		// One pass, because the controller does not requeue on an unfinished cleanup.
		done, err := handler.CleanUpCluster(ctx, dc)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue())

		g.Expect(apierrors.IsNotFound(envtestClient.Get(ctx, key, &k0smotronv1.Cluster{}))).To(BeTrue())

		// And still finished when it has already gone.
		done, err = handler.CleanUpCluster(ctx, dc)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue())
	})
}

// TestReconcileConvergesAgainstAPIServer is the guard against a patch that never settles. Server
// side defaulting can render a field differently from the way the handler built it, and the
// comparison would then differ on every pass and rewrite the Cluster forever.
func TestReconcileConvergesAgainstAPIServer(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "handler-converge"}}
	g.Expect(envtestClient.Create(ctx, namespace)).To(Succeed())

	dc := testDPUCluster()
	dc.Namespace = namespace.Name
	dc.UID = ""
	// An overlay reaching fields the handler never sets, since those are the ones whose
	// round trip through the API server has nothing else asserting it.
	dc.Spec.ClusterManagerConfig = rawExtension(g, map[string]any{
		"replicas":          1,
		"controlPlaneFlags": []any{"--enable-metrics-scraper=true"},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "250m", "memory": "512Mi"},
		},
		"service": map[string]any{
			"type":             string(corev1.ServiceTypeNodePort),
			"apiPort":          31443,
			"konnectivityPort": 31132,
		},
	})
	g.Expect(envtestClient.Create(ctx, dc)).To(Succeed())

	handler := &clusterHandler{Client: envtestClient, k0sVersion: DefaultK0sVersion, etcdStorageClass: "local-path"}
	key := client.ObjectKeyFromObject(dc)

	_, err := handler.reconcileK0smotronCluster(ctx, dc)
	g.Expect(err).NotTo(HaveOccurred())

	created := &k0smotronv1.Cluster{}
	g.Expect(envtestClient.Get(ctx, key, created)).To(Succeed())

	// Three more passes. A Cluster the handler keeps rewriting bumps its resourceVersion.
	for i := 0; i < 3; i++ {
		conds, err := handler.reconcileK0smotronCluster(ctx, dc)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(conditionOfType(conds, ConditionSpecApplied).Status).To(Equal(metav1.ConditionTrue))
	}

	settled := &k0smotronv1.Cluster{}
	g.Expect(envtestClient.Get(ctx, key, settled)).To(Succeed())
	g.Expect(settled.ResourceVersion).To(Equal(created.ResourceVersion),
		"the handler rewrote the Cluster on a pass that should have been a no-op")

	// The overlay is still what is running, not something defaulting walked back.
	g.Expect(settled.Spec.Service.APIPort).To(Equal(31443))
	g.Expect(settled.Spec.ControlPlaneFlags).To(ConsistOf("--enable-metrics-scraper=true"))
	g.Expect(settled.Spec.Resources.Requests.Cpu().String()).To(Equal("250m"))
}

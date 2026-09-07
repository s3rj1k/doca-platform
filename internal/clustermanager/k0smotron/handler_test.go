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
	"encoding/json"
	"errors"
	"testing"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	dutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/dpu/util"
	k0smotronv1 "github.com/nvidia/doca-platform/third_party/forked/github.com/k0sproject/k0smotron/api/k0smotron.io/v1beta2"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	testNamespace = "dpf-operator-system"
	testCluster   = "dpu-cplane"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(provisioningv1.AddToScheme(s))
	utilruntime.Must(k0smotronv1.AddToScheme(s))
	return s
}

func testDPUCluster() *provisioningv1.DPUCluster {
	return &provisioningv1.DPUCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testCluster, Namespace: testNamespace, UID: "a-uid"},
		Spec:       provisioningv1.DPUClusterSpec{Type: dutil.K0smotronClusterType},
	}
}

// k0smotronKubeconfigSecret is what k0smotron publishes once a replica has served it.
func k0smotronKubeconfigSecret(data string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testCluster + "-kubeconfig", Namespace: testNamespace},
		Data:       map[string][]byte{"value": []byte(data)},
	}
}

// ownedK0smotronCluster builds a Cluster the test DPUCluster controls, which is what the
// handler requires before it will patch or delete one.
func ownedK0smotronCluster(spec k0smotronv1.ClusterSpec) *k0smotronv1.Cluster {
	dc := testDPUCluster()

	return &k0smotronv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testCluster,
			Namespace: testNamespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: provisioningv1.GroupVersion.String(),
				Kind:       "DPUCluster",
				Name:       dc.Name,
				UID:        dc.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: spec,
	}
}

func testClient(objects ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).Build()
}

func TestReconcileCluster(t *testing.T) {
	ctx := context.Background()

	t.Run("creates the hosted control plane and waits for its kubeconfig", func(t *testing.T) {
		g := NewWithT(t)
		c := testClient()
		handler := NewHandler(c)

		// The first pass creates the Cluster. Its kubeconfig does not exist yet, which is
		// the normal path rather than a failure.
		name, conds, err := handler.ReconcileCluster(ctx, testDPUCluster())
		g.Expect(err).To(HaveOccurred())
		g.Expect(name).To(BeEmpty())
		g.Expect(conditionOfType(conds, string(provisioningv1.ConditionCreated)).Reason).To(Equal("KubeconfigNotReady"))
		// The overlay is empty here, so nothing about the spec should be outstanding.
		g.Expect(conditionOfType(conds, ConditionSpecApplied).Status).To(Equal(metav1.ConditionTrue))

		cluster := &k0smotronv1.Cluster{}
		key := client.ObjectKey{Namespace: testNamespace, Name: testCluster}
		g.Expect(c.Get(ctx, key, cluster)).To(Succeed())
		g.Expect(cluster.Spec.Version).To(Equal(DefaultK0sVersion))
		g.Expect(cluster.Spec.Service.Type).To(Equal(corev1.ServiceTypeNodePort))

		// The worker profile the join script names has to be defined, or k0s rejects it.
		profiles, found, err := unstructured.NestedSlice(cluster.Spec.K0sConfig.Object, "spec", "workerProfiles")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(profiles).To(HaveLen(1))
		g.Expect(profiles[0]).To(HaveKeyWithValue("name", workerProfile))
	})

	t.Run("republishes the kubeconfig under the key DPF reads", func(t *testing.T) {
		g := NewWithT(t)
		c := testClient(k0smotronKubeconfigSecret("a-kubeconfig"))
		handler := NewHandler(c)

		name, conds, err := handler.ReconcileCluster(ctx, testDPUCluster())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(name).To(Equal(testCluster + "-admin-kubeconfig"))
		g.Expect(conds[0].Status).To(Equal(metav1.ConditionTrue))

		published := &corev1.Secret{}
		key := client.ObjectKey{Namespace: testNamespace, Name: name}
		g.Expect(c.Get(ctx, key, published)).To(Succeed())
		g.Expect(published.Data).To(HaveKeyWithValue("super-admin.conf", []byte("a-kubeconfig")))
	})

	t.Run("is idempotent across passes", func(t *testing.T) {
		g := NewWithT(t)
		c := testClient(k0smotronKubeconfigSecret("a-kubeconfig"))
		handler := NewHandler(c)

		for range 3 {
			_, _, err := handler.ReconcileCluster(ctx, testDPUCluster())
			g.Expect(err).NotTo(HaveOccurred())
		}

		clusters := &k0smotronv1.ClusterList{}
		g.Expect(c.List(ctx, clusters)).To(Succeed())
		g.Expect(clusters.Items).To(HaveLen(1))
	})

	t.Run("honors a k0s version override", func(t *testing.T) {
		g := NewWithT(t)
		c := testClient(k0smotronKubeconfigSecret("a-kubeconfig"))
		handler := NewHandler(c, WithK0sVersion("v1.34.0+k0s.0"))

		_, _, err := handler.ReconcileCluster(ctx, testDPUCluster())
		g.Expect(err).NotTo(HaveOccurred())

		cluster := &k0smotronv1.Cluster{}
		key := client.ObjectKey{Namespace: testNamespace, Name: testCluster}
		g.Expect(c.Get(ctx, key, cluster)).To(Succeed())
		g.Expect(cluster.Spec.Version).To(Equal("v1.34.0+k0s.0"))
	})

	t.Run("backs etcd with the configured storage class", func(t *testing.T) {
		g := NewWithT(t)
		c := testClient(k0smotronKubeconfigSecret("a-kubeconfig"))
		handler := NewHandler(c, WithEtcdStorageClass("local-path"))

		_, _, err := handler.ReconcileCluster(ctx, testDPUCluster())
		g.Expect(err).NotTo(HaveOccurred())

		cluster := &k0smotronv1.Cluster{}
		key := client.ObjectKey{Namespace: testNamespace, Name: testCluster}
		g.Expect(c.Get(ctx, key, cluster)).To(Succeed())
		g.Expect(cluster.Spec.Storage.Type).To(Equal(k0smotronv1.StorageTypeEtcd))
		g.Expect(cluster.Spec.Storage.Etcd.Persistence.StorageClass).To(Equal("local-path"))
		// Otherwise a DPUCluster recreated under the same name mounts stale etcd data.
		g.Expect(cluster.Spec.Storage.Etcd.AutoDeletePVCs).To(BeTrue())
	})

	t.Run("leaves the cluster default in place when no storage class is configured", func(t *testing.T) {
		g := NewWithT(t)
		c := testClient(k0smotronKubeconfigSecret("a-kubeconfig"))
		handler := NewHandler(c)

		_, _, err := handler.ReconcileCluster(ctx, testDPUCluster())
		g.Expect(err).NotTo(HaveOccurred())

		cluster := &k0smotronv1.Cluster{}
		key := client.ObjectKey{Namespace: testNamespace, Name: testCluster}
		g.Expect(c.Get(ctx, key, cluster)).To(Succeed())
		g.Expect(cluster.Spec.Storage.Etcd.Persistence.StorageClass).To(BeEmpty())
	})

	t.Run("brings an existing control plane up to the configured version", func(t *testing.T) {
		g := NewWithT(t)
		existing := ownedK0smotronCluster(k0smotronv1.ClusterSpec{Version: "v1.32.0+k0s.0", Replicas: 3})
		c := testClient(existing, k0smotronKubeconfigSecret("a-kubeconfig"))
		handler := NewHandler(c, WithK0sVersion("v1.34.0+k0s.0"))

		_, _, err := handler.ReconcileCluster(ctx, testDPUCluster())
		g.Expect(err).NotTo(HaveOccurred())

		cluster := &k0smotronv1.Cluster{}
		key := client.ObjectKey{Namespace: testNamespace, Name: testCluster}
		g.Expect(c.Get(ctx, key, cluster)).To(Succeed())
		g.Expect(cluster.Spec.Version).To(Equal("v1.34.0+k0s.0"))
		// Replicas cannot change under a live control plane, so it is left as it was found.
		g.Expect(cluster.Spec.Replicas).To(Equal(int32(3)))
	})

	t.Run("adds the worker profile to a control plane missing it", func(t *testing.T) {
		g := NewWithT(t)
		existing := ownedK0smotronCluster(k0smotronv1.ClusterSpec{Version: DefaultK0sVersion})
		c := testClient(existing, k0smotronKubeconfigSecret("a-kubeconfig"))
		handler := NewHandler(c)

		_, _, err := handler.ReconcileCluster(ctx, testDPUCluster())
		g.Expect(err).NotTo(HaveOccurred())

		cluster := &k0smotronv1.Cluster{}
		key := client.ObjectKey{Namespace: testNamespace, Name: testCluster}
		g.Expect(c.Get(ctx, key, cluster)).To(Succeed())
		g.Expect(cluster.Spec.K0sConfig).NotTo(BeNil())
		profiles, _, err := unstructured.NestedSlice(cluster.Spec.K0sConfig.Object, "spec", "workerProfiles")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(profiles[0]).To(HaveKeyWithValue("name", workerProfile))
	})

	t.Run("refuses a control plane it does not own", func(t *testing.T) {
		g := NewWithT(t)
		// Same name, someone else's object. Patching it would reconfigure their control
		// plane, and CleanUpCluster would later delete it.
		foreign := &k0smotronv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: testCluster, Namespace: testNamespace},
			Spec:       k0smotronv1.ClusterSpec{Version: "v1.32.0+k0s.0"},
		}
		c := testClient(foreign, k0smotronKubeconfigSecret("a-kubeconfig"))

		_, conds, err := NewHandler(c).ReconcileCluster(ctx, testDPUCluster())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("not owned by this DPUCluster"))
		g.Expect(conds[0].Reason).To(Equal("CreateK0smotronClusterError"))

		// Left exactly as it was found.
		stored := &k0smotronv1.Cluster{}
		key := client.ObjectKey{Namespace: testNamespace, Name: testCluster}
		g.Expect(c.Get(ctx, key, stored)).To(Succeed())
		g.Expect(stored.Spec.Version).To(Equal("v1.32.0+k0s.0"))
	})

	t.Run("brings a control plane created before the tolerations existed up to date", func(t *testing.T) {
		g := NewWithT(t)
		// Patches are reconciled, unlike the rest of the spec, so an upgrade repairs a
		// control plane that would otherwise stay unschedulable.
		existing := ownedK0smotronCluster(k0smotronv1.ClusterSpec{
			Version:   DefaultK0sVersion,
			K0sConfig: workerProfileConfig(),
		})
		c := testClient(existing, k0smotronKubeconfigSecret("a-kubeconfig"))

		_, _, err := NewHandler(c).ReconcileCluster(ctx, testDPUCluster())
		g.Expect(err).NotTo(HaveOccurred())

		cluster := &k0smotronv1.Cluster{}
		key := client.ObjectKey{Namespace: testNamespace, Name: testCluster}
		g.Expect(c.Get(ctx, key, cluster)).To(Succeed())
		g.Expect(cluster.Spec.Patches).To(Equal(controlPlaneTolerationPatches()))
	})

	t.Run("reports when k0smotron has not filled the kubeconfig in", func(t *testing.T) {
		g := NewWithT(t)
		empty := k0smotronKubeconfigSecret("")
		empty.Data = map[string][]byte{}
		handler := NewHandler(testClient(empty))

		_, _, err := handler.ReconcileCluster(ctx, testDPUCluster())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(`has no "value" yet`))
	})
}

func TestCleanUpCluster(t *testing.T) {
	ctx := context.Background()

	t.Run("deletes the hosted control plane", func(t *testing.T) {
		g := NewWithT(t)
		c := testClient(ownedK0smotronCluster(k0smotronv1.ClusterSpec{}))
		handler := NewHandler(c)

		// Finished in one pass. Reporting otherwise would hold the DPUCluster on its
		// finalizer until the next resync, since the controller does not requeue.
		done, err := handler.CleanUpCluster(ctx, testDPUCluster())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue())

		key := client.ObjectKey{Namespace: testNamespace, Name: testCluster}
		g.Expect(apierrors.IsNotFound(c.Get(ctx, key, &k0smotronv1.Cluster{}))).To(BeTrue())

		// And still finished when it has already gone.
		done, err = handler.CleanUpCluster(ctx, testDPUCluster())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue())
	})

	t.Run("leaves a control plane it does not own alone", func(t *testing.T) {
		g := NewWithT(t)
		foreign := &k0smotronv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: testCluster, Namespace: testNamespace},
		}
		c := testClient(foreign)

		done, err := NewHandler(c).CleanUpCluster(ctx, testDPUCluster())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue(), "nothing of ours to clean up")

		key := client.ObjectKey{Namespace: testNamespace, Name: testCluster}
		g.Expect(c.Get(ctx, key, &k0smotronv1.Cluster{})).To(Succeed(), "it should still be there")
	})

	t.Run("reports finished when the k0smotron CRD has gone", func(t *testing.T) {
		g := NewWithT(t)
		// k0smotron uninstalled before the DPUCluster was deleted. Reporting an error here
		// would hold the DPUCluster on its finalizer with nothing left to wait for.
		noCRD := interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return &meta.NoKindMatchError{GroupKind: obj.GetObjectKind().GroupVersionKind().GroupKind()}
			},
		}
		c := fake.NewClientBuilder().WithScheme(testScheme()).
			WithObjects(ownedK0smotronCluster(k0smotronv1.ClusterSpec{})).
			WithInterceptorFuncs(noCRD).Build()

		done, err := NewHandler(c).CleanUpCluster(ctx, testDPUCluster())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue())
	})

	t.Run("keeps polling when the delete fails for another reason", func(t *testing.T) {
		g := NewWithT(t)
		// Guards the tolerance above from swallowing a real failure.
		failing := interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewInternalError(errors.New("etcd is unavailable"))
			},
		}
		c := fake.NewClientBuilder().WithScheme(testScheme()).
			WithObjects(ownedK0smotronCluster(k0smotronv1.ClusterSpec{})).
			WithInterceptorFuncs(failing).Build()

		done, err := NewHandler(c).CleanUpCluster(ctx, testDPUCluster())
		g.Expect(err).To(HaveOccurred())
		g.Expect(done).To(BeFalse())
	})
}

func TestClusterToDPUCluster(t *testing.T) {
	tests := []struct {
		name      string
		cluster   *k0smotronv1.Cluster
		wantName  string
		wantSpace string
	}{
		{
			name:      "maps to the DPUCluster it shares a name with",
			cluster:   &k0smotronv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: testCluster, Namespace: testNamespace}},
			wantName:  testCluster,
			wantSpace: testNamespace,
		},
		{
			name:      "carries the namespace across rather than assuming one",
			cluster:   &k0smotronv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "another", Namespace: "elsewhere"}},
			wantName:  "another",
			wantSpace: "elsewhere",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			requests := clusterToDPUCluster(context.Background(), tc.cluster)
			g.Expect(requests).To(HaveLen(1))
			g.Expect(requests[0].Name).To(Equal(tc.wantName))
			g.Expect(requests[0].Namespace).To(Equal(tc.wantSpace))
		})
	}
}

func TestTypeAndOperatorConfigMapping(t *testing.T) {
	g := NewWithT(t)
	handler := NewHandler(testClient())

	g.Expect(handler.Type()).To(Equal(dutil.K0smotronClusterType))
	g.Expect(handler.DPFOperatorConfigToDPUClusters(context.Background(), nil)).To(BeEmpty())
}

// conditionOfType picks a condition out by type, so an assertion does not break when another
// condition joins the list.
func conditionOfType(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return &metav1.Condition{Type: "absent:" + condType}
}

// rawExtension renders a value the way the API server stores spec.clusterManagerConfig, which
// is JSON regardless of the YAML the user wrote.
func rawExtension(g *WithT, value any) *runtime.RawExtension {
	encoded, err := json.Marshal(value)
	g.Expect(err).NotTo(HaveOccurred())
	return &runtime.RawExtension{Raw: encoded}
}

func TestMergeClusterManagerConfig(t *testing.T) {
	tests := []struct {
		name    string
		overlay any
		nilRaw  bool
		wantErr string
		assert  func(g *WithT, spec *k0smotronv1.ClusterSpec)
	}{
		{
			name:   "a nil overlay leaves the base alone",
			nilRaw: true,
			assert: func(g *WithT, spec *k0smotronv1.ClusterSpec) {
				g.Expect(spec.Version).To(Equal(DefaultK0sVersion))
				g.Expect(spec.Service.APIPort).To(Equal(30443))
			},
		},
		{
			name:    "a field the overlay sets wins, the rest of the base survives",
			overlay: map[string]any{"replicas": 3},
			assert: func(g *WithT, spec *k0smotronv1.ClusterSpec) {
				g.Expect(spec.Replicas).To(Equal(int32(3)))
				g.Expect(spec.Version).To(Equal(DefaultK0sVersion))
				g.Expect(spec.Storage.Etcd.AutoDeletePVCs).To(BeTrue())
			},
		},
		{
			name: "a nested field merges rather than replacing its siblings",
			overlay: map[string]any{
				"service": map[string]any{"apiPort": 31443},
			},
			assert: func(g *WithT, spec *k0smotronv1.ClusterSpec) {
				g.Expect(spec.Service.APIPort).To(Equal(31443))
				g.Expect(spec.Service.KonnectivityPort).To(Equal(30132))
				g.Expect(spec.Service.Type).To(Equal(corev1.ServiceTypeNodePort))
			},
		},
		{
			name: "an array replaces rather than joining",
			overlay: map[string]any{
				"patches": []any{
					map[string]any{
						"target": map[string]any{"kind": "StatefulSet", "component": "control-plane"},
						"patch":  map[string]any{"type": "strategic", "content": "spec: {}"},
					},
				},
			},
			assert: func(g *WithT, spec *k0smotronv1.ClusterSpec) {
				g.Expect(spec.Patches).To(HaveLen(1), "the base's two patches should have been replaced")
			},
		},
		{
			name: "k0sConfig is taken whole, so a replacement drops the base profile",
			overlay: map[string]any{
				"k0sConfig": map[string]any{
					"apiVersion": "k0s.k0sproject.io/v1beta1",
					"kind":       "ClusterConfig",
					"spec":       map[string]any{"workerProfiles": []any{}},
				},
			},
			assert: func(g *WithT, spec *k0smotronv1.ClusterSpec) {
				g.Expect(hasWorkerProfile(spec.K0sConfig, workerProfile)).To(BeFalse())
			},
		},
		{
			name:    "a misspelled field is an error rather than a silent no-op",
			overlay: map[string]any{"replicaz": 3},
			wantErr: "clusterManagerConfig",
		},
		{
			name:    "a wrongly typed field is an error",
			overlay: map[string]any{"replicas": "three"},
			wantErr: "clusterManagerConfig",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			handler := &clusterHandler{k0sVersion: DefaultK0sVersion, etcdStorageClass: "local-path"}
			spec := handler.baseCluster(testDPUCluster()).Spec

			var raw *runtime.RawExtension
			if !tc.nilRaw {
				raw = rawExtension(g, tc.overlay)
			}

			err := mergeClusterManagerConfig(&spec, raw)
			if tc.wantErr != "" {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(tc.wantErr))
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			tc.assert(g, &spec)
		})
	}
}

func TestValidateClusterSpec(t *testing.T) {
	tests := []struct {
		name    string
		overlay any
		wantErr string
	}{
		{
			name: "the spec DPF builds on its own is valid",
		},
		{
			name:    "an empty version is refused, since a worker reads it back",
			overlay: map[string]any{"version": ""},
			wantErr: "version is empty",
		},
		{
			name:    "ClusterIP is refused, since no DPU could reach it",
			overlay: map[string]any{"service": map[string]any{"type": "ClusterIP"}},
			wantErr: "does not expose the control plane",
		},
		{
			name: "a k0sConfig without the dpu worker profile is refused",
			overlay: map[string]any{
				"k0sConfig": map[string]any{
					"apiVersion": "k0s.k0sproject.io/v1beta1",
					"kind":       "ClusterConfig",
					"spec":       map[string]any{"workerProfiles": []any{map[string]any{"name": "other"}}},
				},
			},
			wantErr: "worker profile",
		},
		{
			name:    "a zero etcd volume is refused, since no PVC accepts one",
			overlay: map[string]any{"storage": map[string]any{"etcd": map[string]any{"persistence": map[string]any{"size": "0"}}}},
			wantErr: "size is zero",
		},
		{
			name:    "LoadBalancer is allowed alongside NodePort",
			overlay: map[string]any{"service": map[string]any{"type": "LoadBalancer"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			dc := testDPUCluster()
			if tc.overlay != nil {
				dc.Spec.ClusterManagerConfig = rawExtension(g, tc.overlay)
			}
			handler := &clusterHandler{k0sVersion: DefaultK0sVersion, etcdStorageClass: "local-path"}

			_, err := handler.desiredCluster(dc)
			if tc.wantErr == "" {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantErr))
		})
	}
}

func TestSpecMergePatch(t *testing.T) {
	handler := &clusterHandler{k0sVersion: DefaultK0sVersion, etcdStorageClass: "local-path"}

	t.Run("no patch when the running spec already matches", func(t *testing.T) {
		g := NewWithT(t)
		spec := handler.baseCluster(testDPUCluster()).Spec

		patch, err := specMergePatch(&spec, &spec, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(patch).To(BeNil())
	})

	t.Run("a field DPF owns is reconciled even with no overlay", func(t *testing.T) {
		g := NewWithT(t)
		existing := handler.baseCluster(testDPUCluster()).Spec
		existing.Version = "v1.34.0+k0s.0"
		existing.Patches = nil
		desired := handler.baseCluster(testDPUCluster()).Spec

		patch, err := specMergePatch(&existing, &desired, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(string(patch)).To(ContainSubstring(DefaultK0sVersion))
		g.Expect(string(patch)).To(ContainSubstring("patches"))
	})

	t.Run("a field only the overlay names is reconciled", func(t *testing.T) {
		g := NewWithT(t)
		raw := rawExtension(g, map[string]any{"replicas": 3})
		existing := handler.baseCluster(testDPUCluster()).Spec
		desired := existing
		desired.Replicas = 3

		patch, err := specMergePatch(&existing, &desired, raw)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(string(patch)).To(ContainSubstring(`"replicas":3`))
	})

	t.Run("a field nobody named is left alone, so server defaults survive", func(t *testing.T) {
		g := NewWithT(t)
		existing := handler.baseCluster(testDPUCluster()).Spec
		// Stand in for anything k0smotron defaulted that neither DPF nor the overlay sets.
		existing.Image = "quay.io/k0sproject/k0s"
		desired := handler.baseCluster(testDPUCluster()).Spec

		patch, err := specMergePatch(&existing, &desired, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(patch).To(BeNil(), "an unnamed field must not be reset to its zero value")
	})

	t.Run("storage is never patched, even when the overlay changes it", func(t *testing.T) {
		g := NewWithT(t)
		raw := rawExtension(g, map[string]any{
			"storage": map[string]any{"etcd": map[string]any{"persistence": map[string]any{"storageClass": "other"}}},
		})
		existing := handler.baseCluster(testDPUCluster()).Spec
		desired := existing
		desired.Storage.Etcd.Persistence.StorageClass = "other"

		patch, err := specMergePatch(&existing, &desired, raw)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(patch).To(BeNil())
	})
}

func TestImmutableDrift(t *testing.T) {
	handler := &clusterHandler{k0sVersion: DefaultK0sVersion, etcdStorageClass: "local-path"}

	t.Run("names storage when the overlay asks to change it under a running cluster", func(t *testing.T) {
		g := NewWithT(t)
		raw := rawExtension(g, map[string]any{
			"storage": map[string]any{"etcd": map[string]any{"persistence": map[string]any{"storageClass": "other"}}},
		})
		existing := handler.baseCluster(testDPUCluster()).Spec
		desired := existing
		desired.Storage.Etcd.Persistence.StorageClass = "other"

		g.Expect(immutableDrift(&existing, &desired, raw)).To(ConsistOf("storage"))
	})

	t.Run("stays quiet when the overlay matches what is running", func(t *testing.T) {
		g := NewWithT(t)
		raw := rawExtension(g, map[string]any{
			"storage": map[string]any{"etcd": map[string]any{"persistence": map[string]any{"storageClass": "local-path"}}},
		})
		spec := handler.baseCluster(testDPUCluster()).Spec

		g.Expect(immutableDrift(&spec, &spec, raw)).To(BeEmpty())
	})

	t.Run("stays quiet when the overlay never mentions storage", func(t *testing.T) {
		g := NewWithT(t)
		existing := handler.baseCluster(testDPUCluster()).Spec
		desired := existing
		desired.Storage.Etcd.Persistence.StorageClass = "drifted-by-something-else"

		g.Expect(immutableDrift(&existing, &desired, rawExtension(g, map[string]any{"replicas": 1}))).To(BeEmpty())
	})
}

func TestSpecAppliedCondition(t *testing.T) {
	t.Run("reports the drifted fields", func(t *testing.T) {
		g := NewWithT(t)
		cond := specAppliedCondition([]string{"storage", "persistence"})

		g.Expect(cond.Type).To(Equal(ConditionSpecApplied))
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal("ImmutableFieldChanged"))
		g.Expect(cond.Message).To(ContainSubstring("storage and persistence"))
	})

	// Returned even with nothing to say, so a drift reported once clears rather than sticking
	// around after the overlay stops asking for it.
	t.Run("reports success rather than nothing when there is no drift", func(t *testing.T) {
		g := NewWithT(t)
		cond := specAppliedCondition(nil)

		g.Expect(cond.Type).To(Equal(ConditionSpecApplied))
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal("Applied"))
	})
}

// TestSpecAppliedConditionClears is the regression for a drift condition that could never go
// away, since a clean pass used to return no condition at all and left the stale one in place.
func TestSpecAppliedConditionClears(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	dc := testDPUCluster()
	drifting := map[string]any{
		"storage": map[string]any{"etcd": map[string]any{"persistence": map[string]any{"storageClass": "other"}}},
	}
	dc.Spec.ClusterManagerConfig = rawExtension(g, drifting)

	handler := &clusterHandler{k0sVersion: DefaultK0sVersion, etcdStorageClass: "local-path"}
	existing := handler.baseCluster(dc)
	g.Expect(controllerutil.SetControllerReference(dc, existing, testScheme())).To(Succeed())
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(dc, existing).Build()
	handler.Client = c

	conds, err := handler.reconcileK0smotronCluster(ctx, dc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(conds).To(HaveLen(1))
	g.Expect(conds[0].Status).To(Equal(metav1.ConditionFalse), "the overlay asks to change storage")

	// The user backs the change out.
	dc.Spec.ClusterManagerConfig = nil
	conds, err = handler.reconcileK0smotronCluster(ctx, dc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(conds).To(HaveLen(1))
	g.Expect(conds[0].Status).To(Equal(metav1.ConditionTrue), "the stale drift condition must clear")
}

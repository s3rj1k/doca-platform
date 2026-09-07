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

	k0smotronv1 "github.com/nvidia/doca-platform/third_party/forked/github.com/k0sproject/k0smotron/api/k0smotron.io/v1beta2"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestDesiredClusterAgainstCRD checks the Cluster this handler builds against the real schema.
// The fake client validates nothing, so a wrongly shaped field would only fail on a live server.
func TestDesiredClusterAgainstCRD(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "crd-check"}}
	g.Expect(envtestClient.Create(ctx, namespace)).To(Succeed())

	dc := testDPUCluster()
	dc.Namespace = namespace.Name
	handler := &clusterHandler{
		Client:           envtestClient,
		k0sVersion:       DefaultK0sVersion,
		etcdStorageClass: "local-path",
	}

	cluster, err := handler.desiredCluster(dc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(envtestClient.Create(ctx, cluster)).To(Succeed(), "the CRD rejected the Cluster this handler builds")

	// Read it back, since defaulting and pruning happen server side.
	stored := &k0smotronv1.Cluster{}
	g.Expect(envtestClient.Get(ctx, client.ObjectKeyFromObject(cluster), stored)).To(Succeed())
	g.Expect(stored.Spec.Version).To(Equal(DefaultK0sVersion))
	g.Expect(stored.Spec.Service.Type).To(Equal(corev1.ServiceTypeNodePort))

	// Storage has to survive server side defaulting, which is why Type is set explicitly
	// rather than left for the CRD to fill in around the nested etcd settings.
	g.Expect(stored.Spec.Storage.Type).To(Equal(k0smotronv1.StorageTypeEtcd))
	g.Expect(stored.Spec.Storage.Etcd.Persistence.StorageClass).To(Equal("local-path"))
	g.Expect(stored.Spec.Storage.Etcd.AutoDeletePVCs).To(BeTrue())
	// Sent explicitly, because the vendored types omit `omitempty` so defaulting never fills
	// them, and a size of 0 is rejected by every PersistentVolumeClaim.
	g.Expect(stored.Spec.Storage.Etcd.Image).NotTo(BeEmpty())
	g.Expect(stored.Spec.Storage.Etcd.Persistence.Size.IsZero()).To(BeFalse())
	g.Expect(stored.Spec.Storage.Etcd.Persistence.Size.String()).To(Equal(etcdVolumeSize))

	// The worker profile has to survive the round trip, or the join fails on the card.
	profiles, found, err := unstructured.NestedSlice(stored.Spec.K0sConfig.Object, "spec", "workerProfiles")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue(), "k0sConfig was pruned by the CRD")
	g.Expect(profiles[0]).To(HaveKeyWithValue("name", workerProfile))
}

// TestOverlayAgainstCRD checks that an overlay reaching past the fields DPF sets survives the
// real schema. Fields the overlay leaves alone have to keep the values DPF and the CRD gave them.
func TestOverlayAgainstCRD(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "crd-overlay"}}
	g.Expect(envtestClient.Create(ctx, namespace)).To(Succeed())

	dc := testDPUCluster()
	dc.Namespace = namespace.Name
	dc.Spec.ClusterManagerConfig = rawExtension(g, map[string]any{
		"replicas": 1,
		"service": map[string]any{
			"type":             string(corev1.ServiceTypeNodePort),
			"apiPort":          31443,
			"konnectivityPort": 31132,
		},
		"controlPlaneFlags": []any{"--enable-metrics-scraper=true"},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "250m", "memory": "512Mi"},
		},
	})
	handler := &clusterHandler{
		Client:           envtestClient,
		k0sVersion:       DefaultK0sVersion,
		etcdStorageClass: "local-path",
	}

	cluster, err := handler.desiredCluster(dc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(envtestClient.Create(ctx, cluster)).To(Succeed(), "the CRD rejected the overlaid Cluster")

	stored := &k0smotronv1.Cluster{}
	g.Expect(envtestClient.Get(ctx, client.ObjectKeyFromObject(cluster), stored)).To(Succeed())

	// What the overlay asked for.
	g.Expect(stored.Spec.Service.APIPort).To(Equal(31443))
	g.Expect(stored.Spec.Service.KonnectivityPort).To(Equal(31132))
	g.Expect(stored.Spec.ControlPlaneFlags).To(ConsistOf("--enable-metrics-scraper=true"))
	g.Expect(stored.Spec.Resources.Requests.Cpu().String()).To(Equal("250m"))

	// What it did not, which DPF still owns.
	g.Expect(stored.Spec.Version).To(Equal(DefaultK0sVersion))
	g.Expect(stored.Spec.Storage.Etcd.Persistence.StorageClass).To(Equal("local-path"))
	g.Expect(stored.Spec.Patches).To(HaveLen(2))
	profiles, found, err := unstructured.NestedSlice(stored.Spec.K0sConfig.Object, "spec", "workerProfiles")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(profiles[0]).To(HaveKeyWithValue("name", workerProfile))
}

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

package e2e

import (
	"fmt"
	"time"

	dutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/dpu/util"
	"github.com/nvidia/doca-platform/pkg/dpucluster"
	k0smotronv1 "github.com/nvidia/doca-platform/third_party/forked/github.com/k0sproject/k0smotron/api/k0smotron.io/v1beta2"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// k0smotronK0sVersion is the k0s release the suite pins. Its Kubernetes version matches
// util.KubernetesVersion, so a DPU cluster runs what DPF ships.
const k0smotronK0sVersion = "v1.35.6+k0s.0"

// k0smotronReportedVersion is what the hosted apiserver reports, and so what DPF records in
// DPUCluster.Status.Version. k0s drops the release counter the pin carries.
const k0smotronReportedVersion = "v1.35.6+k0s"

// k0smotronWorkerProfile is the profile the join script names, which the control plane has
// to define or k0s rejects the join.
const k0smotronWorkerProfile = "dpu"

// k0smotronManagerDeployment is the Deployment the operator creates. The name carries the
// kustomize namePrefix, so it is not the component name.
const k0smotronManagerDeployment = "k0smotron-cm-controller-manager"

// k0smotronEtcdStorageClass backs each hosted control plane's etcd. The test environment has
// no default StorageClass, so leaving this unset would leave the etcd PVC forever Pending.
const k0smotronEtcdStorageClass = "local-path"

// Deleting the DPUCluster is deliberately not covered here. This suite runs a single
// DPUCluster, and removing it strands the system DPUServices that target it, which then
// blocks the AfterSuite teardown of the DPFOperatorConfig. multidpucluster_test.go can
// delete a cluster only because it keeps a second one alive. The CleanUpCluster contract,
// including that it finishes in one pass, is covered in internal/clustermanager/k0smotron.
var _ = Describe("DPF System tests - k0smotron cluster manager", Labels{Domain.K0smotron}, Ordered, func() {
	Context("Setup system", Ordered, func() {
		It("create DPFOperatorConfig", func() {
			SystemSetupBeforeSuite(false)
		})
		It("create DPUClusters", func() {
			ProvisionDPUClusters(ctx, getProvisionDPUClustersInput())
		})
	})

	Context("Validate the hosted control plane", Ordered, func() {
		BeforeAll(func() {
			By("Waiting for DPFOperatorConfig to be ready")
			VerifyDPFOperatorConfigReady(ctx, input.client, 20*time.Minute)
		})

		It("should run the k0smotron cluster manager the DPFOperatorConfig asked for", func() {
			// Disabled by default, so its Deployment existing is what proves the operator
			// honored the config.
			Eventually(func(g Gomega) {
				deployment := &appsv1.Deployment{}
				g.Expect(input.client.Get(ctx, client.ObjectKey{
					Namespace: dpfOperatorSystemNamespace,
					Name:      k0smotronManagerDeployment,
				}, deployment)).To(Succeed())
				g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically(">=", 1))

				args := deployment.Spec.Template.Spec.Containers[0].Args
				g.Expect(args).To(ContainElement(fmt.Sprintf("--k0s-version=%s", k0smotronK0sVersion)))
			}).WithTimeout(10 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
		})

		It("should own a k0smotron Cluster per DPUCluster", func() {
			for _, dpuCluster := range input.dpuClusters {
				Expect(dpuCluster.Spec.Type).To(Equal(dutil.K0smotronClusterType))

				cluster := &k0smotronv1.Cluster{}
				Expect(input.client.Get(ctx, client.ObjectKeyFromObject(dpuCluster), cluster)).To(Succeed())
				Expect(cluster.Spec.Version).To(Equal(k0smotronK0sVersion))
				Expect(cluster.Spec.Service.Type).To(Equal(corev1.ServiceTypeNodePort))

				By("the DPUCluster owns it, so a delete cascades")
				owner := metav1.GetControllerOf(cluster)
				Expect(owner).NotTo(BeNil())
				Expect(owner.Name).To(Equal(dpuCluster.Name))

				By("the worker profile the join script names is defined")
				profiles, found, err := unstructured.NestedSlice(cluster.Spec.K0sConfig.Object, "spec", "workerProfiles")
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeTrue())
				Expect(profiles[0]).To(HaveKeyWithValue("name", k0smotronWorkerProfile))
			}
		})

		It("should carry what spec.clusterManagerConfig asked for", func() {
			for _, dpuCluster := range input.dpuClusters {
				Expect(dpuCluster.Spec.ClusterManagerConfig).NotTo(BeNil(),
					"the fixture is meant to exercise the overlay")

				cluster := &k0smotronv1.Cluster{}
				Expect(input.client.Get(ctx, client.ObjectKeyFromObject(dpuCluster), cluster)).To(Succeed())

				By("fields the cluster manager never sets reached the control plane")
				Expect(cluster.Spec.ControlPlaneFlags).To(ContainElement("--enable-metrics-scraper=true"))
				Expect(cluster.Spec.Resources.Requests.Cpu().String()).To(Equal("100m"))

				By("the overlay did not cost the fields the manager owns")
				Expect(cluster.Spec.Version).To(Equal(k0smotronK0sVersion))
				Expect(cluster.Spec.Storage.Etcd.Persistence.StorageClass).To(Equal(k0smotronEtcdStorageClass))
			}
		})

		It("should publish a kubeconfig DPF can reach the control plane with", func() {
			for _, dpuCluster := range input.dpuClusters {
				Expect(input.client.Get(ctx, client.ObjectKeyFromObject(dpuCluster), dpuCluster)).To(Succeed())
				Expect(dpuCluster.Spec.Kubeconfig).NotTo(BeEmpty())

				secret := &corev1.Secret{}
				Expect(input.client.Get(ctx, client.ObjectKey{
					Namespace: dpuCluster.Namespace,
					Name:      dpuCluster.Spec.Kubeconfig,
				}, secret)).To(Succeed())

				// The key pkg/dpucluster reads. A different one would leave every
				// host-cluster controller unable to reach the DPU cluster.
				Expect(secret.Data).To(HaveKey("super-admin.conf"))

				By("DPF can reach the control plane through its own client path")
				// pkg/dpucluster is how every host cluster controller reaches a DPU
				// cluster, so going through it tests the contract rather than a copy.
				dpuClient, err := dpucluster.NewConfig(input.client, dpuCluster).Client(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(dpuClient.List(ctx, &corev1.NodeList{})).To(Succeed())
			}
		})
	})

})

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

package inventory

import (
	"context"
	"strings"
	"testing"

	operatorv1 "github.com/nvidia/doca-platform/api/operator/v1alpha1"
	"github.com/nvidia/doca-platform/internal/release"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestClusterManagerObjects_ComparisonTable(t *testing.T) {
	g := NewWithT(t)
	defaults := &release.Defaults{}
	g.Expect(defaults.Parse()).To(Succeed())

	tests := []struct {
		name                     string
		clusterManager           *clusterManagerObjects
		expectKeepalivedFlag     bool
		expectCommonEdits        bool
		clusterManagerObjectName string
	}{
		{
			name:                     "Kamaji has keepalived flag and common edits",
			clusterManager:           newKamajiClusterManagerObjects(kamajiCMData),
			expectKeepalivedFlag:     true,
			expectCommonEdits:        true,
			clusterManagerObjectName: operatorv1.KamajiClusterManagerName.String(),
		},
		{
			name:                     "Static has NO keepalived flag but has common edits",
			clusterManager:           newStaticClusterManagerObjects(staticCMData),
			expectKeepalivedFlag:     false,
			expectCommonEdits:        true,
			clusterManagerObjectName: operatorv1.StaticClusterManagerName.String(),
		},
		{
			name:                     "k0smotron has NO keepalived flag but has common edits",
			clusterManager:           newK0smotronClusterManagerObjects(k0smotronCMData),
			expectKeepalivedFlag:     false,
			expectCommonEdits:        true,
			clusterManagerObjectName: operatorv1.K0smotronClusterManagerName.String(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(tc.clusterManager.Parse()).NotTo(HaveOccurred())

			testNS := testNamespace
			vars := newDefaultVariables(defaults)
			// The static and k0smotron cluster managers are disabled by default, so they
			// have to be enabled for the test.
			vars.DisableSystemComponents[operatorv1.StaticClusterManagerName] = false
			vars.DisableSystemComponents[operatorv1.K0smotronClusterManagerName] = false
			vars.Namespace = testNS

			objs, err := tc.clusterManager.GenerateManifests(context.Background(), vars)
			g.Expect(err).NotTo(HaveOccurred())

			// Find the Deployment
			var deployment *appsv1.Deployment
			for _, obj := range objs {
				if obj.GetObjectKind().GroupVersionKind().Kind == string(DeploymentKind) {
					deploy := &appsv1.Deployment{}
					unstructuredObj, ok := obj.(*unstructured.Unstructured)
					g.Expect(ok).To(BeTrue())
					err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.UnstructuredContent(), deploy)
					g.Expect(err).NotTo(HaveOccurred())
					deployment = deploy
					break
				}
			}

			g.Expect(deployment).NotTo(BeNil())
			container := deployment.Spec.Template.Spec.Containers[0]

			// Check keepalived flag
			hasKeepalivedFlag := false
			for _, arg := range container.Args {
				if strings.HasPrefix(arg, "--keepalived-image=") {
					hasKeepalivedFlag = true
					break
				}
			}
			g.Expect(hasKeepalivedFlag).To(Equal(tc.expectKeepalivedFlag),
				"Cluster manager %s should have keepalived flag: %v", tc.clusterManagerObjectName, tc.expectKeepalivedFlag)

			// Check common edits if expected
			if tc.expectCommonEdits {
				g.Expect(deployment.Namespace).To(Equal(testNS))
				g.Expect(deployment.Spec.Template.Spec.Tolerations).To(Equal(controlPlaneTolerations))
				g.Expect(deployment.Spec.Template.Spec.Affinity).NotTo(BeNil())
				g.Expect(deployment.Spec.Template.Spec.Affinity.NodeAffinity).NotTo(BeNil())
			}
		})
	}
}

// deploymentFromObjects returns the single Deployment a cluster manager generates.
func deploymentFromObjects(g Gomega, objs []client.Object) *appsv1.Deployment {
	for _, obj := range objs {
		if obj.GetObjectKind().GroupVersionKind().Kind != string(DeploymentKind) {
			continue
		}
		unstructuredObj, ok := obj.(*unstructured.Unstructured)
		g.Expect(ok).To(BeTrue())
		deploy := &appsv1.Deployment{}
		g.Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.UnstructuredContent(), deploy)).To(Succeed())

		return deploy
	}

	return nil
}

func TestK0smotronClusterManagerObjects_Flags(t *testing.T) {
	g := NewWithT(t)
	defaults := &release.Defaults{}
	g.Expect(defaults.Parse()).To(Succeed())

	generate := func(g Gomega, k0sVersion, etcdStorageClass string) *appsv1.Deployment {
		cm := newK0smotronClusterManagerObjects(k0smotronCMData)
		g.Expect(cm.Parse()).NotTo(HaveOccurred())

		vars := newDefaultVariables(defaults)
		vars.DisableSystemComponents[operatorv1.K0smotronClusterManagerName] = false
		vars.K0sVersion = k0sVersion
		vars.EtcdStorageClassName = etcdStorageClass

		objs, err := cm.GenerateManifests(context.Background(), vars)
		g.Expect(err).NotTo(HaveOccurred())

		return deploymentFromObjects(g, objs)
	}

	args := func(g Gomega, deployment *appsv1.Deployment) []string {
		g.Expect(deployment).NotTo(BeNil())

		return deployment.Spec.Template.Spec.Containers[0].Args
	}

	t.Run("passes both pinned settings to the manager", func(t *testing.T) {
		g := NewWithT(t)
		got := args(g, generate(g, "v1.35.6+k0s.0", "local-path"))
		g.Expect(got).To(ContainElement("--k0s-version=v1.35.6+k0s.0"))
		g.Expect(got).To(ContainElement("--etcd-storage-class=local-path"))
	})

	t.Run("passes only the setting that is pinned", func(t *testing.T) {
		g := NewWithT(t)
		got := args(g, generate(g, "", "local-path"))
		g.Expect(got).To(ContainElement("--etcd-storage-class=local-path"))
		for _, arg := range got {
			g.Expect(arg).NotTo(HavePrefix("--k0s-version="))
		}
	})

	t.Run("leaves the manager on its own defaults when nothing is pinned", func(t *testing.T) {
		g := NewWithT(t)
		// An empty flag is worse than none, since it would override the manager's default.
		for _, arg := range args(g, generate(g, "", "")) {
			g.Expect(arg).NotTo(HavePrefix("--k0s-version="))
			g.Expect(arg).NotTo(HavePrefix("--etcd-storage-class="))
		}
	})

	t.Run("generates nothing while the component is disabled", func(t *testing.T) {
		g := NewWithT(t)
		cm := newK0smotronClusterManagerObjects(k0smotronCMData)
		g.Expect(cm.Parse()).NotTo(HaveOccurred())

		// Disabled by default, so an operator that never asks for k0smotron does not get it.
		vars := newDefaultVariables(defaults)
		g.Expect(vars.DisableSystemComponents).To(HaveKeyWithValue(operatorv1.K0smotronClusterManagerName, true))

		objs, err := cm.GenerateManifests(context.Background(), vars)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(objs).To(BeEmpty())
	})
}

// k0smotronTestConfig returns a config carrying only what VariablesFromDPFOperatorConfig
// dereferences, plus the k0smotron manager under test.
func k0smotronTestConfig(cm *operatorv1.K0smotronClusterManagerConfiguration) *operatorv1.DPFOperatorConfig {
	return &operatorv1.DPFOperatorConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "dpfoperatorconfig", Namespace: testNamespace},
		Spec: operatorv1.DPFOperatorConfigSpec{
			DeploymentMode:          operatorv1.DeploymentModeHostTrusted,
			ProvisioningController:  &operatorv1.ProvisioningControllerConfiguration{},
			K0smotronClusterManager: cm,
		},
	}
}

func TestVariablesFromDPFOperatorConfig_K0smotron(t *testing.T) {
	g := NewWithT(t)
	defaults := &release.Defaults{}
	g.Expect(defaults.Parse()).To(Succeed())

	t.Run("stays untouched when the config says nothing about k0smotron", func(t *testing.T) {
		g := NewWithT(t)
		vars := VariablesFromDPFOperatorConfig(defaults, k0smotronTestConfig(nil), nil)
		g.Expect(vars.K0sVersion).To(BeEmpty())
		g.Expect(vars.EtcdStorageClassName).To(BeEmpty())
		g.Expect(vars.Replicas).NotTo(HaveKey(operatorv1.K0smotronClusterManagerName))
		g.Expect(vars.DisableSystemComponents).To(HaveKeyWithValue(operatorv1.K0smotronClusterManagerName, true))
	})

	t.Run("carries the k0s version and replicas across", func(t *testing.T) {
		g := NewWithT(t)
		vars := VariablesFromDPFOperatorConfig(defaults, k0smotronTestConfig(
			&operatorv1.K0smotronClusterManagerConfiguration{
				BaseComponentConfig:  operatorv1.BaseComponentConfig{Disable: ptr.To(false)},
				BaseControllerConfig: operatorv1.BaseControllerConfig{Replicas: ptr.To[int32](2)},
				K0sVersion:           "v1.35.6+k0s.0",
				EtcdStorageClassName: "local-path",
			}), nil)
		g.Expect(vars.K0sVersion).To(Equal("v1.35.6+k0s.0"))
		g.Expect(vars.EtcdStorageClassName).To(Equal("local-path"))
		g.Expect(vars.Replicas).To(HaveKeyWithValue(operatorv1.K0smotronClusterManagerName, ptr.To[int32](2)))
		g.Expect(vars.DisableSystemComponents).To(HaveKeyWithValue(operatorv1.K0smotronClusterManagerName, false))
	})

	t.Run("enables the component without pinning a version", func(t *testing.T) {
		g := NewWithT(t)
		vars := VariablesFromDPFOperatorConfig(defaults, k0smotronTestConfig(
			&operatorv1.K0smotronClusterManagerConfiguration{
				BaseComponentConfig: operatorv1.BaseComponentConfig{Disable: ptr.To(false)},
			}), nil)
		g.Expect(vars.K0sVersion).To(BeEmpty())
		g.Expect(vars.DisableSystemComponents).To(HaveKeyWithValue(operatorv1.K0smotronClusterManagerName, false))
	})

	t.Run("takes the image and resources the config overrides", func(t *testing.T) {
		g := NewWithT(t)
		vars := VariablesFromDPFOperatorConfig(defaults, k0smotronTestConfig(
			&operatorv1.K0smotronClusterManagerConfiguration{
				BaseComponentConfig: operatorv1.BaseComponentConfig{Disable: ptr.To(false)},
				Controller: &operatorv1.DefaultOverridesConfiguration{
					ImageComponentConfig: operatorv1.ImageComponentConfig{
						Image: ptr.To("example.com/k0smotron-cluster-manager:v1"),
					},
				},
			}), nil)
		image := operatorv1.K0smotronClusterManagerName.WithContainer(operatorv1.ControllerManagerContainer)
		g.Expect(vars.Images).To(HaveKeyWithValue(image, "example.com/k0smotron-cluster-manager:v1"))
	})
}

// TestK0smotronClusterManagerIsReachableFromItsConfig guards the registration in
// DPFOperatorConfig.ComponentConfigs, without which the manager can never be deployed.
func TestK0smotronClusterManagerIsReachableFromItsConfig(t *testing.T) {
	g := NewWithT(t)
	defaults := &release.Defaults{}
	g.Expect(defaults.Parse()).To(Succeed())

	inv := New()
	g.Expect(inv.ParseAll()).To(Succeed())

	enabled := func(vars Variables) bool {
		for _, component := range inv.EnabledComponents(vars) {
			if component.Name() == operatorv1.K0smotronClusterManagerName {
				return true
			}
		}

		return false
	}

	g.Expect(enabled(VariablesFromDPFOperatorConfig(defaults, k0smotronTestConfig(nil), nil))).To(BeFalse(),
		"k0smotron should stay off for a config that does not ask for it")

	g.Expect(enabled(VariablesFromDPFOperatorConfig(defaults, k0smotronTestConfig(
		&operatorv1.K0smotronClusterManagerConfiguration{
			BaseComponentConfig: operatorv1.BaseComponentConfig{Disable: ptr.To(false)},
		}), nil))).To(BeTrue(),
		"k0smotron should be deployed once the config enables it")

	g.Expect(enabled(VariablesFromDPFOperatorConfig(defaults, k0smotronTestConfig(
		&operatorv1.K0smotronClusterManagerConfiguration{
			BaseComponentConfig: operatorv1.BaseComponentConfig{Disable: ptr.To(true)},
		}), nil))).To(BeFalse(),
		"k0smotron should stay off when the config disables it")
}

func TestClusterManagerObjects_ResourcesAndReplicas(t *testing.T) {
	g := NewWithT(t)
	defaults := &release.Defaults{}
	g.Expect(defaults.Parse()).To(Succeed())

	t.Run("Kamaji respects resources configuration", func(t *testing.T) {
		g := NewWithT(t)
		kamajiCM := newKamajiClusterManagerObjects(kamajiCMData)
		g.Expect(kamajiCM.Parse()).NotTo(HaveOccurred())

		vars := newDefaultVariables(defaults)
		vars.Resources[operatorv1.KamajiClusterManagerName.WithContainer(operatorv1.ControllerManagerContainer)] = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		}

		objs, err := kamajiCM.GenerateManifests(context.Background(), vars)
		g.Expect(err).NotTo(HaveOccurred())

		var deployment *appsv1.Deployment
		for _, obj := range objs {
			if obj.GetObjectKind().GroupVersionKind().Kind == string(DeploymentKind) {
				deploy := &appsv1.Deployment{}
				unstructuredObj, ok := obj.(*unstructured.Unstructured)
				g.Expect(ok).To(BeTrue())
				err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.UnstructuredContent(), deploy)
				g.Expect(err).NotTo(HaveOccurred())
				deployment = deploy
				break
			}
		}

		g.Expect(deployment).NotTo(BeNil())
		container := deployment.Spec.Template.Spec.Containers[0]
		g.Expect(container.Resources.Requests.Cpu().String()).To(Equal("100m"))
		g.Expect(container.Resources.Requests.Memory().String()).To(Equal("128Mi"))
		g.Expect(container.Resources.Limits.Cpu().String()).To(Equal("200m"))
		g.Expect(container.Resources.Limits.Memory().String()).To(Equal("256Mi"))
	})

	t.Run("Static respects resources configuration", func(t *testing.T) {
		g := NewWithT(t)
		staticCM := newStaticClusterManagerObjects(staticCMData)
		g.Expect(staticCM.Parse()).NotTo(HaveOccurred())

		vars := newDefaultVariables(defaults)
		// Static cluster manager is disabled by default, so we need to enable it
		vars.DisableSystemComponents[operatorv1.StaticClusterManagerName] = false
		vars.Resources[operatorv1.StaticClusterManagerName.WithContainer(operatorv1.ControllerManagerContainer)] = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		}

		objs, err := staticCM.GenerateManifests(context.Background(), vars)
		g.Expect(err).NotTo(HaveOccurred())

		var deployment *appsv1.Deployment
		for _, obj := range objs {
			if obj.GetObjectKind().GroupVersionKind().Kind == string(DeploymentKind) {
				deploy := &appsv1.Deployment{}
				unstructuredObj, ok := obj.(*unstructured.Unstructured)
				g.Expect(ok).To(BeTrue())
				err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.UnstructuredContent(), deploy)
				g.Expect(err).NotTo(HaveOccurred())
				deployment = deploy
				break
			}
		}

		g.Expect(deployment).NotTo(BeNil())
		container := deployment.Spec.Template.Spec.Containers[0]
		g.Expect(container.Resources.Requests.Cpu().String()).To(Equal("100m"))
		g.Expect(container.Resources.Requests.Memory().String()).To(Equal("128Mi"))
		g.Expect(container.Resources.Limits.Cpu().String()).To(Equal("200m"))
		g.Expect(container.Resources.Limits.Memory().String()).To(Equal("256Mi"))
	})
}

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
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("DPUCluster joinToken", func() {
	var clusterWithJoinToken = func(name string, joinToken *provisioningv1.JoinTokenSpec) *provisioningv1.DPUCluster {
		return &provisioningv1.DPUCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: provisioningv1.DPUClusterSpec{
				Type:      string(provisioningv1.StaticCluster),
				JoinToken: joinToken,
			},
		}
	}

	It("accepts kubeadm as a type", func() {
		obj := clusterWithJoinToken("jointoken-kubeadm", &provisioningv1.JoinTokenSpec{
			Type: provisioningv1.JoinTokenKubeadm,
		})
		Expect(k8sClient.Create(context.Background(), obj)).To(Succeed())
		DeferCleanup(k8sClient.Delete, context.Background(), obj)
	})

	It("accepts k0s as a type", func() {
		obj := clusterWithJoinToken("jointoken-k0s", &provisioningv1.JoinTokenSpec{
			Type: provisioningv1.JoinTokenK0s,
		})
		Expect(k8sClient.Create(context.Background(), obj)).To(Succeed())
		DeferCleanup(k8sClient.Delete, context.Background(), obj)
	})

	It("rejects a type no build implements", func() {
		err := k8sClient.Create(context.Background(), clusterWithJoinToken("jointoken-rke2",
			&provisioningv1.JoinTokenSpec{Type: "rke2"}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("Unsupported value"))
	})

	// The block is opaque on purpose, so the API server has to store what it is given rather
	// than prune keys it has no schema for.
	It("keeps a config block it has no schema for", func() {
		obj := clusterWithJoinToken("jointoken-config", &provisioningv1.JoinTokenSpec{
			Type: provisioningv1.JoinTokenK0s,
			Config: &runtime.RawExtension{Raw: []byte(
				`{"version":"1.36.3+k0s.2","profile":"dpu","somethingTheAPIHasNeverHeardOf":"kept"}`)},
		})
		Expect(k8sClient.Create(context.Background(), obj)).To(Succeed())
		DeferCleanup(k8sClient.Delete, context.Background(), obj)

		fetched := &provisioningv1.DPUCluster{}
		Expect(k8sClient.Get(context.Background(),
			types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace}, fetched)).To(Succeed())

		Expect(fetched.Spec.JoinToken.Config).NotTo(BeNil())
		Expect(string(fetched.Spec.JoinToken.Config.Raw)).To(ContainSubstring("somethingTheAPIHasNeverHeardOf"))
		Expect(string(fetched.Spec.JoinToken.Config.Raw)).To(ContainSubstring("1.36.3+k0s.2"))
	})

	It("keeps a script template reference", func() {
		obj := clusterWithJoinToken("jointoken-script-ref", &provisioningv1.JoinTokenSpec{
			Type:              provisioningv1.JoinTokenK0s,
			ScriptTemplateRef: &provisioningv1.ScriptTemplateRef{Name: "k0s-join-template", Key: "JOIN_SCRIPT"},
		})
		Expect(k8sClient.Create(context.Background(), obj)).To(Succeed())
		DeferCleanup(k8sClient.Delete, context.Background(), obj)

		fetched := &provisioningv1.DPUCluster{}
		Expect(k8sClient.Get(context.Background(),
			types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace}, fetched)).To(Succeed())
		Expect(fetched.Spec.JoinToken.ScriptTemplateRef).NotTo(BeNil())
		Expect(fetched.Spec.JoinToken.ScriptTemplateRef.Name).To(Equal("k0s-join-template"))
		Expect(fetched.Spec.JoinToken.ScriptTemplateRef.Key).To(Equal("JOIN_SCRIPT"))
	})

	It("rejects a script template reference naming no ConfigMap", func() {
		err := k8sClient.Create(context.Background(), clusterWithJoinToken("jointoken-script-noname",
			&provisioningv1.JoinTokenSpec{
				Type:              provisioningv1.JoinTokenK0s,
				ScriptTemplateRef: &provisioningv1.ScriptTemplateRef{},
			}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("name"))
	})

	// metav1.Duration always serializes a value of a minute or more as a compound duration, so 2h
	// leaves the client as "2h0m0s" and the round trip is what has to be asserted.
	DescribeTable("accepts a ttl set through the Go type",
		func(name string, ttl time.Duration) {
			obj := clusterWithJoinToken(name, &provisioningv1.JoinTokenSpec{
				TTL: &metav1.Duration{Duration: ttl},
			})
			Expect(k8sClient.Create(context.Background(), obj)).To(Succeed())
			DeferCleanup(k8sClient.Delete, context.Background(), obj)

			fetched := &provisioningv1.DPUCluster{}
			Expect(k8sClient.Get(context.Background(),
				types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace}, fetched)).To(Succeed())
			Expect(fetched.Spec.JoinToken.TTL).NotTo(BeNil())
			Expect(fetched.Spec.JoinToken.TTL.Duration).To(Equal(ttl))
		},
		Entry("the lower bound", "jointoken-ttl-min", 10*time.Minute),
		Entry("the upper bound", "jointoken-ttl-max", 24*time.Hour),
		Entry("the default", "jointoken-ttl-default", 2*time.Hour),
		Entry("an hour and a half", "jointoken-ttl-compound", 90*time.Minute),
		Entry("every component populated", "jointoken-ttl-hms", 23*time.Hour+59*time.Minute+59*time.Second),
	)

	DescribeTable("rejects a ttl outside the accepted range",
		func(name string, ttl time.Duration, wantMessage string) {
			err := k8sClient.Create(context.Background(), clusterWithJoinToken(name,
				&provisioningv1.JoinTokenSpec{TTL: &metav1.Duration{Duration: ttl}}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(wantMessage))
		},
		Entry("under the lower bound", "jointoken-ttl-short", 5*time.Minute, "at least 10m"),
		Entry("over the upper bound", "jointoken-ttl-long", 25*time.Hour, "at most 24h"),
	)

	// An hour count this large overflows the int64 the bounds are evaluated in, so the pattern has
	// to turn it away first. time.Duration cannot hold the value, so it is sent unstructured.
	It("rejects a ttl whose hour count would overflow the range check", func() {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(provisioningv1.DPUClusterGroupVersionKind)
		obj.SetName("jointoken-ttl-overflow")
		obj.SetNamespace("default")
		Expect(unstructured.SetNestedField(obj.Object,
			string(provisioningv1.StaticCluster), "spec", "type")).To(Succeed())
		Expect(unstructured.SetNestedField(obj.Object,
			"9999999h", "spec", "joinToken", "ttl")).To(Succeed())

		err := k8sClient.Create(context.Background(), obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("ttl"))
	})
})

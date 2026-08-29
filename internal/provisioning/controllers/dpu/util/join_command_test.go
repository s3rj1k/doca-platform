/*
Copyright 2025 NVIDIA

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

package util

import (
	"testing"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	"github.com/nvidia/doca-platform/test/utils"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGenerateJoinCommand(t *testing.T) {
	g := NewWithT(t)

	// Setup test resources
	namespace := "default"
	dpuCluster := utils.GetTestDPUCluster(namespace, "test-cluster")
	secret, err := utils.GetFakeKamajiClusterSecretFromEnvtest(dpuCluster, cfg)
	g.Expect(err).NotTo(HaveOccurred())

	// Create the DPUCluster and its secret
	g.Expect(testClient.Create(ctx, &dpuCluster)).To(Succeed())
	g.Expect(testClient.Create(ctx, secret)).To(Succeed())

	// Cleanup after test
	defer func() {
		g.Expect(utils.CleanupAndWait(ctx, testClient, &dpuCluster)).To(Succeed())
		g.Expect(utils.CleanupAndWait(ctx, testClient, secret)).To(Succeed())
	}()

	t.Run("valid join command generation", func(t *testing.T) {
		generator := &KubeadmBootstrapTokenGenerator{testClient}
		cmd, err := generator.GenerateJoinCommand(ctx, &dpuCluster)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cmd.Command).NotTo(BeEmpty())

		// The token behind the command, which is what makes revoking it possible.
		g.Expect(cmd.TokenID).NotTo(BeEmpty())
		g.Expect(cmd.Command).To(ContainSubstring(cmd.TokenID))
		g.Expect(cmd.ExpiresAt).To(BeTemporally("~", time.Now().Add(DefaultJoinTokenTTL), time.Minute))
	})
}

// TestJoinTokenTTL pins the gate. A kamaji cluster keeps the default whatever it asks for,
// which is the point of scoping this to static clusters.
func TestJoinTokenTTL(t *testing.T) {
	for _, tc := range []struct {
		name        string
		clusterType provisioningv1.ClusterType
		joinToken   *provisioningv1.JoinTokenSpec
		want        time.Duration
	}{
		{
			name:        "static with a ttl uses it",
			clusterType: provisioningv1.StaticCluster,
			joinToken:   &provisioningv1.JoinTokenSpec{TTL: &metav1.Duration{Duration: 4 * time.Hour}},
			want:        4 * time.Hour,
		},
		{
			name:        "static without a joinToken falls back",
			clusterType: provisioningv1.StaticCluster,
			want:        DefaultJoinTokenTTL,
		},
		{
			name:        "static with an empty joinToken falls back",
			clusterType: provisioningv1.StaticCluster,
			joinToken:   &provisioningv1.JoinTokenSpec{},
			want:        DefaultJoinTokenTTL,
		},
		{
			name:        "static with a zero ttl falls back",
			clusterType: provisioningv1.StaticCluster,
			joinToken:   &provisioningv1.JoinTokenSpec{TTL: &metav1.Duration{}},
			want:        DefaultJoinTokenTTL,
		},
		{
			name:        "kamaji ignores a ttl it was given",
			clusterType: provisioningv1.KamajiCluster,
			joinToken:   &provisioningv1.JoinTokenSpec{TTL: &metav1.Duration{Duration: 4 * time.Hour}},
			want:        DefaultJoinTokenTTL,
		},
		{
			name:        "kamaji without a ttl",
			clusterType: provisioningv1.KamajiCluster,
			want:        DefaultJoinTokenTTL,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			dc := &provisioningv1.DPUCluster{Spec: provisioningv1.DPUClusterSpec{
				Type:      string(tc.clusterType),
				JoinToken: tc.joinToken,
			}}
			g.Expect(JoinTokenTTL(dc)).To(Equal(tc.want))
		})
	}
}

func TestBootstrapTokenSecretName(t *testing.T) {
	g := NewWithT(t)
	g.Expect(BootstrapTokenSecretName("abc123")).To(Equal("bootstrap-token-abc123"))
}

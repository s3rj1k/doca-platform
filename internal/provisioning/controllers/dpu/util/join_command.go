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
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	"github.com/nvidia/doca-platform/pkg/dpucluster"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultJoinTokenTTL is how long a minted join token authenticates for when the DPUCluster
// does not ask for something else. It is also what a kamaji cluster always gets.
const DefaultJoinTokenTTL = 2 * time.Hour

// JoinCommand is a rendered join command together with the token behind it, so a caller can
// record which token a DPU was given and when it stops working.
type JoinCommand struct {
	// Command is the join command the DPU agent executes.
	Command string
	// TokenID names the bootstrap token Secret in the DPU cluster.
	TokenID string
	// ExpiresAt is when the token stops authenticating.
	ExpiresAt time.Time
}

// NodeJoinCommandGenerator is an interface for generating join commands for DPU cluster nodes.
type NodeJoinCommandGenerator interface {
	GenerateJoinCommand(ctx context.Context, dc *provisioningv1.DPUCluster) (JoinCommand, error)
}

// JoinTokenTTL reports how long a token minted for this cluster should live. Only a static
// cluster may ask for something other than the default, since kamaji clusters are DPF managed.
func JoinTokenTTL(dc *provisioningv1.DPUCluster) time.Duration {
	if dc.Spec.Type != string(provisioningv1.StaticCluster) {
		return DefaultJoinTokenTTL
	}
	if dc.Spec.JoinToken == nil || dc.Spec.JoinToken.TTL == nil || dc.Spec.JoinToken.TTL.Duration <= 0 {
		return DefaultJoinTokenTTL
	}

	return dc.Spec.JoinToken.TTL.Duration
}

// BootstrapTokenSecretName is how a bootstrap token id names its Secret. The name is fixed by
// the Kubernetes bootstrap token API, so any mechanism minting one revokes it through here.
func BootstrapTokenSecretName(tokenID string) string {
	return "bootstrap-token-" + tokenID
}

// GenerateBootstrapToken returns a token split into its id and secret halves, in the lengths
// the Kubernetes bootstrap token API requires.
func GenerateBootstrapToken() (id string, secret string, err error) {
	idBytes := make([]byte, 3)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", fmt.Errorf("failed to generate token ID: %w", err)
	}
	secretBytes := make([]byte, 8)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", fmt.Errorf("failed to generate token secret: %w", err)
	}

	return hex.EncodeToString(idBytes), hex.EncodeToString(secretBytes), nil
}

// KubeadmBootstrapTokenGenerator is a NodeJoinCommandGenerator that generates join commands following the kubeadm bootstrap token authentication method.
// It creates a bootstrap token secret and returns the join command.
// This join process is based on the kubeadm implementation.
// More details can be found in the kubeadm documentation:
// https://kubernetes.io/docs/reference/setup-tools/kubeadm/kubeadm-init/#bootstrap-token-authentication
type KubeadmBootstrapTokenGenerator struct {
	client.Client
}

// GenerateJoinCommand generates a join command for a DPU cluster node.
func (s *KubeadmBootstrapTokenGenerator) GenerateJoinCommand(ctx context.Context, dc *provisioningv1.DPUCluster) (JoinCommand, error) {
	id, secret, err := GenerateBootstrapToken()
	if err != nil {
		return JoinCommand{}, err
	}
	expiresAt := time.Now().Add(JoinTokenTTL(dc))

	// Create the bootstrap token secret.
	bootstrapToken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BootstrapTokenSecretName(id),
			Namespace: "kube-system",
		},
		Type: corev1.SecretTypeBootstrapToken,
		StringData: map[string]string{
			// This group is created by default when using kamaji clusters.
			"auth-extra-groups": "system:bootstrappers:kubeadm:default-node-token",
			// A static cluster may ask for a longer window, since its token has to survive
			// BFB flashing. A kamaji cluster always gets the default.
			"expiration":                     expiresAt.Format(time.RFC3339),
			"usage-bootstrap-authentication": "true",
			"usage-bootstrap-signing":        "true",
			"description":                    "Bootstrap token for DPU cluster node join",
			"token-id":                       id,
			"token-secret":                   secret,
		},
	}

	clusterConfig := dpucluster.NewConfig(s, dc)
	dpuClusterClient, err := clusterConfig.Client(ctx)
	if err != nil {
		return JoinCommand{}, err
	}
	err = dpuClusterClient.Create(ctx, bootstrapToken)
	if err != nil {
		return JoinCommand{}, fmt.Errorf("failed to create bootstrap token secret: %v", err)
	}
	server, err := dpucluster.NewConfig(s, dc).Server(ctx)
	if err != nil {
		return JoinCommand{}, err
	}
	// Strip the scheme (http:// or https://) from the host if present.
	// This is necessary because the join command expects a host without a scheme.
	server = strings.TrimPrefix(server, "https://")
	server = strings.TrimPrefix(server, "http://")

	// Construct the join command
	joinCommand := fmt.Sprintf("kubeadm join %s --token %s.%s --v=5",
		server,
		id,
		secret)

	// Add the CA certificate hash to the join command.
	caCertHashes, err := clusterConfig.CACertHashes(ctx)
	if err != nil {
		return JoinCommand{}, err
	}
	for _, hash := range caCertHashes {
		joinCommand += fmt.Sprintf(" --discovery-token-ca-cert-hash %s", hash)
	}

	return JoinCommand{Command: joinCommand, TokenID: id, ExpiresAt: expiresAt}, nil
}

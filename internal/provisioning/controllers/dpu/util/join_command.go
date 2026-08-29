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
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"

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
	GenerateJoinCommand(ctx context.Context, dc *provisioningv1.DPUCluster, dpu *provisioningv1.DPU) (JoinCommand, error)
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

// JoinTokenTypeFor reports how nodes join this cluster. Only a static cluster may choose,
// and kubeadm is the answer everywhere else.
func JoinTokenTypeFor(dc *provisioningv1.DPUCluster) provisioningv1.JoinTokenType {
	if dc.Spec.Type != string(provisioningv1.StaticCluster) {
		return provisioningv1.JoinTokenKubeadm
	}
	if dc.Spec.JoinToken == nil || dc.Spec.JoinToken.Type == "" {
		return provisioningv1.JoinTokenKubeadm
	}

	return dc.Spec.JoinToken.Type
}

// JoinCommandGenerators dispatches to the generator a cluster asked for. It is itself a
// NodeJoinCommandGenerator, so the call site does not know a choice is being made.
type JoinCommandGenerators struct {
	kubeadm NodeJoinCommandGenerator
	k0s     NodeJoinCommandGenerator
}

// NewJoinCommandGenerators returns the generators this build knows how to run.
func NewJoinCommandGenerators(c client.Client) *JoinCommandGenerators {
	return &JoinCommandGenerators{
		kubeadm: &KubeadmBootstrapTokenGenerator{Client: c},
		k0s:     &K0sJoinTokenGenerator{Client: c},
	}
}

// GenerateJoinCommand hands the cluster and the DPU to the generator its join token type names.
// An unknown type is an error rather than a fallback, since a wrong join shape fails on the card.
func (g *JoinCommandGenerators) GenerateJoinCommand(ctx context.Context, dc *provisioningv1.DPUCluster, dpu *provisioningv1.DPU) (JoinCommand, error) {
	if dpu == nil {
		return JoinCommand{}, fmt.Errorf("a join command needs the DPU it is for")
	}
	tokenType := JoinTokenTypeFor(dc)
	switch tokenType {
	case provisioningv1.JoinTokenKubeadm:
		return g.kubeadm.GenerateJoinCommand(ctx, dc, dpu)
	case provisioningv1.JoinTokenK0s:
		return g.k0s.GenerateJoinCommand(ctx, dc, dpu)
	default:
		return JoinCommand{}, fmt.Errorf("no join command generator for token type %q", tokenType)
	}
}

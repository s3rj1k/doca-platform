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

package util

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	cutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/util"
	"github.com/nvidia/doca-platform/pkg/dpucluster"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed join_kubeadm.sh.tmpl
var kubeadmJoinScript string

// kubeadmScriptData is what the kubeadm join script template can reference.
type kubeadmScriptData struct {
	JoinScriptData
	// APIServer is the control plane address with the scheme stripped, which is the shape
	// kubeadm expects to be handed.
	APIServer string
	// CACertHashes pin the control plane the DPU is about to trust.
	CACertHashes []string
}

// KubeadmBootstrapTokenGenerator is a NodeJoinCommandGenerator that generates join commands following the kubeadm bootstrap token authentication method.
// It creates a bootstrap token secret and returns the join command.
// This join process is based on the kubeadm implementation.
// More details can be found in the kubeadm documentation:
// https://kubernetes.io/docs/reference/setup-tools/kubeadm/kubeadm-init/#bootstrap-token-authentication
type KubeadmBootstrapTokenGenerator struct {
	client.Client
}

// GenerateJoinCommand generates a join command for a DPU cluster node, rendered from the
// template this build ships unless the cluster names a ConfigMap holding its own.
func (s *KubeadmBootstrapTokenGenerator) GenerateJoinCommand(ctx context.Context, dc *provisioningv1.DPUCluster, dpu *provisioningv1.DPU) (JoinCommand, error) {
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

	caCertHashes, err := clusterConfig.CACertHashes(ctx)
	if err != nil {
		return JoinCommand{}, err
	}

	script, err := JoinScriptTemplate(ctx, s, dc, kubeadmJoinScript)
	if err != nil {
		return JoinCommand{}, err
	}
	joinCommand, err := RenderJoinScript("join_kubeadm.sh", script, &kubeadmScriptData{
		JoinScriptData: JoinScriptData{
			JoinToken:        id + "." + secret,
			NodeName:         cutil.GenerateNodeName(dpu),
			DPUName:          dpu.Name,
			DPUNamespace:     dpu.Namespace,
			ClusterName:      dc.Name,
			ClusterNamespace: dc.Namespace,
		},
		APIServer:    server,
		CACertHashes: caCertHashes,
	})
	if err != nil {
		return JoinCommand{}, err
	}

	return JoinCommand{Command: joinCommand, TokenID: id, ExpiresAt: expiresAt}, nil
}

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

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	k0smotronv1 "github.com/nvidia/doca-platform/third_party/forked/github.com/k0sproject/k0smotron/api/k0smotron.io/v1beta2"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed join_k0s.sh.tmpl
var k0sJoinScript string

// The Cluster read goes through the manager cache, so it needs list and watch as well as get.
// JoinTokenRequests are only ever written, and writes bypass the cache.
// +kubebuilder:rbac:groups=k0smotron.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=k0smotron.io,resources=jointokenrequests,verbs=create;delete

const (
	// K0smotronClusterType is the DPUCluster type the k0smotron cluster manager owns. The
	// prefixed form is what DPUClusterSpec.Type reserves for an out of tree manager.
	K0smotronClusterType = "k0smotron.io/k0smotron"

	// k0smotronTokenExpiry has to cover minting through BFB flashing and the worker's
	// first join attempt.
	k0smotronTokenExpiry = "2h"

	// k0sInstallPath is where the k0s get script puts the binary by default.
	k0sInstallPath = "/usr/local/bin"

	// k0sWorkerProfile is the worker profile a DPU joins under. The k0smotron Cluster has
	// to define it in spec.k0sConfig, since k0s rejects a profile it does not know.
	k0sWorkerProfile = "dpu"

	// k0smotronWorkerRole is the role a DPU joins under, as opposed to a controller.
	k0smotronWorkerRole = "worker"

	// joinTokenSecretKey is the Secret key k0smotron writes the worker token under.
	joinTokenSecretKey = "token"
)

// k0sJoinScriptData is what join_k0s.sh.tmpl can reference.
type k0sJoinScriptData struct {
	JoinToken      string
	NodeName       string
	K0sVersion     string
	K0sInstallPath string
	K0sProfile     string
	DPUName        string
	DPUNamespace   string
}

// K0smotronJoinTokenGenerator joins a DPU to a control plane k0smotron hosts. It asks
// k0smotron for a worker token and renders the script that presents it.
type K0smotronJoinTokenGenerator struct {
	client.Client
}

// K0smotronClusterKey names the k0smotron Cluster backing a DPUCluster. The two share a
// name and namespace, which is also how the cluster manager creates it.
func K0smotronClusterKey(dc *provisioningv1.DPUCluster) types.NamespacedName {
	return types.NamespacedName{Namespace: dc.Namespace, Name: dc.Name}
}

// K0smotronJoinTokenRequestName names the JoinTokenRequest minted for a DPU. It carries the
// DPU namespace because the request lives beside the Cluster, which may be elsewhere.
func K0smotronJoinTokenRequestName(dpu *provisioningv1.DPU) string {
	return fmt.Sprintf("join-%s-%s", dpu.Namespace, dpu.Name)
}

// GenerateJoinCommand returns the script that joins this DPU as a k0s worker. The token is
// minted by k0smotron rather than by DPF, so revoking it means deleting the request.
func (g *K0smotronJoinTokenGenerator) GenerateJoinCommand(ctx context.Context, dc *provisioningv1.DPUCluster, dpu *provisioningv1.DPU) (string, error) {
	if dpu == nil {
		return "", fmt.Errorf("a join command needs the DPU it is for")
	}
	clusterKey := K0smotronClusterKey(dc)

	version, err := g.k0sVersion(ctx, clusterKey)
	if err != nil {
		return "", err
	}

	if err := g.ensureJoinTokenRequest(ctx, clusterKey, dpu); err != nil {
		return "", err
	}

	token, err := g.joinToken(ctx, clusterKey.Namespace, K0smotronJoinTokenRequestName(dpu))
	if err != nil {
		return "", err
	}

	// Every value lands inside single quotes in the script, so one carrying a quote would
	// escape it and run as root.
	for field, value := range map[string]string{
		"token": token, "version": version, "node name": dpu.Name,
	} {
		if !ShellSafe(value) {
			return "", fmt.Errorf("the k0s %s contains a character a shell would read", field)
		}
	}

	return RenderJoinScript("k0s", k0sJoinScript, k0sJoinScriptData{
		JoinToken:      token,
		NodeName:       dpu.Name,
		K0sVersion:     version,
		K0sInstallPath: k0sInstallPath,
		K0sProfile:     k0sWorkerProfile,
		DPUName:        dpu.Name,
		DPUNamespace:   dpu.Namespace,
	})
}

// k0sVersion reads the k0s version off the k0smotron Cluster, so a worker never runs a
// version the control plane did not ask for.
func (g *K0smotronJoinTokenGenerator) k0sVersion(ctx context.Context, key types.NamespacedName) (string, error) {
	cluster := &k0smotronv1.Cluster{}
	if err := g.Get(ctx, key, cluster); err != nil {
		return "", fmt.Errorf("failed to get k0smotron Cluster %s: %w", key, err)
	}

	if cluster.Spec.Version == "" {
		// k0smotron picks a version when the field is empty, but it does not report which,
		// so a worker has nothing to match and would drift from the control plane.
		return "", fmt.Errorf("k0smotron Cluster %s has no spec.version, which a worker needs to match", key)
	}

	return cluster.Spec.Version, nil
}

// ensureJoinTokenRequest mints the token, reusing an existing request only when it still asks
// for what this DPU needs and its Secret is still there.
func (g *K0smotronJoinTokenGenerator) ensureJoinTokenRequest(ctx context.Context, clusterKey types.NamespacedName, dpu *provisioningv1.DPU) error {
	request := &k0smotronv1.JoinTokenRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name: K0smotronJoinTokenRequestName(dpu),
			// v1beta2 dropped the cross namespace reference, so it sits beside the Cluster.
			Namespace: clusterKey.Namespace,
		},
		Spec: k0smotronv1.JoinTokenRequestSpec{
			ClusterName: clusterKey.Name,
			Expiry:      k0smotronTokenExpiry,
			Role:        k0smotronWorkerRole,
		},
	}
	key := client.ObjectKeyFromObject(request)

	existing := &k0smotronv1.JoinTokenRequest{}
	err := g.Get(ctx, key, existing)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("failed to get JoinTokenRequest %s: %w", key, err)
	default:
		reusable, err := g.reusableJoinTokenRequest(ctx, existing, request)
		if err != nil {
			return err
		}
		if reusable {
			return nil
		}
		// k0smotron invalidates the old token as the request goes, so this both retires a
		// stale credential and lets a fresh one be minted.
		if err := g.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to replace JoinTokenRequest %s: %w", key, err)
		}
	}

	if err := g.Create(ctx, request); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create JoinTokenRequest %s: %w", key, err)
	}

	return nil
}

// reusableJoinTokenRequest reports whether an existing request can be left alone. Not if it asks
// for something else, the spec being immutable, nor if its Secret has gone, as an expired one has.
func (g *K0smotronJoinTokenGenerator) reusableJoinTokenRequest(ctx context.Context, existing, desired *k0smotronv1.JoinTokenRequest) (bool, error) {
	if existing.Spec.ClusterName != desired.Spec.ClusterName || existing.Spec.Role != desired.Spec.Role {
		return false, nil
	}

	secretKey := types.NamespacedName{Namespace: existing.Namespace, Name: existing.Name}
	err := g.Get(ctx, secretKey, &corev1.Secret{})
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("failed to get the join token Secret %s: %w", secretKey, err)
	}

	return true, nil
}

// joinToken reads the Secret k0smotron writes for a request. The Secret trails the request,
// so a miss here is reported and the caller retries.
func (g *K0smotronJoinTokenGenerator) joinToken(ctx context.Context, namespace, name string) (string, error) {
	secret := &corev1.Secret{}
	if err := g.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		return "", fmt.Errorf("failed to get the join token Secret %s/%s: %w", namespace, name, err)
	}

	token, ok := secret.Data[joinTokenSecretKey]
	if !ok || len(token) == 0 {
		return "", fmt.Errorf("the join token Secret %s/%s has no %q yet", namespace, name, joinTokenSecretKey)
	}

	// k0s emits the token with a trailing newline and k0smotron stores it verbatim. Left in,
	// it fails the shell safety check and no DPU could ever join.
	trimmed := strings.TrimSpace(string(token))
	if trimmed == "" {
		return "", fmt.Errorf("the join token Secret %s/%s has an empty %q", namespace, name, joinTokenSecretKey)
	}

	return trimmed, nil
}

// DeleteK0smotronJoinToken revokes the token minted for a DPU. k0smotron invalidates it when
// the request goes, so nothing else has to reach into the child cluster.
func DeleteK0smotronJoinToken(ctx context.Context, c client.Client, dc *provisioningv1.DPUCluster, dpu *provisioningv1.DPU) error {
	request := &k0smotronv1.JoinTokenRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      K0smotronJoinTokenRequestName(dpu),
			Namespace: K0smotronClusterKey(dc).Namespace,
		},
	}

	err := c.Delete(ctx, request)
	// A missing CRD means k0smotron itself is gone, which takes the token with it.
	if err == nil || apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}

	return fmt.Errorf("failed to delete JoinTokenRequest %s/%s: %w", request.GetNamespace(), request.GetName(), err)
}

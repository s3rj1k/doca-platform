/*
Copyright 2024 NVIDIA

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

package state

import (
	"context"
	"fmt"
	"os"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	dutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/dpu/util"
	cutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/util"
	providentity "github.com/nvidia/doca-platform/internal/provisioning/utils/certificate/identity"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func Deleting(ctx context.Context, dpu *provisioningv1.DPU, ctrlCtx *dutil.ControllerContext) (provisioningv1.DPUStatus, error) {
	logger := log.FromContext(ctx)
	state := dpu.Status.DeepCopy()

	ctrlCtx.DPUInProvisioningMap.Remove(dutil.DPUID(dpu.UID))
	ctrlCtx.ClusterAllocator.ReleaseDPU(dpu)

	if err := RemoveDpuDeviceFinalizer(ctx, dpu, ctrlCtx); err != nil {
		err = fmt.Errorf("failed to remove DpuDevice finalizer: %w", err)
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondDeleting.String(), err, "RemoveDpuDeviceFinalizerError", err.Error()))
		return *state, err
	}

	if err := RemoveRequestorFromDPUNodeMaintenance(ctx, dpu, ctrlCtx); err != nil {
		err = fmt.Errorf("failed to remove requestor from dpunodemaintenance: %w", err)
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondDeleting.String(), err, "RemoveRequestorFromDPUNodeMaintenanceError", err.Error()))
		return *state, err
	}

	cfgVersion := cutil.GenerateBFCFGFileName(dpu.Name, string(dpu.UID))

	// Make sure there is no old bf cfg file in the shared volume
	cfgFile := cutil.GenerateBFBCFGFilePath(cfgVersion)
	if err := os.Remove(cfgFile); err != nil && !os.IsNotExist(err) {
		err = fmt.Errorf("delete BFB CFG file %s failed: %w", cfgFile, err)
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondDeleting.String(), err, "DeleteBFBCFGFileError", err.Error()))
		return *state, err
	}

	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cert-manager.io",
		Version: "v1",
		Kind:    "Certificate",
	})
	certificate.SetName(cutil.GenerateDMSServerCertName(dpu.Name))
	certificate.SetNamespace(dpu.Namespace)

	certificateRequest := &unstructured.Unstructured{}
	certificateRequest.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cert-manager.io",
		Version: "v1",
		Kind:    "CertificateRequest",
	})
	certificateRequest.SetName(dpu.Name)
	certificateRequest.SetNamespace(dpu.Namespace)

	deleteObjects := []crclient.Object{
		certificate,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cutil.GenerateDMSServerSecretName(dpu.Name),
				Namespace: dpu.Namespace,
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cutil.KubeadmJoinSecretName(dpu.Name),
				Namespace: dpu.Namespace,
			},
		},
		certificateRequest,
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      providentity.DPUAgentUsername(dpu.Name),
				Namespace: dpu.Namespace,
			},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      providentity.DPUAgentUsername(dpu.Name),
				Namespace: dpu.Namespace,
			},
		},
	}

	// Before the join Secret goes, since the annotation naming the token is on it. A failure
	// has to stop the deletion, or the Secret is removed and the token can never be found.
	if err := revokeJoinToken(ctx, ctrlCtx.Client, dpu); err != nil {
		err = fmt.Errorf("failed to revoke join token: %w", err)
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondDeleting.String(), err, "RevokeJoinTokenError", err.Error()))
		return *state, err
	}

	objects, err := cutil.GetObjects(ctx, ctrlCtx.Client, deleteObjects)
	if err != nil {
		err = fmt.Errorf("failed to get objects: %w", err)
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondDeleting.String(), err, "GetObjectsError", err.Error()))
		return *state, err
	}
	for _, object := range objects {
		logger.V(3).Info(fmt.Sprintf("delete object %s/%s", object.GetNamespace(), object.GetName()))
		if err := cutil.DeleteObjects(ctx, ctrlCtx.Client, object); err != nil {
			err = fmt.Errorf("failed to delete objects: %w", err)
			cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondDeleting.String(), err, "DeleteObjectsError", err.Error()))
			return *state, err
		}
	}

	if err := cutil.DeleteDPUAgentBootstrapTokens(ctx, ctrlCtx.Client, dpu.Name, dpu.Namespace); err != nil {
		err = fmt.Errorf("failed to delete bootstrap tokens: %w", err)
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondDeleting.String(), err, "DeleteBootstrapTokensError", err.Error()))
		return *state, err
	}

	if err := deleteNode(ctx, ctrlCtx.Client, dpu); err != nil {
		err = fmt.Errorf("failed to delete node: %w", err)
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondDeleting.String(), err, "DeleteNodeError", err.Error()))
		return *state, err
	}

	if dpu.Status.DPUInstallInterface != nil {
		switch *dpu.Status.DPUInstallInterface {
		case string(provisioningv1.InstallViaRedFish), string(provisioningv1.InstallViaGNOI), string(provisioningv1.InstallViaHostAgent):
			// TODO: Deploy a K8S Job to run "cleanup-dpf" script to remove any containers and api server reference in the dpu
		default:
			err = fmt.Errorf("invalid DPUInstallInterface: %s", *dpu.Status.DPUInstallInterface)
			cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondDeleting.String(), err, "InvalidDPUInstallInterface", err.Error()))
			return *state, err
		}
	}

	if len(objects) == 0 {
		controllerutil.RemoveFinalizer(dpu, provisioningv1.DPUFinalizer)
		if err := ctrlCtx.Update(ctx, dpu); err != nil {
			err = fmt.Errorf("failed to update DPU: %w", err)
			cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondDeleting.String(), err, "UpdateError", err.Error()))
			return *state, err
		}
	}

	return *state, nil
}

// RemoveDpuDeviceFinalizer removes the DpuDevice finalizer when DPU is being deleted.
// Exported so mock.Deleting can reuse it for tests that expect finalizer removal.
func RemoveDpuDeviceFinalizer(ctx context.Context, dpu *provisioningv1.DPU, ctrlCtx *dutil.ControllerContext) error {
	dpuDevice := &provisioningv1.DPUDevice{}
	if err := ctrlCtx.Client.Get(ctx, crclient.ObjectKey{Namespace: dpu.Namespace, Name: dpu.Spec.DPUDeviceName}, dpuDevice); err != nil {
		if apierrors.IsNotFound(err) {
			// DpuDevice not found, this is expected in some cases
			return nil
		}
		return fmt.Errorf("failed to get DpuDevice %s: %w", dpu.Spec.DPUDeviceName, err)
	}

	if controllerutil.ContainsFinalizer(dpuDevice, provisioningv1.DPUDeviceFinalizer) {
		controllerutil.RemoveFinalizer(dpuDevice, provisioningv1.DPUDeviceFinalizer)
		if err := ctrlCtx.Client.Update(ctx, dpuDevice); err != nil {
			return fmt.Errorf("failed to remove DpuDevice finalizer: %w", err)
		}
	}
	return nil
}

// revokeJoinToken deletes the bootstrap token the join Secret names. It has to run before that
// Secret is deleted, since the annotation is the only record of which token to revoke.
func revokeJoinToken(ctx context.Context, client crclient.Client, dpu *provisioningv1.DPU) error {
	// Nothing was ever minted for a DPU with no cluster.
	if dpu.Spec.Cluster.Name == "" {
		return nil
	}

	joinSecret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: dpu.Namespace, Name: cutil.KubeadmJoinSecretName(dpu.Name)}
	if err := client.Get(ctx, key, joinSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading join Secret %s: %w", key, err)
	}

	tokenID := joinSecret.Annotations[cutil.JoinTokenIDAnnotation]
	if tokenID == "" {
		// A kamaji cluster, or a Secret written before the token was recorded.
		return nil
	}

	dc := &provisioningv1.DPUCluster{}
	nn := types.NamespacedName{Namespace: dpu.Spec.Cluster.Namespace, Name: dpu.Spec.Cluster.Name}
	if err := client.Get(ctx, nn, dc); err != nil {
		if apierrors.IsNotFound(err) {
			// The cluster is gone, so its tokens went with it.
			return nil
		}
		return fmt.Errorf("reading DPUCluster %s: %w", nn, err)
	}

	return revokeBootstrapToken(ctx, client, dc, tokenID)
}

// revokeBootstrapToken deletes the token Secret an id names from the DPU cluster, where nothing
// owns it and no garbage collector reaches it. A token already gone is a revoked token.
func revokeBootstrapToken(ctx context.Context, client crclient.Client, dc *provisioningv1.DPUCluster, tokenID string) error {
	dpuClient, _, err := cutil.GetClientset(ctx, client, dc)
	if err != nil {
		return fmt.Errorf("reaching DPU cluster to revoke token %s: %w", tokenID, err)
	}

	name := dutil.BootstrapTokenSecretName(tokenID)
	if err := dpuClient.CoreV1().Secrets(metav1.NamespaceSystem).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("revoking join token %s: %w", name, err)
		}
		return nil
	}
	log.FromContext(ctx).Info("revoked join token", "token", name)

	return nil
}

func deleteNode(ctx context.Context, client crclient.Client, dpu *provisioningv1.DPU) error {
	logger := log.FromContext(ctx)
	if dpu.Spec.Cluster.Name == "" {
		logger.Info("DPU not assigned, skip deleting Node")
		return nil
	}

	nn := types.NamespacedName{
		Namespace: dpu.Spec.Cluster.Namespace,
		Name:      dpu.Spec.Cluster.Name,
	}
	dc := &provisioningv1.DPUCluster{}
	if err := client.Get(ctx, nn, dc); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("DPUCluster has been deleted, skip deleting Node")
			return nil
		}
		return err
	}
	dpuClient, _, err := cutil.GetClientset(ctx, client, dc)
	if err != nil {
		return fmt.Errorf("failed to create client for DPU cluster, err: %v", err)
	}
	err = dpuClient.CoreV1().Nodes().Delete(ctx, cutil.GenerateNodeName(dpu), metav1.DeleteOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	logger.Info("deleted Node from DPU cluster")
	return nil
}

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

package state

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	dutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/dpu/util"
	cutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/util"
	"github.com/nvidia/doca-platform/pkg/bfcfg"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	BFCFGDir = "bfcfg"
)

func PrepareBFB(ctx context.Context, dpu *provisioningv1.DPU, ctrlCtx *dutil.ControllerContext) (provisioningv1.DPUStatus, error) {
	state := dpu.Status.DeepCopy()
	logger := log.FromContext(ctx)
	flavor := &provisioningv1.DPUFlavor{}
	if err := ctrlCtx.Get(ctx, types.NamespacedName{
		Namespace: dpu.Namespace,
		Name:      dpu.Spec.DPUFlavor,
	}, flavor); err != nil {
		if apierrors.IsNotFound(err) {
			cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "DPUFlavorNotFound", err.Error()))
			return *state, err
		}
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "FailedToGetFlavor", err.Error()))
		return *state, err
	}
	dc := &provisioningv1.DPUCluster{}
	if err := ctrlCtx.Get(ctx, types.NamespacedName{Namespace: dpu.Spec.Cluster.Namespace, Name: dpu.Spec.Cluster.Name}, dc); err != nil {
		if apierrors.IsNotFound(err) {
			cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "DPUClusterNotFound", err.Error()))
			return *state, err
		}
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "FailedToGetDPUCluster", err.Error()))
		return *state, err
	}
	dpuNode := &provisioningv1.DPUNode{}
	if err := ctrlCtx.Get(ctx, types.NamespacedName{Namespace: dpu.Namespace, Name: dpu.Spec.DPUNodeName}, dpuNode); err != nil {
		if apierrors.IsNotFound(err) {
			cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "DPUNodeNotFound", err.Error()))
			return *state, err
		}
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "FailedToGetDPUNode", err.Error()))
		return *state, err
	}
	bfCFGPath := filepath.Join("/", cutil.BFBBaseDir, BFCFGDir, fmt.Sprintf("%s_%s_%s", dpu.Namespace, dpu.Name, dpu.UID))
	if err := os.MkdirAll(filepath.Dir(bfCFGPath), os.ModePerm); err != nil {
		err = fmt.Errorf("failed to create directory %s: %w", filepath.Dir(bfCFGPath), err)
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "FailedToCreateDirectory", err.Error()))
		return *state, err
	}

	kubeadmSecretName := cutil.KubeadmJoinSecretName(dpu.Name)

	// This phase is re-entered on every later error, so the Secret is looked up first and a
	// token is minted only when it is absent. Minting per attempt would leak every token but
	// the first and record an expiry the DPU never presents.
	existingSecret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: dpu.Namespace, Name: kubeadmSecretName}
	getErr := ctrlCtx.Client.Get(ctx, secretKey, existingSecret)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		err := fmt.Errorf("failed to read kubeadm join secret: %w", getErr)
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "FailedToCreateKubeadmSecret", err.Error()))
		return *state, err
	}

	if getErr == nil {
		// Report the token the DPU will actually present rather than a fresher one.
		if expiresAt, ok := cutil.JoinTokenExpiresAtFrom(existingSecret); ok {
			state.JoinTokenExpiresAt = ptr.To(metav1.NewTime(expiresAt))
		}
	} else {
		joinCommand, err := ctrlCtx.JoinCommandGenerator.GenerateJoinCommand(ctx, dc)
		if err != nil {
			err = fmt.Errorf("failed to generate join command: %w", err)
			cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "FailedToGenerateJoinCommand", err.Error()))
			return *state, err
		}

		kubeadmSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      kubeadmSecretName,
				Namespace: dpu.Namespace,
				// Stamped at creation, because the provisioning role has no update on Secrets.
				// Only for a static cluster, so a kamaji Secret is unchanged.
				Annotations: cutil.JoinTokenAnnotations(dc, joinCommand.TokenID, joinCommand.ExpiresAt),
			},
			Data: map[string][]byte{
				"join": []byte(joinCommand.Command),
			},
		}
		if err := controllerutil.SetOwnerReference(dpu, kubeadmSecret, ctrlCtx.Client.Scheme()); err != nil {
			err = fmt.Errorf("failed to set owner reference on kubeadm join secret: %w", err)
			cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "FailedToCreateKubeadmSecret", err.Error()))
			return *state, err
		}
		createErr := ctrlCtx.Client.Create(ctx, kubeadmSecret)
		if createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			err := fmt.Errorf("failed to create kubeadm join secret: %w", createErr)
			cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "FailedToCreateKubeadmSecret", err.Error()))
			return *state, err
		}
		if apierrors.IsAlreadyExists(createErr) {
			// Another writer won, so the token just minted is one no Secret names and nothing
			// would ever revoke. Revoke it here and report the expiry of the one that did land.
			if joinCommand.TokenID != "" {
				if err := revokeBootstrapToken(ctx, ctrlCtx.Client, dc, joinCommand.TokenID); err != nil {
					err = fmt.Errorf("failed to revoke the superseded join token: %w", err)
					cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "FailedToRevokeJoinToken", err.Error()))
					return *state, err
				}
			}
			storedSecret := &corev1.Secret{}
			if err := ctrlCtx.Client.Get(ctx, secretKey, storedSecret); err != nil {
				err = fmt.Errorf("failed to read the kubeadm join secret that already existed: %w", err)
				cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "FailedToCreateKubeadmSecret", err.Error()))
				return *state, err
			}
			if expiresAt, ok := cutil.JoinTokenExpiresAtFrom(storedSecret); ok {
				state.JoinTokenExpiresAt = ptr.To(metav1.NewTime(expiresAt))
			}
		}
		// Recorded only when this token is the one that was stored, so the expiry always
		// describes the join command the Secret carries.
		if createErr == nil && dc.Spec.Type == string(provisioningv1.StaticCluster) {
			state.JoinTokenExpiresAt = ptr.To(metav1.NewTime(joinCommand.ExpiresAt))
		}
	}

	dpuDevice := &provisioningv1.DPUDevice{}
	if err := ctrlCtx.Get(ctx, types.NamespacedName{Namespace: dpu.Namespace, Name: dpu.Spec.DPUDeviceName}, dpuDevice); err != nil {
		if apierrors.IsNotFound(err) {
			cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "DPUDeviceNotFound", err.Error()))
			return *state, err
		}
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "FailedToGetDPUDevice", err.Error()))
		return *state, err
	}

	cfg, err := bfcfg.GenerateBFConfig(ctx, ctrlCtx, dpu, flavor)
	if err != nil {
		err = fmt.Errorf("failed to generate bf.cfg: %w", err)
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "FailedToGenerateBFConfig", err.Error()))
		return *state, err
	}
	logger.Info(fmt.Sprintf("write bf.cfg to %s", bfCFGPath))
	if err := os.WriteFile(bfCFGPath, cfg, os.ModePerm); err != nil {
		err = fmt.Errorf("failed to write bf.cfg to %s: %w", bfCFGPath, err)
		cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), err, "FailedToPushBFCFG", err.Error()))
		return *state, err
	}
	state.BFCFGFile = bfCFGPath
	cutil.SetDPUCondition(state, cutil.NewCondition(provisioningv1.DPUCondBFBPrepared.String(), nil, "", ""))
	state.Phase = provisioningv1.DPUOSInstalling
	return *state, nil
}

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

// Package joinpayload runs the join payload a cluster manager rendered for this DPU. It is
// the distribution neutral half of ConfigureKubelet, carrying none of the kubeadm side effects.
package joinpayload

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/nvidia/doca-platform/internal/provisioning/dpuagent/operations"
	"github.com/nvidia/doca-platform/internal/provisioning/utils/bash"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	conditionType = "JoinPayloadExecuted"

	// payloadKey is the Secret key the provisioning controller writes the payload under.
	// It matches the key ConfigureKubelet reads, since both consume the same Secret.
	payloadKey = "join"

	// fetchTimeout bounds the read of the payload Secret. The agent retries the whole
	// operation, so a slow API server costs a poll interval rather than the run.
	fetchTimeout = time.Minute
)

// RunJoinPayload fetches the join payload for this DPU and executes it as bash. The payload
// is opaque here, so the controller decides what joining means and this only runs it.
type RunJoinPayload struct {
	// runBash, if non-nil, replaces the default bash runner (tests only).
	runBash func(cmd string) (bytes.Buffer, bytes.Buffer, error)
}

func (r *RunJoinPayload) Name() string {
	return "Run Join Payload"
}

func (r *RunJoinPayload) ConditionType() string {
	return conditionType
}

// ShouldSkip keeps the operation inert unless a cluster manager asked for it, so a DPU
// joined by ConfigureKubelet is unaffected.
func (r *RunJoinPayload) ShouldSkip(optCtx *operations.Context) bool {
	return !optCtx.Options.RunJoinPayload
}

func (r *RunJoinPayload) ShouldUpdateStatusBeforeContinue(_ *operations.Context) bool {
	return false
}

func (r *RunJoinPayload) Execute(execCtx context.Context, optCtx *operations.Context) error {
	if optCtx.LatestDPU == nil {
		return fmt.Errorf("latest DPU not retrieved (this should never happen)")
	}
	// Running a join twice would re-register the node, so the condition is the record
	// of it having already run.
	if optCtx.LatestDPU.Status.AgentStatus != nil {
		cond := meta.FindStatusCondition(optCtx.LatestDPU.Status.AgentStatus.Conditions, conditionType)
		if cond != nil && cond.Status == metav1.ConditionTrue {
			klog.Infof("Join payload already executed, skip")
			return nil
		}
	}

	timeCtx, cancel := context.WithTimeout(execCtx, fetchTimeout)
	defer cancel()

	name, namespace := optCtx.Options.JoinSecret()
	key := client.ObjectKey{Namespace: namespace, Name: name}
	secret := &corev1.Secret{}
	if err := optCtx.Client.Get(timeCtx, key, secret); err != nil {
		return fmt.Errorf("failed to get join payload secret %s/%s: %w", key.Namespace, key.Name, err)
	}

	payload, ok := secret.Data[payloadKey]
	if !ok {
		return fmt.Errorf("join payload secret %s/%s does not contain key %q", key.Namespace, key.Name, payloadKey)
	}
	if len(payload) == 0 {
		return fmt.Errorf("join payload secret %s/%s has an empty key %q", key.Namespace, key.Name, payloadKey)
	}

	run := r.runBash
	if run == nil {
		run = bash.Run
	}
	stdout, stderr, err := run(string(payload))
	if err != nil {
		// The payload comes from a template the cluster manager owns, so its own output
		// is the only useful diagnostic when it fails.
		return fmt.Errorf("failed to run join payload: %w, stdout: %s, stderr: %s", err, stdout.String(), stderr.String())
	}

	klog.Infof("Executed join payload from secret %s/%s", key.Namespace, key.Name)

	return nil
}

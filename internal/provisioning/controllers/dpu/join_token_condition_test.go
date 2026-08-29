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

package dpu

import (
	"testing"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	cutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/util"
	"github.com/nvidia/doca-platform/internal/provisioning/dpuagent/operations/kubelet"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestSetJoinTokenValidCondition(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	joined := &provisioningv1.AgentStatus{Conditions: []metav1.Condition{{
		Type:   kubelet.ConditionType,
		Status: metav1.ConditionTrue,
	}}}

	for _, tc := range []struct {
		name        string
		expiresAt   *metav1.Time
		agentStatus *provisioningv1.AgentStatus
		seed        *metav1.Condition
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantAbsent  bool
	}{
		{
			name:       "no expiry recorded leaves the condition unset",
			wantAbsent: true,
		},
		{
			name:       "a token still in date is valid",
			expiresAt:  ptr.To(metav1.NewTime(now.Add(time.Hour))),
			wantStatus: metav1.ConditionTrue,
			wantReason: "Valid",
		},
		{
			name:       "a lapsed token is reported expired",
			expiresAt:  ptr.To(metav1.NewTime(now.Add(-time.Minute))),
			wantStatus: metav1.ConditionFalse,
			wantReason: "Expired",
		},
		{
			name:        "a joined DPU keeps whatever it had, since nothing reads the token again",
			expiresAt:   ptr.To(metav1.NewTime(now.Add(-time.Hour))),
			agentStatus: joined,
			seed: &metav1.Condition{
				Type:    provisioningv1.DPUCondJoinTokenValid.String(),
				Status:  metav1.ConditionTrue,
				Reason:  "Valid",
				Message: "join token expires later",
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: "Valid",
		},
		{
			name:        "a joined DPU that never had the condition still gets one",
			expiresAt:   ptr.To(metav1.NewTime(now.Add(time.Hour))),
			agentStatus: joined,
			wantStatus:  metav1.ConditionTrue,
			wantReason:  "Valid",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			state := &provisioningv1.DPUStatus{
				JoinTokenExpiresAt: tc.expiresAt,
				AgentStatus:        tc.agentStatus,
			}
			if tc.seed != nil {
				cutil.SetDPUCondition(state, tc.seed)
			}

			setJoinTokenValidCondition(state, now)

			_, cond := cutil.GetDPUCondition(state, provisioningv1.DPUCondJoinTokenValid.String())
			if tc.wantAbsent {
				g.Expect(cond).To(BeNil())
				return
			}
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(tc.wantStatus))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
		})
	}
}

func TestJoinTokenSpent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state *provisioningv1.DPUStatus
		want  bool
	}{
		{name: "no agent status", state: &provisioningv1.DPUStatus{}},
		{
			name:  "agent reports nothing",
			state: &provisioningv1.DPUStatus{AgentStatus: &provisioningv1.AgentStatus{}},
		},
		{
			name: "condition present but false",
			state: &provisioningv1.DPUStatus{AgentStatus: &provisioningv1.AgentStatus{Conditions: []metav1.Condition{{
				Type: kubelet.ConditionType, Status: metav1.ConditionFalse,
			}}}},
		},
		{
			name: "condition true",
			state: &provisioningv1.DPUStatus{AgentStatus: &provisioningv1.AgentStatus{Conditions: []metav1.Condition{{
				Type: kubelet.ConditionType, Status: metav1.ConditionTrue,
			}}}},
			want: true,
		},
		{
			// A flavor that skips ConfigureKubelet never reports the condition, so reaching
			// Ready has to count or the token is reported expired on a healthy DPU forever.
			name:  "ready without the agent condition",
			state: &provisioningv1.DPUStatus{Phase: provisioningv1.DPUReady},
			want:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			NewWithT(t).Expect(joinTokenSpent(tc.state)).To(Equal(tc.want))
		})
	}
}

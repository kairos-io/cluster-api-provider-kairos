/*
Copyright 2024 The Kairos CAPI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing
permissions and limitations under the License.
*/

package controlplane

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/log"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// TestSSHFallbackReconciler_EvaluateEligibility exercises the predicate
// math: condition status + reason + LastNodePushObserved age determine
// when the worker fires. The actual Enqueue() pathway is covered by the
// worker tests; here we pin the eligibility table.
func TestSSHFallbackReconciler_EvaluateEligibility(t *testing.T) {
	r := &SSHFallbackReconciler{}

	stale := metav1.NewTime(time.Now().Add(-30 * time.Minute))
	fresh := metav1.NewTime(time.Now().Add(-1 * time.Minute))

	cases := []struct {
		name        string
		condStatus  corev1.ConditionStatus
		condReason  string
		anchor      *metav1.Time
		activateAft *metav1.Duration
		wantEligi   bool
	}{
		{
			name:       "no condition → not eligible",
			condStatus: "", condReason: "",
			anchor: &stale, wantEligi: false,
		},
		{
			name:       "condition True → not eligible (already done)",
			condStatus: corev1.ConditionTrue, condReason: controlplanev1beta2.KubeconfigReadyReason,
			anchor: &stale, wantEligi: false,
		},
		{
			name:       "condition False(WaitingForNodePush) + stale anchor → eligible",
			condStatus: corev1.ConditionFalse, condReason: controlplanev1beta2.WaitingForNodePushReason,
			anchor: &stale, wantEligi: true,
		},
		{
			name:       "condition False(WaitingForNodePush) + fresh anchor → not eligible (too early)",
			condStatus: corev1.ConditionFalse, condReason: controlplanev1beta2.WaitingForNodePushReason,
			anchor: &fresh, wantEligi: false,
		},
		{
			name:       "condition False(WaitingForNodePush) + nil anchor → not eligible",
			condStatus: corev1.ConditionFalse, condReason: controlplanev1beta2.WaitingForNodePushReason,
			anchor: nil, wantEligi: false,
		},
		{
			name:       "condition False(SSHFallbackDialing) → not eligible (in flight)",
			condStatus: corev1.ConditionFalse, condReason: controlplanev1beta2.SSHFallbackDialingReason,
			anchor: &stale, wantEligi: false,
		},
		{
			name:       "condition False(SSHFallbackFailed) + stale anchor → eligible (retry)",
			condStatus: corev1.ConditionFalse, condReason: controlplanev1beta2.SSHFallbackFailedReason,
			anchor: &stale, wantEligi: true,
		},
		{
			name:       "condition False(SSHFallbackMisconfigured) + stale anchor → eligible (retry)",
			condStatus: corev1.ConditionFalse, condReason: controlplanev1beta2.SSHFallbackMisconfiguredReason,
			anchor: &stale, wantEligi: true,
		},
		{
			name:       "custom-set False(SomeOtherReason) → not eligible (defensive)",
			condStatus: corev1.ConditionFalse, condReason: "SomeOtherReason",
			anchor: &stale, wantEligi: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			kcp := &controlplanev1beta2.KairosControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "kcp", Namespace: "default"},
				Spec: controlplanev1beta2.KairosControlPlaneSpec{
					SSHFallback: &controlplanev1beta2.SSHFallback{
						Enabled:       true,
						ActivateAfter: tc.activateAft,
					},
				},
				Status: controlplanev1beta2.KairosControlPlaneStatus{
					LastNodePushObserved: tc.anchor,
				},
			}
			if tc.condStatus != "" {
				conditions.Set(kcp, &clusterv1.Condition{
					Type:   controlplanev1beta2.KubeconfigReadyCondition,
					Status: tc.condStatus,
					Reason: tc.condReason,
				})
			}
			eligible, _ := r.evaluateEligibility(t.Context(), log.Log, kcp)
			g.Expect(eligible).To(Equal(tc.wantEligi))
		})
	}
}

// TestPreferredMachineAddress exercises the InternalIP > ExternalIP >
// any priority order used by the reconciler when resolving the worker's
// dial target.
func TestPreferredMachineAddress(t *testing.T) {
	cases := []struct {
		name string
		in   []clusterv1.MachineAddress
		want string
	}{
		{
			name: "InternalIP wins over ExternalIP",
			in: []clusterv1.MachineAddress{
				{Type: clusterv1.MachineExternalIP, Address: "203.0.113.5"},
				{Type: clusterv1.MachineInternalIP, Address: "10.0.0.5"},
			},
			want: "10.0.0.5",
		},
		{
			name: "ExternalIP wins over Hostname (unknown type)",
			in: []clusterv1.MachineAddress{
				{Type: clusterv1.MachineHostName, Address: "node-01"},
				{Type: clusterv1.MachineExternalIP, Address: "203.0.113.5"},
			},
			want: "203.0.113.5",
		},
		{
			name: "fallback to any when neither Internal nor External present",
			in: []clusterv1.MachineAddress{
				{Type: clusterv1.MachineHostName, Address: "node-01"},
			},
			want: "node-01",
		},
		{
			name: "empty list → empty string",
			in:   nil,
			want: "",
		},
		{
			name: "skip empty Address entries",
			in: []clusterv1.MachineAddress{
				{Type: clusterv1.MachineInternalIP, Address: ""},
				{Type: clusterv1.MachineExternalIP, Address: "203.0.113.5"},
			},
			want: "203.0.113.5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			m := &clusterv1.Machine{Status: clusterv1.MachineStatus{Addresses: tc.in}}
			g.Expect(preferredMachineAddress(m)).To(Equal(tc.want))
		})
	}
}

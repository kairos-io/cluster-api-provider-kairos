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
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

// TestSSHFallbackOwnsKubeconfigCondition pins the predicate the main reconciler
// uses to defer to the SSH-fallback sibling: only the SSH-fallback Reasons
// (Dialing / Failed / Misconfigured) count as sibling-owned; WaitingForNodePush,
// the ready Reason, and an unset condition do not.
func TestSSHFallbackOwnsKubeconfigCondition(t *testing.T) {
	cases := []struct {
		name   string
		set    bool
		status corev1.ConditionStatus
		reason string
		want   bool
	}{
		{name: "no condition", set: false, want: false},
		{name: "WaitingForNodePush", set: true, status: corev1.ConditionFalse, reason: controlplanev1beta2.WaitingForNodePushReason, want: false},
		{name: "ready True", set: true, status: corev1.ConditionTrue, reason: controlplanev1beta2.KubeconfigReadyReason, want: false},
		{name: "SSHFallbackDialing", set: true, status: corev1.ConditionFalse, reason: controlplanev1beta2.SSHFallbackDialingReason, want: true},
		{name: "SSHFallbackFailed", set: true, status: corev1.ConditionFalse, reason: controlplanev1beta2.SSHFallbackFailedReason, want: true},
		{name: "SSHFallbackMisconfigured", set: true, status: corev1.ConditionFalse, reason: controlplanev1beta2.SSHFallbackMisconfiguredReason, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kcp := &controlplanev1beta2.KairosControlPlane{}
			if tc.set {
				conditions.Set(kcp, &clusterv1.Condition{
					Type:   controlplanev1beta2.KubeconfigReadyCondition,
					Status: tc.status,
					Reason: tc.reason,
				})
			}
			if got := sshFallbackOwnsKubeconfigCondition(kcp); got != tc.want {
				t.Errorf("sshFallbackOwnsKubeconfigCondition() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSSHFallbackReconciler_MissingSSHFallbackSecrets covers the synchronous
// missing/empty-Secret detection that lets the reconciler decide
// SSHFallbackMisconfigured without a worker round-trip.
func TestSSHFallbackReconciler_MissingSSHFallbackSecrets(t *testing.T) {
	const ns = "test-ns"
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	secret := func(name, key string, val []byte) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       map[string][]byte{key: val},
		}
	}
	ref := func(name string) *controlplanev1beta2.SSHFallbackSecretReference {
		return &controlplanev1beta2.SSHFallbackSecretReference{Name: name}
	}

	cases := []struct {
		name    string
		objs    []client.Object
		spec    *controlplanev1beta2.SSHFallback
		wantLen int
	}{
		{name: "nil spec", spec: nil, wantLen: 0},
		{
			name:    "both present and non-empty",
			objs:    []client.Object{secret("kh", "known_hosts", []byte("host key")), secret("id", "ssh-privatekey", []byte("pem"))},
			spec:    &controlplanev1beta2.SSHFallback{KnownHostsSecretRef: ref("kh"), IdentitySecretRef: ref("id")},
			wantLen: 0,
		},
		{
			name:    "both missing",
			spec:    &controlplanev1beta2.SSHFallback{KnownHostsSecretRef: ref("kh"), IdentitySecretRef: ref("id")},
			wantLen: 2,
		},
		{
			name:    "known-hosts ref unset",
			objs:    []client.Object{secret("id", "ssh-privatekey", []byte("pem"))},
			spec:    &controlplanev1beta2.SSHFallback{IdentitySecretRef: ref("id")},
			wantLen: 1,
		},
		{
			name:    "known-hosts data key empty",
			objs:    []client.Object{secret("kh", "known_hosts", []byte{}), secret("id", "ssh-privatekey", []byte("pem"))},
			spec:    &controlplanev1beta2.SSHFallback{KnownHostsSecretRef: ref("kh"), IdentitySecretRef: ref("id")},
			wantLen: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objs...).Build()
			r := &SSHFallbackReconciler{Client: c}
			got := r.missingSSHFallbackSecrets(context.Background(), tc.spec, ns)
			if len(got) != tc.wantLen {
				t.Errorf("missingSSHFallbackSecrets() = %v (len %d), want len %d", got, len(got), tc.wantLen)
			}
		})
	}
}

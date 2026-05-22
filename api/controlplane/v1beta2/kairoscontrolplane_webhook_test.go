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

package v1beta2

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ptr returns a pointer to v. Local helper rather than pulling in k8s.io/utils/ptr
// just for one use in tests.
func ptr[T any](v T) *T { return &v }

// newValidKCP returns a KairosControlPlane that satisfies every validation rule.
// Tests mutate one field at a time to isolate what they're checking.
func newValidKCP() *KairosControlPlane {
	return &KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kcp",
			Namespace: "default",
		},
		Spec: KairosControlPlaneSpec{
			Replicas:     ptr(int32(1)),
			Version:      "v1.30.0",
			Distribution: "k0s",
			MachineTemplate: KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "kcp-infra",
				},
			},
			KairosConfigTemplate: KairosConfigTemplateReference{
				Name: "kcp-config",
			},
		},
	}
}

func TestKairosControlPlane_Validate_Replicas(t *testing.T) {
	cases := []struct {
		name        string
		replicas    *int32
		wantErr     bool
		wantMessage string
	}{
		{
			name:     "valid: replicas=1",
			replicas: ptr(int32(1)),
			wantErr:  false,
		},
		{
			name:     "valid: replicas=nil (defaulter sets to 1, validate sees nil)",
			replicas: nil,
			wantErr:  false,
		},
		{
			name:        "invalid: replicas=0 (lower bound)",
			replicas:    ptr(int32(0)),
			wantErr:     true,
			wantMessage: "must be greater than or equal to 1",
		},
		{
			name:        "invalid: negative replicas",
			replicas:    ptr(int32(-3)),
			wantErr:     true,
			wantMessage: "must be greater than or equal to 1",
		},
		{
			// KD-5a: the public, reputational change. The error message must
			// explain *why* — pointing at the open HA implementation, not
			// just saying "must be <= 1".
			name:        "invalid: replicas=2 (HA not yet supported)",
			replicas:    ptr(int32(2)),
			wantErr:     true,
			wantMessage: "spec.replicas > 1 is not supported in this release",
		},
		{
			name:        "invalid: replicas=3",
			replicas:    ptr(int32(3)),
			wantErr:     true,
			wantMessage: "spec.replicas > 1 is not supported in this release",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kcp := newValidKCP()
			kcp.Spec.Replicas = tc.replicas
			err := kcp.validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validate() returned nil error, expected one containing %q", tc.wantMessage)
				}
				if !strings.Contains(err.Error(), tc.wantMessage) {
					t.Errorf("validate() error = %q; expected substring %q", err.Error(), tc.wantMessage)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate() returned unexpected error: %v", err)
			}
		})
	}
}

// TestKairosControlPlane_Default_PreservesReplicas asserts the defaulter does
// not overwrite an explicit replicas value, even an invalid one. The defaulter
// only fills in when nil — validation (which runs after defaulting) is what
// catches invalid explicit values.
func TestKairosControlPlane_Default_PreservesReplicas(t *testing.T) {
	for _, in := range []*int32{ptr(int32(0)), ptr(int32(1)), ptr(int32(7))} {
		kcp := newValidKCP()
		kcp.Spec.Replicas = in
		kcp.Default()
		if kcp.Spec.Replicas == nil || *kcp.Spec.Replicas != *in {
			t.Errorf("Default() changed explicit replicas %d to %v; expected unchanged", *in, kcp.Spec.Replicas)
		}
	}
}

// TestKairosControlPlane_Default_FillsNilReplicas asserts the defaulter fills
// nil replicas with 1 (the only currently-supported value).
func TestKairosControlPlane_Default_FillsNilReplicas(t *testing.T) {
	kcp := newValidKCP()
	kcp.Spec.Replicas = nil
	kcp.Default()
	if kcp.Spec.Replicas == nil {
		t.Fatal("Default() left Spec.Replicas nil; expected it to be set to 1")
	}
	if *kcp.Spec.Replicas != 1 {
		t.Errorf("Default() set Spec.Replicas to %d; expected 1", *kcp.Spec.Replicas)
	}
}

func TestKairosControlPlane_Validate_Distribution(t *testing.T) {
	cases := []struct {
		name    string
		dist    string
		wantErr bool
	}{
		{"valid: k0s", "k0s", false},
		{"valid: k3s", "k3s", false},
		{"valid: empty (defaulter fills)", "", false},
		{"invalid: kubeadm", "kubeadm", true},
		{"invalid: arbitrary", "rke2", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kcp := newValidKCP()
			kcp.Spec.Distribution = tc.dist
			err := kcp.validate()
			if tc.wantErr && err == nil {
				t.Errorf("validate() returned nil; expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validate() returned %v; expected nil", err)
			}
		})
	}
}

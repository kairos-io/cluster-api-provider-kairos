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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// newValidKCPTemplate returns a KairosControlPlaneTemplate that satisfies
// every validation rule when no SSHFallback block is configured. Tests
// mutate one field at a time on the nested template spec to isolate what
// they're checking.
func newValidKCPTemplate() *KairosControlPlaneTemplate {
	return &KairosControlPlaneTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kcp-tmpl",
			Namespace: "default",
		},
		Spec: KairosControlPlaneTemplateSpec{
			Template: KairosControlPlaneTemplateResource{
				Spec: KairosControlPlaneSpec{
					Replicas:     ptr(int32(1)),
					Version:      "v1.30.0",
					Distribution: "k0s",
					MachineTemplate: KairosControlPlaneMachineTemplate{
						InfrastructureRef: corev1.ObjectReference{
							APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
							Kind:       "DockerMachineTemplate",
							Name:       "kcp-tmpl-infra",
						},
					},
					KairosConfigTemplate: KairosConfigTemplateReference{
						Name: "kcp-tmpl-config",
					},
				},
			},
		},
	}
}

func TestKairosControlPlaneTemplate_Validate_Replicas(t *testing.T) {
	cases := []struct {
		name       string
		replicas   *int32
		wantSubstr string // non-empty: error must contain; empty: validate cleanly
	}{
		// === valid ===
		{"nil-replicas-valid", nil, ""},
		{"one-replica-valid", ptr(int32(1)), ""},
		{"three-replicas-valid", ptr(int32(3)), ""},
		{"five-replicas-valid", ptr(int32(5)), ""},
		// === invalid: below minimum ===
		{"zero-replica-rejected", ptr(int32(0)), "at least 1"},
		// === invalid: above maximum ===
		{"six-replicas-rejected", ptr(int32(6)), "above 5 are not supported"},
		// === invalid: even counts ===
		{"two-replicas-rejected", ptr(int32(2)), "odd number"},
		{"four-replicas-rejected", ptr(int32(4)), "odd number"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tmpl := newValidKCPTemplate()
			tmpl.Spec.Template.Spec.Replicas = tc.replicas
			err := tmpl.validate()
			if tc.wantSubstr == "" {
				if err != nil {
					t.Fatalf("validate() returned %v; expected nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate() returned nil; expected error containing %q", tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("validate() error %q does not contain %q", err.Error(), tc.wantSubstr)
			}
			// Field path MUST reflect the template nesting — that's the
			// distinguishing feature of this webhook vs. KCP's.
			if !strings.Contains(err.Error(), "spec.template.spec") {
				t.Errorf("validate() error %q must include `spec.template.spec.*` field path, not the bare `spec.*` form", err.Error())
			}
		})
	}
}

func TestKairosControlPlaneTemplate_Validate_Distribution(t *testing.T) {
	cases := []struct {
		name       string
		dist       string
		wantSubstr string
	}{
		{"empty-valid", "", ""},
		{"k0s-valid", "k0s", ""},
		{"k3s-valid", "k3s", ""},
		{"unknown-rejected", "kthreesomething", "must be one of [k0s, k3s]"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tmpl := newValidKCPTemplate()
			tmpl.Spec.Template.Spec.Distribution = tc.dist
			err := tmpl.validate()
			if tc.wantSubstr == "" {
				if err != nil {
					t.Fatalf("validate() returned %v; expected nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate() returned nil; expected error containing %q", tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("validate() error %q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

// TestKairosControlPlaneTemplate_Validate_SSHFallback verifies the
// template webhook adopts the same validateSSHFallback helper that KCP
// uses (PR-9), and that the field-path component of admission errors
// reflects the template nesting (`spec.template.spec.sshFallback.*`
// rather than `spec.sshFallback.*`).
//
// The cases below are a representative subset of KCP's
// TestKairosControlPlane_Validate_SSHFallback — the helper itself is
// already exhaustively tested there. Here we verify the helper IS being
// called from the template path with the correct base field.
func TestKairosControlPlaneTemplate_Validate_SSHFallback(t *testing.T) {
	validRef := func(name string) *SSHFallbackSecretReference {
		return &SSHFallbackSecretReference{Name: name}
	}

	cases := []struct {
		name       string
		fallback   *SSHFallback
		wantSubstr string
	}{
		{
			name:     "nil-block-valid",
			fallback: nil,
		},
		{
			name: "enabled-no-known-hosts-rejected",
			fallback: &SSHFallback{
				Enabled:           true,
				IdentitySecretRef: validRef("id"),
				ActivateAfter:     &metav1.Duration{Duration: 15 * time.Minute},
			},
			wantSubstr: "knownHostsSecretRef is required",
		},
		{
			name: "enabled-activate-after-too-small-rejected",
			fallback: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: validRef("kh"),
				IdentitySecretRef:   validRef("id"),
				ActivateAfter:       &metav1.Duration{Duration: 5 * time.Minute},
			},
			wantSubstr: "activateAfter must be strictly greater than",
		},
		{
			name: "enabled-valid-minimal",
			fallback: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: validRef("kh"),
				IdentitySecretRef:   validRef("id"),
				ActivateAfter:       &metav1.Duration{Duration: 16 * time.Minute},
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tmpl := newValidKCPTemplate()
			tmpl.Spec.Template.Spec.SSHFallback = tc.fallback
			err := tmpl.validate()
			if tc.wantSubstr == "" {
				if err != nil {
					t.Fatalf("validate() returned %v; expected nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate() returned nil; expected error containing %q", tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("validate() error %q does not contain %q", err.Error(), tc.wantSubstr)
			}
			// Distinguishing assertion vs. the KCP test: errors must
			// carry the template-nested field path. If a future refactor
			// breaks the field.NewPath("spec", "template", "spec")
			// argument in validate(), THIS test catches it where the
			// KCP test cannot.
			if !strings.Contains(err.Error(), "spec.template.spec.sshFallback") {
				t.Errorf("validate() error %q must include `spec.template.spec.sshFallback.*` field path (got the bare `spec.sshFallback.*` form?)", err.Error())
			}
		})
	}
}

func TestKairosControlPlaneTemplate_Default_FillsDefaults(t *testing.T) {
	tmpl := newValidKCPTemplate()
	// Wipe defaults so Default() has work to do.
	tmpl.Spec.Template.Spec.Replicas = nil
	tmpl.Spec.Template.Spec.Distribution = ""
	tmpl.Spec.Template.Spec.SSHFallback = &SSHFallback{
		Enabled:             true,
		KnownHostsSecretRef: &SSHFallbackSecretReference{Name: "kh"},
		IdentitySecretRef:   &SSHFallbackSecretReference{Name: "id"},
		// User/Port/ActivateAfter empty — defaulter should fill them.
	}

	tmpl.Default()

	if tmpl.Spec.Template.Spec.Replicas == nil || *tmpl.Spec.Template.Spec.Replicas != 1 {
		t.Errorf("Replicas not defaulted to 1: %v", tmpl.Spec.Template.Spec.Replicas)
	}
	if tmpl.Spec.Template.Spec.Distribution != "k0s" {
		t.Errorf("Distribution not defaulted to k0s: %q", tmpl.Spec.Template.Spec.Distribution)
	}
	s := tmpl.Spec.Template.Spec.SSHFallback
	if s.User != "kairos" {
		t.Errorf("SSHFallback.User not defaulted to kairos: %q", s.User)
	}
	if s.Port != 22 {
		t.Errorf("SSHFallback.Port not defaulted to 22: %d", s.Port)
	}
	if s.ActivateAfter == nil {
		t.Errorf("SSHFallback.ActivateAfter not defaulted: nil")
	}
}

func TestKairosControlPlaneTemplate_Default_NilSSHFallbackStaysNil(t *testing.T) {
	tmpl := newValidKCPTemplate()
	tmpl.Spec.Template.Spec.SSHFallback = nil
	tmpl.Default()
	if tmpl.Spec.Template.Spec.SSHFallback != nil {
		t.Errorf("nil SSHFallback became non-nil after Default(); got %+v", tmpl.Spec.Template.Spec.SSHFallback)
	}
}

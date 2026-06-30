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
		// === valid: odd values 1, 3, 5 and nil (default 1) ===
		{
			name:     "valid: replicas=1 (single-node, backward-compat)",
			replicas: ptr(int32(1)),
			wantErr:  false,
		},
		{
			name:     "valid: replicas=3 (HA)",
			replicas: ptr(int32(3)),
			wantErr:  false,
		},
		{
			name:     "valid: replicas=5 (HA)",
			replicas: ptr(int32(5)),
			wantErr:  false,
		},
		{
			name:     "valid: replicas=nil (defaulter fills to 1)",
			replicas: nil,
			wantErr:  false,
		},
		// === invalid: below minimum ===
		{
			name:        "invalid: replicas=0 (below minimum)",
			replicas:    ptr(int32(0)),
			wantErr:     true,
			wantMessage: "at least 1",
		},
		{
			name:        "invalid: negative replicas",
			replicas:    ptr(int32(-3)),
			wantErr:     true,
			wantMessage: "at least 1",
		},
		// === invalid: above maximum ===
		{
			name:        "invalid: replicas=6 (above maximum of 5)",
			replicas:    ptr(int32(6)),
			wantErr:     true,
			wantMessage: "above 5 are not supported",
		},
		{
			name:        "invalid: replicas=7 (above maximum of 5)",
			replicas:    ptr(int32(7)),
			wantErr:     true,
			wantMessage: "above 5 are not supported",
		},
		// === invalid: even counts (etcd quorum message) ===
		{
			name:        "invalid: replicas=2 (even count rejected)",
			replicas:    ptr(int32(2)),
			wantErr:     true,
			wantMessage: "odd number",
		},
		{
			name:        "invalid: replicas=4 (even count rejected)",
			replicas:    ptr(int32(4)),
			wantErr:     true,
			wantMessage: "odd number",
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

func TestKairosControlPlane_Validate_SSHFallback(t *testing.T) {
	validRef := func(name string) *SSHFallbackSecretReference {
		return &SSHFallbackSecretReference{Name: name}
	}
	refInNamespace := func(name, ns string) *SSHFallbackSecretReference {
		return &SSHFallbackSecretReference{Name: name, Namespace: ns}
	}

	cases := []struct {
		name        string
		sshFallback *SSHFallback
		wantSubstr  string // non-empty: error MUST contain; empty: must validate cleanly
	}{
		{
			name:        "nil-block is always valid",
			sshFallback: nil,
		},
		{
			name: "disabled-is-always-valid-even-with-bogus-fields",
			sshFallback: &SSHFallback{
				Enabled:       false,
				User:          "ROOT!",                       // invalid pattern, ignored when Enabled=false
				ActivateAfter: &metav1.Duration{Duration: 1}, // too small, ignored when Enabled=false
			},
		},
		{
			name: "enabled-without-known-hosts-secret-ref-rejected",
			sshFallback: &SSHFallback{
				Enabled:           true,
				IdentitySecretRef: validRef("id"),
				ActivateAfter:     &metav1.Duration{Duration: 15 * 60 * 1_000_000_000},
			},
			wantSubstr: "knownHostsSecretRef is required",
		},
		{
			name: "enabled-without-identity-secret-ref-rejected",
			sshFallback: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: validRef("kh"),
				ActivateAfter:       &metav1.Duration{Duration: 15 * 60 * 1_000_000_000},
			},
			wantSubstr: "identitySecretRef is required",
		},
		{
			name: "enabled-with-empty-name-ref-rejected",
			sshFallback: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: &SSHFallbackSecretReference{}, // Name=""
				IdentitySecretRef:   validRef("id"),
				ActivateAfter:       &metav1.Duration{Duration: 15 * 60 * 1_000_000_000},
			},
			wantSubstr: "knownHostsSecretRef is required",
		},
		{
			name: "enabled-with-cross-namespace-known-hosts-ref-rejected",
			sshFallback: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: refInNamespace("kh", "other-ns"),
				IdentitySecretRef:   validRef("id"),
				ActivateAfter:       &metav1.Duration{Duration: 15 * 60 * 1_000_000_000},
			},
			wantSubstr: "cross-namespace Secret references are not allowed",
		},
		{
			name: "enabled-with-cross-namespace-identity-ref-rejected",
			sshFallback: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: validRef("kh"),
				IdentitySecretRef:   refInNamespace("id", "other-ns"),
				ActivateAfter:       &metav1.Duration{Duration: 15 * 60 * 1_000_000_000},
			},
			wantSubstr: "cross-namespace Secret references are not allowed",
		},
		{
			name: "same-namespace-explicit-is-accepted",
			sshFallback: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: refInNamespace("kh", "default"),
				IdentitySecretRef:   refInNamespace("id", "default"),
				ActivateAfter:       &metav1.Duration{Duration: 15 * 60 * 1_000_000_000},
			},
		},
		{
			name: "activate-after-equal-to-timeout-rejected",
			sshFallback: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: validRef("kh"),
				IdentitySecretRef:   validRef("id"),
				ActivateAfter:       &metav1.Duration{Duration: KubeconfigReadyTimeout},
			},
			wantSubstr: "activateAfter must be strictly greater than",
		},
		{
			name: "activate-after-below-timeout-rejected",
			sshFallback: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: validRef("kh"),
				IdentitySecretRef:   validRef("id"),
				ActivateAfter:       &metav1.Duration{Duration: KubeconfigReadyTimeout - 1},
			},
			wantSubstr: "activateAfter must be strictly greater than",
		},
		{
			name: "activate-after-strictly-greater-is-accepted",
			sshFallback: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: validRef("kh"),
				IdentitySecretRef:   validRef("id"),
				ActivateAfter:       &metav1.Duration{Duration: KubeconfigReadyTimeout + 1},
			},
		},
		{
			name: "user-shell-injection-rejected",
			sshFallback: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: validRef("kh"),
				IdentitySecretRef:   validRef("id"),
				ActivateAfter:       &metav1.Duration{Duration: 15 * 60 * 1_000_000_000},
				User:                "kairos; rm -rf /",
			},
			wantSubstr: "user must match",
		},
		{
			name: "user-uppercase-rejected",
			sshFallback: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: validRef("kh"),
				IdentitySecretRef:   validRef("id"),
				ActivateAfter:       &metav1.Duration{Duration: 15 * 60 * 1_000_000_000},
				User:                "Root",
			},
			wantSubstr: "user must match",
		},
		{
			name: "user-empty-string-is-fine-defaulter-fills-it",
			sshFallback: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: validRef("kh"),
				IdentitySecretRef:   validRef("id"),
				ActivateAfter:       &metav1.Duration{Duration: 15 * 60 * 1_000_000_000},
				User:                "",
			},
		},
		{
			name: "valid-minimal-enabled-config",
			sshFallback: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: validRef("kh"),
				IdentitySecretRef:   validRef("id"),
				ActivateAfter:       &metav1.Duration{Duration: 16 * 60 * 1_000_000_000},
				User:                "kairos",
				Port:                22,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			kcp := newValidKCP()
			kcp.Spec.SSHFallback = tc.sshFallback
			err := kcp.validate()
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

func TestKairosControlPlane_Default_SSHFallback(t *testing.T) {
	cases := []struct {
		name            string
		in              *SSHFallback
		wantUser        string
		wantPort        int32
		wantActivateNNS bool // ActivateAfter should be non-nil after defaulting
	}{
		{
			name: "nil block stays nil",
			in:   nil,
		},
		{
			name: "empty block gets every default",
			in: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: &SSHFallbackSecretReference{Name: "kh"},
				IdentitySecretRef:   &SSHFallbackSecretReference{Name: "id"},
			},
			wantUser:        "kairos",
			wantPort:        22,
			wantActivateNNS: true,
		},
		{
			name: "operator overrides are preserved",
			in: &SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: &SSHFallbackSecretReference{Name: "kh"},
				IdentitySecretRef:   &SSHFallbackSecretReference{Name: "id"},
				User:                "custom",
				Port:                2222,
				ActivateAfter:       &metav1.Duration{Duration: 30 * 60 * 1_000_000_000},
			},
			wantUser:        "custom",
			wantPort:        2222,
			wantActivateNNS: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			kcp := newValidKCP()
			kcp.Spec.SSHFallback = tc.in
			kcp.Default()
			if tc.in == nil {
				if kcp.Spec.SSHFallback != nil {
					t.Fatalf("nil block became non-nil after Default(); want nil, got %+v", kcp.Spec.SSHFallback)
				}
				return
			}
			if got := kcp.Spec.SSHFallback.User; got != tc.wantUser {
				t.Errorf("User: got %q, want %q", got, tc.wantUser)
			}
			if got := kcp.Spec.SSHFallback.Port; got != tc.wantPort {
				t.Errorf("Port: got %d, want %d", got, tc.wantPort)
			}
			if tc.wantActivateNNS && kcp.Spec.SSHFallback.ActivateAfter == nil {
				t.Errorf("ActivateAfter: got nil, want non-nil after defaulting")
			}
		})
	}
}

// TestKairosControlPlane_Validate_HA covers the optional HA block validation
// (VIP address shape, interface name regex, nil block, and combinations).
func TestKairosControlPlane_Validate_HA(t *testing.T) {
	cases := []struct {
		name       string
		ha         *HAConfig
		wantErrStr string // non-empty: error must contain; empty: must validate cleanly
	}{
		// --- nil block is always valid (single-node or unset HA) ---
		{
			name: "nil ha block is valid",
			ha:   nil,
		},
		// --- nil VIP within a non-nil HA block is valid ---
		{
			name: "ha block with nil vip is valid",
			ha:   &HAConfig{VIP: nil},
		},
		// --- valid VIP configurations ---
		{
			name: "valid: IPv4 address ARP mode",
			ha: &HAConfig{VIP: &KubeVIPConfig{
				Address:   "192.168.1.10",
				Interface: "eth0",
				Mode:      KubeVIPModeARP,
			}},
		},
		{
			name: "valid: IPv4 address BGP mode",
			ha: &HAConfig{VIP: &KubeVIPConfig{
				Address:   "10.0.0.5",
				Interface: "ens3",
				Mode:      KubeVIPModeBGP,
			}},
		},
		{
			name: "valid: IPv6 address",
			ha: &HAConfig{VIP: &KubeVIPConfig{
				Address:   "2001:db8::1",
				Interface: "eth0",
			}},
		},
		{
			name: "valid: RFC-1123 hostname",
			ha: &HAConfig{VIP: &KubeVIPConfig{
				Address:   "cp.example.com",
				Interface: "ens3",
			}},
		},
		{
			name: "valid: interface with dot (VLAN-style)",
			ha: &HAConfig{VIP: &KubeVIPConfig{
				Address:   "192.168.1.10",
				Interface: "eth0.1",
			}},
		},
		{
			name: "valid: bond interface",
			ha: &HAConfig{VIP: &KubeVIPConfig{
				Address:   "192.168.1.10",
				Interface: "bond0",
			}},
		},
		// --- invalid address ---
		{
			name: "invalid: address contains exclamation mark",
			ha: &HAConfig{VIP: &KubeVIPConfig{
				Address:   "not-a-valid-addr!!",
				Interface: "eth0",
			}},
			wantErrStr: "vip.address",
		},
		{
			name: "invalid: address is empty string",
			ha: &HAConfig{VIP: &KubeVIPConfig{
				Address:   "",
				Interface: "eth0",
			}},
			wantErrStr: "vip.address",
		},
		{
			name: "invalid: address with spaces",
			ha: &HAConfig{VIP: &KubeVIPConfig{
				Address:   "192.168.1 .10",
				Interface: "eth0",
			}},
			wantErrStr: "vip.address",
		},
		// --- invalid interface ---
		{
			name: "invalid: interface name too long (>15 chars)",
			ha: &HAConfig{VIP: &KubeVIPConfig{
				Address:   "192.168.1.10",
				Interface: "averylongiface0", // 15 chars — valid
			}},
			// exactly 15 chars is allowed; test a 16-char one below
		},
		{
			name: "invalid: interface name 16 chars (>15 limit)",
			ha: &HAConfig{VIP: &KubeVIPConfig{
				Address:   "192.168.1.10",
				Interface: "averylongiface00", // 16 chars — invalid
			}},
			wantErrStr: "vip.interface",
		},
		{
			name: "invalid: interface starts with digit",
			ha: &HAConfig{VIP: &KubeVIPConfig{
				Address:   "192.168.1.10",
				Interface: "0eth",
			}},
			wantErrStr: "vip.interface",
		},
		{
			name: "invalid: interface with space",
			ha: &HAConfig{VIP: &KubeVIPConfig{
				Address:   "192.168.1.10",
				Interface: "eth 0",
			}},
			wantErrStr: "vip.interface",
		},
		{
			name: "invalid: interface empty string",
			ha: &HAConfig{VIP: &KubeVIPConfig{
				Address:   "192.168.1.10",
				Interface: "",
			}},
			wantErrStr: "vip.interface",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			kcp := newValidKCP()
			kcp.Spec.HA = tc.ha
			err := kcp.validate()
			if tc.wantErrStr == "" {
				if err != nil {
					t.Fatalf("validate() returned unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate() returned nil; expected error containing %q", tc.wantErrStr)
			}
			if !strings.Contains(err.Error(), tc.wantErrStr) {
				t.Errorf("validate() error %q does not contain %q", err.Error(), tc.wantErrStr)
			}
		})
	}
}

// TestKairosControlPlane_ValidateWithWarnings_VIPOnSingleNode asserts that
// setting spec.ha.vip when spec.replicas==1 produces an admission Warning
// (not an error) — the resource is admitted but the operator is informed that
// the VIP block will be ignored.
func TestKairosControlPlane_ValidateWithWarnings_VIPOnSingleNode(t *testing.T) {
	kcp := newValidKCP()
	kcp.Spec.Replicas = ptr(int32(1))
	kcp.Spec.HA = &HAConfig{VIP: &KubeVIPConfig{
		Address:   "192.168.1.10",
		Interface: "eth0",
	}}

	warnings, err := kcp.validateWithWarnings()
	if err != nil {
		t.Fatalf("validateWithWarnings() returned error %v; expected nil (VIP on single-node is a warning, not an error)", err)
	}
	if len(warnings) == 0 {
		t.Fatal("validateWithWarnings() returned no warnings; expected at least one warning about VIP being ignored for single-node")
	}
	if !strings.Contains(warnings[0], "spec.ha.vip") {
		t.Errorf("warning[0] = %q; expected it to mention spec.ha.vip", warnings[0])
	}
}

// TestKairosControlPlane_ValidateWithWarnings_HAOnSingleNodeNilVIP asserts that
// a non-nil HA block with nil VIP on a single-node cluster does NOT produce a
// warning — only a non-nil VIP triggers the warning.
func TestKairosControlPlane_ValidateWithWarnings_HAOnSingleNodeNilVIP(t *testing.T) {
	kcp := newValidKCP()
	kcp.Spec.Replicas = ptr(int32(1))
	kcp.Spec.HA = &HAConfig{VIP: nil}

	warnings, err := kcp.validateWithWarnings()
	if err != nil {
		t.Fatalf("validateWithWarnings() returned unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("validateWithWarnings() returned %d warnings; expected 0 for nil VIP: %v", len(warnings), warnings)
	}
}

// TestKairosControlPlane_Validate_HANilOnReplicas3 asserts that replicas=3
// with no HA block (nil) is valid — VIP is not required at admission time.
func TestKairosControlPlane_Validate_HANilOnReplicas3(t *testing.T) {
	kcp := newValidKCP()
	kcp.Spec.Replicas = ptr(int32(3))
	kcp.Spec.HA = nil

	if err := kcp.validate(); err != nil {
		t.Fatalf("validate() returned %v; expected nil (VIP is not required at admission)", err)
	}
}

// TestKairosControlPlane_Validate_SingleNodeNoHAField is the backward-compat
// proof: an existing KairosControlPlane with replicas:1 and no HA field
// behaves identically after Phase 1 — admitted with no error and no warning.
func TestKairosControlPlane_Validate_SingleNodeNoHAField(t *testing.T) {
	kcp := newValidKCP() // replicas=1, ha=nil — the current default shape
	warnings, err := kcp.validateWithWarnings()
	if err != nil {
		t.Fatalf("validateWithWarnings() returned %v; expected nil for single-node default config", err)
	}
	if len(warnings) != 0 {
		t.Errorf("validateWithWarnings() returned %d warnings; expected 0 for clean single-node config", len(warnings))
	}
}

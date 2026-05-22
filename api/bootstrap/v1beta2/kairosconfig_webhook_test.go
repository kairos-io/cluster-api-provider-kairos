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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// newValidKairosConfig returns a control-plane KairosConfig that satisfies
// every validation rule. Tests mutate one field at a time to isolate the
// rule they're checking.
func newValidKairosConfig() *KairosConfig {
	return &KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "kc", Namespace: "default"},
		Spec: KairosConfigSpec{
			Role:              "control-plane",
			Distribution:      "k0s",
			KubernetesVersion: "v1.30.0+k0s.0",
			UserName:          "kairos",
			SSHPublicKey:      "ssh-ed25519 AAAA test@example",
		},
	}
}

func TestKairosConfig_Validate_RequiresOneCredential(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(kc *KairosConfig)
		wantErr bool
	}{
		// === each individual credential is sufficient ===
		{
			name: "ok: sshPublicKey alone",
			mutate: func(kc *KairosConfig) {
				kc.Spec.UserPassword = ""
				kc.Spec.UserPasswordSecretRef = nil
				kc.Spec.SSHPublicKey = "ssh-ed25519 AAAA test"
				kc.Spec.GitHubUser = ""
			},
			wantErr: false,
		},
		{
			name: "ok: gitHubUser alone",
			mutate: func(kc *KairosConfig) {
				kc.Spec.UserPassword = ""
				kc.Spec.UserPasswordSecretRef = nil
				kc.Spec.SSHPublicKey = ""
				kc.Spec.GitHubUser = "octocat"
			},
			wantErr: false,
		},
		{
			name: "ok: inline userPassword alone",
			mutate: func(kc *KairosConfig) {
				kc.Spec.UserPassword = "strong-password"
				kc.Spec.UserPasswordSecretRef = nil
				kc.Spec.SSHPublicKey = ""
				kc.Spec.GitHubUser = ""
			},
			wantErr: false,
		},
		{
			name: "ok: userPasswordSecretRef alone",
			mutate: func(kc *KairosConfig) {
				kc.Spec.UserPassword = ""
				kc.Spec.UserPasswordSecretRef = &UserPasswordSecretReference{Name: "creds"}
				kc.Spec.SSHPublicKey = ""
				kc.Spec.GitHubUser = ""
			},
			wantErr: false,
		},

		// === missing all credentials must be rejected ===
		{
			name: "fail: no credential at all (KD-3a)",
			mutate: func(kc *KairosConfig) {
				kc.Spec.UserPassword = ""
				kc.Spec.UserPasswordSecretRef = nil
				kc.Spec.SSHPublicKey = ""
				kc.Spec.GitHubUser = ""
			},
			wantErr: true,
		},
		{
			name: "fail: SecretRef with empty name is treated as missing",
			mutate: func(kc *KairosConfig) {
				kc.Spec.UserPassword = ""
				kc.Spec.UserPasswordSecretRef = &UserPasswordSecretReference{Name: ""}
				kc.Spec.SSHPublicKey = ""
				kc.Spec.GitHubUser = ""
			},
			wantErr: true,
		},

		// === combinations are allowed (precedence is a controller-side concern) ===
		{
			name: "ok: SecretRef AND sshPublicKey both set",
			mutate: func(kc *KairosConfig) {
				kc.Spec.UserPassword = ""
				kc.Spec.UserPasswordSecretRef = &UserPasswordSecretReference{Name: "creds"}
				kc.Spec.SSHPublicKey = "ssh-ed25519 AAAA test"
				kc.Spec.GitHubUser = ""
			},
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kc := newValidKairosConfig()
			tc.mutate(kc)
			err := kc.validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validate() returned nil; expected a credential-required error")
				}
				if !strings.Contains(err.Error(), "userPassword") || !strings.Contains(err.Error(), "sshPublicKey") {
					t.Errorf("validate() error didn't mention all credential options: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("validate() unexpected error: %v", err)
			}
		})
	}
}

func TestKairosConfig_Default_DoesNotSetUserPassword(t *testing.T) {
	// KD-3a: the previous behaviour of defaulting UserPassword to "kairos"
	// was removed. Default() must NOT populate UserPassword from nothing.
	kc := newValidKairosConfig()
	kc.Spec.UserPassword = ""
	kc.Default()
	if kc.Spec.UserPassword != "" {
		t.Errorf("Default() set UserPassword to %q; expected it to remain empty (KD-3a)", kc.Spec.UserPassword)
	}
}

func TestKairosConfig_Default_StillSetsOtherDefaults(t *testing.T) {
	// Default() should still default UserName, UserGroups, Distribution, Role.
	kc := &KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "kc", Namespace: "default"},
		Spec: KairosConfigSpec{
			// Leave everything empty.
			KubernetesVersion: "v1.30.0+k0s.0",
		},
	}
	kc.Default()
	if kc.Spec.UserName != "kairos" {
		t.Errorf("Default() UserName = %q; expected %q", kc.Spec.UserName, "kairos")
	}
	if len(kc.Spec.UserGroups) == 0 || kc.Spec.UserGroups[0] != "admin" {
		t.Errorf("Default() UserGroups = %v; expected [admin]", kc.Spec.UserGroups)
	}
	if kc.Spec.Distribution != "k0s" {
		t.Errorf("Default() Distribution = %q; expected k0s", kc.Spec.Distribution)
	}
	if kc.Spec.Role != "worker" {
		t.Errorf("Default() Role = %q; expected worker", kc.Spec.Role)
	}
}

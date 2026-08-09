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

package main

import "testing"

// TestParseControllerRole_ValidValues asserts every documented --controllers
// value parses without error and yields the role whose enable* truth table the
// registration seam gates on. These are the only three values the provider
// Deployment overlays (ADR 0007) ever set.
func TestParseControllerRole_ValidValues(t *testing.T) {
	tests := []struct {
		name              string
		flag              string
		wantRole          controllerRole
		wantBootstrap     bool
		wantControlPlane  bool
		wantLeaderElectID string
	}{
		{
			name:              "all registers both providers",
			flag:              "all",
			wantRole:          roleAll,
			wantBootstrap:     true,
			wantControlPlane:  true,
			wantLeaderElectID: "kairos-capi-leader-election",
		},
		{
			name:              "bootstrap registers only bootstrap",
			flag:              "bootstrap",
			wantRole:          roleBootstrap,
			wantBootstrap:     true,
			wantControlPlane:  false,
			wantLeaderElectID: "kairos-bootstrap-leader-election",
		},
		{
			name:              "control-plane registers only control-plane",
			flag:              "control-plane",
			wantRole:          roleControlPlane,
			wantBootstrap:     false,
			wantControlPlane:  true,
			wantLeaderElectID: "kairos-control-plane-leader-election",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			role, err := parseControllerRole(tc.flag)
			if err != nil {
				t.Fatalf("parseControllerRole(%q) returned unexpected error: %v", tc.flag, err)
			}
			if role != tc.wantRole {
				t.Errorf("parseControllerRole(%q) = %d, want %d", tc.flag, role, tc.wantRole)
			}
			if got := role.enablesBootstrap(); got != tc.wantBootstrap {
				t.Errorf("role %q enablesBootstrap() = %v, want %v", tc.flag, got, tc.wantBootstrap)
			}
			if got := role.enablesControlPlane(); got != tc.wantControlPlane {
				t.Errorf("role %q enablesControlPlane() = %v, want %v", tc.flag, got, tc.wantControlPlane)
			}
			if got := role.leaderElectionID(); got != tc.wantLeaderElectID {
				t.Errorf("role %q leaderElectionID() = %q, want %q", tc.flag, got, tc.wantLeaderElectID)
			}
		})
	}
}

// TestParseControllerRole_InvalidValues asserts that anything outside the exact
// set of {all, bootstrap, control-plane} is a hard error. The match is
// case-sensitive and does NOT trim surrounding whitespace by design (see
// parseControllerRole's doc comment): --controllers is a manifest-baked knob,
// so a typo like "Bootstrap" or " all " must fail startup loudly rather than
// silently degrading to a partial-registration surprise. The empty string is
// included because it is the "explicitly passed --controllers=”" case (the
// flag's own default is "all", so an unset flag never reaches an error).
func TestParseControllerRole_InvalidValues(t *testing.T) {
	invalid := []string{
		"",                        // explicit empty
		" ",                       // whitespace only
		"worker",                  // not a role
		"controlplane",            // missing hyphen
		"control_plane",           // wrong separator
		"Bootstrap",               // wrong case
		"BOOTSTRAP",               // wrong case
		"ALL",                     // wrong case
		"Control-Plane",           // wrong case
		" all",                    // leading whitespace (not trimmed)
		"all ",                    // trailing whitespace (not trimmed)
		" bootstrap ",             // surrounding whitespace (not trimmed)
		"bootstrap,control-plane", // list form is not accepted
		"both",                    // not a role
	}

	for _, v := range invalid {
		t.Run("invalid_"+v, func(t *testing.T) {
			if _, err := parseControllerRole(v); err == nil {
				t.Errorf("parseControllerRole(%q) = nil error, want error", v)
			}
		})
	}
}

// TestControllerRole_EnableTruthTable pins the full bootstrap/control-plane
// enable matrix independently of the parser, so a future refactor of the enum
// values can't quietly flip which controllers a role registers.
func TestControllerRole_EnableTruthTable(t *testing.T) {
	tests := []struct {
		role             controllerRole
		wantBootstrap    bool
		wantControlPlane bool
	}{
		{roleAll, true, true},
		{roleBootstrap, true, false},
		{roleControlPlane, false, true},
	}
	for _, tc := range tests {
		if got := tc.role.enablesBootstrap(); got != tc.wantBootstrap {
			t.Errorf("role %d enablesBootstrap() = %v, want %v", tc.role, got, tc.wantBootstrap)
		}
		if got := tc.role.enablesControlPlane(); got != tc.wantControlPlane {
			t.Errorf("role %d enablesControlPlane() = %v, want %v", tc.role, got, tc.wantControlPlane)
		}
	}
}

// TestControllerRole_LeaderElectionIDsAreDistinct guards the ADR 0007
// invariant that the two role-scoped Deployments must never share a lease: a
// shared lease name would let one provider's leader permanently starve the
// other. It also pins the exact roleAll ID so an in-place flat-manifest upgrade
// keeps its existing lease.
func TestControllerRole_LeaderElectionIDsAreDistinct(t *testing.T) {
	all := roleAll.leaderElectionID()
	bootstrap := roleBootstrap.leaderElectionID()
	controlPlane := roleControlPlane.leaderElectionID()

	if all != "kairos-capi-leader-election" {
		t.Errorf("roleAll leaderElectionID() = %q, want %q (must stay unchanged for flat-manifest upgrades)",
			all, "kairos-capi-leader-election")
	}
	if bootstrap == controlPlane {
		t.Errorf("bootstrap and control-plane share lease %q; separate Deployments must not share a lease", bootstrap)
	}
	if bootstrap == all || controlPlane == all {
		t.Errorf("a split role shares the flat-manifest lease %q; leases must be distinct per role", all)
	}
}

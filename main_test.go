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

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

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

// TestManagerOptions_SecretsAreNotCached asserts the manager the provider
// actually starts reads Secrets straight from the API server, in every role and
// in both watch scopes.
//
// This is the resident-copy guard. controller-runtime starts an informer on the
// first cached read of a type, and that informer lists and watches the manager's
// whole scope, not the one object asked for. One cached Secret read in any of
// the readers below would make the process hold, for its lifetime, every Secret
// in the management cluster: the <cluster>-kubeconfig of every workload cluster
// (admin credentials, Kairos or not) and Kamaji's CA key material. The readers
// this covers, all of which go through mgr.GetClient():
//
//   - internal/controllers/bootstrap: the bootstrap-data Secret and the join tokens
//   - internal/controllers/controlplane: the HA token, etcd and SSH-fallback Secrets
//   - the kubeadm trust check, which reads <cluster>-kubeconfig before a join
//
// If a later change drops this the regression is silent: nothing fails, those
// reads just start populating the cache again. Hence a test rather than a comment.
//
// What this does NOT cover, and what no assertion on manager.Options can: the
// three Watches(&corev1.Secret{}) calls in kairosconfig_controller.go and
// kairoscontrolplane_controller.go build their informers on mgr.GetCache()
// directly, which Client.Cache.DisableFor does not reach. Those watches are
// metadata-only; test/envtest/manager_secret_cache_test.go is the guard that
// runs the real controllers and asserts no Secret body reaches the cache.
func TestManagerOptions_SecretsAreNotCached(t *testing.T) {
	secretGVK, err := apiutil.GVKForObject(&corev1.Secret{}, scheme)
	if err != nil {
		t.Fatalf("resolving the Secret GVK against the manager scheme: %v", err)
	}

	for _, role := range []controllerRole{roleAll, roleBootstrap, roleControlPlane} {
		for _, watchSingle := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/watchSingle=%v", role, watchSingle), func(t *testing.T) {
				opts := managerOptions(role, ":8080", ":8081", false, "capi-kairos-system", watchSingle)

				if opts.Client.Cache == nil {
					t.Fatal("manager has no client CacheOptions; it would cache every Secret it reads")
				}

				found := false
				for _, obj := range opts.Client.Cache.DisableFor {
					got, err := apiutil.GVKForObject(obj, scheme)
					if err != nil {
						t.Fatalf("resolving %T against the manager scheme: %v", obj, err)
					}
					if got == secretGVK {
						found = true
					}
				}
				if !found {
					t.Errorf("Secret is not in Client.Cache.DisableFor (%v); the manager would hold every Secret in the management cluster", opts.Client.Cache.DisableFor)
				}
			})
		}
	}
}

// TestManagerOptions_WatchScopeUnchanged pins the behaviour the extraction of
// managerOptions had to preserve: --namespace still restricts the cache to that
// one namespace, and the all-namespaces default still sets no cache scope.
func TestManagerOptions_WatchScopeUnchanged(t *testing.T) {
	all := managerOptions(roleAll, ":8080", ":8081", false, "ignored-when-not-single", false)
	if len(all.Cache.DefaultNamespaces) != 0 {
		t.Errorf("all-namespaces manager scoped its cache to %v; it must watch every namespace", all.Cache.DefaultNamespaces)
	}

	single := managerOptions(roleAll, ":8080", ":8081", false, "capi-kairos-system", true)
	if _, ok := single.Cache.DefaultNamespaces["capi-kairos-system"]; !ok || len(single.Cache.DefaultNamespaces) != 1 {
		t.Errorf("single-namespace manager scoped its cache to %v, want exactly capi-kairos-system", single.Cache.DefaultNamespaces)
	}
}

// TestManagerOptions_LeaderElectionIDFollowsRole guards the other thing the
// extraction moved: each role keeps its own lease.
func TestManagerOptions_LeaderElectionIDFollowsRole(t *testing.T) {
	for _, role := range []controllerRole{roleAll, roleBootstrap, roleControlPlane} {
		if got, want := managerOptions(role, ":8080", ":8081", true, "", false).LeaderElectionID, role.leaderElectionID(); got != want {
			t.Errorf("role %v: manager LeaderElectionID = %q, want %q", role, got, want)
		}
	}
}

// TestManagerOptions_SecretTransformInstalled asserts the manager the provider
// actually starts installs secretcache.StripPayload on v1/Secret, in every role
// and both watch scopes. The single-namespace branch replaces opts.Cache, so an
// assignment made before it would be silently lost.
func TestManagerOptions_SecretTransformInstalled(t *testing.T) {
	for _, role := range []controllerRole{roleAll, roleBootstrap, roleControlPlane} {
		for _, watchSingle := range []bool{false, true} {
			opts := managerOptions(role, ":8080", ":8081", false, "capi-kairos-system", watchSingle)
			found := false
			for obj, byObj := range opts.Cache.ByObject {
				if _, ok := obj.(*corev1.Secret); ok && byObj.Transform != nil {
					found = true
				}
			}
			if !found {
				t.Errorf("role %v watchSingle=%v: no Secret cache transform installed", role, watchSingle)
			}
		}
	}
}

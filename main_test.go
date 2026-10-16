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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// TestStripSecretCachePayload_RemovesBodyCarryingMetadata is the guard for the
// cache transform installed on v1/Secret. The Secret watches are metadata-only,
// so the cache holds PartialObjectMetadata and never the Secret body — but
// ObjectMeta can smuggle the body back in two ways, and both must be gone
// before the object is committed.
func TestStripSecretCachePayload_RemovesBodyCarryingMetadata(t *testing.T) {
	in := &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workload-kubeconfig",
			Namespace: "default",
			Labels:    map[string]string{"cluster.x-k8s.io/cluster-name": "workload"},
			Annotations: map[string]string{
				// kubectl's copy of the applied object: the whole Secret,
				// base64 data included.
				lastAppliedConfigAnnotation: `{"apiVersion":"v1","kind":"Secret","data":{"value":"c3VwZXItc2VjcmV0"}}`,
				"keep-me":                   "yes",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}},
		},
	}

	out, err := stripSecretCachePayload(in)
	if err != nil {
		t.Fatalf("stripSecretCachePayload returned an error: %v", err)
	}
	got, ok := out.(*metav1.PartialObjectMetadata)
	if !ok {
		t.Fatalf("transform changed the object type: got %T", out)
	}
	if got.ManagedFields != nil {
		t.Errorf("managedFields survived the transform: %v", got.ManagedFields)
	}
	if v, present := got.Annotations[lastAppliedConfigAnnotation]; present {
		t.Errorf("last-applied-configuration survived the transform, carrying the Secret body: %q", v)
	}
	// The transform must strip, not sanitise: everything else is load-bearing
	// for the mappers and predicates that read this object.
	if got.Annotations["keep-me"] != "yes" {
		t.Errorf("unrelated annotation was dropped: %v", got.Annotations)
	}
	if got.Labels["cluster.x-k8s.io/cluster-name"] != "workload" {
		t.Errorf("labels were dropped, which would break every Secret mapper: %v", got.Labels)
	}
	if got.Name != "workload-kubeconfig" || got.Namespace != "default" {
		t.Errorf("name/namespace were altered: %s/%s", got.Namespace, got.Name)
	}
}

// TestStripSecretCachePayload_PassesThroughNonObjects asserts the transform
// never drops a value it cannot introspect. A cache transform that returns an
// error on a DeletedFinalStateUnknown tombstone loses the delete event, so the
// controller would keep a Secret alive in cache forever.
func TestStripSecretCachePayload_PassesThroughNonObjects(t *testing.T) {
	in := "not an api object"
	out, err := stripSecretCachePayload(in)
	if err != nil {
		t.Fatalf("stripSecretCachePayload returned an error: %v", err)
	}
	if out != in {
		t.Errorf("transform did not pass the value through: got %v", out)
	}
}

// TestStripSecretCachePayload_NoAnnotations asserts the nil-annotation path
// does not panic and does not materialise an empty map.
func TestStripSecretCachePayload_NoAnnotations(t *testing.T) {
	in := &metav1.PartialObjectMetadata{
		ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "default"},
	}
	out, err := stripSecretCachePayload(in)
	if err != nil {
		t.Fatalf("stripSecretCachePayload returned an error: %v", err)
	}
	if got := out.(*metav1.PartialObjectMetadata); got.Annotations != nil {
		t.Errorf("transform created an annotations map where there was none: %v", got.Annotations)
	}
}

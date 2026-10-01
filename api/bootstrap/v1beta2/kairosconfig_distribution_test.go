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
	"context"
	"strings"
	"testing"
)

// These tests pin the KairosConfig distribution admission messages and the
// worker-token aggregate messages verbatim, before the P0 refactor moves the
// per-distribution validation into distribution.go. err.Error() is user-visible
// and is copied into conditions and status.failureMessage downstream, so the
// exact strings and their aggregate order must not drift.

const (
	msgDistributionEnum = "spec.distribution must be one of [k0s, k3s]"
	msgK0sWorkerToken   = "worker KairosConfig requires either spec.workerToken or spec.workerTokenSecretRef to be set"
	msgK3sWorkerToken   = "k3s worker requires spec.k3sToken, spec.k3sTokenSecretRef, spec.workerToken, or spec.workerTokenSecretRef to be set"
)

// TestKairosConfig_Validate_DistributionMessages pins the exact distribution
// enum message and confirms the accepted set.
func TestKairosConfig_Validate_DistributionMessages(t *testing.T) {
	cases := []struct {
		name    string
		dist    string
		wantErr bool
	}{
		{"empty is valid (defaulter fills k0s)", "", false},
		{"k0s is valid", "k0s", false},
		{"k3s is valid", "k3s", false},
		{"kubeadm is rejected in P0", "kubeadm", true},
		{"arbitrary is rejected", "rke2", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			kc := newValidKairosConfig() // control-plane: no worker-token rule
			kc.Spec.Distribution = tc.dist
			err := kc.validate()
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("validate() returned %v; expected nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate() returned nil; expected the distribution enum error")
			}
			if !strings.Contains(err.Error(), msgDistributionEnum) {
				t.Errorf("validate() error %q does not contain %q", err.Error(), msgDistributionEnum)
			}
		})
	}
}

// TestKairosConfig_Validate_WorkerTokenMessages pins the k0s and k3s worker
// empty-token messages verbatim.
func TestKairosConfig_Validate_WorkerTokenMessages(t *testing.T) {
	cases := []struct {
		name    string
		dist    string
		wantMsg string
	}{
		{"k0s worker with no token", "k0s", msgK0sWorkerToken},
		{"k3s worker with no token", "k3s", msgK3sWorkerToken},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			kc := newValidKairosConfig()
			kc.Spec.Role = "worker"
			kc.Spec.Distribution = tc.dist
			// Leave all token fields unset.
			err := kc.validate()
			if err == nil {
				t.Fatalf("validate() returned nil; expected %q", tc.wantMsg)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("validate() error %q does not contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestKairosConfig_Validate_UnknownDistributionWorker_TwoErrors pins that an
// unknown distribution on a worker with no token aggregates BOTH the distribution
// enum error AND the default (k0s) worker-token error — the worker rule's
// default arm applies to any unrecognised name (OQ-C). This two-error behaviour
// must survive the move to the per-distribution table.
func TestKairosConfig_Validate_UnknownDistributionWorker_TwoErrors(t *testing.T) {
	kc := newValidKairosConfig()
	kc.Spec.Role = "worker"
	kc.Spec.Distribution = "rke2"
	err := kc.validate()
	if err == nil {
		t.Fatalf("validate() returned nil; expected two aggregated errors")
	}
	if !strings.Contains(err.Error(), msgDistributionEnum) {
		t.Errorf("validate() error %q is missing the distribution enum error", err.Error())
	}
	if !strings.Contains(err.Error(), msgK0sWorkerToken) {
		t.Errorf("validate() error %q is missing the default (k0s) worker-token error", err.Error())
	}
}

// TestKairosConfig_Default_SetsK0sDistribution pins that the defaulter writes k0s
// for an unset distribution.
func TestKairosConfig_Default_SetsK0sDistribution(t *testing.T) {
	kc := newValidKairosConfig()
	kc.Spec.Distribution = ""
	if err := (&kairosConfigDefaulter{}).Default(context.Background(), kc); err != nil {
		t.Fatalf("Default() returned error: %v", err)
	}
	if kc.Spec.Distribution != "k0s" {
		t.Errorf("Default() Distribution = %q; expected k0s", kc.Spec.Distribution)
	}
}

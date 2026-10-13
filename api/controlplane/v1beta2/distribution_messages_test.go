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
)

// These tests pin the KCP and KCPT distribution enum messages verbatim. The P0
// distribution seam leaves both webhook source files unchanged (M1: zero diff),
// so these messages must not drift — they document the frozen behaviour and will
// fail if a later P0 commit touches these webhooks.

// TestKairosControlPlane_Validate_DistributionMessage pins the exact KCP message.
func TestKairosControlPlane_Validate_DistributionMessage(t *testing.T) {
	const want = "spec.distribution must be one of [k0s, k3s]"
	kcp := newValidKCP()
	kcp.Spec.Distribution = "kubeadm"
	err := kcp.validate()
	if err == nil {
		t.Fatalf("validate() returned nil; expected %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("validate() error %q does not contain %q", err.Error(), want)
	}
}

// TestKairosControlPlaneTemplate_Validate_DistributionMessage pins the exact KCPT
// message, including the spec.template.spec.* field-path prefix.
func TestKairosControlPlaneTemplate_Validate_DistributionMessage(t *testing.T) {
	const want = "spec.template.spec.distribution must be one of [k0s, k3s]"
	tmpl := newValidKCPTemplate()
	tmpl.Spec.Template.Spec.Distribution = "kubeadm"
	err := tmpl.validate()
	if err == nil {
		t.Fatalf("validate() returned nil; expected %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("validate() error %q does not contain %q", err.Error(), want)
	}
}

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

// A machine template in another namespace was cloned into this cluster, and
// the Cluster was made an owner of it. Kubernetes treats a cross-namespace
// owner reference as absent, so that could get another namespace's template
// garbage-collected.
func TestKairosControlPlane_Validate_RejectsCrossNamespaceInfrastructureRef(t *testing.T) {
	kcp := newValidKCP()
	kcp.Spec.MachineTemplate.InfrastructureRef.Namespace = "other-tenant"

	err := kcp.validate()
	if err == nil {
		t.Fatal("an infrastructureRef into another namespace must be rejected")
	}
	if !strings.Contains(err.Error(), "spec.machineTemplate.infrastructureRef.namespace") ||
		!strings.Contains(err.Error(), "cross-namespace references are not allowed") {
		t.Fatalf("error must name the field and the rule, got: %v", err)
	}
}

// Omitting the namespace is the common case (clusterctl cluster templates rely
// on it), and naming the object's own namespace explicitly is equally fine.
func TestKairosControlPlane_Validate_AcceptsOwnNamespaceInfrastructureRef(t *testing.T) {
	for _, ns := range []string{"", "default"} {
		kcp := newValidKCP()
		kcp.Spec.MachineTemplate.InfrastructureRef.Namespace = ns
		if err := kcp.validate(); err != nil {
			t.Fatalf("namespace %q must be accepted, got: %v", ns, err)
		}
	}
}

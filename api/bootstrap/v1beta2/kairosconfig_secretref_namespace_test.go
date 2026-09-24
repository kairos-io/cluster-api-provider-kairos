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
)

// The controller copies what these references point at into the bootstrap
// Secret beside the KairosConfig, and it can read Secrets in every namespace.
// A reference into another namespace would therefore copy that namespace's
// Secret into data the KairosConfig's author can read.
func TestKairosConfig_Validate_RejectsCrossNamespaceSecretRefs(t *testing.T) {
	cases := []struct {
		name  string
		set   func(kc *KairosConfig, ns string)
		field string
	}{
		{"userPasswordSecretRef", func(kc *KairosConfig, ns string) {
			kc.Spec.UserPasswordSecretRef = &UserPasswordSecretReference{Name: "pw", Namespace: ns}
		}, "spec.userPasswordSecretRef.namespace"},
		{"workerTokenSecretRef", func(kc *KairosConfig, ns string) {
			kc.Spec.WorkerTokenSecretRef = &WorkerTokenSecretReference{Name: "tok", Namespace: ns}
		}, "spec.workerTokenSecretRef.namespace"},
		{"k3sTokenSecretRef", func(kc *KairosConfig, ns string) {
			kc.Spec.K3sTokenSecretRef = &WorkerTokenSecretReference{Name: "tok", Namespace: ns}
		}, "spec.k3sTokenSecretRef.namespace"},
		{"controlPlaneJoinTokenSecretRef", func(kc *KairosConfig, ns string) {
			kc.Spec.ControlPlaneJoinTokenSecretRef = &WorkerTokenSecretReference{Name: "tok", Namespace: ns}
		}, "spec.controlPlaneJoinTokenSecretRef.namespace"},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/other-namespace-rejected", func(t *testing.T) {
			kc := newValidKairosConfig()
			tc.set(kc, "other-tenant")
			err := kc.validate()
			if err == nil {
				t.Fatalf("a %s into another namespace must be rejected", tc.name)
			}
			if !strings.Contains(err.Error(), tc.field) ||
				!strings.Contains(err.Error(), "cross-namespace Secret references are not allowed") {
				t.Fatalf("error must name %s and the rule, got: %v", tc.field, err)
			}
		})
		// An empty namespace means the KairosConfig's own, and naming it
		// explicitly is fine; the control plane does exactly that for the
		// join-token reference it sets on the KairosConfigs it creates.
		for _, ns := range []string{"", "default"} {
			t.Run(tc.name+"/own-namespace-accepted/"+ns, func(t *testing.T) {
				kc := newValidKairosConfig()
				tc.set(kc, ns)
				if err := kc.validate(); err != nil {
					t.Fatalf("a %s in the object's own namespace must be accepted, got: %v", tc.name, err)
				}
			})
		}
	}
}

// tokenSecretRef is left alone on purpose: the controller ignores its namespace
// and always reads from the Cluster's, so there is nothing to exploit, and
// rejecting it now would break objects that only ever set it harmlessly.
func TestKairosConfig_Validate_LeavesLegacyTokenSecretRefNamespaceAlone(t *testing.T) {
	kc := newValidKairosConfig()
	kc.Spec.TokenSecretRef = &corev1.ObjectReference{Name: "tok", Namespace: "other-tenant"}
	if err := kc.validate(); err != nil {
		t.Fatalf("tokenSecretRef's namespace is ignored by the controller and must stay accepted, got: %v", err)
	}
}

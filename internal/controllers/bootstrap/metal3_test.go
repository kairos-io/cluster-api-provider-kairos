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

// Tests for Phase 1a Metal3 (CAPM3) detection seams in the bootstrap controller.
//
// Covered: isMetal3Machine, supportsManagementEndpoint (Metal3 arm),
// and getProviderID (Metal3 must return "" so CAPM3 retains ownership).

package bootstrap

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

func machineWithInfraKind(kind string) *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: corev1.ObjectReference{
				APIVersion: "infrastructure.cluster.x-k8s.io/v1beta2",
				Kind:       kind,
				Name:       "infra-obj",
				Namespace:  "default",
			},
		},
	}
}

func TestIsMetal3Machine(t *testing.T) {
	tests := []struct {
		name    string
		machine *clusterv1.Machine
		want    bool
	}{
		{"Metal3Machine is true", machineWithInfraKind("Metal3Machine"), true},
		{"VSphereMachine is false", machineWithInfraKind("VSphereMachine"), false},
		{"KubevirtMachine is false", machineWithInfraKind("KubevirtMachine"), false},
		{"DockerMachine is false", machineWithInfraKind("DockerMachine"), false},
		{"nil machine is false", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isMetal3Machine(tc.machine)
			if got != tc.want {
				t.Errorf("isMetal3Machine() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSupportsManagementEndpoint_Metal3(t *testing.T) {
	tests := []struct {
		name    string
		machine *clusterv1.Machine
		want    bool
	}{
		{"Metal3Machine is supported", machineWithInfraKind("Metal3Machine"), true},
		{"VSphereMachine is supported", machineWithInfraKind("VSphereMachine"), true},
		{"KubevirtMachine is supported", machineWithInfraKind("KubevirtMachine"), true},
		{"DockerMachine is not supported", machineWithInfraKind("DockerMachine"), false},
		{"nil machine is false", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := supportsManagementEndpoint(tc.machine)
			if got != tc.want {
				t.Errorf("supportsManagementEndpoint() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGetProviderID_Metal3ReturnsEmpty verifies that getProviderID returns "" (no error)
// for a Metal3Machine so that CAPM3 retains full ownership of Node.spec.providerID.
// The test also exercises the case where Machine.Spec.ProviderID is unset
// (the normal CAPM3 pre-provisioning state) and where it's set to a real value
// (normal CAPI path — returned immediately without touching the infra object).
func TestGetProviderID_Metal3ReturnsEmpty(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = bootstrapv1beta2.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	// Build a fake Metal3Machine with a spec.providerID field set — this simulates
	// a misbehaving operator; our code must still not try to read it, because
	// Metal3Machine providerID is managed exclusively by CAPM3.
	metal3Obj := &unstructured.Unstructured{}
	metal3Obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta2",
		Kind:    "Metal3Machine",
	})
	metal3Obj.SetName("infra-obj")
	metal3Obj.SetNamespace("default")
	_ = unstructured.SetNestedField(metal3Obj.Object, "metal3://ns/bmh/m3m", "spec", "providerID")

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(metal3Obj).Build()
	r := &KairosConfigReconciler{Client: fakeClient, Scheme: scheme}

	t.Run("no Machine.Spec.ProviderID set — getProviderID returns empty", func(t *testing.T) {
		machine := machineWithInfraKind("Metal3Machine")
		// Machine.Spec.ProviderID is nil (CAPM3 pre-provisioning state)
		machine.Spec.ProviderID = nil

		got := r.getProviderID(context.Background(), log.Log, machine)
		if got != "" {
			t.Errorf("getProviderID() = %q, want empty string for Metal3Machine", got)
		}
	})

	t.Run("Machine.Spec.ProviderID set — returned directly (not Metal3-specific)", func(t *testing.T) {
		// This is the generic early-return path: if CAPI core has already set
		// Machine.Spec.ProviderID (e.g. after CAPM3 patched the Node), the
		// bootstrap controller gets that value directly, regardless of infra kind.
		providerID := "metal3://default/my-bmh/my-m3m"
		machine := machineWithInfraKind("Metal3Machine")
		machine.Spec.ProviderID = &providerID

		got := r.getProviderID(context.Background(), log.Log, machine)
		if got != providerID {
			t.Errorf("getProviderID() = %q, want %q", got, providerID)
		}
	})
}

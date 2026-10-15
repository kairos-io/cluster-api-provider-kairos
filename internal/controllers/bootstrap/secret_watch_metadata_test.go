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

package bootstrap

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// secretMeta builds what a metadata-only informer actually delivers to a
// handler: a PartialObjectMetadata, not a *corev1.Secret. Both Secret watches
// on this controller are registered with builder.OnlyMetadata so the manager's
// cache never holds Secret bodies, which means every mapper and predicate on
// that path has to read through the metav1.Object accessors.
func secretMeta(name, namespace string, labels map[string]string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
	}
}

// TestSecretToKairosConfig_MapsPartialObjectMetadata guards the userdata Secret
// mapper against the metadata-only watch. A type assertion to *corev1.Secret
// here returns nil for every event the informer delivers, which silently stops
// a KairosConfig from being re-reconciled when its userdata Secret changes.
func TestSecretToKairosConfig_MapsPartialObjectMetadata(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Namespace: "default"},
		Spec: clusterv1.MachineSpec{
			Bootstrap: clusterv1.Bootstrap{
				DataSecretName: ptr.To("kc-1"),
				ConfigRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: bootstrapv1beta2.GroupVersion.Group,
					Kind:     "KairosConfig",
					Name:     "kc-1",
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).Build()
	r := &KairosConfigReconciler{Client: c, Scheme: scheme}

	got := r.secretToKairosConfig(context.Background(), secretMeta(
		"kc-1-userdata", "default",
		map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
	))
	g.Expect(got).To(HaveLen(1), "a metadata-only userdata Secret event must still enqueue its KairosConfig")
	g.Expect(got[0].Name).To(Equal("kc-1"))
	g.Expect(got[0].Namespace).To(Equal("default"))

	// The two filters the mapper applies must keep working off metadata.
	g.Expect(r.secretToKairosConfig(context.Background(), secretMeta(
		"kc-1-something-else", "default",
		map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
	))).To(BeEmpty(), "a Secret that is not *-userdata must not enqueue anything")

	g.Expect(r.secretToKairosConfig(context.Background(), secretMeta(
		"kc-1-userdata", "default", nil,
	))).To(BeEmpty(), "a userdata Secret with no cluster-name label must not enqueue anything")
}

// TestJoinTokenSecretToKairosConfig_MapsPartialObjectMetadata guards the HA
// join-token Secret mapper (ADR 0005 Phase 3) against the same regression. If
// this mapper stops firing, a joining control-plane KairosConfig waits for the
// requeue backstop instead of reacting to the token push.
func TestJoinTokenSecretToKairosConfig_MapsPartialObjectMetadata(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())

	cp := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cp-join",
			Namespace: "default",
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{Role: "control-plane"},
	}
	worker := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker",
			Namespace: "default",
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{Role: "worker"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp, worker).Build()
	r := &KairosConfigReconciler{Client: c, Scheme: scheme}

	tokenLabels := map[string]string{
		bootstrapv1beta2.ControlPlaneJoinTokenSecretTypeLabel: bootstrapv1beta2.ControlPlaneJoinTokenSecretTypeValue,
		clusterv1.ClusterNameLabel:                            "test-cluster",
	}
	got := r.joinTokenSecretToKairosConfig(context.Background(), secretMeta("join-token", "default", tokenLabels))
	g.Expect(got).To(HaveLen(1), "a metadata-only join-token Secret event must still enqueue the control-plane KairosConfig")
	g.Expect(got[0].Name).To(Equal("cp-join"), "only the control-plane role is enqueued, never the worker")

	// Both label filters must still gate.
	g.Expect(r.joinTokenSecretToKairosConfig(context.Background(), secretMeta(
		"join-token", "default",
		map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
	))).To(BeEmpty(), "a Secret without the join-token type label must not enqueue anything")

	g.Expect(r.joinTokenSecretToKairosConfig(context.Background(), secretMeta(
		"join-token", "default",
		map[string]string{bootstrapv1beta2.ControlPlaneJoinTokenSecretTypeLabel: bootstrapv1beta2.ControlPlaneJoinTokenSecretTypeValue},
	))).To(BeEmpty(), "a join-token Secret without a cluster-name label must not enqueue anything")
}

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

package controlplane

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// TestSecretToKairosControlPlane_MapsPartialObjectMetadata guards the
// kubeconfig Secret mapper against the metadata-only watch. The watch is
// registered with builder.OnlyMetadata so the manager's cache never holds the
// body of every Secret in the management cluster; what reaches this mapper is
// therefore a PartialObjectMetadata, and a type assertion to *corev1.Secret
// returns nil for every event — which stops a KairosControlPlane from ever
// seeing its node-pushed kubeconfig (KD-3b).
func TestSecretToKairosControlPlane_MapsPartialObjectMetadata(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: controlplanev1beta2.GroupVersion.Group,
				Kind:     "KairosControlPlane",
				Name:     "test-kcp",
			},
		},
	}
	// A second Cluster whose control plane is some other provider's. The
	// mapper, not the watch predicate, is what keeps this one out — which is
	// why dropping the Type check from the predicate cannot widen what is
	// actually enqueued.
	foreign := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "foreign-cluster", Namespace: "default"},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: "controlplane.cluster.x-k8s.io",
				Kind:     "KubeadmControlPlane",
				Name:     "foreign-kcp",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, foreign).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	meta := func(name, ns string, labels map[string]string) *metav1.PartialObjectMetadata {
		return &metav1.PartialObjectMetadata{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		}
	}

	got := r.secretToKairosControlPlane(context.Background(), meta(
		"test-cluster-kubeconfig", "default",
		map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
	))
	g.Expect(got).To(HaveLen(1), "a metadata-only kubeconfig Secret event must still enqueue its KairosControlPlane")
	g.Expect(got[0].Name).To(Equal("test-kcp"))
	g.Expect(got[0].Namespace).To(Equal("default"))

	// No cluster-name label: the watch predicate drops these, and the mapper
	// does too.
	g.Expect(r.secretToKairosControlPlane(context.Background(), meta(
		"test-cluster-kubeconfig", "default", nil,
	))).To(BeEmpty(), "a Secret with no cluster-name label must not enqueue anything")

	// Labelled for a Cluster this provider does not own. This is the case the
	// removed Type predicate used to pre-filter; the Cluster lookup is what
	// actually decides, and it still refuses.
	g.Expect(r.secretToKairosControlPlane(context.Background(), meta(
		"random-user-secret", "default",
		map[string]string{clusterv1.ClusterNameLabel: "foreign-cluster"},
	))).To(BeEmpty(), "a Secret labelled for a non-Kairos control plane must not enqueue anything")

	// Labelled for a Cluster that does not exist at all.
	g.Expect(r.secretToKairosControlPlane(context.Background(), meta(
		"orphan", "default",
		map[string]string{clusterv1.ClusterNameLabel: "no-such-cluster"},
	))).To(BeEmpty(), "a Secret naming an absent Cluster must not enqueue anything")
}

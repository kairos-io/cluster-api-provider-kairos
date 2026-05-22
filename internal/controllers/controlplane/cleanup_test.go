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
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

const (
	cleanupClusterName = "drain-cluster"
	cleanupKCPName     = "drain-kcp"
	cleanupNamespace   = "default"
)

func newCleanupTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	return scheme
}

func newDrainKCP() *controlplanev1beta2.KairosControlPlane {
	return &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cleanupKCPName,
			Namespace: cleanupNamespace,
			UID:       types.UID("kcp-uid-aaaa"),
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: cleanupClusterName,
			},
		},
	}
}

func newOwnedMachine(name string, kcp *controlplanev1beta2.KairosControlPlane) *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cleanupNamespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         cleanupClusterName,
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: controlplanev1beta2.GroupVersion.String(),
					Kind:       "KairosControlPlane",
					Name:       kcp.Name,
					UID:        kcp.UID,
					Controller: ptr.To(true),
				},
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: cleanupClusterName,
		},
	}
}

// TestDrainOwnedMachines_NoMachines confirms the happy path: nothing owned,
// returns remaining=0 and no error.
func TestDrainOwnedMachines_NoMachines(t *testing.T) {
	g := NewWithT(t)
	scheme := newCleanupTestScheme(t)
	kcp := newDrainKCP()

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp).Build()
	remaining, err := drainOwnedMachines(context.Background(), c, kcp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remaining).To(Equal(0))
}

// TestDrainOwnedMachines_NoClusterNameLabel proves we no-op (no error, no
// deletes) when the KCP lacks a cluster-name label. This is the "deleted
// before ever finding its Cluster" edge case.
func TestDrainOwnedMachines_NoClusterNameLabel(t *testing.T) {
	g := NewWithT(t)
	scheme := newCleanupTestScheme(t)
	kcp := newDrainKCP()
	delete(kcp.Labels, clusterv1.ClusterNameLabel)
	// Pre-create a Machine that DOES carry the cluster-name label and would
	// be owned by us if we knew the cluster name. Without the label on the
	// KCP, we must NOT delete it.
	bystander := newOwnedMachine("bystander", kcp)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp, bystander).Build()

	remaining, err := drainOwnedMachines(context.Background(), c, kcp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remaining).To(Equal(0))

	// Verify the bystander was NOT deleted.
	got := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(bystander), got)).To(Succeed())
	g.Expect(got.DeletionTimestamp.IsZero()).To(BeTrue())
}

// TestDrainOwnedMachines_DeletesOwned issues Delete on each owned Machine and
// returns the count remaining (which equals the number we just asked the
// apiserver to remove). After a follow-up reconcile the fake client would
// have GC'd them, but for the in-flight call we still report them as
// remaining so the caller requeues.
func TestDrainOwnedMachines_DeletesOwned(t *testing.T) {
	g := NewWithT(t)
	scheme := newCleanupTestScheme(t)
	kcp := newDrainKCP()
	m1 := newOwnedMachine("drain-kcp-0", kcp)
	m2 := newOwnedMachine("drain-kcp-1", kcp)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp, m1, m2).Build()

	remaining, err := drainOwnedMachines(context.Background(), c, kcp)
	g.Expect(err).NotTo(HaveOccurred())
	// The fake client deletes objects synchronously rather than setting a
	// deletion timestamp (no finalizers attached). After the call both
	// Machines have been removed, but the count we report is what was
	// observed at List time -- the contract is "Machines we asked to delete
	// or are mid-delete". For the fake client both delete immediately, so
	// the remaining is decremented inside drainOwnedMachines only for the
	// IsNotFound mid-delete race. Here both deletes succeed normally so we
	// return 2.
	g.Expect(remaining).To(Equal(2))

	// Verify both are gone.
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(m1), &clusterv1.Machine{})).NotTo(Succeed())
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(m2), &clusterv1.Machine{})).NotTo(Succeed())
}

// TestDrainOwnedMachines_SkipsForeignOwned verifies that Machines owned by a
// different KCP (matching the same cluster-name label) are NOT deleted and
// NOT counted in remaining. This is the safety net against deleting another
// control plane's nodes when two KCPs happen to share a cluster name (e.g.
// rolling KCP migration).
func TestDrainOwnedMachines_SkipsForeignOwned(t *testing.T) {
	g := NewWithT(t)
	scheme := newCleanupTestScheme(t)
	kcp := newDrainKCP()

	// Foreign Machine: same cluster-name label, different KCP UID.
	foreignKCP := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-kcp",
			Namespace: cleanupNamespace,
			UID:       types.UID("kcp-uid-zzzz"),
		},
	}
	foreign := newOwnedMachine("other-kcp-0", foreignKCP)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp, foreign).Build()

	remaining, err := drainOwnedMachines(context.Background(), c, kcp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remaining).To(Equal(0), "foreign-owned Machine must not count toward remaining")

	// Verify the foreign Machine was NOT deleted.
	got := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(foreign), got)).To(Succeed())
	g.Expect(got.DeletionTimestamp.IsZero()).To(BeTrue(), "foreign-owned Machine must not be deleted")
}

// TestDrainOwnedMachines_MidDeletionStillCounts verifies that a Machine
// already carrying a DeletionTimestamp (e.g. another controller holds a
// finalizer on it) is counted in remaining so the caller keeps requeuing
// rather than removing the KCP finalizer prematurely. This is the
// negative path that proves KD-4 is fixed: the KCP cannot disappear while
// orphan-prone children still linger.
func TestDrainOwnedMachines_MidDeletionStillCounts(t *testing.T) {
	g := NewWithT(t)
	scheme := newCleanupTestScheme(t)
	kcp := newDrainKCP()

	// Build a Machine that is mid-deletion: has DeletionTimestamp set and a
	// foreign finalizer holding it. The fake client requires a finalizer to
	// preserve DeletionTimestamp; otherwise it would GC immediately.
	mid := newOwnedMachine("drain-kcp-mid", kcp)
	mid.Finalizers = []string{"foreign-controller.example.com/wait"}
	now := metav1.NewTime(time.Now())
	mid.DeletionTimestamp = &now

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(kcp, mid).
		Build()

	remaining, err := drainOwnedMachines(context.Background(), c, kcp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remaining).To(Equal(1), "mid-deletion Machine must still count toward remaining so the KCP finalizer is held")

	// And the Machine is still present (still holding the foreign finalizer).
	got := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(mid), got)).To(Succeed())
}

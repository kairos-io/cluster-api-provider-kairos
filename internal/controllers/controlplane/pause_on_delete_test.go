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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// deletingKCP is a KairosControlPlane that has been asked to go away and still
// holds its finalizer, as the API server leaves it until the controller lets go.
func deletingKCP() *controlplanev1beta2.KairosControlPlane {
	kcp := newDrainKCP()
	now := metav1.Now()
	kcp.DeletionTimestamp = &now
	kcp.Finalizers = []string{controlplanev1beta2.KairosControlPlaneFinalizer}
	return kcp
}

func pauseTestCluster(paused bool) *clusterv1.Cluster {
	return &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: cleanupClusterName, Namespace: cleanupNamespace},
		Spec:       clusterv1.ClusterSpec{Paused: ptr.To(paused)},
	}
}

func reconcileKCP(g *WithT, r *KairosControlPlaneReconciler, kcp *controlplanev1beta2.KairosControlPlane) {
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(kcp)})
	g.Expect(err).NotTo(HaveOccurred())
}

// machineStillLive reports whether the Machine exists and is not being deleted.
func machineStillLive(g *WithT, c client.Client, name string) bool {
	m := &clusterv1.Machine{}
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: cleanupNamespace}, m)
	if apierrors.IsNotFound(err) {
		return false
	}
	g.Expect(err).NotTo(HaveOccurred())
	return m.DeletionTimestamp.IsZero()
}

func kcpFinalizers(g *WithT, c client.Client) []string {
	got := &controlplanev1beta2.KairosControlPlane{}
	err := c.Get(context.Background(), types.NamespacedName{Name: cleanupKCPName, Namespace: cleanupNamespace}, got)
	if apierrors.IsNotFound(err) {
		return nil
	}
	g.Expect(err).NotTo(HaveOccurred())
	return got.Finalizers
}

// Deleting the control plane marks every control-plane Machine for deletion and
// strips their etcd-leave hooks, neither of which can be undone. A paused
// Cluster must hold that back until it is unpaused, as it does upstream.
func TestReconcile_DeletionWaitsForAPausedCluster(t *testing.T) {
	g := NewWithT(t)
	scheme := newCleanupTestScheme(t)
	kcp := deletingKCP()
	machine := newOwnedMachine("cp-0", kcp)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(kcp, machine, pauseTestCluster(true)).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	reconcileKCP(g, r, kcp)

	g.Expect(machineStillLive(g, c, "cp-0")).To(BeTrue(), "no control-plane Machine may be deleted while the Cluster is paused")
	g.Expect(kcpFinalizers(g, c)).To(ContainElement(controlplanev1beta2.KairosControlPlaneFinalizer),
		"the finalizer stays so deletion resumes on unpause")
}

// The paused annotation on the KairosControlPlane itself holds deletion too.
func TestReconcile_DeletionWaitsForAPausedKairosControlPlane(t *testing.T) {
	g := NewWithT(t)
	scheme := newCleanupTestScheme(t)
	kcp := deletingKCP()
	kcp.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
	machine := newOwnedMachine("cp-0", kcp)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(kcp, machine, pauseTestCluster(false)).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	reconcileKCP(g, r, kcp)

	g.Expect(machineStillLive(g, c, "cp-0")).To(BeTrue())
}

// Unpaused, deletion goes ahead as before.
func TestReconcile_DeletionProceedsWhenNotPaused(t *testing.T) {
	g := NewWithT(t)
	scheme := newCleanupTestScheme(t)
	kcp := deletingKCP()
	machine := newOwnedMachine("cp-0", kcp)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(kcp, machine, pauseTestCluster(false)).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	reconcileKCP(g, r, kcp)

	g.Expect(machineStillLive(g, c, "cp-0")).To(BeFalse(), "an unpaused deletion must remove the control-plane Machines")
}

// A Cluster that is already gone cannot be paused, and deletion runs before the
// Cluster lookup precisely so a control plane whose Cluster was deleted first
// still finishes. The pause check must not turn that into a hang.
func TestReconcile_DeletionProceedsWhenTheClusterIsGone(t *testing.T) {
	g := NewWithT(t)
	scheme := newCleanupTestScheme(t)
	kcp := deletingKCP()
	machine := newOwnedMachine("cp-0", kcp)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp, machine).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	reconcileKCP(g, r, kcp)

	g.Expect(machineStillLive(g, c, "cp-0")).To(BeFalse())
}

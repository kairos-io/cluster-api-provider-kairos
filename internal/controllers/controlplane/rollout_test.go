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
	"encoding/json"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

const (
	rolloutOldVersion = "v1.30.0+k3s1"
	rolloutNewVersion = "v1.31.0+k3s1"
)

func rolloutKCP(replicas int32) *controlplanev1beta2.KairosControlPlane {
	kcp := machineMetaKCP("k3s", nil)
	kcp.Spec.Replicas = &replicas
	kcp.Spec.Version = rolloutNewVersion
	return kcp
}

func rolloutCluster() *clusterv1.Cluster {
	c := machineMetaCluster()
	c.UID = types.UID("cluster-uid")
	return c
}

// rolloutMachine is a control-plane machine owned by the test KCP. age orders
// the machines, oldest first, as reconcileMachines does. joined gives it a
// NodeRef and the Running phase, which is what counts as a live member.
func rolloutMachine(kcp *controlplanev1beta2.KairosControlPlane, name, version string, age time.Duration, joined bool) *clusterv1.Machine {
	m := machineMetaMachine(name, nil, nil)
	m.Spec.Version = version
	m.CreationTimestamp = metav1.NewTime(time.Now().Add(-age))
	m.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: controlplanev1beta2.GroupVersion.String(),
		Kind:       "KairosControlPlane",
		Name:       kcp.Name,
		UID:        kcp.UID,
		Controller: ptrBool(true),
	}}
	if joined {
		m.Status.NodeRef = clusterv1.MachineNodeReference{Name: name + "-node"}
		m.Status.Phase = string(clusterv1.MachinePhaseRunning)
	}
	return m
}

// rolloutEtcdStatus reports every named machine as a healthy voting etcd
// member, so the quorum guard permits removing any one of them. That isolates
// the rollout's own ordering from the guard.
func rolloutEtcdStatus(cluster *clusterv1.Cluster, machines ...*clusterv1.Machine) *corev1.Secret {
	data := map[string][]byte{}
	for _, m := range machines {
		node := m.Status.NodeRef.Name
		b, _ := json.Marshal(etcdMemberStatus{Name: node, Healthy: true, Voting: true, Members: len(machines)})
		data[node] = b
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: etcdStatusSecretName(cluster.Name), Namespace: cluster.Namespace},
		Data:       data,
	}
}

// remainingMachines returns the machines that still exist, and how many are being deleted.
func remainingMachines(g *WithT, c client.Client) (names []string, deleting int) {
	list := &clusterv1.MachineList{}
	g.Expect(c.List(context.Background(), list, client.InNamespace("default"))).To(Succeed())
	for _, m := range list.Items {
		names = append(names, m.Name)
		if !m.DeletionTimestamp.IsZero() {
			deleting++
		}
	}
	return names, deleting
}

func machinesUpToDate(kcp *controlplanev1beta2.KairosControlPlane) *clusterv1.Condition {
	return conditions.Get(kcp, controlplanev1beta2.MachinesUpToDateCondition)
}

// A version bump on a single-node control plane used to create a second machine
// with role "single" -- a separate, empty cluster -- and on the next pass delete
// the original, destroying everything the cluster held. spec.version is
// documented as informational, so an edit to it must never do that.
func TestReconcileMachines_SingleNodeVersionBumpLeavesTheMachineAlone(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := rolloutKCP(1)
	original := rolloutMachine(kcp, "test-kcp-0", rolloutOldVersion, time.Hour, true)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(original, machineMetaInfraTemplate()).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	// Two passes: the destructive sequence took one pass to create the
	// replacement and a second to delete the original.
	for range 2 {
		_, err := r.reconcileMachines(context.Background(), log.Log, kcp, rolloutCluster())
		g.Expect(err).NotTo(HaveOccurred())
	}

	names, deleting := remainingMachines(g, c)
	g.Expect(names).To(ConsistOf("test-kcp-0"), "no replacement may be created for a single-node control plane")
	g.Expect(deleting).To(BeZero(), "the only control-plane machine must not be deleted")

	cond := machinesUpToDate(kcp)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(corev1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(controlplanev1beta2.SingleNodeRolloutUnsupportedReason))
	g.Expect(cond.Severity).To(Equal(clusterv1.ConditionSeverityWarning),
		"the refusal must be visible, not an Info-level wait")
}

// The HA rollout used to fall through to the plain scale-down path, which does
// not wait for the replacement: an old member was deleted while the new one was
// still booting. A bad new image then left the cluster at two healthy members
// plus a dead one.
func TestReconcileMachines_HARolloutKeepsOldMembersWhileTheReplacementBoots(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := rolloutKCP(3)
	cluster := rolloutCluster()

	old0 := rolloutMachine(kcp, "test-kcp-0", rolloutOldVersion, 4*time.Hour, true)
	old1 := rolloutMachine(kcp, "test-kcp-1", rolloutOldVersion, 3*time.Hour, true)
	old2 := rolloutMachine(kcp, "test-kcp-2", rolloutOldVersion, 2*time.Hour, true)
	booting := rolloutMachine(kcp, "test-kcp-3", rolloutNewVersion, time.Minute, false)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(old0, old1, old2, booting, machineMetaInfraTemplate(),
			rolloutEtcdStatus(cluster, old0, old1, old2)).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	res, err := r.reconcileMachines(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(BeNumerically(">", 0), "a held rollout must come back to check on the replacement")

	names, deleting := remainingMachines(g, c)
	g.Expect(names).To(ConsistOf("test-kcp-0", "test-kcp-1", "test-kcp-2", "test-kcp-3"),
		"no old member may be removed until its replacement has joined")
	g.Expect(deleting).To(BeZero())

	cond := machinesUpToDate(kcp)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(controlplanev1beta2.RollingOutReason))
}

// Once the replacement has joined, the rollout must make progress. The old gate
// required "updated and joined >= desired", which with the default surge of one
// is never true mid-rollout; blocking the fall-through without replacing that
// gate would have wedged every HA rollout here instead.
func TestReconcileMachines_HARolloutRemovesTheOldestMemberOnceTheReplacementJoins(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := rolloutKCP(3)
	cluster := rolloutCluster()

	old0 := rolloutMachine(kcp, "test-kcp-0", rolloutOldVersion, 4*time.Hour, true)
	old1 := rolloutMachine(kcp, "test-kcp-1", rolloutOldVersion, 3*time.Hour, true)
	old2 := rolloutMachine(kcp, "test-kcp-2", rolloutOldVersion, 2*time.Hour, true)
	joined := rolloutMachine(kcp, "test-kcp-3", rolloutNewVersion, time.Minute, true)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(old0, old1, old2, joined, machineMetaInfraTemplate(),
			rolloutEtcdStatus(cluster, old0, old1, old2, joined)).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	_, err := r.reconcileMachines(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())

	names, _ := remainingMachines(g, c)
	g.Expect(names).To(ConsistOf("test-kcp-1", "test-kcp-2", "test-kcp-3"),
		"exactly one member is replaced per step, oldest first")
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, &clusterv1.Machine{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

// One membership change at a time. A machine already being deleted still counts
// toward the replica total, and a k3s member has no etcd-leave hook to hold the
// next step, so the rollout must wait for it to go before removing another.
func TestReconcileMachines_HARolloutWaitsForADeletingMemberToGo(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := rolloutKCP(3)
	cluster := rolloutCluster()

	old0 := rolloutMachine(kcp, "test-kcp-0", rolloutOldVersion, 4*time.Hour, true)
	// Not the oldest, so the step that would wrongly act next targets a
	// different, still-healthy member.
	leaving := rolloutMachine(kcp, "test-kcp-1", rolloutOldVersion, 3*time.Hour, true)
	now := metav1.Now()
	leaving.DeletionTimestamp = &now
	leaving.Finalizers = []string{"test.kairos.io/hold"}
	old2 := rolloutMachine(kcp, "test-kcp-2", rolloutOldVersion, 2*time.Hour, true)
	joined := rolloutMachine(kcp, "test-kcp-3", rolloutNewVersion, time.Minute, true)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(old0, leaving, old2, joined, machineMetaInfraTemplate(),
			rolloutEtcdStatus(cluster, old0, leaving, old2, joined)).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	res, err := r.reconcileMachines(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(BeNumerically(">", 0))

	names, deleting := remainingMachines(g, c)
	g.Expect(names).To(ConsistOf("test-kcp-0", "test-kcp-1", "test-kcp-2", "test-kcp-3"))
	g.Expect(deleting).To(Equal(1), "only the member already leaving may be on its way out")
}

// With nothing outdated the condition reports True, so the Warning and Info
// states above are cleared once a rollout finishes.
func TestReconcileMachines_MachinesUpToDateTrueWhenNothingIsOutdated(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := rolloutKCP(1)
	current := rolloutMachine(kcp, "test-kcp-0", rolloutNewVersion, time.Hour, true)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(current, machineMetaInfraTemplate()).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	_, err := r.reconcileMachines(context.Background(), log.Log, kcp, rolloutCluster())
	g.Expect(err).NotTo(HaveOccurred())

	cond := machinesUpToDate(kcp)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(corev1.ConditionTrue))
}

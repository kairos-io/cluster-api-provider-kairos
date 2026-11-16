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
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// foreignMachine is a control-plane Machine of the test Cluster that this
// KairosControlPlane does not own: it carries the cluster's control-plane
// labels, so clusterControlPlaneMachines lists it, but it is never in the
// KCP's own machines.
func foreignMachine(m *clusterv1.Machine) *clusterv1.Machine {
	m.Labels = map[string]string{clusterv1.ClusterNameLabel: testClusterName, clusterv1.MachineControlPlaneLabel: ""}
	m.OwnerReferences = []metav1.OwnerReference{{APIVersion: "controlplane.cluster.x-k8s.io/v1beta2", Kind: "KairosControlPlane", Name: "other-kcp", UID: "other-uid"}}
	return m
}

// failMachineLists makes every Machine List fail.
func failMachineLists(f *leaveFixture) {
	f.r.Client = interceptor.NewClient(f.mgmt.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*clusterv1.MachineList); ok {
			return errors.New("etcdserver: request timed out")
		}
		return c.List(ctx, list, opts...)
	}})
}

// TestLeaveGuardMachines: the KCP's own Machines, as given, and every other
// control-plane Machine of the Cluster once; nothing of another Cluster or
// without the control-plane label.
func TestLeaveGuardMachines(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	owned := []*clusterv1.Machine{nodeMachine("cp-0", true), nodeMachine("cp-1", false)}
	listedOwned := foreignMachine(nodeMachine("cp-1", false))
	foreign := foreignMachine(nodeMachine("old-cp", false))
	worker := nodeMachine("worker-0", false)
	worker.Labels = map[string]string{clusterv1.ClusterNameLabel: testClusterName}
	otherCluster := foreignMachine(nodeMachine("other-cp", false))
	otherCluster.Labels[clusterv1.ClusterNameLabel] = "another"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(listedOwned, foreign, worker, otherCluster).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	got, err := r.leaveGuardMachines(context.Background(), pinnedVIPKCP(), haCluster(), owned)
	g.Expect(err).ToNot(HaveOccurred())
	names := []string{}
	for _, m := range got {
		names = append(names, m.Name)
	}
	g.Expect(names).To(Equal([]string{"cp-0", "cp-1", "old-cp"}))
	g.Expect(got[1]).To(BeIdenticalTo(owned[1]), "the KCP's own Machines are kept as given")

	failing := &leaveFixture{mgmt: c, r: r}
	failMachineLists(failing)
	_, err = failing.r.leaveGuardMachines(context.Background(), pinnedVIPKCP(), haCluster(), owned)
	g.Expect(err).To(HaveOccurred())
}

// TestUnregisteredLeave_GuardsSeeMachinesOfOtherOwners is KD-93 for a
// leaving Machine without a NodeRef: a control-plane Machine of the Cluster
// that this KCP does not own is a possible owner of the member, as one of
// its own would be.
func TestUnregisteredLeave_GuardsSeeMachinesOfOtherOwners(t *testing.T) {
	t.Run("its KairosConfig renders the leaver's name: abandoned, nothing sent", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, recordedLeaver("cp-host", nil), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
		g.Expect(f.mgmt.Create(context.Background(), foreignMachine(withKairosConfigRef(noNodeRef(nodeMachine("old-cp", false)), "old-cp"))))
		g.Expect(f.mgmt.Create(context.Background(), kairosConfigWithHostname("old-cp", "cp-host"))).To(Succeed())

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
		g.Expect(leaveCM(g, f.member)).ToNot(HaveKey("cp-host"), "nothing is sent to a node that may be old-cp's")
		g.Expect(f.events()).To(ConsistOf(ContainSubstring(`Machine "old-cp" has the same node name`)))
	})
	t.Run("its Node has the leaver's name: abandoned", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, recordedLeaver("cp-host", nil), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
		other := foreignMachine(nodeMachine("old-cp", false))
		other.Status.NodeRef.Name = "cp-host"
		g.Expect(f.mgmt.Create(context.Background(), other)).To(Succeed())

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
		g.Expect(leaveCM(g, f.member)).ToNot(HaveKey("cp-host"))
	})
	t.Run("it reports the member's peer address: abandoned", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, recordedLeaver("cp-host", nil), []client.Object{k0sEtcdMember("cp-host", "10.0.0.20", false, "True")}, nil)
		g.Expect(f.mgmt.Create(context.Background(), foreignMachine(nodeMachine("old-cp", false, "10.0.0.20")))).To(Succeed())

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
		g.Expect(leaveCM(g, f.member)).ToNot(HaveKey("cp-host"))
		g.Expect(f.events()).To(ConsistOf(ContainSubstring(`peer address of Machine "old-cp"`)))
	})
	t.Run("one appears with the leaver's name after the request: held, nothing sent", func(t *testing.T) {
		g := NewWithT(t)
		requested := map[string]string{etcdLeaveRequestedAtAnnotation: ago(time.Minute)}
		f := newLeaveFixtureFor(g, recordedLeaver("cp-host", requested), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
		g.Expect(f.mgmt.Create(context.Background(), foreignMachine(withRecord(noNodeRef(nodeMachine("old-cp", false)), "cp-host")))).To(Succeed())

		done, err := f.reconcile()

		g.Expect(err).To(HaveOccurred())
		g.Expect(done).To(BeFalse())
		g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
		g.Expect(leaveCM(g, f.member)).ToNot(HaveKey("cp-host"))
		g.Expect(f.events()).To(ConsistOf(ContainSubstring("EtcdLeaveNodeNameUnusable")))
	})
	t.Run("on another bootstrap provider, named like the leaver's node: abandoned", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, recordedLeaver("cp-host", nil), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
		other := foreignMachine(noNodeRef(nodeMachine("cp-host", false)))
		other.Spec.Bootstrap.ConfigRef = clusterv1.ContractVersionedObjectReference{APIGroup: "bootstrap.cluster.x-k8s.io", Kind: "KubeadmConfig", Name: "cp-host"}
		g.Expect(f.mgmt.Create(context.Background(), other)).To(Succeed())

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
		g.Expect(leaveCM(g, f.member)).ToNot(HaveKey("cp-host"))
		g.Expect(f.events()).To(ConsistOf(ContainSubstring(`Machine "cp-host" has the same node name`)))
	})
	t.Run("same name and peer address, but of another Cluster: ignored", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, recordedLeaver("cp-host", nil), []client.Object{k0sEtcdMember("cp-host", "10.0.0.20", false, "True")}, nil)
		other := foreignMachine(nodeMachine("old-cp", false, "10.0.0.20"))
		other.Status.NodeRef.Name = "cp-host"
		other.Labels[clusterv1.ClusterNameLabel] = "another"
		g.Expect(f.mgmt.Create(context.Background(), other)).To(Succeed())

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeFalse())
		g.Expect(leaveCM(g, f.member)).To(HaveKeyWithValue("cp-host", etcdLeaveRequestedValue))
		g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
	})
	t.Run("one appears with the leaver's Node name after the request: held, nothing sent", func(t *testing.T) {
		g := NewWithT(t)
		requested := map[string]string{etcdLeaveRequestedAtAnnotation: ago(time.Minute)}
		f := newLeaveFixtureFor(g, recordedLeaver("cp-host", requested), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
		other := foreignMachine(nodeMachine("old-cp", false))
		other.Status.NodeRef.Name = "cp-host"
		g.Expect(f.mgmt.Create(context.Background(), other)).To(Succeed())

		done, err := f.reconcile()

		g.Expect(err).To(HaveOccurred())
		g.Expect(done).To(BeFalse())
		g.Expect(leaveCM(g, f.member)).ToNot(HaveKey("cp-host"))
		g.Expect(f.events()).To(ConsistOf(ContainSubstring("EtcdLeaveNodeNameUnusable")))
	})
}

// TestUnregisteredLeave_UncheckableGuardsAreBounded: when a guard's input
// cannot be read, the leave holds with nothing sent, and is released without
// a leave after the bound that phase already has (ADR 0014's ~15 minutes),
// never handed to k0s.
func TestUnregisteredLeave_UncheckableGuardsAreBounded(t *testing.T) {
	t.Run("before the request, the Machines cannot be listed: the unreachable bound", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, recordedLeaver("cp-host", nil), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
		failMachineLists(f)

		done, err := f.reconcile()
		g.Expect(err).To(HaveOccurred())
		g.Expect(done).To(BeFalse())
		got := getMachine(g, f.mgmt, "cp-0")
		g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
		g.Expect(got.Annotations).To(HaveKey(etcdLeaveUnreachableSinceAnnotation))
		g.Expect(leaveCM(g, f.member)).ToNot(HaveKey("cp-host"))

		got.Annotations[etcdLeaveUnreachableSinceAnnotation] = ago(leaveUnreachableBound + time.Minute)
		g.Expect(f.mgmt.Update(context.Background(), got)).To(Succeed())
		f.refreshTarget(g)
		done, err = f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
		g.Expect(leaveCM(g, f.member)).ToNot(HaveKey("cp-host"))
		g.Expect(f.events()).To(ContainElement(ContainSubstring("could not be listed")))
	})
	for _, tc := range []struct {
		name   string
		break_ func(f *leaveFixture)
	}{
		{name: "the Machines cannot be listed", break_: failMachineLists},
		{name: "another Machine's KairosConfig cannot be read", break_: func(f *leaveFixture) {
			f.machines[2] = withKairosConfigRef(noNodeRef(nodeMachine("cp-2", false, "10.0.0.12")), "cp-2")
			f.r.Client = interceptor.NewClient(f.mgmt.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*bootstrapv1beta2.KairosConfig); ok && key.Name == "cp-2" {
					return errors.New("etcdserver: request timed out")
				}
				return c.Get(ctx, key, obj, opts...)
			}})
		}},
	} {
		t.Run("after the request, "+tc.name+": held, then released at the cap, never delegated", func(t *testing.T) {
			g := NewWithT(t)
			requested := map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)}
			f := newLeaveFixtureFor(g, recordedLeaver("cp-host", requested), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
			tc.break_(f)

			done, err := f.reconcile()
			g.Expect(err).To(HaveOccurred())
			g.Expect(done).To(BeFalse())
			got := getMachine(g, f.mgmt, "cp-0")
			g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
			g.Expect(got.Annotations).To(HaveKey(etcdMemberLeaveRequestedAtAnnotation), "past the ack timeout the cap's clock starts")
			g.Expect(leaveCM(g, f.member)).ToNot(HaveKey("cp-host"))
			g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-host"))).To(BeFalse())
			g.Expect(f.events()).To(BeEmpty(), "a read error is logged, not reported as an unusable name")

			got.Annotations[etcdMemberLeaveRequestedAtAnnotation] = ago(etcdMemberLeaveCap + time.Minute)
			g.Expect(f.mgmt.Update(context.Background(), got)).To(Succeed())
			f.refreshTarget(g)
			done, err = f.reconcile()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(done).To(BeTrue())
			assertHookGone(g, f.mgmt, "cp-0")
			g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-host"))).To(BeFalse(), "never handed to k0s")
			g.Expect(f.events()).To(ConsistOf(ContainSubstring("EtcdMemberLeaveTimedOut")))
		})
	}
}

// TestReconcileMemberLeave_ListsTheMachinesOncePerPass: a pass that runs both
// the per-pass name check and the delegation lists the control-plane
// Machines once, so both guards check against the same snapshot.
func TestReconcileMemberLeave_ListsTheMachinesOncePerPass(t *testing.T) {
	g := NewWithT(t)
	requested := map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)}
	f := newLeaveFixtureFor(g, recordedLeaver("cp-host", requested), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
	lists := 0
	f.r.Client = interceptor.NewClient(f.mgmt.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*clusterv1.MachineList); ok {
			lists++
		}
		return c.List(ctx, list, opts...)
	}})

	done, err := f.reconcile()

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-host"))).To(BeTrue(), "the delegation ran in this pass")
	g.Expect(lists).To(Equal(1))
}

// TestDelegateMemberLeave_PeerOwnerSeesMachinesOfOtherOwners is KD-93 at the
// delegation, which runs for a registered leaving Machine too: an EtcdMember
// whose peer address a control-plane Machine of another owner reports is
// never asked to leave.
func TestDelegateMemberLeave_PeerOwnerSeesMachinesOfOtherOwners(t *testing.T) {
	t.Run("another owner's Machine reports the peer address: abandoned, no spec.leave", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)},
			[]client.Object{k0sEtcdMember("cp-0", "10.0.0.20", false, "True")}, nil)
		g.Expect(f.mgmt.Create(context.Background(), foreignMachine(nodeMachine("old-cp", false, "10.0.0.20")))).To(Succeed())

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
		g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeFalse())
		g.Expect(f.events()).To(ConsistOf(ContainSubstring(`peer address of Machine "old-cp"`)))
	})
	t.Run("the Machines cannot be listed: retried, then abandoned at the cap with no spec.leave", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)},
			[]client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "True")}, nil)
		failMachineLists(f)

		done, err := f.reconcile()
		g.Expect(err).To(HaveOccurred())
		g.Expect(done).To(BeFalse())
		got := getMachine(g, f.mgmt, "cp-0")
		g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
		g.Expect(got.Annotations).To(HaveKey(etcdMemberLeaveRequestedAtAnnotation), "the cap's clock starts")
		g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeFalse())

		got.Annotations[etcdMemberLeaveRequestedAtAnnotation] = ago(etcdMemberLeaveCap + time.Minute)
		g.Expect(f.mgmt.Update(context.Background(), got)).To(Succeed())
		f.refreshTarget(g)
		done, err = f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
		g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeFalse())
		g.Expect(f.events()).To(ContainElement(ContainSubstring("could not be listed")))
	})
	t.Run("a registered Machine's ack needs no list", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(time.Minute)}, nil, nil)
		g.Expect(f.member.Update(context.Background(), leaveConfigMapWithRV(g, f.member, map[string]string{"cp-0": etcdLeftValue}))).To(Succeed())
		failMachineLists(f)

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
	})
}

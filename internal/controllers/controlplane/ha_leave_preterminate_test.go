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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestReconcileMemberLeave_WaitsForCAPIToFinishWithTheNode: while CAPI is
// still working on the Node (pre-drain hooks, drain, volume detach) or has
// not reported a stage yet, the KCP neither requests the leave nor sends
// anything to the workload cluster, and the hook stays.
func TestReconcileMemberLeave_WaitsForCAPIToFinishWithTheNode(t *testing.T) {
	notDeleting := func(m *clusterv1.Machine) *clusterv1.Machine {
		apimeta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type: clusterv1.MachineDeletingCondition, Status: metav1.ConditionFalse, Reason: clusterv1.MachineNotDeletingReason,
		})
		return m
	}
	for _, tc := range []struct {
		name  string
		stage func(*clusterv1.Machine) *clusterv1.Machine
	}{
		{"no Deleting condition yet", func(m *clusterv1.Machine) *clusterv1.Machine { m.Status.Conditions = nil; return m }},
		{"deletion just started", func(m *clusterv1.Machine) *clusterv1.Machine {
			return withDeletingReason(m, clusterv1.MachineDeletingReason)
		}},
		{"waiting for pre-drain hooks", func(m *clusterv1.Machine) *clusterv1.Machine {
			return withDeletingReason(m, clusterv1.MachineDeletingWaitingForPreDrainHookReason)
		}},
		{"draining the node", func(m *clusterv1.Machine) *clusterv1.Machine {
			return withDeletingReason(m, clusterv1.MachineDeletingDrainingNodeReason)
		}},
		{"waiting for volume detach", func(m *clusterv1.Machine) *clusterv1.Machine {
			return withDeletingReason(m, clusterv1.MachineDeletingWaitingForVolumeDetachReason)
		}},
		{"Deleting condition False", notDeleting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			f := newLeaveFixtureFor(g, tc.stage(leavingMachine("cp-0", nil, "10.0.0.10")), nil, nil)
			workload := pinnedWorkloadBuilder(f.scheme).Build() // no leave ConfigMap yet
			members := &fakeMembers{clients: map[string]client.Client{"10.0.0.11": workload}}
			endpointCalls := 0
			f.r.MemberClientFactory = members.factory
			f.r.WorkloadClientFactory = func(context.Context, *clusterv1.Cluster) (client.Client, error) {
				endpointCalls++
				return workload, nil
			}

			done, err := f.reconcile()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(done).To(BeFalse())
			g.Expect(members.dialed).To(BeEmpty(), "no workload traffic before CAPI is done with the node")
			g.Expect(endpointCalls).To(BeZero())
			assertNoLeaveCM(g, workload) // no leave is requested
			got := getMachine(g, f.mgmt, "cp-0")
			g.Expect(got.Annotations).ToNot(HaveKey(etcdLeaveRequestedAtAnnotation), "the timeout clock does not start")
			g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
			g.Expect(f.events()).To(BeEmpty())
		})
	}
}

// TestReconcileMemberLeave_RequestsAtPreTerminate: once CAPI waits on the
// pre-terminate hooks, the leave is requested and the clock starts.
func TestReconcileMemberLeave_RequestsAtPreTerminate(t *testing.T) {
	g := NewWithT(t)
	f := newLeaveFixtureFor(g, leavingMachine("cp-0", nil, "10.0.0.10"), nil, nil)

	done, err := f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(leaveCM(g, f.member)).To(HaveKeyWithValue("cp-0", etcdLeaveRequestedValue))
	got := getMachine(g, f.mgmt, "cp-0")
	g.Expect(got.Annotations).To(HaveKey(etcdLeaveRequestedAtAnnotation))
	g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
}

// TestReconcileMemberLeave_AlreadyRequestedCarriesOn: a leave requested before
// this gate existed (a cluster stuck at the hook when the KCP is upgraded)
// carries on whatever stage CAPI reports, through to the EtcdMember fallback.
func TestReconcileMemberLeave_AlreadyRequestedCarriesOn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(*clusterv1.Machine) *clusterv1.Machine
	}{
		{"no Deleting condition", func(m *clusterv1.Machine) *clusterv1.Machine { m.Status.Conditions = nil; return m }},
		{"still reported as draining", func(m *clusterv1.Machine) *clusterv1.Machine {
			return withDeletingReason(m, clusterv1.MachineDeletingDrainingNodeReason)
		}},
	} {
		t.Run(tc.name+": the node's ack ends it", func(t *testing.T) {
			g := NewWithT(t)
			target := tc.stage(leavingMachine("cp-0", map[string]string{etcdLeaveRequestedAtAnnotation: ago(time.Minute)}, "10.0.0.10"))
			f := newLeaveFixtureFor(g, target, nil, nil)
			cm := leaveConfigMap(nil)
			g.Expect(f.member.Get(context.Background(), leaveConfigMapKey, cm)).To(Succeed())
			cm.Data["cp-0"] = etcdLeftValue
			g.Expect(f.member.Update(context.Background(), cm)).To(Succeed())

			done, err := f.reconcile()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(done).To(BeTrue())
			assertHookGone(g, f.mgmt, "cp-0")
		})
		t.Run(tc.name+": a timed-out leave is handed to k0s", func(t *testing.T) {
			g := NewWithT(t)
			target := tc.stage(leavingMachine("cp-0", map[string]string{etcdLeaveRequestedAtAnnotation: ago(20 * time.Minute)}, "10.0.0.10"))
			f := newLeaveFixtureFor(g, target, []client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "True")}, nil)

			done, err := f.reconcile()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(done).To(BeFalse())
			g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeTrue())
			g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
		})
	}
}

// TestReconcileMemberLeave_TimeoutCountsFromTheRequest: memberLeaveTimeout
// runs from when the leave was requested, not from the Machine's
// deletionTimestamp, however long CAPI took to get to pre-terminate.
func TestReconcileMemberLeave_TimeoutCountsFromTheRequest(t *testing.T) {
	longAgo := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	t.Run("first pass at pre-terminate after a long drain requests the leave", func(t *testing.T) {
		g := NewWithT(t)
		target := leavingMachine("cp-0", nil, "10.0.0.10")
		target.DeletionTimestamp = &longAgo
		f := newLeaveFixtureFor(g, target, []client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "True")}, nil)

		done, err := f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeFalse())
		g.Expect(leaveCM(g, f.member)).To(HaveKeyWithValue("cp-0", etcdLeaveRequestedValue))
		requestedAt, perr := time.Parse(time.RFC3339, getMachine(g, f.mgmt, "cp-0").Annotations[etcdLeaveRequestedAtAnnotation])
		g.Expect(perr).ToNot(HaveOccurred())
		g.Expect(time.Since(requestedAt)).To(BeNumerically("<", time.Minute), "the clock starts at the request")
		g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeFalse(), "not handed to k0s")
		g.Expect(f.events()).To(BeEmpty())
	})
	t.Run("a recent request on an old deletion is still waited on", func(t *testing.T) {
		g := NewWithT(t)
		target := leavingMachine("cp-0", map[string]string{etcdLeaveRequestedAtAnnotation: ago(time.Minute)}, "10.0.0.10")
		target.DeletionTimestamp = &longAgo
		f := newLeaveFixtureFor(g, target, []client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "True")}, nil)

		done, err := f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeFalse())
		g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeFalse(), "not handed to k0s")
		g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
		g.Expect(f.events()).To(BeEmpty())
	})
}

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

// ADR 0015 PR-3: the test table for a k0s control-plane Machine without a
// NodeRef, and for the bound before the first leave request.

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// unregisteredLeaver is the Machine cp-0 being removed, at pre-terminate,
// hooked, without a NodeRef, reporting addrs.
func unregisteredLeaver(annotations map[string]string, addrs ...string) *clusterv1.Machine {
	return noNodeRef(leavingMachine("cp-0", annotations, addrs...))
}

// assertNoSentinel checks nothing asked cp-0 to leave: the fixture's leave
// ConfigMap still holds only what it was built with.
func assertNoSentinelFor(g *WithT, f *leaveFixture, key string) {
	g.THelper()
	if key == "cp-0" {
		// The fixture seeds cp-0 with leave-requested; a write would be the
		// same value, so check the timestamp instead.
		g.Expect(getMachine(g, f.mgmt, "cp-0").Annotations).ToNot(HaveKey(etcdLeaveRequestedAtAnnotation))
		return
	}
	g.Expect(leaveCM(g, f.member)).ToNot(HaveKey(key))
}

// noAPIGroupClient makes every EtcdMember request fail as when the
// etcd.k0sproject.io API is not served.
func noEtcdMemberAPI() *interceptor.Funcs {
	noMatch := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: etcdMemberGVK.Group, Kind: etcdMemberGVK.Kind}}
	return &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if obj.GetObjectKind().GroupVersionKind() == etcdMemberGVK {
			return noMatch
		}
		return c.Get(ctx, key, obj, opts...)
	}}
}

// TestUnregisteredLeave_NoEtcdMemberReleasesTheHook: a Machine without a
// NodeRef whose node name has no EtcdMember in the pinned cluster never
// started a member. The hook goes at once, with no sentinel and no warning.
// This also covers a VM that never booted: no addresses, nothing in etcd.
func TestUnregisteredLeave_NoEtcdMemberReleasesTheHook(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target *clusterv1.Machine
	}{
		{"joined nothing", unregisteredLeaver(nil, "10.0.0.10")},
		{"a VM that never booted", unregisteredLeaver(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			f := newLeaveFixtureFor(g, tc.target, []client.Object{k0sEtcdMember("cp-1", "10.0.0.11", false, "True")}, nil)

			done, err := f.reconcile()

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(done).To(BeTrue())
			assertHookGone(g, f.mgmt, "cp-0")
			assertNoSentinelFor(g, f, "cp-0")
			g.Expect(f.events()).To(BeEmpty(), "verified absence is not a timeout")
		})
	}
}

// TestUnregisteredLeave_MemberPresentGetsTheHandshake is the incident case:
// a joiner joined etcd but never got a NodeRef. Its EtcdMember is found under
// its rendered name, so the KCP requests the leave, records the member ID,
// and keeps the hook. At memberLeaveTimeout it hands the leave to k0s under
// that ID, and releases the hook only once k0s reports Joined=False.
func TestUnregisteredLeave_MemberPresentGetsTheHandshake(t *testing.T) {
	g := NewWithT(t)
	target := withKairosConfigRef(unregisteredLeaver(nil, "10.0.0.10"), "cp-0")
	f := newLeaveFixtureFor(g, target, []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
	g.Expect(f.mgmt.Create(context.Background(), kairosConfigWithHostname("cp-0", "cp-host"))).To(Succeed())

	// First pass: the sentinel goes under the rendered name, not the Machine name.
	done, err := f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(leaveCM(g, f.member)).To(HaveKeyWithValue("cp-host", etcdLeaveRequestedValue))
	got := getMachine(g, f.mgmt, "cp-0")
	g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
	g.Expect(got.Annotations).To(HaveKey(etcdLeaveRequestedAtAnnotation))
	g.Expect(got.Annotations).To(HaveKeyWithValue(etcdMemberLeaveMemberIDAnnotation, testMemberID), "pinned at the first inspection")
	g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-host"))).To(BeFalse(), "k0s is not asked before the timeout")

	// The node never acks. Its etcd-status absence (it never reported) must
	// not end the leave.
	got.Annotations[etcdLeaveRequestedAtAnnotation] = ago(2 * memberLeaveTimeout)
	g.Expect(f.mgmt.Update(context.Background(), got)).To(Succeed())
	f.target = getMachine(g, f.mgmt, "cp-0")
	f.machines[0] = f.target
	done, err = f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-host"))).To(BeTrue(), "delegated to k0s")
	g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue(), "kept while k0s reports it joined")

	// k0s removes the member.
	m := getEtcdMember(g, f.member, "cp-host")
	m.Object["status"].(map[string]interface{})["conditions"] = []interface{}{map[string]interface{}{"type": "Joined", "status": "False"}}
	g.Expect(f.member.Update(context.Background(), m)).To(Succeed())
	f.target = getMachine(g, f.mgmt, "cp-0")
	f.machines[0] = f.target
	done, err = f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeTrue())
	assertHookGone(g, f.mgmt, "cp-0")
}

// TestUnregisteredLeave_RequestedLeaveIgnoresEtcdStatusAbsence: once the
// leave is requested, a node without a NodeRef that is missing from
// etcd-status has not therefore left: it may never have reported.
func TestUnregisteredLeave_RequestedLeaveIgnoresEtcdStatusAbsence(t *testing.T) {
	g := NewWithT(t)
	target := unregisteredLeaver(map[string]string{etcdLeaveRequestedAtAnnotation: ago(time.Minute)}, "10.0.0.10")
	f := newLeaveFixtureFor(g, target, []client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "True")}, nil)
	g.Expect(f.mgmt.Delete(context.Background(), etcdStatusSecretForMembers())).To(Succeed())

	done, err := f.reconcile()

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
}

// TestUnregisteredLeave_JoinedFalseReleasesTheHook: an EtcdMember k0s
// already reports Joined=False is not in etcd. The hook goes, nothing is
// written.
func TestUnregisteredLeave_JoinedFalseReleasesTheHook(t *testing.T) {
	g := NewWithT(t)
	f := newLeaveFixtureFor(g, unregisteredLeaver(nil, "10.0.0.10"), []client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "False")}, nil)

	done, err := f.reconcile()

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeTrue())
	assertHookGone(g, f.mgmt, "cp-0")
	assertNoSentinelFor(g, f, "cp-0")
	g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeFalse())
}

// TestUnregisteredLeave_AmbiguousMemberIsNotTouched: an EtcdMember under the
// node name that may belong to another Machine (its peer address, or the same
// rendered name) is never written to. The hook goes with a warning.
func TestUnregisteredLeave_AmbiguousMemberIsNotTouched(t *testing.T) {
	t.Run("another Machine reports its peer address", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, unregisteredLeaver(nil, "10.0.0.10"), []client.Object{k0sEtcdMember("cp-0", "10.0.0.11", false, "True")}, nil)

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
		assertNoSentinelFor(g, f, "cp-0")
		g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeFalse())
		g.Expect(f.events()).To(ConsistOf(And(ContainSubstring("Warning EtcdMemberLeaveTimedOut"), ContainSubstring(`Machine "cp-1"`))))
	})
	t.Run("another Machine renders the same node name", func(t *testing.T) {
		g := NewWithT(t)
		target := withKairosConfigRef(unregisteredLeaver(nil, "10.0.0.10"), "cp-0")
		f := newLeaveFixtureFor(g, target, []client.Object{k0sEtcdMember("shared", "10.0.0.10", false, "True")}, nil)
		f.machines[2] = withKairosConfigRef(noNodeRef(nodeMachine("cp-2", false, "10.0.0.12")), "cp-2")
		g.Expect(f.mgmt.Create(context.Background(), kairosConfigWithHostname("cp-0", "shared"))).To(Succeed())
		g.Expect(f.mgmt.Create(context.Background(), kairosConfigWithHostname("cp-2", "shared"))).To(Succeed())

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
		g.Expect(leaveCM(g, f.member)).ToNot(HaveKey("shared"))
		g.Expect(leaveRequested(getEtcdMember(g, f.member, "shared"))).To(BeFalse())
		g.Expect(f.events()).To(ConsistOf(And(ContainSubstring("Warning EtcdMemberLeaveTimedOut"), ContainSubstring(`Machine "cp-2" has the same node name`))))
	})
}

// TestUnregisteredLeave_NoEtcdMemberAPI: a cluster without k0s's EtcdMember
// API takes the warning path, as the ADR 0014 delegation does.
func TestUnregisteredLeave_NoEtcdMemberAPI(t *testing.T) {
	g := NewWithT(t)
	f := newLeaveFixtureFor(g, unregisteredLeaver(nil, "10.0.0.10"), nil, noEtcdMemberAPI())

	done, err := f.reconcile()

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeTrue())
	assertHookGone(g, f.mgmt, "cp-0")
	g.Expect(f.events()).To(ConsistOf(And(ContainSubstring("Warning EtcdMemberLeaveTimedOut"), ContainSubstring("no EtcdMember API"))))
}

// TestUnregisteredLeave_CAPKBridgePeerAddress: on CAPK the etcd peer address
// is on a bridge NIC that no Machine reports. That address is not a reason to
// stop: the member is matched by its name, and the leave proceeds.
func TestUnregisteredLeave_CAPKBridgePeerAddress(t *testing.T) {
	g := NewWithT(t)
	// Machines report masquerade pod IPs; etcd peers on the bridge network.
	f := newLeaveFixtureFor(g, unregisteredLeaver(nil, "10.0.0.10"), []client.Object{k0sEtcdMember("cp-0", "192.168.50.20:2380", false, "True")}, nil)

	done, err := f.reconcile()

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(getMachine(g, f.mgmt, "cp-0").Annotations).To(HaveKey(etcdLeaveRequestedAtAnnotation), "the handshake started")
	g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
	g.Expect(f.events()).To(BeEmpty())
}

// TestUnregisteredLeave_UnreadableEtcdMemberIsBounded: an EtcdMember read
// that fails is no evidence either way. The hook is kept, and released with
// a warning only after leaveUnreachableBound.
func TestUnregisteredLeave_UnreadableEtcdMemberIsBounded(t *testing.T) {
	failMembers := &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if obj.GetObjectKind().GroupVersionKind() == etcdMemberGVK {
			return errors.New("etcdserver: request timed out")
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	t.Run("within the bound: kept", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, unregisteredLeaver(nil, "10.0.0.10"), nil, failMembers)
		done, err := f.reconcile()
		g.Expect(err).To(HaveOccurred())
		g.Expect(done).To(BeFalse())
		got := getMachine(g, f.mgmt, "cp-0")
		g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
		g.Expect(got.Annotations).To(HaveKey(etcdLeaveUnreachableSinceAnnotation))
	})
	t.Run("past the bound: released with a warning", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, unregisteredLeaver(map[string]string{etcdLeaveUnreachableSinceAnnotation: ago(leaveUnreachableBound + time.Minute)}, "10.0.0.10"), nil, failMembers)
		done, err := f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
		g.Expect(f.events()).To(ConsistOf(ContainSubstring("Warning EtcdMemberLeaveTimedOut")))
	})
}

// TestLeave_NothingAnswersBeforeTheFirstRequestIsBounded covers both
// Machines without a NodeRef and, per OQ-5, Machines with one: when no
// verified workload apiserver answers before the first leave request, the
// KCP stamps etcd-leave-unreachable-since and keeps the hook, and once that
// stamp is older than leaveUnreachableBound it takes the warning path. Before
// ADR 0015 this phase had no bound at all (GT-15).
func TestLeave_NothingAnswersBeforeTheFirstRequestIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func(map[string]string) *clusterv1.Machine
	}{
		{"no NodeRef", func(a map[string]string) *clusterv1.Machine { return unregisteredLeaver(a, "10.0.0.10") }},
		{"a VM that never booted", func(a map[string]string) *clusterv1.Machine { return unregisteredLeaver(a) }},
		{"with a NodeRef (OQ-5)", func(a map[string]string) *clusterv1.Machine { return leavingMachine("cp-0", a, "10.0.0.10") }},
	} {
		t.Run(tc.name+": the first pass stamps and keeps the hook", func(t *testing.T) {
			g := NewWithT(t)
			f := newLeaveFixtureFor(g, tc.target(nil), nil, nil)
			f.r.MemberClientFactory = (&fakeMembers{}).factory // no member answers; the endpoint hangs

			done, err := f.reconcile()

			g.Expect(err).To(HaveOccurred())
			g.Expect(done).To(BeFalse())
			got := getMachine(g, f.mgmt, "cp-0")
			g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
			g.Expect(got.Annotations).To(HaveKey(etcdLeaveUnreachableSinceAnnotation))
			g.Expect(got.Annotations).ToNot(HaveKey(etcdLeaveRequestedAtAnnotation), "no leave was requested")
		})
		t.Run(tc.name+": a later pass keeps the first stamp", func(t *testing.T) {
			g := NewWithT(t)
			first := ago(3 * time.Minute)
			f := newLeaveFixtureFor(g, tc.target(map[string]string{etcdLeaveUnreachableSinceAnnotation: first}), nil, nil)
			f.r.MemberClientFactory = (&fakeMembers{}).factory

			_, err := f.reconcile()

			g.Expect(err).To(HaveOccurred())
			g.Expect(getMachine(g, f.mgmt, "cp-0").Annotations).To(HaveKeyWithValue(etcdLeaveUnreachableSinceAnnotation, first))
		})
		t.Run(tc.name+": past the bound the hook goes with a warning", func(t *testing.T) {
			g := NewWithT(t)
			f := newLeaveFixtureFor(g, tc.target(map[string]string{etcdLeaveUnreachableSinceAnnotation: ago(leaveUnreachableBound + time.Minute)}), nil, nil)
			f.r.MemberClientFactory = (&fakeMembers{}).factory

			done, err := f.reconcile()

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(done).To(BeTrue())
			assertHookGone(g, f.mgmt, "cp-0")
			g.Expect(f.events()).To(ConsistOf(And(ContainSubstring("Warning EtcdMemberLeaveTimedOut"), ContainSubstring("no workload apiserver answered"))))
		})
	}
	t.Run("answering again before the bound starts the normal handshake", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, leavingMachine("cp-0", map[string]string{etcdLeaveUnreachableSinceAnnotation: ago(10 * time.Minute)}, "10.0.0.10"), nil, nil)
		g.Expect(f.member.Update(context.Background(), leaveConfigMapWithRV(g, f.member, map[string]string{"cp-0": etcdLeftValue}))).To(Succeed())

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeFalse(), "a stale ack never ends the first pass")
		g.Expect(leaveCM(g, f.member)).To(HaveKeyWithValue("cp-0", etcdLeaveRequestedValue), "the stale ack is overwritten")
		g.Expect(getMachine(g, f.mgmt, "cp-0").Annotations).To(HaveKey(etcdLeaveRequestedAtAnnotation))
	})
}

// TestUnregisteredLeave_IdentityIsRequired: a "no EtcdMember" read through a
// cluster that does not match the pin never releases the hook (the
// 2026-10-17 abandon); and with no pin at all nothing counts as an answer,
// so the hook is kept until the bound.
func TestUnregisteredLeave_IdentityIsRequired(t *testing.T) {
	t.Run("another cluster: frozen, the hook is kept, nothing is written", func(t *testing.T) {
		g := NewWithT(t)
		// The other cluster has no EtcdMember for cp-0, as the new cluster had none
		// for the old members.
		f := newLeaveFixtureFor(g, unregisteredLeaver(nil, "10.0.0.10"), []client.Object{kubeSystem(uidB)}, nil)
		kcp := leaveKCP(g, f, map[string]string{WorkloadClusterUIDAnnotation: uidA})

		done, err := f.r.reconcileMemberLeave(context.Background(), log.Log, kcp, haCluster(), f.machines, f.target)

		g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
		g.Expect(done).To(BeFalse())
		got := getMachine(g, f.mgmt, "cp-0")
		g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
		g.Expect(got.Annotations).ToNot(HaveKey(etcdLeaveRequestedAtAnnotation))
		g.Expect(got.Annotations).ToNot(HaveKey(etcdLeaveUnreachableSinceAnnotation), "a foreign answer is not 'unreachable': it never starts the bound")
		stored := &controlplanev1beta2.KairosControlPlane{}
		g.Expect(f.mgmt.Get(context.Background(), client.ObjectKeyFromObject(kcp), stored)).To(Succeed())
		g.Expect(stored.Annotations).To(HaveKey(WorkloadClusterIdentityMismatchAnnotation))
	})
	t.Run("another cluster past every bound: still kept", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, unregisteredLeaver(map[string]string{etcdLeaveUnreachableSinceAnnotation: ago(time.Hour)}, "10.0.0.10"), []client.Object{kubeSystem(uidB)}, nil)
		kcp := leaveKCP(g, f, map[string]string{WorkloadClusterUIDAnnotation: uidA})

		done, err := f.r.reconcileMemberLeave(context.Background(), log.Log, kcp, haCluster(), f.machines, f.target)

		g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
		g.Expect(done).To(BeFalse())
		g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
	})
	t.Run("no pin: no connection, the hook is kept within the bound", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, unregisteredLeaver(nil, "10.0.0.10"), nil, nil)

		done, err := f.r.reconcileMemberLeave(context.Background(), log.Log, vipKCP(), haCluster(), f.machines, f.target)

		g.Expect(errors.Is(err, errWorkloadIdentityNotPinned)).To(BeTrue())
		g.Expect(done).To(BeFalse())
		got := getMachine(g, f.mgmt, "cp-0")
		g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
		g.Expect(got.Annotations).To(HaveKey(etcdLeaveUnreachableSinceAnnotation))
	})
}

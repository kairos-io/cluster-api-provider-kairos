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

package envtest

// ADR 0015 PR-3 in envtest: the leave of a k0s joiner without a NodeRef, and
// the orphan etcd-member cleanup.

import (
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

const (
	etcdMemberLeaveMemberIDKey = "controlplane.cluster.x-k8s.io/etcd-member-leave-member-id"
	etcdOrphanMemberLeaveKey   = "controlplane.cluster.x-k8s.io/etcd-orphan-member-leave"
)

// backdateLeaverAnnotation sets key on the leaving Machine to a time d ago,
// as if d had passed since the KCP stamped it.
func (k *k0sLeaveCluster) backdateLeaverAnnotation(g *WithT, key string, d time.Duration) {
	g.Eventually(func() error {
		m := &clusterv1.Machine{}
		if err := k.c.Get(k.ctx, k.leaverKey, m); err != nil {
			return err
		}
		m.Annotations[key] = time.Now().Add(-d).UTC().Format(time.RFC3339)
		return k.c.Update(k.ctx, m)
	}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
}

// TestHA_K0sEtcdLeave_JoinerWithoutNodeRefLeavesItsMember reproduces the
// first link of the 2026-10-17 chain. A k0s joiner joined etcd (k0s created
// its EtcdMember under the node's hostname) but never got a NodeRef, and is
// deleted. The KCP used to release its hook at once, orphaning the member.
// Now it finds the EtcdMember under the node's rendered name, requests the
// leave, hands it to k0s at the timeout, and releases the hook only once k0s
// reports the member Joined=False: never immediately.
func TestHA_K0sEtcdLeave_JoinerWithoutNodeRefLeavesItsMember(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	k, teardown := startK0sLeaveClusterWith(t, "ha-k0s-noref", k0sLeaveOptions{unregisteredLeaver: true})
	defer teardown()
	ctx, wc := k.ctx, k.wc
	node := k.leaverNode()
	g.Expect(node).To(Equal(k.leaverKey.Name), "with no spec.hostname the node is named after the Machine")

	// Without a NodeRef, CAPI goes straight to the pre-terminate hooks.
	k.setLeaverDeletingReason(g, clusterv1.MachineDeletingWaitingForPreTerminateHookReason)

	// The leave is requested under the node's name, and its member is pinned.
	g.Eventually(func() string { return k.leaveRequestFor(node) },
		30*time.Second, 500*time.Millisecond).Should(Equal("leave-requested"))
	g.Eventually(func() map[string]string { return k.leaver(g).Annotations },
		10*time.Second, 500*time.Millisecond).Should(And(HaveKey(etcdLeaveRequestedAtKey), HaveKeyWithValue(etcdMemberLeaveMemberIDKey, "8e9e05c52164694d")))

	// Never released while the member may be in etcd.
	touchKCP(g, ctx, k.c, k.nsName, k.kcpName)
	g.Consistently(func() map[string]string { return k.leaver(g).Annotations },
		4*time.Second, 500*time.Millisecond).Should(HaveKey(etcdLeaveHookKey), "the hook is never released immediately")

	// The node never acks. Once memberLeaveTimeout has passed, the KCP asks
	// k0s to remove the member.
	k.backdateLeaverAnnotation(g, etcdLeaveRequestedAtKey, 20*time.Minute)
	g.Eventually(func() (bool, error) { return k0sEtcdMemberLeave(ctx, wc, node) },
		30*time.Second, 500*time.Millisecond).Should(BeTrue(), "spec.leave must be set on the joiner's EtcdMember")
	touchKCP(g, ctx, k.c, k.nsName, k.kcpName)
	g.Consistently(func() map[string]string { return k.leaver(g).Annotations },
		4*time.Second, 500*time.Millisecond).Should(HaveKey(etcdLeaveHookKey), "kept while k0s reports the member joined")

	// k0s removes the member: now the hook goes.
	setK0sEtcdMemberJoined(t, ctx, wc, node, k0sLeaverIP, "False")
	touchKCP(g, ctx, k.c, k.nsName, k.kcpName)
	g.Eventually(func() map[string]string { return k.leaver(g).Annotations },
		45*time.Second, 500*time.Millisecond).ShouldNot(HaveKey(etcdLeaveHookKey), "the hook goes once k0s reports Joined=False")

	for _, keeper := range []string{"keeper-a", "keeper-b"} {
		leave, err := k0sEtcdMemberLeave(ctx, wc, keeper)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(leave).To(BeFalse(), "EtcdMember %s must not be asked to leave", keeper)
	}
	g.Expect(k.members.addresses()).To(HaveEach(BeElementOf("10.11.0.11", "10.11.0.12")))
}

// TestHA_K0sOrphanEtcdMemberIsRemoved: a steady three-member k0s control plane
// whose etcd still holds a member with no Machine (the orphan an old
// no-NodeRef delete left behind). The KCP asks k0s to remove that member,
// and only that one, records it on the KCP while k0s works, and clears the
// record once k0s reports the member gone.
func TestHA_K0sOrphanEtcdMemberIsRemoved(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	k, teardown := startK0sLeaveClusterWith(t, "ha-k0s-orphan", k0sLeaveOptions{
		keepLeaver:   true,
		extraMembers: map[string]string{"ghost": "10.11.0.9"},
	})
	defer teardown()
	ctx, c, wc := k.ctx, k.c, k.wc
	touchKCP(g, ctx, c, k.nsName, k.kcpName)

	g.Eventually(func() (bool, error) { return k0sEtcdMemberLeave(ctx, wc, "ghost") },
		45*time.Second, 500*time.Millisecond).Should(BeTrue(), "spec.leave must be set on the orphan")
	g.Eventually(func() string { return getKCP(g, ctx, c, k.nsName, k.kcpName).Annotations[etcdOrphanMemberLeaveKey] },
		10*time.Second, 500*time.Millisecond).Should(HavePrefix("ghost 8e9e05c52164694d "))
	for _, member := range []string{"leaver-node", "keeper-a", "keeper-b"} {
		leave, err := k0sEtcdMemberLeave(ctx, wc, member)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(leave).To(BeFalse(), "EtcdMember %s has a Machine and must not be asked to leave", member)
	}

	setK0sEtcdMemberJoined(t, ctx, wc, "ghost", "10.11.0.9", "False")
	touchKCP(g, ctx, c, k.nsName, k.kcpName)
	g.Eventually(func() map[string]string { return getKCP(g, ctx, c, k.nsName, k.kcpName).Annotations },
		45*time.Second, 500*time.Millisecond).ShouldNot(HaveKey(etcdOrphanMemberLeaveKey), "the record goes once k0s reports the orphan gone")

	// Nothing else was asked to leave, and every Machine is still there.
	for _, member := range []string{"leaver-node", "keeper-a", "keeper-b"} {
		leave, err := k0sEtcdMemberLeave(ctx, wc, member)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(leave).To(BeFalse())
	}
	ms := &clusterv1.MachineList{}
	g.Expect(c.List(ctx, ms)).To(Succeed())
	var ours []string
	for _, m := range ms.Items {
		if m.Namespace == k.nsName && strings.HasPrefix(m.Name, k.kcpName) {
			g.Expect(m.DeletionTimestamp.IsZero()).To(BeTrue(), "Machine %s must not be deleted", m.Name)
			ours = append(ours, m.Name)
		}
	}
	g.Expect(ours).To(HaveLen(3))
}

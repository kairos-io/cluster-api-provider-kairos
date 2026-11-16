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
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

const (
	etcdMemberLeaveMemberIDKey = "controlplane.cluster.x-k8s.io/etcd-member-leave-member-id"
	etcdOrphanMemberLeaveKey   = "controlplane.cluster.x-k8s.io/etcd-orphan-member-leave"
	// renderedNodeNameKey is the node name the KCP records on a control-plane
	// Machine at creation (KD-91).
	renderedNodeNameKey = "controlplane.cluster.x-k8s.io/rendered-node-name"
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

// TestHA_K0sEtcdLeave_RecordedNodeNameSurvivesKairosConfigDeletion is KD-91.
// A k0s joiner whose KairosConfig set spec.hostname joined etcd under that
// name and never got a NodeRef. Its KairosConfig is gone by pre-terminate, as
// after a foreground delete of the Machine. The KCP used to fall back to the
// Machine name, find no EtcdMember of that name, and release the hook with no
// leave, orphaning the member. With the node name recorded on the Machine it
// finds the member, requests the leave, hands it to k0s at the timeout, and
// releases the hook only once k0s reports the member Joined=False.
func TestHA_K0sEtcdLeave_RecordedNodeNameSurvivesKairosConfigDeletion(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	k, teardown := startK0sLeaveClusterWith(t, "ha-k0s-record", k0sLeaveOptions{
		unregisteredLeaver: true,
		leaverHostname:     "cp-leaver",
		leaverAnnotations:  map[string]string{renderedNodeNameKey: "cp-leaver"},
	})
	defer teardown()
	ctx, wc := k.ctx, k.wc
	node := k.leaverNode()
	g.Expect(node).To(Equal("cp-leaver"))

	k.deleteLeaverKairosConfig(g)
	k.setLeaverDeletingReason(g, clusterv1.MachineDeletingWaitingForPreTerminateHookReason)

	g.Eventually(func() string { return k.leaveRequestFor(node) },
		30*time.Second, 500*time.Millisecond).Should(Equal("leave-requested"))
	g.Eventually(func() map[string]string { return k.leaver(g).Annotations },
		10*time.Second, 500*time.Millisecond).Should(And(HaveKey(etcdLeaveRequestedAtKey), HaveKeyWithValue(etcdMemberLeaveMemberIDKey, "8e9e05c52164694d")))
	g.Expect(k.leaveRequestFor(k.leaverKey.Name)).To(BeEmpty(), "nothing is requested under the Machine name")

	touchKCP(g, ctx, k.c, k.nsName, k.kcpName)
	g.Consistently(func() map[string]string { return k.leaver(g).Annotations },
		4*time.Second, 500*time.Millisecond).Should(HaveKey(etcdLeaveHookKey), "the hook is never released immediately")

	k.backdateLeaverAnnotation(g, etcdLeaveRequestedAtKey, 20*time.Minute)
	g.Eventually(func() (bool, error) { return k0sEtcdMemberLeave(ctx, wc, node) },
		30*time.Second, 500*time.Millisecond).Should(BeTrue(), "spec.leave must be set on the joiner's EtcdMember")
	touchKCP(g, ctx, k.c, k.nsName, k.kcpName)
	g.Consistently(func() map[string]string { return k.leaver(g).Annotations },
		4*time.Second, 500*time.Millisecond).Should(HaveKey(etcdLeaveHookKey), "kept while k0s reports the member joined")

	setK0sEtcdMemberJoined(t, ctx, wc, node, k0sLeaverIP, "False")
	touchKCP(g, ctx, k.c, k.nsName, k.kcpName)
	g.Eventually(func() map[string]string { return k.leaver(g).Annotations },
		45*time.Second, 500*time.Millisecond).ShouldNot(HaveKey(etcdLeaveHookKey), "the hook goes once k0s reports Joined=False")

	for _, keeper := range []string{"keeper-a", "keeper-b"} {
		leave, err := k0sEtcdMemberLeave(ctx, wc, keeper)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(leave).To(BeFalse(), "EtcdMember %s must not be asked to leave", keeper)
	}
}

// TestHA_K0sEtcdLeave_HostnameCollisionWithAnUnregisteredMachine: another
// control-plane Machine without a NodeRef records its own name, but its
// KairosConfig renders the leaver's node name. Its node may have been
// bootstrapped with that name, and the leave-requested sentinel is keyed by
// name alone, so the KCP sends nothing and releases the leaver's hook with a
// warning, as before the node name was recorded (KD-91).
func TestHA_K0sEtcdLeave_HostnameCollisionWithAnUnregisteredMachine(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	name := "ha-k0s-collide"
	other := name + "-kcp-3"
	k, teardown := startK0sLeaveClusterWith(t, name, k0sLeaveOptions{
		unregisteredLeaver: true,
		leaverHostname:     "cp-leaver",
		leaverAnnotations:  map[string]string{renderedNodeNameKey: "cp-leaver"},
		extraMachines: []k0sExtraMachine{{
			name: other, hostname: "cp-leaver",
			annotations: map[string]string{renderedNodeNameKey: other},
		}},
	})
	defer teardown()
	node := k.leaverNode()

	k.setLeaverDeletingReason(g, clusterv1.MachineDeletingWaitingForPreTerminateHookReason)

	g.Eventually(func() map[string]string { return k.leaver(g).Annotations },
		30*time.Second, 500*time.Millisecond).ShouldNot(HaveKey(etcdLeaveHookKey), "abandoned as a collision")
	g.Expect(k.leaveRequestFor(node)).To(BeEmpty(), "nothing is sent under a name another node may have")
	leave, err := k0sEtcdMemberLeave(k.ctx, k.wc, node)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(leave).To(BeFalse())
}

// TestHA_K0sEtcdLeave_EditedHostnameOnARegisteredMachineIsNoCollision: a
// registered keeper's KairosConfig is edited to render the leaver's node
// name. A registered node goes by its Node name, so this is no collision and
// the leave runs. Before the node name was recorded, the edit on any other
// Machine's KairosConfig was read the same way (KD-91).
func TestHA_K0sEtcdLeave_EditedHostnameOnARegisteredMachineIsNoCollision(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	k, teardown := startK0sLeaveClusterWith(t, "ha-k0s-nocollide", k0sLeaveOptions{
		unregisteredLeaver: true,
		leaverHostname:     "cp-leaver",
		leaverAnnotations:  map[string]string{renderedNodeNameKey: "cp-leaver"},
	})
	defer teardown()
	node := k.leaverNode()

	keeper := &bootstrapv1beta2.KairosConfig{}
	g.Expect(k.c.Get(k.ctx, types.NamespacedName{Namespace: k.nsName, Name: k.kcpName + "-1"}, keeper)).To(Succeed())
	keeper.Spec.Hostname = "cp-leaver"
	g.Expect(k.c.Update(k.ctx, keeper)).To(Succeed())

	k.setLeaverDeletingReason(g, clusterv1.MachineDeletingWaitingForPreTerminateHookReason)

	g.Eventually(func() string { return k.leaveRequestFor(node) },
		30*time.Second, 500*time.Millisecond).Should(Equal("leave-requested"))
	touchKCP(g, k.ctx, k.c, k.nsName, k.kcpName)
	g.Consistently(func() map[string]string { return k.leaver(g).Annotations },
		4*time.Second, 500*time.Millisecond).Should(HaveKey(etcdLeaveHookKey), "not abandoned as a collision")
}

// TestHA_K0sEtcdLeave_ChangedNodeNameAfterTheRequestIsHeld: once the leave
// of a Machine without a NodeRef is requested, its recorded node name is
// changed to a keeper's Node name. Nothing is sent under the keeper's name,
// whose node would act on the sentinel by itself, and the hook is kept;
// restoring the name lets the leave carry on (KD-91).
func TestHA_K0sEtcdLeave_ChangedNodeNameAfterTheRequestIsHeld(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	k, teardown := startK0sLeaveClusterWith(t, "ha-k0s-steer", k0sLeaveOptions{
		unregisteredLeaver: true,
		leaverHostname:     "cp-leaver",
		leaverAnnotations:  map[string]string{renderedNodeNameKey: "cp-leaver"},
	})
	defer teardown()
	ctx, c, wc := k.ctx, k.c, k.wc

	k.setLeaverDeletingReason(g, clusterv1.MachineDeletingWaitingForPreTerminateHookReason)
	g.Eventually(func() string { return k.leaveRequestFor("cp-leaver") },
		30*time.Second, 500*time.Millisecond).Should(Equal("leave-requested"))

	setRecord := func(v string) {
		g.Eventually(func() error {
			m := &clusterv1.Machine{}
			if err := c.Get(ctx, k.leaverKey, m); err != nil {
				return err
			}
			m.Annotations[renderedNodeNameKey] = v
			return c.Update(ctx, m)
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
	}
	setRecord("keeper-a")
	// Past the ack timeout too, so a delegation would run if it could.
	k.backdateLeaverAnnotation(g, etcdLeaveRequestedAtKey, 6*time.Minute)
	touchKCP(g, ctx, c, k.nsName, k.kcpName)
	g.Consistently(func() (string, error) {
		if v := k.leaveRequestFor("keeper-a"); v != "" {
			return v, nil
		}
		leave, err := k0sEtcdMemberLeave(ctx, wc, "keeper-a")
		if leave {
			return "spec.leave", err
		}
		return "", err
	}, 6*time.Second, 500*time.Millisecond).Should(BeEmpty(), "nothing is aimed at keeper-a")
	g.Expect(k.leaver(g).Annotations).To(HaveKey(etcdLeaveHookKey), "held, not released")

	// Restored: the leave carries on under its name; k0s is asked to remove
	// the leaver's member, never keeper-a's.
	setRecord("cp-leaver")
	touchKCP(g, ctx, c, k.nsName, k.kcpName)
	g.Eventually(func() (bool, error) { return k0sEtcdMemberLeave(ctx, wc, "cp-leaver") },
		30*time.Second, 500*time.Millisecond).Should(BeTrue())
	leave, err := k0sEtcdMemberLeave(ctx, wc, "keeper-a")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(leave).To(BeFalse())
}

// TestHA_K0sEtcdLeave_LegacyMachinesAreBackfilled: control-plane Machines
// created before the record existed get one on the next reconcile: the Node
// name of a registered Machine, the hostname its KairosConfig renders for
// one without a NodeRef. The leave then uses it after the KairosConfig is
// gone.
func TestHA_K0sEtcdLeave_LegacyMachinesAreBackfilled(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	k, teardown := startK0sLeaveClusterWith(t, "ha-k0s-backfill", k0sLeaveOptions{
		unregisteredLeaver: true,
		keepLeaver:         true,
		leaverHostname:     "cp-leaver",
	})
	defer teardown()
	ctx, c := k.ctx, k.c
	touchKCP(g, ctx, c, k.nsName, k.kcpName)

	recordOf := func(name string) func() (string, error) {
		return func() (string, error) {
			m := &clusterv1.Machine{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: k.nsName, Name: name}, m); err != nil {
				return "", err
			}
			return m.Annotations[renderedNodeNameKey], nil
		}
	}
	g.Eventually(recordOf(k.leaverKey.Name), 30*time.Second, 500*time.Millisecond).Should(Equal("cp-leaver"))
	g.Eventually(recordOf(k.kcpName+"-1"), 10*time.Second, 500*time.Millisecond).Should(Equal("keeper-a"))
	g.Eventually(recordOf(k.kcpName+"-2"), 10*time.Second, 500*time.Millisecond).Should(Equal("keeper-b"))

	// Now the KairosConfig goes, then the Machine.
	k.deleteLeaverKairosConfig(g)
	g.Expect(c.Delete(ctx, k.leaver(g))).To(Succeed())
	k.setLeaverDeletingReason(g, clusterv1.MachineDeletingWaitingForPreTerminateHookReason)

	g.Eventually(func() string { return k.leaveRequestFor("cp-leaver") },
		30*time.Second, 500*time.Millisecond).Should(Equal("leave-requested"))
	g.Expect(k.leaver(g).Annotations).To(HaveKey(etcdLeaveHookKey))
}

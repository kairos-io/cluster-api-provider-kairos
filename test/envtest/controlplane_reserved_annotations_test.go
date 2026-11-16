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

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

const etcdLeaveUnreachableSinceKey = "controlplane.cluster.x-k8s.io/etcd-leave-unreachable-since"

// otherControllersHookKey is a pre-terminate hook another controller owns.
const otherControllersHookKey = clusterv1.PreTerminateDeleteHookAnnotationPrefix + "/backup-agent"

// TestHA_K0sEtcdLeave_TemplateCannotPlantTheLeaveState is KD-92.
// machineTemplate.metadata sets the etcd-leave hook and every piece of the
// leave's bookkeeping, backdated, next to an ordinary annotation and another
// controller's hook. The same bookkeeping, backdated, is also written by hand
// on the hooked Machine, and a stale `left` from an earlier cycle is in the
// leave ConfigMap.
//   - The ordinary annotation and the other hook reach every Machine; none of
//     the leave state does, so the Machines without the hook do not get one,
//     and the bookkeeping written by hand is removed.
//   - When the hooked Machine is deleted, its leave starts from the
//     beginning: nothing while CAPI drains, then a request that overwrites
//     the stale `left`, carries the KCP's own timestamp and goes to the
//     node, not to k0s.
//   - Once the other controller removes its own hook from the deleting
//     Machine, the template does not put it back.
//   - Once the node acks and the KCP removes the etcd-leave hook, that stays
//     removed too, so the deletion can finish.
func TestHA_K0sEtcdLeave_TemplateCannotPlantTheLeaveState(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	k, teardown := startK0sLeaveClusterWith(t, "ha-k0s-reserved", k0sLeaveOptions{keepLeaver: true})
	defer teardown()
	ctx, c := k.ctx, k.c

	backdated := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	planted := map[string]string{
		etcdLeaveHookKey:              "",
		etcdLeaveRequestedAtKey:       backdated,
		etcdLeaveUnreachableSinceKey:  backdated,
		etcdMemberLeaveRequestedAtKey: backdated,
		etcdMemberLeaveMemberIDKey:    "deadbeef",
	}
	// By hand, on the live hooked Machine: the bookkeeping, not the hook.
	g.Eventually(func() error {
		m := k.leaver(g)
		for key, v := range planted {
			if key != etcdLeaveHookKey {
				m.Annotations[key] = v
			}
		}
		return c.Update(ctx, m)
	}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
	// From the template: all of it.
	g.Eventually(func() error {
		kcp := getKCP(g, ctx, c, k.nsName, k.kcpName)
		annotations := map[string]string{"example.com/owner": "platform-team", otherControllersHookKey: ""}
		for key, v := range planted {
			annotations[key] = v
		}
		kcp.Spec.MachineTemplate.Metadata = &clusterv1.ObjectMeta{Annotations: annotations}
		return c.Update(ctx, kcp)
	}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
	// A stale ack from an earlier leave cycle of the same node.
	k.setLeaveValueFor(g, "leaver-node", "left")

	machines := []string{k.kcpName + "-0", k.kcpName + "-1", k.kcpName + "-2"}
	machine := func(name string) (*clusterv1.Machine, error) {
		m := &clusterv1.Machine{}
		return m, c.Get(ctx, types.NamespacedName{Name: name, Namespace: k.nsName}, m)
	}
	for _, name := range machines {
		g.Eventually(func() (map[string]string, error) {
			m, err := machine(name)
			return m.Annotations, err
		}, 30*time.Second, 500*time.Millisecond).Should(And(
			HaveKeyWithValue("example.com/owner", "platform-team"), HaveKeyWithValue(otherControllersHookKey, "")), name)
	}
	// Every leave key on a Machine that the KCP did not stamp itself.
	plantedKeys := func() ([]string, error) {
		var found []string
		for _, name := range machines {
			m, err := machine(name)
			if err != nil {
				return nil, err
			}
			for key := range planted {
				stampedByTheKCP := key == etcdLeaveHookKey && name == machines[0]
				if _, ok := m.Annotations[key]; ok && !stampedByTheKCP {
					found = append(found, name+" "+key)
				}
			}
		}
		return found, nil
	}
	g.Eventually(plantedKeys, 30*time.Second, 500*time.Millisecond).Should(BeEmpty(),
		"the bookkeeping written by hand on a live Machine is removed")
	touchKCP(g, ctx, c, k.nsName, k.kcpName)
	g.Consistently(plantedKeys, 4*time.Second, 500*time.Millisecond).Should(BeEmpty(),
		"the template hooks no Machine and plants none of the leave's bookkeeping")
	g.Expect(k.leaver(g).Annotations).To(HaveKeyWithValue(etcdLeaveHookKey, ""), "the hook the KCP stamped is kept")

	// The hooked Machine is deleted. Its leave starts from the beginning.
	leaver := k.leaver(g)
	g.Expect(c.Delete(ctx, leaver)).To(Succeed())
	k.setLeaverDeletingReason(g, clusterv1.MachineDeletingDrainingNodeReason)
	touchKCP(g, ctx, c, k.nsName, k.kcpName)
	g.Consistently(func() string { return k.leaveRequestFor("leaver-node") },
		4*time.Second, 500*time.Millisecond).Should(Equal("left"), "no leave while CAPI drains the node, and the stale ack is not acted on")
	g.Expect(k.leaver(g).Annotations).To(HaveKey(etcdLeaveHookKey))
	k.setLeaverDeletingReason(g, clusterv1.MachineDeletingWaitingForPreTerminateHookReason)
	g.Eventually(func() string { return k.leaveRequestFor("leaver-node") },
		30*time.Second, 500*time.Millisecond).Should(Equal("leave-requested"), "the first request overwrites the stale ack")
	g.Eventually(func() map[string]string { return k.leaver(g).Annotations },
		10*time.Second, 500*time.Millisecond).Should(HaveKey(etcdLeaveRequestedAtKey))
	got := k.leaver(g)
	requestedAt, err := time.Parse(time.RFC3339, got.Annotations[etcdLeaveRequestedAtKey])
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(requestedAt).To(BeTemporally(">", got.DeletionTimestamp.Add(-time.Second)), "the KCP's own request time, not the template's")
	g.Expect(got.Annotations).ToNot(HaveKey(etcdMemberLeaveRequestedAtKey), "the leave went to the node, not to k0s")
	g.Expect(got.Annotations).To(HaveKey(etcdLeaveHookKey))

	// The other controller is done with the deleting Machine and removes its
	// own hook. The template does not put it back.
	g.Eventually(func() error {
		m := k.leaver(g)
		delete(m.Annotations, otherControllersHookKey)
		return c.Update(ctx, m)
	}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
	touchKCP(g, ctx, c, k.nsName, k.kcpName)
	g.Consistently(func() map[string]string { return k.leaver(g).Annotations },
		4*time.Second, 500*time.Millisecond).ShouldNot(HaveKey(otherControllersHookKey), "another controller's hook stays removed")

	// The node acks; the etcd-leave hook goes and stays gone.
	k.setLeaveValueFor(g, "leaver-node", "left")
	g.Eventually(func() map[string]string { return k.leaver(g).Annotations },
		30*time.Second, 500*time.Millisecond).ShouldNot(HaveKey(etcdLeaveHookKey))
	touchKCP(g, ctx, c, k.nsName, k.kcpName)
	g.Consistently(func() map[string]string { return k.leaver(g).Annotations },
		6*time.Second, 500*time.Millisecond).ShouldNot(HaveKey(etcdLeaveHookKey), "the template never puts the hook back")
	leave, err := k0sEtcdMemberLeave(ctx, k.wc, "leaver-node")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(leave).To(BeFalse(), "no spec.leave: the leave was never delegated")
}

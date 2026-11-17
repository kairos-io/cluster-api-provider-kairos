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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// leaveUntilAck drives the leaver to the pre-terminate stage, waits for the
// leave request, and then checks, while the node has not acked, that the
// hook stays and no leave is handed to k0s, after calling during. It ends
// with the node's ack and the hook's removal.
func leaveUntilAck(t *testing.T, g *WithT, k *k0sLeaveCluster, during func()) {
	t.Helper()
	k.setLeaverDeletingReason(g, clusterv1.MachineDeletingWaitingForPreTerminateHookReason)
	g.Eventually(func() string { return k.leaveRequestFor("leaver-node") },
		30*time.Second, 500*time.Millisecond).Should(Equal("leave-requested"))
	if during != nil {
		during()
	}
	touchKCP(g, k.ctx, k.c, k.nsName, k.kcpName)
	g.Consistently(func() map[string]string { return k.leaver(g).Annotations },
		8*time.Second, 500*time.Millisecond).Should(HaveKey(etcdLeaveHookKey), "no ack yet: the hook stays")
	g.Expect(k.leaveRequestFor("leaver-node")).To(Equal("leave-requested"))
	leave, err := k0sEtcdMemberLeave(k.ctx, k.wc, "leaver-node")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(leave).To(BeFalse(), "not handed to k0s before the timeout")

	k.setLeaveValueFor(g, "leaver-node", "left")
	g.Eventually(func() map[string]string { return k.leaver(g).Annotations },
		30*time.Second, 500*time.Millisecond).ShouldNot(HaveKey(etcdLeaveHookKey), "the ack releases the hook")
}

// TestHA_K0sEtcdLeave_UnreportedMemberWaitsForItsAck is KD-94. A registered
// k0s member whose node never reported into etcd-status is deleted. Its
// absence there does not mean it has left etcd: the hook stays until the
// node acks (or, past the timeout, until k0s reports the member gone).
func TestHA_K0sEtcdLeave_UnreportedMemberWaitsForItsAck(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	k, teardown := startK0sLeaveClusterWith(t, "ha-k0s-leave-unreported", k0sLeaveOptions{leaverUnreported: true})
	defer teardown()
	leaveUntilAck(t, g, k, nil)
}

// TestHA_K0sEtcdLeave_DeletedEtcdStatusDoesNotReleaseTheHook is KD-94. The
// etcd-status Secret is deleted while a reported member's leave is in
// progress. That takes the member's key with it, and is not a leave either.
func TestHA_K0sEtcdLeave_DeletedEtcdStatusDoesNotReleaseTheHook(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	k, teardown := startK0sLeaveClusterWith(t, "ha-k0s-leave-status-gone", k0sLeaveOptions{})
	defer teardown()
	leaveUntilAck(t, g, k, func() {
		es := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: bootstrapv1beta2.EtcdStatusSecretName(k.nsName + "-cluster"), Namespace: k.nsName,
		}}
		g.Expect(k.c.Delete(k.ctx, es)).To(Succeed())
	})
}

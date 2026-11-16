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
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// TestHA_K0sEtcdLeave_GuardsSeeMachinesTheKCPDoesNotOwn is KD-93. The
// Cluster has a control-plane Machine this KCP does not own (left behind by
// another control plane), registered under the Node name the leaving
// Machine, which never got a NodeRef, records. The member of that name may be
// the other Machine's, so the leave is abandoned as a collision: nothing is
// sent under the name, k0s is not asked to remove the member, and the hook
// goes with EtcdMemberLeaveTimedOut.
func TestHA_K0sEtcdLeave_GuardsSeeMachinesTheKCPDoesNotOwn(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	events := &recordingRecorder{}
	name := "ha-k0s-foreign"
	other := name + "-old-cp"
	k, teardown := startK0sLeaveClusterWith(t, name, k0sLeaveOptions{
		unregisteredLeaver: true,
		leaverHostname:     "cp-leaver",
		leaverAnnotations:  map[string]string{renderedNodeNameKey: "cp-leaver"},
		extraMachines:      []k0sExtraMachine{{name: other, nodeRef: "cp-leaver", notOwned: true}},
		recorder:           events,
	})
	defer teardown()
	node := k.leaverNode()

	k.setLeaverDeletingReason(g, clusterv1.MachineDeletingWaitingForPreTerminateHookReason)

	g.Eventually(func() map[string]string { return k.leaver(g).Annotations },
		30*time.Second, 500*time.Millisecond).ShouldNot(HaveKey(etcdLeaveHookKey), "abandoned as a collision")
	g.Expect(k.leaveRequestFor(node)).To(BeEmpty(), "nothing is sent under a name another Machine's node has")
	leave, err := k0sEtcdMemberLeave(k.ctx, k.wc, node)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(leave).To(BeFalse())
	g.Expect(events.all()).To(ContainElement(SatisfyAll(
		HavePrefix("Warning EtcdMemberLeaveTimedOut"),
		ContainSubstring(`Machine "`+other+`" has the same node name`),
	)))
}

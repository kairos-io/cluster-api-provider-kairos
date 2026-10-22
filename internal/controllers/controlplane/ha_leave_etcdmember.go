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

// When the node has not acked its etcd leave by memberLeaveTimeout, the KCP
// asks k0s to finish it (ADR 0014).
//
// k0s keeps one cluster-scoped EtcdMember (etcd.k0sproject.io/v1beta1) per
// controller, named after the node. Setting spec.leave on it makes the k0s
// leader remove that member from etcd; k0s then reports the Joined condition
// as False. That is the answer the node's own ack cannot always deliver: the
// node acks through an apiserver that may itself have lost etcd.
//
// The KCP sets spec.leave only on the EtcdMember of the Machine it is
// removing: the one named after that Machine's Node, only when no other
// Machine reports its peer address, and only while it carries the member ID
// recorded when the KCP first inspected it. It removes the hook once k0s
// reports Joined=False on that same member. Without the EtcdMember API, without an EtcdMember for the
// node, or once etcdMemberLeaveCap has passed, it falls back to the old
// behaviour: a warning, and the hook removed.

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

const (
	// etcdMemberLeaveRequestedAtAnnotation records (RFC3339) when the KCP first
	// tried to hand a timed-out leave to k0s. It anchors etcdMemberLeaveCap, so
	// a Machine whose leave was requested long ago still gets the full wait.
	etcdMemberLeaveRequestedAtAnnotation = "controlplane.cluster.x-k8s.io/etcd-member-leave-requested-at"

	// etcdMemberLeaveMemberIDAnnotation records the status.memberID of the
	// EtcdMember the KCP first inspected for this leave. k0s removes a member
	// by that ID, so the KCP acts on the object only while it still carries it.
	etcdMemberLeaveMemberIDAnnotation = "controlplane.cluster.x-k8s.io/etcd-member-leave-member-id"

	// etcdMemberLeaveCap bounds the wait for k0s to report the member gone.
	// k0s v1.36 waits up to 5 minutes for the member's controller lease to
	// lapse before it removes the member.
	etcdMemberLeaveCap = 10 * time.Minute

	// k0sEtcdMemberJoinedCondition is the condition type k0s sets on an
	// EtcdMember. k0sEtcdMemberNotJoined is its status once the member is not
	// in etcd.
	k0sEtcdMemberJoinedCondition = "Joined"
	k0sEtcdMemberNotJoined       = "False"
)

// etcdMemberGVK is k0s's EtcdMember. It is read and patched as unstructured
// so the KCP takes no Go dependency on k0s.
var etcdMemberGVK = schema.GroupVersionKind{Group: "etcd.k0sproject.io", Version: "v1beta1", Kind: "EtcdMember"}

// delegateMemberLeave runs once the leave of target has timed out without an
// ack. nodeName is the member's name in the workload cluster (leaveNodeName);
// conn and connErr are what connectForLeave returned. The result has the
// meaning reconcileMemberLeave's has.
//
//   - Nothing answered: stamp the start of this phase, and keep the hook
//     until etcdMemberLeaveCap.
//   - No EtcdMember API, or no EtcdMember named after the node: the old
//     timeout path.
//   - The EtcdMember's peer address belongs to another Machine: it is not
//     the leaving member's. Leave it alone, and take the old timeout path.
//   - The EtcdMember has no member ID, or not the one recorded at the first
//     inspection: it may describe another member. Leave it alone, and take
//     the old timeout path.
//   - Otherwise: stamp, record the member ID, set spec.leave if it is not
//     set, and remove the hook once k0s reports Joined=False. After
//     etcdMemberLeaveCap, the old timeout path.
func (r *KairosControlPlaneReconciler) delegateMemberLeave(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, machines []*clusterv1.Machine, target *clusterv1.Machine, nodeName string, conn leaveConn, connErr error) (bool, error) {
	if connErr != nil {
		if err := r.stampAnnotationOnce(ctx, target, etcdMemberLeaveRequestedAtAnnotation); err != nil {
			return false, err
		}
		if etcdMemberLeaveCapped(target) {
			return true, r.abandonMemberLeave(ctx, log, kcp, target, nodeName, "no workload apiserver answered")
		}
		return false, fmt.Errorf("etcd-leave: no workload apiserver answered: %w", connErr)
	}

	member := &unstructured.Unstructured{}
	member.SetGroupVersionKind(etcdMemberGVK)
	if err := conn.client.Get(ctx, client.ObjectKey{Name: nodeName}, member); err != nil {
		switch {
		case meta.IsNoMatchError(err):
			return true, r.abandonMemberLeave(ctx, log, kcp, target, nodeName, "the cluster has no EtcdMember API")
		case apierrors.IsNotFound(err):
			return true, r.abandonMemberLeave(ctx, log, kcp, target, nodeName, "the cluster has no EtcdMember for the node")
		}
		if serr := r.stampAnnotationOnce(ctx, target, etcdMemberLeaveRequestedAtAnnotation); serr != nil {
			return false, serr
		}
		if etcdMemberLeaveCapped(target) {
			return true, r.abandonMemberLeave(ctx, log, kcp, target, nodeName, "its EtcdMember could not be read")
		}
		return false, fmt.Errorf("etcd-leave: read EtcdMember %s via %s: %w", nodeName, conn.via, err)
	}
	if owner := etcdMemberPeerOwner(member, machines, target); owner != "" {
		log.Info("etcd-leave: EtcdMember named after the node has another Machine's peer address; not touching it",
			"machine", target.Name, "node", nodeName, "peerOwner", owner)
		return true, r.abandonMemberLeave(ctx, log, kcp, target, nodeName, fmt.Sprintf("its EtcdMember has the peer address of Machine %q", owner))
	}

	// k0s removes the member by status.memberID. The ID seen at the first
	// inspection is recorded, and the object is acted on only while it still
	// carries it: both the spec.leave write and the Joined=False below are
	// read from this object, and the patch is pinned to its resourceVersion.
	memberID, ok := etcdMemberID(member)
	if !ok {
		log.Info("etcd-leave: EtcdMember named after the node reports no member ID; not touching it",
			"machine", target.Name, "node", nodeName)
		return true, r.abandonMemberLeave(ctx, log, kcp, target, nodeName, "its EtcdMember reports no member ID")
	}
	if recorded, set := target.Annotations[etcdMemberLeaveMemberIDAnnotation]; set && recorded != memberID {
		log.Info("etcd-leave: EtcdMember named after the node has a different member ID than first seen; not touching it",
			"machine", target.Name, "node", nodeName, "recordedMemberID", recorded, "memberID", memberID)
		return true, r.abandonMemberLeave(ctx, log, kcp, target, nodeName, "its EtcdMember now describes a different etcd member")
	}
	if err := r.annotateOnce(ctx, target, map[string]string{
		etcdMemberLeaveRequestedAtAnnotation: time.Now().UTC().Format(time.RFC3339),
		etcdMemberLeaveMemberIDAnnotation:    memberID,
	}); err != nil {
		return false, err
	}
	if leave, _, _ := unstructured.NestedBool(member.Object, "spec", "leave"); !leave {
		base := member.DeepCopy()
		if err := unstructured.SetNestedField(member.Object, true, "spec", "leave"); err != nil {
			return false, err
		}
		// The resourceVersion in the patch makes it apply only to the object
		// inspected above.
		if err := conn.client.Patch(ctx, member, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if etcdMemberLeaveCapped(target) {
				return true, r.abandonMemberLeave(ctx, log, kcp, target, nodeName, "spec.leave could not be set on its EtcdMember")
			}
			return false, fmt.Errorf("etcd-leave: set spec.leave on EtcdMember %s via %s: %w", nodeName, conn.via, err)
		}
		log.Info("etcd-leave: node did not ack; asked k0s to remove the etcd member",
			"machine", target.Name, "node", nodeName, "memberID", memberID, "via", conn.via, "timeout", memberLeaveTimeout)
		if r.Recorder != nil {
			r.Recorder.Eventf(kcp, corev1.EventTypeNormal, "EtcdMemberLeaveDelegated",
				"Control-plane node %q did not acknowledge its etcd leave within %s; asked k0s to remove its etcd member", nodeName, memberLeaveTimeout)
		}
	}

	if etcdMemberJoinedStatus(member) == k0sEtcdMemberNotJoined {
		log.Info("etcd-leave: k0s reports the member is not in etcd; removing hook", "machine", target.Name, "node", nodeName, "via", conn.via)
		return true, r.removeEtcdLeaveHook(ctx, kcp, target)
	}
	if etcdMemberLeaveCapped(target) {
		return true, r.abandonMemberLeave(ctx, log, kcp, target, nodeName, fmt.Sprintf("k0s did not report its member gone within %s", etcdMemberLeaveCap))
	}
	log.Info("etcd-leave: waiting for k0s to remove the etcd member", "machine", target.Name, "node", nodeName, "via", conn.via)
	return false, nil
}

// abandonMemberLeave is the old timeout path: warn that the member may be
// left in etcd, and remove the hook so the delete proceeds. why says what
// the KCP could not do, for the log and the event. nodeName is the member's
// name in the workload cluster (leaveNodeName).
func (r *KairosControlPlaneReconciler) abandonMemberLeave(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, target *clusterv1.Machine, nodeName, why string) error {
	log.Info("etcd-leave: timed out waiting for node ack; removing hook (etcd member may be orphaned)",
		"machine", target.Name, "node", nodeName, "timeout", memberLeaveTimeout, "reason", why)
	if r.Recorder != nil {
		r.Recorder.Eventf(kcp, corev1.EventTypeWarning, "EtcdMemberLeaveTimedOut",
			"Control-plane node %q did not leave etcd within %s and %s; proceeding with deletion — the etcd member may need manual removal", nodeName, memberLeaveTimeout, why)
	}
	return r.removeEtcdLeaveHook(ctx, kcp, target)
}

// etcdMemberLeaveCapped reports whether the KCP first tried to hand the leave
// of m to k0s more than etcdMemberLeaveCap ago. An unparsable stamp never
// expires.
func etcdMemberLeaveCapped(m *clusterv1.Machine) bool {
	at, err := time.Parse(time.RFC3339, m.Annotations[etcdMemberLeaveRequestedAtAnnotation])
	return err == nil && time.Since(at) > etcdMemberLeaveCap
}

// etcdMemberPeerOwner returns the name of a Machine other than target that
// reports the EtcdMember's status.peerAddress, or "" when none does. A peer
// address the leaving Machine does not report is accepted: on CAPK the peer
// address is a bridge IP that KubeVirt reports only with a guest agent.
func etcdMemberPeerOwner(member *unstructured.Unstructured, machines []*clusterv1.Machine, target *clusterv1.Machine) string {
	peer, _, _ := unstructured.NestedString(member.Object, "status", "peerAddress")
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	ip := net.ParseIP(peer)
	if ip == nil {
		return ""
	}
	for _, m := range machines {
		if m.Name == target.Name {
			continue
		}
		for _, a := range m.Status.Addresses {
			if other := net.ParseIP(a.Address); other != nil && other.Equal(ip) {
				return m.Name
			}
		}
	}
	return ""
}

// etcdMemberID returns the EtcdMember's status.memberID in canonical form
// (lowercase hex, as k0s writes it), and false when it is missing or is not a
// hexadecimal etcd member ID.
func etcdMemberID(member *unstructured.Unstructured) (string, bool) {
	raw, _, _ := unstructured.NestedString(member.Object, "status", "memberID")
	return canonicalMemberID(raw)
}

// canonicalMemberID returns raw as a canonical etcd member ID (lowercase
// hex), and false when it is empty, zero or not hexadecimal.
func canonicalMemberID(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	id, err := strconv.ParseUint(raw, 16, 64)
	if err != nil || id == 0 {
		return "", false
	}
	return strconv.FormatUint(id, 16), true
}

// etcdMemberJoinedStatus returns the status of the EtcdMember's Joined
// condition, or "" when k0s has not reported one.
func etcdMemberJoinedStatus(member *unstructured.Unstructured) string {
	conds, _, _ := unstructured.NestedSlice(member.Object, "status", "conditions")
	for _, c := range conds {
		cond, ok := c.(map[string]interface{})
		if !ok || cond["type"] != k0sEtcdMemberJoinedCondition {
			continue
		}
		status, _ := cond["status"].(string)
		return status
	}
	return ""
}

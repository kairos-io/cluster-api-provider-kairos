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

// A k0s control-plane Machine without a NodeRef gets a real etcd leave (ADR
// 0015 PR-3).
//
// etcd membership comes before the NodeRef. A joiner adds itself to etcd and
// k0s creates its EtcdMember as soon as the controller starts; the kubelet
// registers the Node later, and the Machine gets its NodeRef only once the
// node's providerID is set. On 2026-10-17 a joiner that never got a NodeRef
// was deleted, its hook was released with no leave, and its etcd member was
// orphaned. The next joiner reused its name, failed to join because of the
// orphan, and k0s then bootstrapped a second cluster.
//
// So the leave is keyed on the node's rendered name and goes through the same
// handshake and the same k0s delegation as any other member. The hook is
// released before that only on positive evidence read from the pinned
// cluster: no EtcdMember of that name, or one k0s reports Joined=False.

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// leaveUnreachable handles a pass, before any leave was requested, on which
// the KCP could not get a verified answer from the workload cluster. It
// stamps etcdLeaveUnreachableSinceAnnotation once and keeps the hook, until
// leaveUnreachableBound has passed since that stamp; then it takes the
// warning path with why. cause is returned (wrapped) while the hook is kept,
// so the reconcile requeues with backoff.
//
// This makes ADR 0014's ~15-minute bound hold in the phase before the first
// request too (ADR 0015 GT-15, OQ-5), for Machines with and without a NodeRef.
// A never-booted VM in a cluster nobody can reach is released after it, so it
// never wedges deletion.
func (r *KairosControlPlaneReconciler) leaveUnreachable(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, target *clusterv1.Machine, nodeName, why string, cause error) (bool, error) {
	if err := r.stampAnnotationOnce(ctx, target, etcdLeaveUnreachableSinceAnnotation); err != nil {
		return false, err
	}
	if leaveUnreachableExpired(target) {
		return true, r.abandonMemberLeave(ctx, log, kcp, target, nodeName,
			fmt.Sprintf("%s for %s before a leave could be requested", why, leaveUnreachableBound))
	}
	return false, fmt.Errorf("etcd-leave: %s: %w", why, cause)
}

// leaveUnreachableExpired reports whether m was first found unreachable,
// before any leave was requested, more than leaveUnreachableBound ago. An
// unparsable stamp never expires.
func leaveUnreachableExpired(m *clusterv1.Machine) bool {
	at, err := time.Parse(time.RFC3339, m.Annotations[etcdLeaveUnreachableSinceAnnotation])
	return err == nil && time.Since(at) > leaveUnreachableBound
}

// inspectUnregisteredLeave decides, before the first leave request, what to
// do with a Machine without a NodeRef. conn is a verified connection to the
// pinned cluster. proceed=true means run the handshake under nodeName;
// otherwise done and err are reconcileMemberLeave's result.
//
//   - No EtcdMember API: the warning path (as ADR 0014's delegation does).
//   - No EtcdMember named nodeName: no member ever started under that name in
//     the pinned cluster. Release the hook. (An unstarted member, added but
//     never run, has no EtcdMember and stays invisible here: KD-78.)
//   - It cannot be read: as if nothing answered (leaveUnreachable).
//   - Its peer address is one another Machine reports, or another Machine
//     renders the same node name: it may be that Machine's member. The
//     warning path, nothing written.
//   - k0s reports it Joined=False: it is not in etcd. Release the hook.
//   - Otherwise it may be a live member: record its member ID, so a later
//     delegation acts on this member only, and proceed.
func (r *KairosControlPlaneReconciler) inspectUnregisteredLeave(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, machines []*clusterv1.Machine, target *clusterv1.Machine, nodeName string, conn leaveConn) (proceed, done bool, err error) {
	// The lookup is only evidence when the rendered hostname is exactly the
	// name the node goes by. One that is not a lowercase DNS-1123 label, or
	// carries "{{" that Kairos expands at boot, may name another member on the
	// node (an old KairosConfig the webhook check predates). Then "not found"
	// proves nothing: hold for the bound, with no read and no write.
	raw, err := r.renderedHostname(ctx, target)
	if err != nil {
		return false, false, err
	}
	if !bootstrapv1beta2.ValidNodeHostname(raw) {
		log.Info("etcd-leave: the Machine has no NodeRef and its rendered hostname cannot be matched to an EtcdMember; holding until the bound",
			"machine", target.Name, "reasons", bootstrapv1beta2.NodeHostnameErrors(raw))
		done, err := r.leaveUnreachable(ctx, log, kcp, target, nodeName, "its rendered hostname is not a lowercase DNS-1123 label",
			fmt.Errorf("rendered hostname of Machine %s is not a lowercase DNS-1123 label", target.Name))
		return false, done, err
	}

	member := &unstructured.Unstructured{}
	member.SetGroupVersionKind(etcdMemberGVK)
	if gerr := conn.client.Get(ctx, client.ObjectKey{Name: nodeName}, member); gerr != nil {
		switch {
		case meta.IsNoMatchError(gerr):
			return false, true, r.abandonMemberLeave(ctx, log, kcp, target, nodeName, "the cluster has no EtcdMember API")
		case apierrors.IsNotFound(gerr):
			log.Info("etcd-leave: the Machine has no NodeRef and the pinned cluster has no EtcdMember of its node name; nothing to leave, removing hook",
				"machine", target.Name, "node", nodeName, "via", conn.via)
			return false, true, r.removeEtcdLeaveHook(ctx, kcp, target)
		}
		done, err := r.leaveUnreachable(ctx, log, kcp, target, nodeName, "its EtcdMember could not be read",
			fmt.Errorf("read EtcdMember %s via %s: %w", nodeName, conn.via, gerr))
		return false, done, err
	}

	if owner := etcdMemberPeerOwner(member, machines, target); owner != "" {
		log.Info("etcd-leave: EtcdMember named after the unregistered node has another Machine's peer address; not touching it",
			"machine", target.Name, "node", nodeName, "peerOwner", owner)
		return false, true, r.abandonMemberLeave(ctx, log, kcp, target, nodeName, fmt.Sprintf("its EtcdMember has the peer address of Machine %q", owner))
	}
	other, err := r.otherMachineWithNodeName(ctx, machines, target, nodeName)
	if err != nil {
		return false, false, err
	}
	if other != "" {
		log.Info("etcd-leave: another Machine renders the same node name; not touching its EtcdMember",
			"machine", target.Name, "node", nodeName, "other", other)
		return false, true, r.abandonMemberLeave(ctx, log, kcp, target, nodeName, fmt.Sprintf("Machine %q has the same node name", other))
	}

	if etcdMemberJoinedStatus(member) == k0sEtcdMemberNotJoined {
		log.Info("etcd-leave: the Machine has no NodeRef and k0s reports its member is not in etcd; removing hook",
			"machine", target.Name, "node", nodeName, "via", conn.via)
		return false, true, r.removeEtcdLeaveHook(ctx, kcp, target)
	}

	// Pin the member now, at the first inspection: a delegation later acts on
	// the EtcdMember only while it still carries this ID. Without an ID yet
	// (k0s has not written status), the delegation inspects it then.
	if id, ok := etcdMemberID(member); ok {
		if err := r.annotateOnce(ctx, target, map[string]string{etcdMemberLeaveMemberIDAnnotation: id}); err != nil {
			return false, false, err
		}
	}
	log.Info("etcd-leave: the Machine has no NodeRef but its node is an etcd member; leaving it",
		"machine", target.Name, "node", nodeName, "via", conn.via)
	return true, false, nil
}

// otherMachineWithNodeName returns the name of a Machine in machines, other
// than target, whose member goes by nodeName (leaveNodeName), or "" when
// there is none. A spec.hostname shared through the KairosConfigTemplate
// gives every control-plane node the same name, and then an EtcdMember of
// that name cannot be told apart.
func (r *KairosControlPlaneReconciler) otherMachineWithNodeName(ctx context.Context, machines []*clusterv1.Machine, target *clusterv1.Machine, nodeName string) (string, error) {
	for _, m := range machines {
		if m.Name == target.Name {
			continue
		}
		name, err := r.leaveNodeName(ctx, m)
		if err != nil {
			return "", err
		}
		if name == nodeName {
			return m.Name, nil
		}
	}
	return "", nil
}

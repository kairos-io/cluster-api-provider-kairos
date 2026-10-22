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

// Orphan etcd-member cleanup (ADR 0015 PR-3, maintainer decision).
//
// An etcd member whose control-plane Machine is gone costs a voter, and a
// later joiner under the same name splits a k0s cluster (ADR 0015 GT-22).
// Upstream's kubeadm control plane removes such members continuously
// (reconcileEtcdMembers); this is the same reconciliation for k0s, done
// through k0s's EtcdMember spec.leave because the KCP has no etcd client
// (KD-10).
//
// The rules mirror upstream's, made stricter where this KCP sees less:
//   - Only on a connection verified against the pinned identity, never while
//     frozen. "This member has no Machine" is an absence, and an absence read
//     from another cluster is how the 2026-10-17 incident deleted the
//     original members.
//   - Only in a steady state: replicas as desired, no rollout, no Machine
//     being deleted, and every Machine registered with a healthy voting etcd
//     report. Upstream also waits while any Machine is still provisioning,
//     because the unexpected member may be that Machine's.
//   - A member is expected when its name is any control-plane Machine's
//     node name (leaveNodeName: the Node name, or the rendered hostname), or
//     its peer address is one any control-plane Machine reports. Only a
//     member that matches neither is an orphan. On CAPK the peer address is
//     on a bridge NIC no Machine reports, so the name alone decides there.
//   - Only when quorum allows: after the removal, the healthy voting members
//     (registered Machines whose node reports healthy and voting) must still
//     be a majority of the members left, and of the desired replicas.
//   - One at a time: the member being removed is recorded on the KCP
//     (etcdOrphanLeaveAnnotation, with its member ID), and nothing else
//     changes membership until k0s reports it gone or etcdMemberLeaveCap
//     passes.
//   - Member-ID pinned, as ADR 0014's delegation is: spec.leave is set only
//     while the EtcdMember carries the member ID first seen, with the patch
//     pinned to the inspected resourceVersion.
//
// It cannot see an unstarted member (added to etcd, never run): k0s creates
// no EtcdMember for it (KD-78).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

const (
	// etcdOrphanLeaveAnnotation records, on the KairosControlPlane, the orphan
	// etcd member the KCP asked k0s to remove and has not seen gone yet:
	// "<EtcdMember name> <member ID> <RFC3339 time of the request>". While it
	// is set, no other orphan is removed and no control-plane Machine is
	// created or deleted.
	etcdOrphanLeaveAnnotation = "controlplane.cluster.x-k8s.io/etcd-orphan-member-leave"

	// etcdOrphanCheckInterval bounds how often the KCP lists the EtcdMembers
	// when no orphan removal is in progress.
	etcdOrphanCheckInterval = time.Minute

	// orphanLeaveClockSkew is how far in the future a recorded removal time
	// may be and still be accepted.
	orphanLeaveClockSkew = 5 * time.Minute
)

// orphanLeave is a parsed etcdOrphanLeaveAnnotation.
type orphanLeave struct {
	name     string
	memberID string
	at       time.Time
}

func (o orphanLeave) String() string {
	return o.name + " " + o.memberID + " " + o.at.UTC().Format(time.RFC3339)
}

// parseOrphanLeave parses an etcdOrphanLeaveAnnotation value. Every part is
// validated, so nothing else is ever acted on or copied into an event.
func parseOrphanLeave(v string) (orphanLeave, bool) {
	parts := strings.Fields(v)
	if len(parts) != 3 || !validEtcdMemberName(parts[0]) {
		return orphanLeave{}, false
	}
	id, ok := canonicalMemberID(parts[1])
	if !ok || id != parts[1] {
		return orphanLeave{}, false
	}
	at, err := time.Parse(time.RFC3339, parts[2])
	if err != nil {
		return orphanLeave{}, false
	}
	// A record from the future would stretch etcdMemberLeaveCap, and with it
	// the hold on every membership change, past its bound.
	if at.After(time.Now().Add(orphanLeaveClockSkew)) {
		return orphanLeave{}, false
	}
	return orphanLeave{name: parts[0], memberID: id, at: at}, true
}

// validEtcdMemberName reports whether name can be an EtcdMember name (a
// DNS-1123 subdomain, as every object name is). A name a workload apiserver
// returns is checked before it reaches an annotation, a log or an event.
func validEtcdMemberName(name string) bool {
	return len(validation.IsDNS1123Subdomain(name)) == 0
}

// reconcileOrphanEtcdMembers removes, one at a time, k0s etcd members that no
// control-plane Machine accounts for. It runs on every reconcile, after the
// etcd-leave sweep and before any scale or rollout decision. machines are the
// KCP's own control-plane Machines. hold=true means a removal is in progress
// and the caller returns res; err is an errWorkloadIdentityChanged error when
// a connection serves another cluster, or a management-cluster error.
//
// A removal already recorded is followed to its end, and holds every other
// membership change, whatever the distribution and replica count are now: a
// scale to one replica in the middle of it must not let a delete run while
// k0s is still removing a member. New removals start only on k0s HA.
func (r *KairosControlPlaneReconciler) reconcileOrphanEtcdMembers(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine, desiredReplicas int32) (res ctrl.Result, hold bool, err error) {
	raw, recorded := kcp.Annotations[etcdOrphanLeaveAnnotation]
	if !recorded && (distributionOf(kcp) != "k0s" || desiredReplicas <= 1) {
		return ctrl.Result{}, false, nil
	}
	if err := errIfIdentityFrozen(kcp, "remove an orphan etcd member"); err != nil {
		return ctrl.Result{}, false, err
	}
	all, err := r.clusterControlPlaneMachines(ctx, kcp, cluster)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	if recorded {
		rec, valid := parseOrphanLeave(raw)
		if !valid {
			log.Info("Removing an unparsable orphan etcd-member record", "annotation", etcdOrphanLeaveAnnotation)
			return ctrl.Result{}, false, r.patchKCPAnnotations(ctx, kcp, nil, etcdOrphanLeaveAnnotation)
		}
		return r.progressOrphanLeave(ctx, log, kcp, cluster, all, desiredReplicas, rec)
	}

	if why := orphanCleanupBlocked(kcp, machines, all, desiredReplicas); why != "" {
		log.V(4).Info("Not checking for orphan etcd members", "reason", why)
		return ctrl.Result{}, false, nil
	}
	if !r.orphanCheckDue(kcp) {
		return ctrl.Result{}, false, nil
	}
	// removeOneOrphan marks the check done itself, once it has an answer to
	// act on: a pass that stops early (a member not reported healthy yet, no
	// verified apiserver, a failed list) retries on the next reconcile instead
	// of waiting out etcdOrphanCheckInterval.
	return r.removeOneOrphan(ctx, log, kcp, cluster, all, desiredReplicas)
}

// orphanCleanupBlocked returns why the control plane is not in the steady
// state orphan cleanup needs, or "" when it is. owned are the KCP's
// control-plane Machines, all those of the cluster whoever owns them: a
// Machine of any owner that is being deleted or has no Node yet may be the
// one a member that looks orphaned belongs to.
func orphanCleanupBlocked(kcp *controlplanev1beta2.KairosControlPlane, owned, all []*clusterv1.Machine, desiredReplicas int32) string {
	if !kcp.DeletionTimestamp.IsZero() {
		return "the control plane is being deleted"
	}
	if int32(len(owned)) != desiredReplicas {
		return fmt.Sprintf("scaling: %d of %d control-plane Machines", len(owned), desiredReplicas)
	}
	for _, m := range owned {
		if m.Spec.Version == "" || m.Spec.Version != kcp.Spec.Version {
			return fmt.Sprintf("rolling out: Machine %s does not run %s", m.Name, kcp.Spec.Version)
		}
	}
	for _, m := range all {
		switch {
		case !m.DeletionTimestamp.IsZero():
			return fmt.Sprintf("Machine %s is being deleted", m.Name)
		case !m.Status.NodeRef.IsDefined():
			return fmt.Sprintf("Machine %s has not registered a Node", m.Name)
		}
	}
	return ""
}

// clusterControlPlaneMachines lists every control-plane Machine of cluster,
// whoever owns it.
func (r *KairosControlPlaneReconciler) clusterControlPlaneMachines(ctx context.Context, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) ([]*clusterv1.Machine, error) {
	list := &clusterv1.MachineList{}
	selector := labels.SelectorFromSet(map[string]string{
		clusterv1.ClusterNameLabel:         cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	})
	if err := r.List(ctx, list, client.InNamespace(kcp.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list control-plane Machines: %w", err)
	}
	out := make([]*clusterv1.Machine, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// unmatchedMachines returns the Machines no joined EtcdMember can be paired
// with, each pairing using a distinct member: first by Node name, then, for
// the Machines left, by a peer address the Machine reports.
//
// Orphan cleanup proceeds only when it returns none. A member whose name has
// drifted from its Node name (and whose peer address no Machine reports, as
// on CAPK) would otherwise look like an orphan, and the quorum check, built
// from the same name match, would count one healthy member too few and still
// pass: the KCP would remove a live voter.
func unmatchedMachines(machines []*clusterv1.Machine, joined []*unstructured.Unstructured) []string {
	used := map[string]bool{}
	matched := map[string]bool{}
	byName := map[string]*unstructured.Unstructured{}
	for _, member := range joined {
		byName[member.GetName()] = member
	}
	for _, m := range machines {
		if !m.Status.NodeRef.IsDefined() {
			continue
		}
		if member, ok := byName[m.Status.NodeRef.Name]; ok && !used[member.GetName()] {
			used[member.GetName()] = true
			matched[m.Name] = true
		}
	}
	var out []string
	for _, m := range machines {
		if matched[m.Name] {
			continue
		}
		addrs := map[string]bool{}
		for _, a := range m.Status.Addresses {
			if ip := net.ParseIP(a.Address); ip != nil {
				addrs[ip.String()] = true
			}
		}
		found := false
		for _, member := range joined {
			if used[member.GetName()] {
				continue
			}
			if ip := etcdMemberPeerIP(member); ip != "" && addrs[ip] {
				used[member.GetName()] = true
				found = true
				break
			}
		}
		if !found {
			out = append(out, m.Name)
		}
	}
	return out
}

// etcdMemberPeerIP returns the IP of the EtcdMember's status.peerAddress in
// canonical form, or "" when it has none.
func etcdMemberPeerIP(member *unstructured.Unstructured) string {
	peer, _, _ := unstructured.NestedString(member.Object, "status", "peerAddress")
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	if ip := net.ParseIP(peer); ip != nil {
		return ip.String()
	}
	return ""
}

// removeOneOrphan finds the orphan etcd members and, when it is safe, asks
// k0s to remove the first of them. machines are every control-plane Machine
// of the cluster.
func (r *KairosControlPlaneReconciler) removeOneOrphan(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine, desiredReplicas int32) (ctrl.Result, bool, error) {
	// Every member must report healthy and voting: a joiner that has not
	// finished is pending, and its member may look like an orphan.
	status, err := r.readEtcdStatus(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	for _, m := range machines {
		if st, ok := status[m.Status.NodeRef.Name]; !ok || !st.Healthy || !st.Voting {
			log.V(4).Info("Not checking for orphan etcd members: a member has not reported healthy and voting", "machine", m.Name)
			return ctrl.Result{}, false, nil
		}
	}

	conn, err := r.connectForOrphans(ctx, log, kcp, cluster, machines)
	if err != nil {
		if errors.Is(err, errWorkloadIdentityChanged) {
			return ctrl.Result{}, false, err
		}
		log.V(1).Info("Not checking for orphan etcd members: no verified workload apiserver answered", "reason", err.Error())
		return ctrl.Result{}, false, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(etcdMemberGVK.GroupVersion().WithKind(etcdMemberGVK.Kind + "List"))
	if err := conn.client.List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) {
			// A definite answer: this cluster has no EtcdMember API.
			r.markOrphanChecked(kcp)
		} else {
			log.V(1).Info("Not checking for orphan etcd members: the EtcdMembers could not be listed", "via", conn.via, "reason", err.Error())
		}
		return ctrl.Result{}, false, nil
	}
	// The members are listed: from here on every outcome (nothing to do,
	// blocked, or a removal started) is an evaluation, and the next one waits
	// for etcdOrphanCheckInterval.
	r.markOrphanChecked(kcp)

	expected, err := r.expectedEtcdMembers(ctx, machines)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	var joined, orphans []*unstructured.Unstructured
	for i := range list.Items {
		member := &list.Items[i]
		if etcdMemberJoinedStatus(member) == k0sEtcdMemberNotJoined {
			continue // not in etcd; k0s never deletes the object (GT-7)
		}
		joined = append(joined, member)
		if !expected.has(member) {
			orphans = append(orphans, member)
		}
	}
	if len(orphans) == 0 {
		return ctrl.Result{}, false, nil
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].GetName() < orphans[j].GetName() })

	var target *unstructured.Unstructured
	var targetID string
	var names []string
	for _, o := range orphans {
		if !validEtcdMemberName(o.GetName()) {
			log.Info("Ignoring an EtcdMember with an invalid name", "via", conn.via)
			continue
		}
		names = append(names, o.GetName())
		id, ok := etcdMemberID(o)
		if !ok {
			log.Info("Orphan etcd member reports no member ID; not touching it", "member", o.GetName())
			continue
		}
		if target == nil {
			target, targetID = o, id
		}
	}
	if target == nil {
		return ctrl.Result{}, false, nil
	}

	if r.orphanRemovalBlocked(log, kcp, machines, joined, target, status, desiredReplicas) {
		return ctrl.Result{}, false, nil
	}

	// Record first, so a crash after the patch is still tracked.
	rec := orphanLeave{name: target.GetName(), memberID: targetID, at: time.Now()}
	if err := r.patchKCPAnnotations(ctx, kcp, map[string]string{etcdOrphanLeaveAnnotation: rec.String()}); err != nil {
		// Nothing was asked of k0s. Give the check slot back, so the next
		// reconcile (on a fresh KCP) tries again at once instead of after
		// etcdOrphanCheckInterval with no record to follow.
		r.forgetOrphanCheck(kcp)
		if apierrors.IsConflict(err) {
			log.Info("The KairosControlPlane changed while the orphan etcd member removal was being recorded; retrying", "member", rec.name)
			return ctrl.Result{RequeueAfter: time.Second}, true, nil
		}
		return ctrl.Result{}, false, fmt.Errorf("record the orphan etcd member removal: %w", err)
	}
	log.Info("Asking k0s to remove an etcd member that has no control-plane Machine",
		"member", rec.name, "memberID", rec.memberID, "orphans", names, "via", conn.via)
	if err := setEtcdMemberLeave(ctx, conn.client, target, true); err != nil {
		log.Info("Could not set spec.leave on the orphan etcd member; retrying", "member", rec.name, "reason", err.Error())
		return ctrl.Result{RequeueAfter: joinerGateRequeueAfter}, true, nil
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(kcp, corev1.EventTypeNormal, "EtcdOrphanMemberLeaveRequested",
			"etcd member %q (ID %s) has no control-plane Machine; asked k0s to remove it", rec.name, rec.memberID)
	}
	return ctrl.Result{RequeueAfter: joinerGateRequeueAfter}, true, nil
}

// progressOrphanLeave follows the recorded orphan removal until k0s reports
// the member gone, the EtcdMember describes another member, or
// etcdMemberLeaveCap passes. It holds every other membership change while it
// runs.
func (r *KairosControlPlaneReconciler) progressOrphanLeave(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine, desiredReplicas int32, rec orphanLeave) (ctrl.Result, bool, error) {
	wait := ctrl.Result{RequeueAfter: joinerGateRequeueAfter}
	finish := func(eventType, reason, msg string, args ...interface{}) (ctrl.Result, bool, error) {
		if err := r.patchKCPAnnotations(ctx, kcp, nil, etcdOrphanLeaveAnnotation); err != nil {
			if apierrors.IsConflict(err) {
				return wait, true, nil
			}
			return ctrl.Result{}, false, fmt.Errorf("clear the orphan etcd member record: %w", err)
		}
		log.Info("Orphan etcd member removal finished", "member", rec.name, "memberID", rec.memberID, "reason", reason)
		if r.Recorder != nil {
			r.Recorder.Eventf(kcp, eventType, reason, msg, args...)
		}
		return ctrl.Result{}, false, nil
	}
	capped := time.Since(rec.at) > etcdMemberLeaveCap
	timedOut := func() (ctrl.Result, bool, error) {
		return finish(corev1.EventTypeWarning, "EtcdOrphanMemberRemovalTimedOut",
			"k0s did not report etcd member %q (ID %s) removed within %s; it may need removing by hand", rec.name, rec.memberID, etcdMemberLeaveCap)
	}

	conn, err := r.connectForOrphans(ctx, log, kcp, cluster, machines)
	if err != nil {
		if errors.Is(err, errWorkloadIdentityChanged) {
			return ctrl.Result{}, false, err
		}
		if capped {
			return timedOut()
		}
		log.Info("Waiting to follow the orphan etcd member removal: no verified workload apiserver answered", "member", rec.name, "reason", err.Error())
		return wait, true, nil
	}
	member := &unstructured.Unstructured{}
	member.SetGroupVersionKind(etcdMemberGVK)
	if err := conn.client.Get(ctx, client.ObjectKey{Name: rec.name}, member); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return finish(corev1.EventTypeNormal, "EtcdOrphanMemberRemoved", "EtcdMember %q is gone", rec.name)
		case meta.IsNoMatchError(err):
			return finish(corev1.EventTypeWarning, "EtcdOrphanMemberRemovalTimedOut", "the cluster no longer serves the EtcdMember API; etcd member %q may need removing by hand", rec.name)
		case capped:
			return timedOut()
		}
		log.Info("Waiting to follow the orphan etcd member removal: its EtcdMember could not be read", "member", rec.name, "reason", err.Error())
		return wait, true, nil
	}

	if id, ok := etcdMemberID(member); !ok || id != rec.memberID {
		return finish(corev1.EventTypeWarning, "EtcdOrphanMemberChanged",
			"EtcdMember %q no longer describes etcd member %s; left it alone", rec.name, rec.memberID)
	}
	if etcdMemberJoinedStatus(member) == k0sEtcdMemberNotJoined {
		return finish(corev1.EventTypeNormal, "EtcdOrphanMemberRemoved",
			"k0s removed etcd member %q (ID %s), which had no control-plane Machine", rec.name, rec.memberID)
	}
	expected, err := r.expectedEtcdMembers(ctx, machines)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	if expected.has(member) {
		// A control-plane Machine now accounts for it: take the request back.
		if err := setEtcdMemberLeave(ctx, conn.client, member, false); err != nil {
			log.Info("Could not take back spec.leave on an etcd member that now has a Machine; retrying", "member", rec.name, "reason", err.Error())
			return wait, true, nil
		}
		return finish(corev1.EventTypeWarning, "EtcdOrphanMemberChanged",
			"etcd member %q now has a control-plane Machine; took back the request to remove it", rec.name)
	}
	if leave, _, _ := unstructured.NestedBool(member.Object, "spec", "leave"); !leave {
		// The record exists but spec.leave was never written (a crash between
		// the two writes, or a failed patch): the cluster may have changed
		// since the record. Check again, as for a first request, before this
		// first write.
		joined, lerr := listJoinedEtcdMembers(ctx, conn.client)
		status, serr := r.readEtcdStatus(ctx, cluster)
		switch {
		case deletingMachine(machines) != "":
			// As for a first request: never start a removal while a
			// control-plane Machine is being deleted, which has its own leave
			// to make.
			if capped {
				return timedOut()
			}
			log.Info("Waiting to request the recorded orphan etcd member removal: a control-plane Machine is being deleted",
				"member", rec.name, "machine", deletingMachine(machines))
			return wait, true, nil
		case lerr != nil || serr != nil:
			if capped {
				return timedOut()
			}
			log.Info("Waiting to request the recorded orphan etcd member removal: cannot re-check its safety", "member", rec.name)
			return wait, true, nil
		case r.orphanRemovalBlocked(log, kcp, machines, joined, member, status, desiredReplicas):
			if capped {
				return timedOut()
			}
			return wait, true, nil
		}
		if err := setEtcdMemberLeave(ctx, conn.client, member, true); err != nil {
			log.Info("Could not set spec.leave on the orphan etcd member; retrying", "member", rec.name, "reason", err.Error())
		}
	}
	if capped {
		return timedOut()
	}
	log.Info("Waiting for k0s to remove the orphan etcd member", "member", rec.name, "memberID", rec.memberID, "via", conn.via)
	return wait, true, nil
}

// orphanRemovalBlocked reports whether removing target from joined is unsafe
// now, and if so emits EtcdOrphanMemberRemovalBlocked saying why. Both the
// first request and a recorded removal about to write its first spec.leave
// run it.
//
//   - Every control-plane Machine must own a distinct joined member
//     (unmatchedMachines); otherwise a member whose name drifted could be a
//     live voter.
//   - Quorum: the healthy voting members left must be a majority of the
//     members left and of the desired replicas.
func (r *KairosControlPlaneReconciler) orphanRemovalBlocked(log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, machines []*clusterv1.Machine, joined []*unstructured.Unstructured, target *unstructured.Unstructured, status map[string]etcdMemberStatus, desiredReplicas int32) bool {
	if unmatched := unmatchedMachines(machines, joined); len(unmatched) > 0 {
		log.Info("Not removing orphan etcd members: some Machines have no EtcdMember by Node name or peer address", "machines", unmatched)
		if r.Recorder != nil {
			r.Recorder.Eventf(kcp, corev1.EventTypeWarning, "EtcdOrphanMemberRemovalBlocked",
				"etcd member naming does not match the control-plane Machines: no EtcdMember matches Machine(s) %s by Node name or peer address, so no member is removed; compare `k0s etcd member-list` with the Machines",
				strings.Join(unmatched, ", "))
		}
		return true
	}
	healthy := healthyVotingMembers(joined, target, machines, status)
	left := len(joined) - 1
	need := left/2 + 1
	if q := int(desiredReplicas/2 + 1); q > need {
		need = q
	}
	if healthy < need {
		log.Info("Not removing an orphan etcd member: quorum would not hold", "member", target.GetName(), "healthyLeft", healthy, "need", need)
		if r.Recorder != nil {
			r.Recorder.Eventf(kcp, corev1.EventTypeWarning, "EtcdOrphanMemberRemovalBlocked",
				"etcd member %q has no control-plane Machine, but removing it would leave %d healthy voting members of %d, below the %d needed; remove it by hand once the cluster is healthy",
				target.GetName(), healthy, left, need)
		}
		return true
	}
	return false
}

// deletingMachine returns the name of the first of machines being deleted,
// or "" when none is.
func deletingMachine(machines []*clusterv1.Machine) string {
	for _, m := range machines {
		if !m.DeletionTimestamp.IsZero() {
			return m.Name
		}
	}
	return ""
}

// listJoinedEtcdMembers lists the EtcdMembers through wc and returns those k0s
// does not report Joined=False.
func listJoinedEtcdMembers(ctx context.Context, wc client.Client) ([]*unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(etcdMemberGVK.GroupVersion().WithKind(etcdMemberGVK.Kind + "List"))
	if err := wc.List(ctx, list); err != nil {
		return nil, err
	}
	var joined []*unstructured.Unstructured
	for i := range list.Items {
		if etcdMemberJoinedStatus(&list.Items[i]) != k0sEtcdMemberNotJoined {
			joined = append(joined, &list.Items[i])
		}
	}
	return joined, nil
}

// setEtcdMemberLeave sets spec.leave on member to leave with a merge patch
// pinned to member's resourceVersion, so it applies only to the object that
// was inspected.
func setEtcdMemberLeave(ctx context.Context, wc client.Client, member *unstructured.Unstructured, leave bool) error {
	base := member.DeepCopy()
	patched := member.DeepCopy()
	if err := unstructured.SetNestedField(patched.Object, leave, "spec", "leave"); err != nil {
		return err
	}
	return wc.Patch(ctx, patched, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// expectedMembers is what the control-plane Machines account for: their node
// names and every address they report.
type expectedMembers struct {
	names map[string]bool
	addrs map[string]bool
}

// has reports whether a control-plane Machine accounts for member, by name
// or by peer address.
func (e expectedMembers) has(member *unstructured.Unstructured) bool {
	if e.names[member.GetName()] {
		return true
	}
	if ip := etcdMemberPeerIP(member); ip != "" {
		return e.addrs[ip]
	}
	return false
}

// expectedEtcdMembers collects the node names and addresses of machines,
// which are every control-plane Machine of the cluster, whoever owns it
// (clusterControlPlaneMachines): a Machine this KCP does not own still has a
// member that must not be removed.
func (r *KairosControlPlaneReconciler) expectedEtcdMembers(ctx context.Context, machines []*clusterv1.Machine) (expectedMembers, error) {
	e := expectedMembers{names: map[string]bool{}, addrs: map[string]bool{}}
	for _, m := range machines {
		rendered, err := r.renderedNodeName(ctx, m)
		if err != nil {
			return expectedMembers{}, err
		}
		e.names[rendered] = true
		if m.Status.NodeRef.IsDefined() {
			e.names[m.Status.NodeRef.Name] = true
		}
		for _, a := range m.Status.Addresses {
			if ip := net.ParseIP(a.Address); ip != nil {
				e.addrs[ip.String()] = true
			}
		}
	}
	return e, nil
}

// healthyVotingMembers counts the joined members other than removing that
// a registered control-plane Machine owns by Node name and whose node
// reports healthy and voting. A member without such a Machine is not counted
// healthy: the KCP has no way to know.
func healthyVotingMembers(joined []*unstructured.Unstructured, removing *unstructured.Unstructured, machines []*clusterv1.Machine, status map[string]etcdMemberStatus) int {
	registered := map[string]bool{}
	for _, m := range machines {
		if m.Status.NodeRef.IsDefined() {
			registered[m.Status.NodeRef.Name] = true
		}
	}
	n := 0
	for _, member := range joined {
		name := member.GetName()
		if name == removing.GetName() || !registered[name] {
			continue
		}
		if st, ok := status[name]; ok && st.Healthy && st.Voting {
			n++
		}
	}
	return n
}

// orphanCheckDue reports whether the last orphan check of kcp is older than
// etcdOrphanCheckInterval.
func (r *KairosControlPlaneReconciler) orphanCheckDue(kcp *controlplanev1beta2.KairosControlPlane) bool {
	r.orphanMu.Lock()
	defer r.orphanMu.Unlock()
	at, ok := r.orphanCheckedAt[identityCheckKey(kcp)]
	return !ok || time.Since(at) >= etcdOrphanCheckInterval
}

// forgetOrphanCheck makes the next reconcile of kcp run an orphan check.
func (r *KairosControlPlaneReconciler) forgetOrphanCheck(kcp *controlplanev1beta2.KairosControlPlane) {
	r.orphanMu.Lock()
	defer r.orphanMu.Unlock()
	delete(r.orphanCheckedAt, identityCheckKey(kcp))
}

// markOrphanChecked records an orphan check of kcp now.
func (r *KairosControlPlaneReconciler) markOrphanChecked(kcp *controlplanev1beta2.KairosControlPlane) {
	r.orphanMu.Lock()
	defer r.orphanMu.Unlock()
	if r.orphanCheckedAt == nil {
		r.orphanCheckedAt = map[string]time.Time{}
	}
	r.orphanCheckedAt[identityCheckKey(kcp)] = time.Now()
}

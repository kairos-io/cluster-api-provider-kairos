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

// The KCP pins the identity of the workload cluster, and freezes when it
// changes (ADR 0015 PR-2).
//
// Two clusters can share a CA. On k0s, a joiner whose join failed half-way can
// start a new etcd cluster with the CA it received while joining. TLS then
// cannot tell the two clusters apart, and on 2026-10-17 the KCP followed the
// new cluster: it read "no EtcdMember" there for each old member, released
// their etcd-leave hooks, and deleted the original cluster's machines.
//
// The kube-system Namespace UID tells the clusters apart. Every apiserver
// creates kube-system the first time it starts against an empty etcd, and the
// UID lives in etcd, so it survives member replacement and differs between
// two clusters that share a CA. KEP-2149 uses the same value as the cluster
// ID.
//
//   - Pin. Once the control plane is HA, KubeconfigReady is True and a
//     control-plane Machine has a Node, the KCP reads the UID through the
//     control-plane endpoint and through every member it can reach directly.
//     It records the UID in WorkloadClusterUIDAnnotation only when at least one
//     answered and every answer agrees. Existing clusters are pinned the same
//     way on the first reconcile after an upgrade.
//   - Check. Every workload connection that feeds a membership decision must
//     return the pinned UID: joiner admission and k0s join-token minting and
//     extension, every KCP-initiated control-plane delete, and every connection
//     the etcd-leave handshake uses. The KCP also rechecks every reachable path
//     at most once a minute, so a switch is noticed with no action pending.
//   - Freeze. On a mismatch the KCP records it in
//     WorkloadClusterIdentityMismatchAnnotation. From then on it creates and
//     deletes no control-plane Machine, removes no etcd-leave hook, and writes
//     nothing to the workload cluster. EtcdHealthy and Available are False with
//     reason WorkloadClusterIdentityChanged, and a Warning event names both
//     UIDs. This is modelled on the preflight checks of the upstream kubeadm
//     control plane, which hold scale operations and set a condition.
//     Whole-cluster deletion is exempt: reconcileDelete never reaches this
//     code, so deleting the cluster cannot deadlock.
//   - Clear. Only an operator clears the freeze, because a changed identity
//     never heals by itself. Re-pin by setting WorkloadClusterUIDAnnotation to
//     the verified UID, or by removing it so the KCP pins again. Removing the
//     mismatch annotation also lifts the freeze, but the next check freezes
//     again if the pin still does not match.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

const (
	// WorkloadClusterUIDAnnotation holds, on the KairosControlPlane, the UID of
	// the workload cluster's kube-system Namespace: the identity the KCP
	// pinned. The KCP sets it once. An operator may set it to a verified UID,
	// or remove it to have the KCP pin again; either clears a freeze.
	WorkloadClusterUIDAnnotation = "controlplane.cluster.x-k8s.io/workload-cluster-uid"

	// WorkloadClusterIdentityMismatchAnnotation is the freeze marker, set by
	// the KCP when a workload apiserver answered with another UID. Its value is
	// "<observed UID> via <member IP|endpoint> at <RFC3339>; pinned <UID>".
	// The pinned part records the pin the mismatch was measured against, so
	// that changing the pin is recognised as a re-pin.
	WorkloadClusterIdentityMismatchAnnotation = "controlplane.cluster.x-k8s.io/workload-cluster-identity-mismatch"

	// workloadIdentityRecheckInterval bounds how often the KCP rechecks every
	// reachable path when no membership action is pending.
	workloadIdentityRecheckInterval = time.Minute

	// workloadIdentityFrozenRequeueAfter is the requeue interval while the
	// control plane is frozen.
	workloadIdentityFrozenRequeueAfter = 60 * time.Second

	// identityMismatchPinnedSep separates the observation from the recorded
	// pin in WorkloadClusterIdentityMismatchAnnotation.
	identityMismatchPinnedSep = "; pinned "

	// identityNone is how an absent pin is recorded. It cannot be a valid UID.
	identityNone = "<none>"

	// identityMalformed replaces a UID that is not UID-shaped, so nothing a
	// workload apiserver returns is copied verbatim into an annotation or an
	// event.
	identityMalformed = "<malformed>"
)

// errWorkloadIdentityChanged marks every error caused by the identity freeze:
// a mismatch found now, or a freeze already in place. Callers test for it with
// errors.Is and hold instead of failing the reconcile.
var errWorkloadIdentityChanged = errors.New("the workload cluster identity does not match the pinned identity")

// uidShape accepts what a Kubernetes UID can look like (an RFC 4122 UUID in
// practice), and nothing that could carry markup or a separator.
var uidShape = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// displayUID returns uid if it is UID-shaped, identityNone when it is empty,
// and identityMalformed otherwise.
func displayUID(uid string) string {
	switch {
	case uid == "":
		return identityNone
	case uidShape.MatchString(uid):
		return uid
	default:
		return identityMalformed
	}
}

// identityGate is what reconcileWorkloadIdentity decided for this reconcile.
type identityGate struct {
	// frozen is true while the identity freeze is in place.
	frozen bool
	// hold, when not empty, says why membership actions must wait. It is set
	// with frozen, and also when the identity should be pinned but could not
	// be yet.
	hold string
}

// identityObservation is one answer, or one failure, of an identity read.
type identityObservation struct {
	via string
	uid string
	err error
}

// workloadIdentityFrozen reports whether the freeze is in place on kcp, and
// a message for the conditions.
func workloadIdentityFrozen(kcp *controlplanev1beta2.KairosControlPlane) (string, bool) {
	v, ok := kcp.Annotations[WorkloadClusterIdentityMismatchAnnotation]
	if !ok {
		return "", false
	}
	return fmt.Sprintf("A workload apiserver answered with a different cluster identity (kube-system UID): %s. "+
		"Control-plane Machines are not created or deleted, and etcd-leave hooks are not removed, until an operator "+
		"verifies the cluster and re-pins annotation %s (see docs/HIGH_AVAILABILITY.md).",
		v, WorkloadClusterUIDAnnotation), true
}

// errIfIdentityFrozen returns an errWorkloadIdentityChanged error while the
// freeze is in place, naming the action it refuses.
func errIfIdentityFrozen(kcp *controlplanev1beta2.KairosControlPlane, action string) error {
	if _, frozen := workloadIdentityFrozen(kcp); frozen {
		return fmt.Errorf("refusing to %s: %w", action, errWorkloadIdentityChanged)
	}
	return nil
}

// recordedPin returns the pin recorded in a mismatch annotation value, and
// false when the value does not have the shape the KCP writes.
func recordedPin(mismatch string) (string, bool) {
	i := strings.LastIndex(mismatch, identityMismatchPinnedSep)
	if i < 0 {
		return "", false
	}
	return mismatch[i+len(identityMismatchPinnedSep):], true
}

// identityApplies reports whether the workload identity must be pinned and
// checked for this control plane: once a pin exists, always; before that,
// when the control plane has etcd membership to protect (more than one
// replica desired or present) and a member has registered a Node, so a
// workload cluster exists to pin.
func identityApplies(kcp *controlplanev1beta2.KairosControlPlane, machines []*clusterv1.Machine, desiredReplicas int32) bool {
	if kcp.Annotations[WorkloadClusterUIDAnnotation] != "" {
		return true
	}
	if desiredReplicas <= 1 && len(machines) <= 1 {
		return false
	}
	for _, m := range machines {
		if m.Status.NodeRef.IsDefined() {
			return true
		}
	}
	return false
}

// reconcileWorkloadIdentity runs once per reconcile, before any control-plane
// membership action. It clears a freeze an operator re-pinned, enforces a
// freeze in place, pins the identity when it should be pinned and is not, and
// otherwise rechecks every reachable path at most once per
// workloadIdentityRecheckInterval. err is non-nil only when the management
// apiserver fails.
func (r *KairosControlPlaneReconciler) reconcileWorkloadIdentity(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine, desiredReplicas int32) (identityGate, error) {
	if err := r.clearRepinnedIdentityFreeze(ctx, log, kcp); err != nil {
		if apierrors.IsConflict(err) {
			return identityGate{hold: "the KairosControlPlane changed while the identity freeze was being cleared; retrying"}, nil
		}
		return identityGate{}, err
	}
	if msg, frozen := workloadIdentityFrozen(kcp); frozen {
		// Keep the next full recheck due, so that the reconcile which sees the
		// marker gone (an operator removed it) rechecks every path at once
		// instead of up to workloadIdentityRecheckInterval later.
		r.forgetIdentityCheck(kcp)
		return identityGate{frozen: true, hold: msg}, nil
	}
	if !identityApplies(kcp, machines, desiredReplicas) {
		return identityGate{}, nil
	}

	pin := kcp.Annotations[WorkloadClusterUIDAnnotation]
	if pin == "" {
		if !conditions.IsTrue(kcp, controlplanev1beta2.KubeconfigReadyCondition) {
			return identityGate{hold: "the workload kubeconfig is not ready, so the workload cluster identity cannot be pinned yet"}, nil
		}
		return r.pinWorkloadIdentity(ctx, log, kcp, cluster, machines)
	}

	if !r.identityRecheckDue(kcp) {
		return identityGate{}, nil
	}
	answered := false
	for _, o := range r.observeWorkloadIdentity(ctx, log, kcp, cluster, machines) {
		if o.err != nil {
			continue
		}
		answered = true
		if o.uid != pin {
			return frozenGate(kcp, r.tripIdentityFreeze(ctx, log, kcp, o.uid, o.via, pin, "")), nil
		}
	}
	if answered {
		r.markIdentityChecked(kcp)
	}
	return identityGate{}, nil
}

// pinWorkloadIdentity reads the identity through every path it can reach and
// pins it when at least one answered and every answer agrees. Answers that
// disagree freeze the control plane: the KCP cannot tell which cluster is the
// real one, and an operator must decide.
func (r *KairosControlPlaneReconciler) pinWorkloadIdentity(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine) (identityGate, error) {
	obs := r.observeWorkloadIdentity(ctx, log, kcp, cluster, machines)
	var answered []identityObservation
	var failures []string
	for _, o := range obs {
		if o.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", o.via, o.err))
			continue
		}
		answered = append(answered, o)
	}
	if len(answered) == 0 {
		return identityGate{hold: "no workload apiserver answered the cluster identity read: " + strings.Join(failures, "; ")}, nil
	}

	// Only a UID-shaped answer may become the pin. An answer that is not
	// UID-shaped is still an answer: it comes from an apiserver trusted by the
	// cluster CA that does not report a real kube-system UID, so it freezes the
	// control plane rather than being dropped, which would let a hostile path
	// hide. tripIdentityFreeze sanitizes it before it reaches the annotation,
	// the log or the event.
	for _, o := range answered {
		if !uidShape.MatchString(o.uid) {
			return frozenGate(kcp, r.tripIdentityFreeze(ctx, log, kcp, o.uid, o.via, "", describeObservations(answered))), nil
		}
	}

	first := answered[0]
	for _, o := range answered[1:] {
		if o.uid == first.uid {
			continue
		}
		return frozenGate(kcp, r.tripIdentityFreeze(ctx, log, kcp, o.uid, o.via, "", describeObservations(answered))), nil
	}

	if err := r.patchKCPAnnotations(ctx, kcp, map[string]string{WorkloadClusterUIDAnnotation: first.uid}); err != nil {
		if apierrors.IsConflict(err) {
			return identityGate{hold: "the KairosControlPlane changed while the workload cluster identity was being pinned; retrying"}, nil
		}
		return identityGate{}, fmt.Errorf("pin the workload cluster identity: %w", err)
	}
	r.markIdentityChecked(kcp)
	vias := make([]string, 0, len(answered))
	for _, o := range answered {
		vias = append(vias, o.via)
	}
	log.Info("Pinned the workload cluster identity", "uid", displayUID(first.uid), "via", vias)
	if r.Recorder != nil {
		r.Recorder.Eventf(kcp, corev1.EventTypeNormal, "WorkloadClusterIdentityPinned",
			"Pinned the workload cluster identity (kube-system UID %s), read through %s", displayUID(first.uid), strings.Join(vias, ", "))
	}
	return identityGate{}, nil
}

// frozenGate is the gate after tripIdentityFreeze returned tripErr. The
// control plane holds this reconcile even when the mismatch annotation could
// not be written; the next check then finds the mismatch again.
func frozenGate(kcp *controlplanev1beta2.KairosControlPlane, tripErr error) identityGate {
	msg, recorded := workloadIdentityFrozen(kcp)
	if !recorded {
		msg = tripErr.Error()
	}
	return identityGate{frozen: true, hold: msg}
}

// describeObservations lists answers as "via=uid" for a log or an event.
func describeObservations(obs []identityObservation) string {
	parts := make([]string, 0, len(obs))
	for _, o := range obs {
		parts = append(parts, o.via+"="+displayUID(o.uid))
	}
	return strings.Join(parts, ", ")
}

// observeWorkloadIdentity reads the identity through the control-plane
// endpoint and then through every member the etcd-leave handshake could use
// (leaveCandidateIPs with no leaving Machine). Each read is bounded. It
// returns one observation per path tried.
func (r *KairosControlPlaneReconciler) observeWorkloadIdentity(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine) []identityObservation {
	var out []identityObservation
	if wc, err := r.workloadClientFor(ctx, cluster); err != nil {
		out = append(out, identityObservation{via: leaveViaEndpoint, err: fmt.Errorf("build client: %w", err)})
	} else {
		uid, err := readWorkloadIdentity(ctx, wc, workloadRequestTimeout)
		out = append(out, identityObservation{via: leaveViaEndpoint, uid: uid, err: err})
	}

	factory := r.MemberClientFactory
	if factory == nil {
		factory = r.defaultMemberClient
	}
	for _, ip := range leaveCandidateIPs(machines, &clusterv1.Machine{}, leaveExcludedHosts(kcp, cluster)...) {
		wc, err := factory(ctx, cluster, ip)
		if err != nil {
			out = append(out, identityObservation{via: ip, err: fmt.Errorf("build client: %w", err)})
			continue
		}
		uid, err := readWorkloadIdentity(ctx, wc, memberRequestTimeout)
		out = append(out, identityObservation{via: ip, uid: uid, err: err})
	}
	for _, o := range out {
		if o.err != nil {
			log.V(1).Info("Workload cluster identity read failed", "via", o.via, "reason", o.err.Error())
		}
	}
	return out
}

// readWorkloadIdentity returns the UID of the kube-system Namespace as wc
// sees it. The read is bounded by timeout. A Namespace without a UID is an
// error, never an identity. Any other UID is returned as read, even one that
// is not UID-shaped: it is an answer, and every caller treats an answer that
// does not equal the pin as a mismatch. Callers must pass it through
// displayUID before it reaches an annotation, a log line or an event; only a
// UID-shaped answer may become the pin (pinWorkloadIdentity).
func readWorkloadIdentity(ctx context.Context, wc client.Client, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ns := &corev1.Namespace{}
	if err := wc.Get(ctx, client.ObjectKey{Name: metav1.NamespaceSystem}, ns); err != nil {
		return "", fmt.Errorf("read Namespace %s: %w", metav1.NamespaceSystem, err)
	}
	if ns.UID == "" {
		return "", fmt.Errorf("namespace %s has no UID", metav1.NamespaceSystem)
	}
	return string(ns.UID), nil
}

// verifyWorkloadIdentity checks that wc, reached through via, serves the
// pinned cluster. A mismatch freezes the control plane and returns an
// errWorkloadIdentityChanged error; a failed read returns another error, and
// the caller holds its action. With the freeze already in place it returns
// errWorkloadIdentityChanged without reading.
//
// With no pin it returns nil. reconcileWorkloadIdentity holds every
// membership action while a pin is due but missing, so this is reached
// unpinned only where no identity applies: a single-node control plane, or
// one with no registered Node yet.
func (r *KairosControlPlaneReconciler) verifyWorkloadIdentity(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, wc client.Client, via string) error {
	if err := errIfIdentityFrozen(kcp, "use workload apiserver "+via); err != nil {
		return err
	}
	pin := kcp.Annotations[WorkloadClusterUIDAnnotation]
	if pin == "" {
		return nil
	}
	timeout := memberRequestTimeout
	if via == leaveViaEndpoint {
		timeout = workloadRequestTimeout
	}
	uid, err := readWorkloadIdentity(ctx, wc, timeout)
	if err != nil {
		return fmt.Errorf("verify the workload cluster identity via %s: %w", via, err)
	}
	if uid != pin {
		return r.tripIdentityFreeze(ctx, log, kcp, uid, via, pin, "")
	}
	return nil
}

// verifyIdentityViaEndpoint runs verifyWorkloadIdentity on a fresh endpoint
// client. With no pin it builds no client.
func (r *KairosControlPlaneReconciler) verifyIdentityViaEndpoint(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) error {
	if err := errIfIdentityFrozen(kcp, "use the workload cluster"); err != nil {
		return err
	}
	if kcp.Annotations[WorkloadClusterUIDAnnotation] == "" {
		return nil
	}
	wc, err := r.workloadClientFor(ctx, cluster)
	if err != nil {
		return fmt.Errorf("verify the workload cluster identity via %s: build client: %w", leaveViaEndpoint, err)
	}
	return r.verifyWorkloadIdentity(ctx, log, kcp, wc, leaveViaEndpoint)
}

// identityPreflight verifies the identity through the endpoint before a
// control-plane membership change. proceed is false when the change must
// wait; result then says when to come back.
func (r *KairosControlPlaneReconciler) identityPreflight(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, action string) (ctrl.Result, bool) {
	err := r.verifyIdentityViaEndpoint(ctx, log, kcp, cluster)
	switch {
	case err == nil:
		return ctrl.Result{}, true
	case errors.Is(err, errWorkloadIdentityChanged):
		log.Info("Holding back "+action+": the workload cluster identity changed", "reason", err.Error())
		return ctrl.Result{RequeueAfter: workloadIdentityFrozenRequeueAfter}, false
	default:
		log.Info("Holding back "+action+": cannot verify the workload cluster identity", "reason", err.Error())
		return ctrl.Result{RequeueAfter: joinerGateRequeueAfter}, false
	}
}

// tripIdentityFreeze records a mismatch: uid was read through via while pin
// was pinned (empty when the answers disagreed before any pin). It sets the
// mismatch annotation once, keeping the first observation, and emits a
// Warning event. detail, when set, replaces the default event text's
// observation (the disagreeing answers at pin time). It always returns an
// errWorkloadIdentityChanged error; when the annotation cannot be written the
// error also carries that failure, and the next check retries.
func (r *KairosControlPlaneReconciler) tripIdentityFreeze(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, uid, via, pin, detail string) error {
	changed := fmt.Errorf("workload apiserver %s answered with kube-system UID %s, but %s is pinned: %w",
		via, displayUID(uid), displayUID(pin), errWorkloadIdentityChanged)
	if _, frozen := workloadIdentityFrozen(kcp); frozen {
		return changed
	}
	value := fmt.Sprintf("%s via %s at %s%s%s",
		displayUID(uid), via, time.Now().UTC().Format(time.RFC3339), identityMismatchPinnedSep, displayUID(pin))
	if err := r.patchKCPAnnotations(ctx, kcp, map[string]string{WorkloadClusterIdentityMismatchAnnotation: value}); err != nil {
		return errors.Join(changed, fmt.Errorf("record the identity mismatch on the KairosControlPlane: %w", err))
	}
	r.forgetIdentityCheck(kcp)
	log.Info("Workload cluster identity changed; freezing control-plane membership changes",
		"pinned", displayUID(pin), "observed", displayUID(uid), "via", via)
	if r.Recorder != nil {
		observation := fmt.Sprintf("workload apiserver %s answered with kube-system UID %s, but UID %s is pinned", via, displayUID(uid), displayUID(pin))
		if detail != "" {
			observation = "workload apiservers answered with different kube-system UIDs (" + detail + ")"
		}
		r.Recorder.Eventf(kcp, corev1.EventTypeWarning, "WorkloadClusterIdentityChanged",
			"The workload cluster identity changed: %s. Control-plane Machine creation and deletion and etcd-leave hook removal are frozen "+
				"until an operator re-pins annotation %s (see docs/HIGH_AVAILABILITY.md).", observation, WorkloadClusterUIDAnnotation)
	}
	return changed
}

// clearRepinnedIdentityFreeze removes the mismatch annotation once the pin no
// longer equals the pin it recorded: an operator set the pin to another UID,
// or removed it. A mismatch annotation the KCP cannot parse stays; the
// operator removes it by hand.
func (r *KairosControlPlaneReconciler) clearRepinnedIdentityFreeze(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane) error {
	mismatch, ok := kcp.Annotations[WorkloadClusterIdentityMismatchAnnotation]
	if !ok {
		return nil
	}
	recorded, parsed := recordedPin(mismatch)
	current := displayUID(kcp.Annotations[WorkloadClusterUIDAnnotation])
	if !parsed || recorded == current {
		return nil
	}
	if err := r.patchKCPAnnotations(ctx, kcp, nil, WorkloadClusterIdentityMismatchAnnotation); err != nil {
		return fmt.Errorf("clear the workload cluster identity freeze: %w", err)
	}
	r.forgetIdentityCheck(kcp)
	log.Info("The workload cluster identity was re-pinned; the freeze is cleared", "previousPin", recorded, "pin", current)
	if r.Recorder != nil {
		r.Recorder.Eventf(kcp, corev1.EventTypeNormal, "WorkloadClusterIdentityFreezeCleared",
			"Annotation %s changed from %s to %s; the identity freeze is cleared and the workload cluster is checked against the new pin",
			WorkloadClusterUIDAnnotation, recorded, current)
	}
	return nil
}

// patchKCPAnnotations sets and removes KairosControlPlane annotations in one
// metadata merge patch, pinned to the resourceVersion the reconcile read. It
// patches a copy, so the status this reconcile has computed in kcp is not
// replaced by the server's; it then copies the annotations and the new
// resourceVersion back, so the later status update does not conflict.
func (r *KairosControlPlaneReconciler) patchKCPAnnotations(ctx context.Context, kcp *controlplanev1beta2.KairosControlPlane, set map[string]string, remove ...string) error {
	obj := kcp.DeepCopy()
	base := obj.DeepCopy()
	if obj.Annotations == nil {
		obj.Annotations = map[string]string{}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		obj.Annotations[k] = set[k]
	}
	for _, k := range remove {
		delete(obj.Annotations, k)
	}
	if err := r.Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	kcp.Annotations = obj.Annotations
	kcp.ResourceVersion = obj.ResourceVersion
	return nil
}

// markWorkloadIdentityConditions sets Ready, Available and EtcdHealthy to
// False(Error, WorkloadClusterIdentityChanged) while the freeze is in place.
// It runs after every other condition is computed, so nothing reports the
// control plane healthy while it may be looking at the wrong cluster: on
// 2026-10-17 every condition read True while the original cluster was
// destroyed (ADR 0015 OQ-4).
func markWorkloadIdentityConditions(kcp *controlplanev1beta2.KairosControlPlane) {
	msg, frozen := workloadIdentityFrozen(kcp)
	if !frozen {
		return
	}
	for _, ct := range []clusterv1.ConditionType{
		clusterv1.ReadyCondition,
		controlplanev1beta2.AvailableCondition,
		controlplanev1beta2.EtcdHealthyCondition,
	} {
		conditions.MarkFalse(kcp, ct, controlplanev1beta2.WorkloadClusterIdentityChangedReason, clusterv1.ConditionSeverityError, "%s", msg)
	}
}

// identityCheckKey keys the recheck rate limit. The UID alone is enough in a
// live cluster; namespace/name keeps keys distinct where no UID is set.
func identityCheckKey(kcp *controlplanev1beta2.KairosControlPlane) string {
	return string(kcp.UID) + "/" + kcp.Namespace + "/" + kcp.Name
}

// identityRecheckDue reports whether the last full identity check of kcp is
// older than workloadIdentityRecheckInterval.
func (r *KairosControlPlaneReconciler) identityRecheckDue(kcp *controlplanev1beta2.KairosControlPlane) bool {
	r.identityMu.Lock()
	defer r.identityMu.Unlock()
	at, ok := r.identityCheckedAt[identityCheckKey(kcp)]
	return !ok || time.Since(at) >= workloadIdentityRecheckInterval
}

// markIdentityChecked records a full identity check of kcp now.
func (r *KairosControlPlaneReconciler) markIdentityChecked(kcp *controlplanev1beta2.KairosControlPlane) {
	r.identityMu.Lock()
	defer r.identityMu.Unlock()
	if r.identityCheckedAt == nil {
		r.identityCheckedAt = map[string]time.Time{}
	}
	r.identityCheckedAt[identityCheckKey(kcp)] = time.Now()
}

// forgetIdentityCheck makes the next reconcile of kcp run a full check.
func (r *KairosControlPlaneReconciler) forgetIdentityCheck(kcp *controlplanev1beta2.KairosControlPlane) {
	r.identityMu.Lock()
	defer r.identityMu.Unlock()
	delete(r.identityCheckedAt, identityCheckKey(kcp))
}

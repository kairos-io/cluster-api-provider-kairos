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

// The control plane's distribution follows its Machines (ADR 0016).
//
// createControlPlaneMachine stamps spec.distribution on each control-plane
// KairosConfig, and the node renders it once. So the KCP-owned Machines'
// KairosConfigs record what the nodes run, and spec.distribution is a request
// the controller checks against them on every reconcile. Nothing is pinned:
// the value is recomputed from the Machines each time.
//
//   - Observe (D1). none: no Machines. D: every readable KairosConfig says D.
//     mixed: readable KairosConfigs disagree, or a Machine that is not being
//     deleted carries the k0s etcd-leave hook while its KairosConfig says
//     otherwise (conflicting evidence: only k0s members get the hook).
//     unknown: Machines exist but none has a readable KairosConfig. A Machine
//     whose KairosConfig is not found gets no vote. The value is what the
//     KairosConfigs record, which is what the nodes rendered unless a
//     KairosConfig was changed later; the messages say so.
//   - Write (D2). An empty spec.distribution is written once: with the
//     Machines' value when they agree, with the KairosConfigTemplate's value
//     when there are no Machines, and never when the Machines are mixed or
//     unknown or kubeadm is involved. The write carries an optimistic lock, so
//     a concurrent edit wins. Once written, this controller's field manager
//     owns the field, so a later server-side apply that omits it (CAPI
//     topology after a KairosControlPlaneTemplate dropped it) leaves it.
//   - Hold (D3). kubeadm in spec or the template, or Machines whose value is
//     mixed, unknown or different from spec, hold every control-plane
//     membership change: Machine create and delete, the join-token Secret and
//     the k0s token, and orphan etcd-member removal. Status, the kubeconfig,
//     metadata, the identity pin and the etcd leave of a Machine an operator
//     deleted carry on. reconcileDelete never reaches the hold.
//   - Surface (D5). Ready=False always; MachinesUpToDate=False when Machines
//     exist; Available=False and the failure fields only when none do, so a
//     serving control plane is never reported unavailable. A Warning event on
//     entering the hold and a Normal event on leaving it, each emitted once.
//
// The leave decision itself (D4) reads the leaving Machine's KairosConfig and
// does not depend on any of this (reconcileMemberLeave step 1).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

const (
	// distributionHoldRequeueAfter is how often a held control plane is
	// reconciled again. There is no KairosConfigTemplate watch, so without it
	// a fix made on the template would never re-enqueue the KCP.
	distributionHoldRequeueAfter = 60 * time.Second

	// distributionWriteConflictRequeueAfter is the backstop after the
	// spec.distribution write lost an optimistic-lock race. The edit that won
	// re-enqueues the KCP through its own watch event first.
	distributionWriteConflictRequeueAfter = 5 * time.Second

	// distributionEventMachineLimit caps the Machine names listed per
	// distribution in an event or condition message.
	distributionEventMachineLimit = 5

	// Event reasons for the spec.distribution write and the end of a hold.
	// The Warning event on entering a hold uses the verdict's condition
	// reason.
	distributionInheritedEventReason = "DistributionInherited"
	distributionRestoredEventReason  = "DistributionRestored"
	distributionResolvedEventReason  = "DistributionMismatchResolved"
	distributionOverrideEventReason  = "DistributionOverride"
)

// errDistributionHeld marks a control-plane Machine create or delete refused
// because membership changes are held (ADR 0016 D3). Callers test for it with
// errors.Is and hold instead of failing the reconcile.
var errDistributionHeld = errors.New("control-plane membership changes are held until spec.distribution matches the control-plane Machines")

// observedKind is the shape of what the Machines run (ADR 0016 D1).
type observedKind int

const (
	// observedNone: the KCP owns no control-plane Machine.
	observedNone observedKind = iota
	// observedAgreed: every readable KairosConfig records the same value.
	observedAgreed
	// observedMixed: readable KairosConfigs record different values, or one
	// contradicts its Machine's etcd-leave hook.
	observedMixed
	// observedUnknown: Machines exist but none has a readable KairosConfig.
	observedUnknown
)

// observedDistribution is what the KCP-owned control-plane Machines run, read
// from their KairosConfigs.
type observedDistribution struct {
	kind observedKind
	// value is the distribution the Machines agree on (observedAgreed only).
	value string
	// byDistribution lists, per distribution, the Machines whose KairosConfig
	// records it, sorted.
	byDistribution map[string][]string
	// unread lists the Machines with no readable KairosConfig, sorted.
	unread []string
	// contradicted lists, sorted, the Machines not being deleted that carry
	// the k0s etcd-leave hook while their KairosConfig records another
	// distribution. Any of them makes the observed value mixed.
	contradicted []string
}

// machinesExist reports whether the KCP owns any control-plane Machine.
func (o observedDistribution) machinesExist() bool {
	return o.kind != observedNone
}

// String is the observed value for logs: none, the distribution, mixed or
// unknown.
func (o observedDistribution) String() string {
	switch o.kind {
	case observedNone:
		return "none"
	case observedAgreed:
		return o.value
	case observedMixed:
		return "mixed"
	default:
		return "unknown"
	}
}

// describeMachines lists the Machines per distribution, at most
// distributionEventMachineLimit names each: "k0s (a, b), k3s (c)".
func (o observedDistribution) describeMachines() string {
	dists := make([]string, 0, len(o.byDistribution))
	for d := range o.byDistribution {
		dists = append(dists, d)
	}
	sort.Strings(dists)
	parts := make([]string, 0, len(dists)+1)
	for _, d := range dists {
		parts = append(parts, fmt.Sprintf("%s (%s)", d, limitNames(o.byDistribution[d])))
	}
	if len(o.unread) > 0 {
		parts = append(parts, fmt.Sprintf("no readable KairosConfig (%s)", limitNames(o.unread)))
	}
	if len(o.contradicted) > 0 {
		parts = append(parts, fmt.Sprintf("KairosConfig contradicts the k0s etcd-leave hook (%s)", limitNames(o.contradicted)))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// limitNames joins at most distributionEventMachineLimit names and says how
// many were left out.
func limitNames(names []string) string {
	if len(names) <= distributionEventMachineLimit {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(names[:distributionEventMachineLimit], ", "),
		len(names)-distributionEventMachineLimit)
}

// summarizeObserved builds the observed value from the per-Machine reads:
// byDistribution maps each distribution to the Machines that record it, and
// unread lists the Machines that have no vote.
func summarizeObserved(byDistribution map[string][]string, unread []string) observedDistribution {
	o := observedDistribution{byDistribution: byDistribution, unread: unread}
	for _, names := range o.byDistribution {
		sort.Strings(names)
	}
	sort.Strings(o.unread)
	switch {
	case len(byDistribution) == 0 && len(unread) == 0:
		o.kind = observedNone
	case len(byDistribution) == 0:
		o.kind = observedUnknown
	case len(byDistribution) > 1:
		o.kind = observedMixed
	default:
		o.kind = observedAgreed
		for d := range byDistribution {
			o.value = d
		}
	}
	return o
}

// withContradictions marks the Machines whose KairosConfig contradicts their
// own etcd-leave hook. Any such Machine makes the observed value mixed: the
// hook says k0s and the KairosConfig says otherwise, so neither can be taken
// as what the control plane runs.
func (o observedDistribution) withContradictions(names []string) observedDistribution {
	if len(names) == 0 {
		return o
	}
	o.contradicted = append([]string(nil), names...)
	sort.Strings(o.contradicted)
	o.kind = observedMixed
	o.value = ""
	return o
}

// observeDistribution reads what machines run from their KairosConfigs,
// through the manager's cache (machineDistribution). A read error other than
// NotFound is returned: the KCP must not guess. A Machine that is not being
// deleted, carries the k0s etcd-leave hook and has a KairosConfig recording
// another distribution is conflicting evidence and makes the value mixed; a
// Machine being deleted votes by its KairosConfig only.
func (r *KairosControlPlaneReconciler) observeDistribution(ctx context.Context, machines []*clusterv1.Machine) (observedDistribution, error) {
	byDistribution := map[string][]string{}
	var unread, contradicted []string
	for _, m := range machines {
		dist, found, err := r.machineDistribution(ctx, m)
		if err != nil {
			return observedDistribution{}, err
		}
		if !found {
			unread = append(unread, m.Name)
			continue
		}
		byDistribution[dist] = append(byDistribution[dist], m.Name)
		if dist != "k0s" && hasEtcdLeaveHook(m) && m.DeletionTimestamp.IsZero() {
			contradicted = append(contradicted, m.Name)
		}
	}
	return summarizeObserved(byDistribution, unread).withContradictions(contradicted), nil
}

// distributionGuard is what reconcileDistribution decided for this
// reconcile. The zero value is not held but builds nothing:
// createControlPlaneMachine refuses a guard without an effective distribution.
type distributionGuard struct {
	// effective is the distribution a new control-plane Machine is built with:
	// spec.distribution after any write, else k0s (distributionOf).
	effective string
	// spec is spec.distribution after any write, as the verdict saw it.
	spec string
	// observed is what the Machines run.
	observed observedDistribution
	// templateName and template are the referenced KairosConfigTemplate and
	// its distribution ("" when it sets none or does not exist).
	templateName string
	template     string
	// reason is the verdict: "" when not held, else
	// UnsupportedDistributionReason or DistributionMismatchReason.
	reason string
	// message explains the verdict, for the conditions and the log.
	message string
	// wroteSpec reports that spec.distribution was written this reconcile, so
	// the caller re-anchors its patch helper.
	wroteSpec bool
	// previousReadyReason is Ready's reason as this reconcile found it, which
	// the events are de-duplicated against.
	previousReadyReason string
}

// held reports whether control-plane membership changes are held.
func (g distributionGuard) held() bool {
	return g.reason != ""
}

// errIfHeld returns an errDistributionHeld error, naming action, while
// membership changes are held.
func (g distributionGuard) errIfHeld(action string) error {
	if !g.held() {
		return nil
	}
	return fmt.Errorf("refusing to %s (%s): %w", action, g.reason, errDistributionHeld)
}

// errIfCannotCreate returns why no control-plane Machine may be built now:
// the hold, a kubeadm KairosConfigTemplate, or no supported distribution
// decided. The last never happens through reconcileDistribution; it keeps a
// caller that skipped it from building a Machine with no distribution.
func (g distributionGuard) errIfCannotCreate() error {
	if err := g.errIfHeld("create a control-plane Machine"); err != nil {
		return err
	}
	if g.template == kcpUnsupportedDistributionKubeadm {
		return fmt.Errorf("refusing to create a control-plane Machine: KairosConfigTemplate %q has distribution kubeadm: %w",
			g.templateName, errDistributionHeld)
	}
	if !isKCPDistribution(g.effective) {
		return fmt.Errorf("refusing to create a control-plane Machine: no supported distribution was decided (%q)", g.effective)
	}
	return nil
}

// describe names spec, the template and the Machines per distribution, for
// the events.
func (g distributionGuard) describe() string {
	template := "no KairosConfigTemplate"
	if g.templateName != "" {
		template = fmt.Sprintf("KairosConfigTemplate %q distribution=%s", g.templateName, displayDistribution(g.template))
	}
	return fmt.Sprintf("spec.distribution=%s, %s, control-plane Machines: %s",
		displayDistribution(g.spec), template, g.observed.describeMachines())
}

// displayDistribution shows an empty distribution as <unset>.
func displayDistribution(d string) string {
	if d == "" {
		return "<unset>"
	}
	return d
}

// isKCPDistribution reports whether d is a distribution a KairosControlPlane
// accepts: the KCP enum and webhook allow k0s and k3s only.
func isKCPDistribution(d string) bool {
	return d == "k0s" || d == "k3s"
}

// distributionToWrite returns the value reconcileDistribution writes into an
// empty spec.distribution, with the event reason naming where it came from,
// or "" when nothing may be written (ADR 0016 D2): with Machines, the value
// they agree on, never the template's; with none, the template's; never when
// the Machines are mixed or unknown, and never when kubeadm is involved,
// either as the template's value or as the value the Machines agree on.
func distributionToWrite(template string, obs observedDistribution) (value, eventReason string) {
	if template == kcpUnsupportedDistributionKubeadm {
		return "", ""
	}
	switch obs.kind {
	case observedNone:
		if isKCPDistribution(template) {
			return template, distributionInheritedEventReason
		}
	case observedAgreed:
		if isKCPDistribution(obs.value) {
			return obs.value, distributionRestoredEventReason
		}
	}
	return "", ""
}

// distributionVerdict decides whether membership changes are held, from
// spec.distribution after any write, the referenced KairosConfigTemplate and
// what the Machines run (ADR 0016 D3). The first match applies: kubeadm in
// spec or the template, then Machines that are mixed, unknown or differ from
// spec. reason is "" when nothing is held.
func distributionVerdict(spec, templateName, template string, obs observedDistribution) (reason, message string) {
	const held = " Control-plane Machines are not created or deleted, join tokens are not minted or extended, and " +
		"orphan etcd members are not removed until this is fixed."
	switch {
	case spec == kcpUnsupportedDistributionKubeadm:
		return controlplanev1beta2.UnsupportedDistributionReason, kubeadmRefusalMessage("spec.distribution is kubeadm", obs)
	case template == kcpUnsupportedDistributionKubeadm:
		return controlplanev1beta2.UnsupportedDistributionReason,
			kubeadmRefusalMessage(fmt.Sprintf("KairosConfigTemplate %q has distribution kubeadm", templateName), obs)
	}
	switch obs.kind {
	case observedNone:
		return "", ""
	case observedMixed:
		return controlplanev1beta2.DistributionMismatchReason, fmt.Sprintf(
			"The distributions recorded on the control-plane Machines' KairosConfigs disagree: %s.%s Confirm what "+
				"each node actually runs, set spec.distribution to the distribution the cluster runs on the "+
				"KairosControlPlane or its KairosControlPlaneTemplate, then delete the Machines whose nodes run "+
				"another one; the controller does not delete them on its own.",
			obs.describeMachines(), held)
	case observedUnknown:
		return controlplanev1beta2.DistributionMismatchReason, fmt.Sprintf(
			"No control-plane Machine has a readable KairosConfig (%s), so no distribution is recorded for them.%s "+
				"Check the KairosConfigs the Machines' spec.bootstrap.configRef names, and confirm what the nodes "+
				"actually run.",
			limitNames(obs.unread), held)
	}
	if spec != obs.value {
		return controlplanev1beta2.DistributionMismatchReason, fmt.Sprintf(
			"spec.distribution is %s, but the control-plane Machines' KairosConfigs record %s: %s.%s The recorded "+
				"value is what the nodes were built with unless a KairosConfig was changed since: confirm what the "+
				"nodes actually run before you set spec.distribution to it, on the KairosControlPlane or its "+
				"KairosControlPlaneTemplate. Removing spec.distribution writes the recorded value back. Changing the "+
				"distribution of a running control plane needs a new Cluster.",
			displayDistribution(spec), obs.value, obs.describeMachines(), held)
	}
	return "", ""
}

// kubeadmRefusalMessage is the UnsupportedDistribution message; cause says
// where kubeadm came from.
func kubeadmRefusalMessage(cause string, obs observedDistribution) string {
	msg := fmt.Sprintf("kubeadm is not supported on a KairosControlPlane in this release (%s); "+
		"kubeadm is a worker-join distribution only. "+
		"Use a k0s or k3s KairosConfigTemplate for the control plane.", cause)
	if obs.machinesExist() {
		msg += " Control-plane Machines are not created or deleted until then."
	}
	return msg
}

// reconcileDistribution replaces the old resolve-and-persist step and both
// kubeadm refusals (ADR 0016). It lists the KCP's control-plane Machines,
// reads what they run, reads the referenced KairosConfigTemplate, writes an
// empty spec.distribution when D2 allows, and decides the verdict. It holds
// nothing itself: reconcileMachines applies the hold and the status path the
// conditions. An error is a management-cluster read failure, or the spec
// write failing (a Conflict when another writer changed the KCP first).
func (r *KairosControlPlaneReconciler) reconcileDistribution(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) (distributionGuard, error) {
	g := distributionGuard{
		templateName:        kcp.Spec.KairosConfigTemplate.Name,
		previousReadyReason: conditions.GetReason(kcp, clusterv1.ReadyCondition),
	}
	machines, err := r.getControlPlaneMachines(ctx, kcp, cluster)
	if err != nil {
		return g, fmt.Errorf("list control-plane Machines: %w", err)
	}
	if g.observed, err = r.observeDistribution(ctx, machines); err != nil {
		return g, err
	}
	if g.template, err = r.resolveEffectiveDistribution(ctx, kcp); err != nil {
		return g, err
	}

	switch {
	case kcp.Spec.Distribution == "":
		value, source := distributionToWrite(g.template, g.observed)
		if value == "" {
			break
		}
		if err := r.writeSpecDistribution(ctx, kcp, value); err != nil {
			return g, fmt.Errorf("write spec.distribution: %w", err)
		}
		g.wroteSpec = true
		from := fmt.Sprintf("the control-plane Machines (%s)", g.observed.describeMachines())
		if source == distributionInheritedEventReason {
			from = fmt.Sprintf("KairosConfigTemplate %q", g.templateName)
		}
		log.Info("Wrote the empty spec.distribution", "distribution", value, "source", source, "template", g.templateName)
		if r.Recorder != nil {
			r.Recorder.Eventf(kcp, corev1.EventTypeNormal, source, "spec.distribution was empty; set it to %s from %s", value, from)
		}
	case r.Recorder != nil && g.template != "" && g.template != kcpUnsupportedDistributionKubeadm && g.template != kcp.Spec.Distribution:
		// An explicit spec.distribution wins over the template's. Say so, so
		// the template value being ignored is not a surprise.
		r.Recorder.Eventf(kcp, corev1.EventTypeWarning, distributionOverrideEventReason,
			"spec.distribution=%s overrides KairosConfigTemplate distribution=%s", kcp.Spec.Distribution, g.template)
	}

	g.spec = kcp.Spec.Distribution
	g.effective = distributionOf(kcp)
	g.reason, g.message = distributionVerdict(g.spec, g.templateName, g.template, g.observed)
	return g, nil
}

// writeSpecDistribution sets spec.distribution to value in its own merge
// patch, separate from the status update, with an optimistic lock on the
// resourceVersion the reconcile read: an operator edit made in between wins
// and this write fails with a Conflict. kcp is updated from the response; on
// failure its spec is left as it was.
func (r *KairosControlPlaneReconciler) writeSpecDistribution(ctx context.Context, kcp *controlplanev1beta2.KairosControlPlane, value string) error {
	base := kcp.DeepCopy()
	kcp.Spec.Distribution = value
	if err := r.Patch(ctx, kcp, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		kcp.Spec.Distribution = base.Spec.Distribution
		return err
	}
	return nil
}

// markDistributionConditions surfaces the hold (ADR 0016 D5). Ready is False
// with the verdict's reason. With Machines, MachinesUpToDate is False too and
// Available is left alone: a serving control plane is never reported
// unavailable for this. With none, Available is False and the failure fields
// are set, as the kubeadm refusal always did. Not held, it drops a
// MachinesUpToDate a hold left behind, which reconcileMachines sets again
// the next time it reaches the version check.
//
// It runs after every other condition but the identity freeze
// (markWorkloadIdentityConditions), so a freeze wins Ready.
func markDistributionConditions(kcp *controlplanev1beta2.KairosControlPlane, g distributionGuard) {
	if !g.held() {
		switch conditions.GetReason(kcp, controlplanev1beta2.MachinesUpToDateCondition) {
		case controlplanev1beta2.DistributionMismatchReason, controlplanev1beta2.UnsupportedDistributionReason:
			conditions.Delete(kcp, controlplanev1beta2.MachinesUpToDateCondition)
		}
		return
	}
	conditions.MarkFalse(kcp, clusterv1.ReadyCondition, g.reason, clusterv1.ConditionSeverityWarning, "%s", g.message)
	if g.observed.machinesExist() {
		conditions.MarkFalse(kcp, controlplanev1beta2.MachinesUpToDateCondition, g.reason, clusterv1.ConditionSeverityWarning, "%s", g.message)
		return
	}
	conditions.MarkFalse(kcp, controlplanev1beta2.AvailableCondition, g.reason, clusterv1.ConditionSeverityWarning, "%s", g.message)
	kcp.Status.FailureReason = g.reason
	kcp.Status.FailureMessage = g.message
}

// recordDistributionEvents emits a Warning event when this reconcile enters
// a hold, or moves to another verdict, and a Normal event when it leaves
// one. Both are de-duplicated against Ready's reason as the reconcile found
// it, so a hold that lasts emits once, not every requeue. Reconcile calls it
// only after the status update that persists the new Ready reason succeeded:
// a reconcile working from a stale cached KCP would otherwise emit a second
// time before its update conflicted.
func (r *KairosControlPlaneReconciler) recordDistributionEvents(log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, g distributionGuard) {
	wasHeld := g.previousReadyReason == controlplanev1beta2.DistributionMismatchReason ||
		g.previousReadyReason == controlplanev1beta2.UnsupportedDistributionReason
	switch {
	case g.held() && g.previousReadyReason != g.reason:
		log.Info("Holding control-plane membership changes because of the distribution",
			"reason", g.reason, "spec", g.spec, "template", g.template, "observed", g.observed.String())
		if r.Recorder != nil {
			r.Recorder.Eventf(kcp, corev1.EventTypeWarning, g.reason,
				"Holding control-plane Machine creation and deletion: %s", g.describe())
		}
	case !g.held() && wasHeld:
		log.Info("Control-plane membership changes resume: the distribution matches the Machines",
			"spec", g.spec, "observed", g.observed.String())
		if r.Recorder != nil {
			r.Recorder.Eventf(kcp, corev1.EventTypeNormal, distributionResolvedEventReason,
				"Control-plane membership changes resume: %s", g.describe())
		}
	}
}

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

// Package controlplane: SSH-fallback sibling reconciler (PR-9 / KD-3b).
//
// This file implements a separate, narrowly-scoped reconciler that
// watches KairosControlPlane and, when the eligibility gate fires,
// enqueues an SSH-fetch job onto the bounded worker pool defined in
// ssh_fallback_worker.go. Per ADR 0002 § B.2 and
// internal/controllers/CLAUDE.md § 7 the reconciler itself performs NO
// SSH I/O — it only schedules work and reflects worker outcomes onto
// KubeconfigReadyCondition via condition Reasons.
//
// The reconciler is intentionally tiny: most of the testable surface
// lives in the worker. The reconciler's responsibilities are:
//
//  1. Filter to KCPs that have Spec.SSHFallback.Enabled=true.
//  2. Evaluate the eligibility gate (KubeconfigReadyCondition=False with
//     Reason in {WaitingForNodePush, SSHFallbackFailed,
//     SSHFallbackMisconfigured} AND LastNodePushObserved older than
//     ActivateAfter).
//  3. Resolve the workload node IP (from the first control-plane
//     Machine's Status.Addresses) and enqueue a SSHFallbackJob.
//  4. On each worker result, requeue the KCP so the main reconciler's
//     observeKubeconfigSecret picks up the new Secret (success path) or
//     so the next reconcile re-evaluates eligibility (failure path).

package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// sshFallbackEvalRequeue is the cadence at which the sibling reconciler
// re-evaluates eligibility when SSHFallback.Enabled=true but the gate
// has not yet fired. Watch events from KCP/Machine wake us when something
// changes; this requeue is the time-based backstop so the predicate
// re-checks elapsed-since-LastNodePushObserved without relying on a KCP
// write.
//
// One minute is short enough to keep the Info → Warning → SSHFallback
// timeline tight at the 15-minute mark and long enough to avoid hot-loop
// reconciles under steady state.
const sshFallbackEvalRequeue = 1 * time.Minute

// SSHFallbackReconciler is the sibling controller that gates the SSH
// fallback path. It is registered separately from
// KairosControlPlaneReconciler (see main.go) and shares the bounded
// worker pool defined in ssh_fallback_worker.go.
//
// The reconciler's hot path is intentionally bounded: a single Get of
// the KCP, a Get of the owning Cluster, a List of control-plane
// Machines for IP resolution, and one Enqueue call. No SSH I/O. No
// long-lived locks.
type SSHFallbackReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Worker is the bounded worker pool shared with main.go. The
	// reconciler enqueues jobs via Worker.Enqueue and uses
	// Worker.IsInFlight as the dedup gate.
	Worker *SSHFallbackWorker
}

//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kairoscontrolplanes,verbs=get;list;watch
//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kairoscontrolplanes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;machines,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile evaluates the SSH-fallback eligibility gate for a single KCP
// and, when eligible, enqueues an SSH-fetch job onto the worker pool.
// The reconciler patches the KCP's status with the Dialing/Failed/
// Misconfigured Reason so operators see the transition in
// `kubectl describe kcp`.
//
// Named returns (result, retErr) are required so the deferred Patch
// closure can combine its error into retErr via errors.Join — the same
// pattern the main KairosControlPlaneReconciler uses.
func (r *SSHFallbackReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	log := ctrl.LoggerFrom(ctx).WithName("ssh-fallback")

	kcp := &controlplanev1beta2.KairosControlPlane{}
	if err := r.Get(ctx, req.NamespacedName, kcp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Hard predicate: only act on KCPs with SSHFallback.Enabled=true.
	// The Watches predicate already filters most non-eligible KCPs out;
	// re-checking here is defensive against late spec mutations between
	// the watch event and this Reconcile.
	if kcp.Spec.SSHFallback == nil || !kcp.Spec.SSHFallback.Enabled {
		return ctrl.Result{}, nil
	}

	// If the worker pool already has a job in flight for this KCP, skip
	// re-enqueueing. The worker will post its result and we'll be
	// re-woken via the watch.
	if r.Worker == nil {
		// No worker wired — should be impossible in production
		// (main.go wires one). Log and skip rather than panic so
		// envtest setup without the worker remains safe.
		log.Info("SSH fallback worker not wired; skipping")
		return ctrl.Result{}, nil
	}
	if r.Worker.IsInFlight(req.NamespacedName) {
		// Re-check on the next cadence; do NOT block.
		return ctrl.Result{RequeueAfter: sshFallbackEvalRequeue}, nil
	}

	// Initialize the patch helper BEFORE any returns so condition
	// transitions on the gate paths still get flushed.
	patchHelper, err := patch.NewHelper(kcp, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	patchOnExit := false
	defer func() {
		if !patchOnExit {
			return
		}
		if perr := patchHelper.Patch(ctx, kcp); perr != nil {
			retErr = ctrlErrJoin(retErr, perr)
		}
	}()

	// Eligibility gate.
	eligible, requeue := r.evaluateEligibility(ctx, log, kcp)
	if !eligible {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	// Resolve the owning Cluster.
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, kcp.ObjectMeta)
	if err != nil {
		log.Info("could not resolve owning Cluster; will retry", "error", err.Error())
		return ctrl.Result{RequeueAfter: sshFallbackEvalRequeue}, nil
	}

	// Resolve the host IP from the first control-plane Machine.
	host, err := r.resolveControlPlaneHost(ctx, log, kcp, cluster)
	if err != nil {
		log.Info("could not resolve control-plane host; will retry", "error", err.Error())
		// Soft retry: no condition change, no job enqueue. The next
		// reconcile (woken by Machine status update) will retry.
		return ctrl.Result{RequeueAfter: sshFallbackEvalRequeue}, nil
	}

	// Mark the condition Dialing and enqueue.
	conditions.MarkFalse(kcp,
		controlplanev1beta2.KubeconfigReadyCondition,
		controlplanev1beta2.SSHFallbackDialingReason,
		clusterv1.ConditionSeverityInfo,
		"%s",
		fmt.Sprintf("SSH fallback dialing %s for cluster %s/%s.", host, cluster.Namespace, cluster.Name),
	)
	patchOnExit = true

	job := SSHFallbackJob{
		KCPKey:       req.NamespacedName,
		Cluster:      cluster,
		Spec:         *kcp.Spec.SSHFallback,
		Distribution: kcp.Spec.Distribution,
		Host:         host,
	}
	if !r.Worker.Enqueue(ctx, job) {
		// Pool full or already in flight. Either way: retry shortly.
		// Note: we deliberately do NOT downgrade the condition back
		// from Dialing here — the in-flight job (if any) will produce
		// its own outcome shortly.
		return ctrl.Result{RequeueAfter: sshFallbackEvalRequeue}, nil
	}

	log.Info("SSH fallback job enqueued", "host", host)
	return ctrl.Result{RequeueAfter: sshFallbackEvalRequeue}, nil
}

// evaluateEligibility returns (true, 0) when the KCP is eligible for an
// SSH fallback attempt. Returns (false, requeue) otherwise, where
// requeue is the duration after which the reconciler should re-check.
//
// Eligibility:
//
//  1. KubeconfigReadyCondition.Status is False.
//  2. Reason is one of {WaitingForNodePush, SSHFallbackFailed,
//     SSHFallbackMisconfigured} — Dialing means we already have a job
//     in flight; True means we're done.
//  3. LastNodePushObserved is non-nil and is older than ActivateAfter
//     (default 15m).
func (r *SSHFallbackReconciler) evaluateEligibility(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane) (bool, time.Duration) {
	_ = ctx
	cond := conditions.Get(kcp, controlplanev1beta2.KubeconfigReadyCondition)
	if cond == nil || cond.Status != corev1.ConditionFalse {
		// Condition is either unset (main reconciler hasn't run) or
		// True (kubeconfig already ready). Nothing for us to do.
		return false, sshFallbackEvalRequeue
	}
	switch cond.Reason {
	case controlplanev1beta2.WaitingForNodePushReason,
		controlplanev1beta2.SSHFallbackFailedReason,
		controlplanev1beta2.SSHFallbackMisconfiguredReason:
		// Fall through to the timing gate.
	case controlplanev1beta2.SSHFallbackDialingReason:
		// A job is in flight (or just enqueued); the worker's result
		// envelope will wake us. Don't re-enqueue.
		return false, sshFallbackEvalRequeue
	default:
		// Some other Reason (operator-set?) — be conservative and
		// don't fire.
		return false, sshFallbackEvalRequeue
	}

	if kcp.Status.LastNodePushObserved == nil {
		// Main reconciler hasn't anchored the timestamp yet; not our
		// turn.
		return false, sshFallbackEvalRequeue
	}

	activateAfter := 15 * time.Minute
	if kcp.Spec.SSHFallback.ActivateAfter != nil {
		activateAfter = kcp.Spec.SSHFallback.ActivateAfter.Duration
	}
	elapsed := time.Since(kcp.Status.LastNodePushObserved.Time)
	if elapsed < activateAfter {
		// Re-check at the earlier of (next watch event, halfway to
		// activation). Halfway is a heuristic that keeps the
		// requeue cadence loose far from the gate and tight near it.
		next := (activateAfter - elapsed) / 2
		if next < sshFallbackEvalRequeue {
			next = sshFallbackEvalRequeue
		}
		log.V(2).Info("SSH fallback gate not yet open",
			"elapsed", elapsed.Round(time.Second).String(),
			"activateAfter", activateAfter.String(),
			"requeueAfter", next.String(),
		)
		return false, next
	}
	return true, 0
}

// resolveControlPlaneHost picks the IP the worker should dial. It uses
// the first control-plane Machine's Status.Addresses (preferring
// InternalIP → ExternalIP → any). The reconciler refuses to fabricate a
// host: if no usable address is present, the caller defers via a soft
// retry.
func (r *SSHFallbackReconciler) resolveControlPlaneHost(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) (string, error) {
	_ = log
	machines := &clusterv1.MachineList{}
	listOpts := []client.ListOption{
		client.InNamespace(kcp.Namespace),
		client.MatchingLabels{
			clusterv1.ClusterNameLabel:         cluster.Name,
			clusterv1.MachineControlPlaneLabel: "",
		},
	}
	if err := r.List(ctx, machines, listOpts...); err != nil {
		return "", fmt.Errorf("list control-plane Machines: %w", err)
	}
	for _, m := range machines.Items {
		// Only consider Machines owned by this KCP.
		ownerRef := metav1.GetControllerOf(&m)
		if ownerRef == nil || ownerRef.UID != kcp.UID {
			continue
		}
		if ip := preferredMachineAddress(&m); ip != "" {
			return ip, nil
		}
	}
	return "", fmt.Errorf("no control-plane Machine with a usable address found")
}

// preferredMachineAddress returns the best dial target from a Machine's
// Status.Addresses, prioritising InternalIP > ExternalIP > any.
func preferredMachineAddress(m *clusterv1.Machine) string {
	if m == nil {
		return ""
	}
	var internalIP, externalIP, anyIP string
	for _, a := range m.Status.Addresses {
		if a.Address == "" {
			continue
		}
		switch a.Type {
		case clusterv1.MachineInternalIP:
			if internalIP == "" {
				internalIP = a.Address
			}
		case clusterv1.MachineExternalIP:
			if externalIP == "" {
				externalIP = a.Address
			}
		default:
			if anyIP == "" {
				anyIP = a.Address
			}
		}
	}
	if internalIP != "" {
		return internalIP
	}
	if externalIP != "" {
		return externalIP
	}
	return anyIP
}

// StartResultDrain runs in a manager-owned goroutine and pumps worker
// outcomes onto the reconciler's status. Each result also produces a
// follow-up reconcile via the same KCP key so the main reconciler's
// observeKubeconfigSecret picks up the new Secret.
//
// The function returns when ctx is canceled; it never panics. It is
// invoked from main.go via mgr.Add(manager.RunnableFunc{...}) so
// graceful shutdown is honored.
func (r *SSHFallbackReconciler) StartResultDrain(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("ssh-fallback-result-drain")
	if r.Worker == nil {
		log.Info("no worker wired; drain exiting immediately")
		return nil
	}
	results := r.Worker.Results()
	for {
		select {
		case <-ctx.Done():
			return nil
		case env, ok := <-results:
			if !ok {
				return nil
			}
			r.applyWorkerResult(ctx, log, env)
		}
	}
}

// applyWorkerResult fetches the KCP, sets KubeconfigReadyCondition to
// the appropriate Reason based on the worker outcome, and patches.
// Success-path Reasons (KubeconfigReadyViaSSHFallback) are NOT set here
// — observeKubeconfigSecret handles that branch when it sees the
// freshly-written Secret with the source annotation.
func (r *SSHFallbackReconciler) applyWorkerResult(ctx context.Context, log logr.Logger, env SSHFallbackResultEnvelope) {
	kcp := &controlplanev1beta2.KairosControlPlane{}
	if err := r.Get(ctx, env.KCPKey, kcp); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		log.Error(err, "fetch KCP for worker result", "kcp", env.KCPKey.String())
		return
	}

	patchHelper, err := patch.NewHelper(kcp, r.Client)
	if err != nil {
		log.Error(err, "patch helper init for worker result", "kcp", env.KCPKey.String())
		return
	}

	switch env.Result.Category {
	case SSHFallbackOK:
		// Success: do not write the condition here. The main reconciler
		// will observe the Secret on its next Reconcile (triggered by
		// the Secret watch) and transition to True with the via-SSH
		// Reason. Writing it here would race the main reconciler's
		// patch helper.
		log.Info("SSH fallback succeeded; main reconciler will transition condition",
			"kcp", env.KCPKey.String())
		return
	case SSHFallbackMisconfigured:
		conditions.MarkFalse(kcp,
			controlplanev1beta2.KubeconfigReadyCondition,
			controlplanev1beta2.SSHFallbackMisconfiguredReason,
			clusterv1.ConditionSeverityWarning,
			"SSH fallback misconfigured; check Spec.SSHFallback Secret references.",
		)
	default:
		// All other categories (host-key mismatch, auth failed, dial
		// timeout, dial refused, remote-file-missing, payload-invalid,
		// write-failed) map to SSHFallbackFailedReason. The Event
		// emitted by the worker carries the specific category.
		conditions.MarkFalse(kcp,
			controlplanev1beta2.KubeconfigReadyCondition,
			controlplanev1beta2.SSHFallbackFailedReason,
			clusterv1.ConditionSeverityWarning,
			"SSH fallback failed: %s.", string(env.Result.Category),
		)
	}

	if err := patchHelper.Patch(ctx, kcp); err != nil {
		log.Error(err, "patch KCP after worker result", "kcp", env.KCPKey.String())
	}
}

// SetupWithManager registers the sibling controller with the manager.
// Watches KCP only — the worker's own status writes wake us via the
// KCP watch, and the Machine-status path is covered by the main
// reconciler waking us through the same watch on KCP-driven requeue.
func (r *SSHFallbackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("kairoscontrolplane-ssh-fallback").
		For(&controlplanev1beta2.KairosControlPlane{},
			// Filter to KCPs with SSHFallback.Enabled=true. This
			// keeps the reconciler completely idle for the default
			// (off) case — important because the reconciler shares
			// the manager workqueue.
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				kcp, ok := obj.(*controlplanev1beta2.KairosControlPlane)
				if !ok {
					return false
				}
				return kcp.Spec.SSHFallback != nil && kcp.Spec.SSHFallback.Enabled
			})),
		).
		Complete(r)
}

// ctrlErrJoin joins two errors into one. Defined locally so the
// reconciler does not pull in errors.Join transitively where the rest
// of the controlplane package uses it (the main controller defines its
// own pattern). Two-argument variant is enough here.
func ctrlErrJoin(a, b error) error {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return fmt.Errorf("%w; %v", a, b)
}

// Compile-time assertion that the reconciler implements the
// controller-runtime Reconciler interface.
var _ reconcile.Reconciler = (*SSHFallbackReconciler)(nil)

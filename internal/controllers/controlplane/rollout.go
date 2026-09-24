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

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// rolloutPlan is the version split of the control-plane machines, computed once
// per reconcile.
type rolloutPlan struct {
	desiredReplicas int32
	maxSurge        int32
	// outdated holds the machines not running spec.version, oldest first.
	outdated []*clusterv1.Machine
	// updated counts machines already at spec.version; updatedJoined counts
	// those among them that have registered a Node.
	updated       int32
	updatedJoined int32
}

// reconcileRollout replaces outdated control-plane machines one at a time. It is
// called only while at least one machine is outdated, and it always returns: the
// plain scale-up and scale-down paths in reconcileMachines do not wait for a
// replacement to join, so letting control fall through to them during a rollout
// removed an old member while its replacement was still booting.
func (r *KairosControlPlaneReconciler) reconcileRollout(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine, plan rolloutPlan) (ctrl.Result, error) {
	currentReplicas := int32(len(machines))

	// A single-node control plane cannot be rolled out by replacement. Its one
	// machine runs the whole cluster (role "single": there is no etcd membership
	// another node could join), so a replacement starts a second, empty cluster,
	// and removing the original afterwards destroys everything the first one
	// held. spec.version is also documented as informational, so an edit to it
	// must never be able to do that. Leave the machine alone and say why.
	if plan.desiredReplicas <= 1 {
		conditions.MarkFalse(kcp, controlplanev1beta2.MachinesUpToDateCondition,
			controlplanev1beta2.SingleNodeRolloutUnsupportedReason, clusterv1.ConditionSeverityWarning,
			"%d control-plane machine(s) do not run spec.version %s, but a single-node control plane cannot be "+
				"rolled out by replacement: a new machine would start a separate, empty cluster. The existing "+
				"machine was left unchanged.", len(plan.outdated), kcp.Spec.Version)
		log.Info("Refusing to roll out a single-node control plane by replacement",
			"outdated", len(plan.outdated), "version", kcp.Spec.Version)
		return ctrl.Result{}, nil
	}

	conditions.MarkFalse(kcp, controlplanev1beta2.MachinesUpToDateCondition,
		controlplanev1beta2.RollingOutReason, clusterv1.ConditionSeverityInfo,
		"%d of %d control-plane machines still to be replaced to reach version %s",
		len(plan.outdated), currentReplicas, kcp.Spec.Version)

	// One membership change at a time. A machine that is already being deleted
	// still counts toward currentReplicas, and a k3s member carries no etcd-leave
	// hook to hold the next step back, so acting now could start removing a second
	// member before the first is gone.
	for _, m := range machines {
		if !m.DeletionTimestamp.IsZero() {
			log.Info("Waiting for a control-plane machine to finish deleting before continuing the rollout", "machine", m.Name)
			return ctrl.Result{RequeueAfter: joinerGateRequeueAfter}, nil
		}
	}

	// Surge: add an up-to-date machine while there is room above the desired count.
	if currentReplicas < plan.desiredReplicas+plan.maxSurge {
		role := r.controlPlaneRoleForNewMachine(plan.desiredReplicas, machines)
		// The surge machine is a joiner like any other: it runs `etcd member
		// add` on boot, so it needs the same quorum-safety gate as the plain
		// scale-up path. Without it a rollout can add a member while a previous
		// joiner is still booting and break quorum (ADR 0005 §E.1).
		if role == bootstrapv1beta2.ControlPlaneRoleJoin {
			joinable, reason, err := r.initMachineJoinable(ctx, kcp, cluster, machines)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to evaluate init machine joinability: %w", err)
			}
			if !joinable {
				log.Info("Holding back rollout surge machine until it is safe to add a member", "reason", reason)
				return ctrl.Result{RequeueAfter: joinerGateRequeueAfter}, nil
			}
		}
		if err := r.createControlPlaneMachine(ctx, log, kcp, cluster, r.nextMachineIndex(machines, kcp.Name), role); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create control plane machine during rollout: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Remove an outdated machine only once every up-to-date machine has joined.
	// Waiting for "updated and joined >= desired", as this used to, can never be
	// satisfied mid-rollout with the default surge of one, which is why the
	// rollout only ever progressed through the unguarded scale-down path.
	if currentReplicas > plan.desiredReplicas && plan.updatedJoined == plan.updated {
		target := plan.outdated[0]
		// ADR 0005 §E.2: refuse a quorum-breaking rollout delete. The guard
		// fails closed and is bypassed only under whole-cluster teardown.
		if ok, reason, err := r.canRemoveMember(ctx, kcp, cluster, target); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to evaluate etcd quorum safety: %w", err)
		} else if !ok {
			log.Info("Holding back outdated-machine rollout — etcd quorum would break", "machine", target.Name, "reason", reason)
			return ctrl.Result{RequeueAfter: joinerGateRequeueAfter}, nil
		}
		// k3s embedded etcd has no supported member-remove (KD-5d); warn that
		// the member will linger. For k0s the etcd-leave sweep drives a clean
		// `k0s etcd leave` while CAPI is paused at the pre-terminate hook.
		r.warnIfK3sEtcdLimitation(kcp, target)
		log.Info("Deleting outdated control plane machine", "machine", target.Name)
		if err := r.Delete(ctx, target); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to delete outdated control plane machine: %w", err)
		}
		return ctrl.Result{}, nil
	}

	log.Info("Waiting for the replacement control-plane machine to join before removing an outdated one",
		"updated", plan.updated, "joined", plan.updatedJoined, "outdated", len(plan.outdated))
	return ctrl.Result{RequeueAfter: joinerGateRequeueAfter}, nil
}

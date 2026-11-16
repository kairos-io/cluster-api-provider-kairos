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
	"sigs.k8s.io/controller-runtime/pkg/client"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// controlPlaneMachineLabels returns the labels every object this control plane
// creates for one replica carries: whatever machineTemplate.metadata asks for,
// with the two labels the controller selects on forced on top.
//
// Forcing is deliberate and matches upstream KubeadmControlPlane
// (ControlPlaneMachineLabels in controlplane/kubeadm/internal/desiredstate):
// getControlPlaneMachines selects Machines by exactly these two labels, so a
// template that set either of them to something else would make its own
// Machines invisible to the controller that owns them.
func controlPlaneMachineLabels(kcp *controlplanev1beta2.KairosControlPlane, clusterName string) map[string]string {
	out := map[string]string{}

	// Copied key by key rather than by reference: the result is mutated by the
	// callers below, and the source map belongs to the KCP spec.
	if md := kcp.Spec.MachineTemplate.Metadata; md != nil {
		for k, v := range md.Labels {
			out[k] = v
		}
	}

	out[clusterv1.ClusterNameLabel] = clusterName
	out[clusterv1.MachineControlPlaneLabel] = ""
	return out
}

// reservedMachineAnnotations are the Machine annotations only the controller
// writes, and reads back as state. machineTemplate.metadata never copies them,
// at create or onto existing Machines: one template value on every Machine
// would overwrite each Machine's own, and the etcd leave would act on a value
// it never wrote (KD-92).
//
//   - renderedNodeNameAnnotation is the node name the etcd leave looks the
//     member up by (KD-91).
//   - The etcd-leave pre-terminate hook is stamped only on k0s HA Machines and
//     removed once the leave is done. From the template it would hook k3s and
//     single-node Machines, and be put back after each removal, so the
//     deletion would never finish. Only this exact key is reserved: other
//     pre-terminate hooks belong to other controllers.
//   - The rest are the leave's own bookkeeping. A template-set
//     etcdLeaveRequestedAtAnnotation, say, would make the leave of every
//     Machine start as already requested: past CAPI's drain, without the
//     write that clears a stale `left`, and with a backdated value straight
//     to the delegation to k0s.
var reservedMachineAnnotations = map[string]bool{
	renderedNodeNameAnnotation:           true,
	etcdLeaveHookAnnotation():            true,
	etcdLeaveRequestedAtAnnotation:       true,
	etcdLeaveUnreachableSinceAnnotation:  true,
	etcdMemberLeaveRequestedAtAnnotation: true,
	etcdMemberLeaveMemberIDAnnotation:    true,
}

// controlPlaneMachineAnnotations returns the annotations from
// machineTemplate.metadata, without reservedMachineAnnotations. Unlike the
// labels there is nothing to force: the etcd-leave pre-terminate hook and the
// recorded node name are stamped by the caller afterwards, where they apply.
func controlPlaneMachineAnnotations(kcp *controlplanev1beta2.KairosControlPlane) map[string]string {
	out := map[string]string{}
	if md := kcp.Spec.MachineTemplate.Metadata; md != nil {
		for k, v := range md.Annotations {
			if reservedMachineAnnotations[k] {
				continue
			}
			out[k] = v
		}
	}
	return out
}

// leaveBookkeepingAnnotations are the etcd leave's own state on a Machine.
// Only reconcileMemberLeave writes them, and the sweep runs it only on a
// Machine being deleted, which is never undone. So on a Machine that is not
// being deleted, any of them was set by someone else: by
// machineTemplate.metadata before it was reserved (KD-92), or by hand.
// reconcileMachineMetadata removes them there, so that the Machine's leave,
// when it comes, starts from the beginning.
//
// The etcd-leave hook is not among them. The hook decides whether a Machine
// gets a leave (ADR 0016 D4), so the controller never removes it from a
// Machine that is not being deleted, whatever else says otherwise.
var leaveBookkeepingAnnotations = []string{
	etcdLeaveRequestedAtAnnotation,
	etcdLeaveUnreachableSinceAnnotation,
	etcdMemberLeaveRequestedAtAnnotation,
	etcdMemberLeaveMemberIDAnnotation,
}

// reconcileMachineMetadata carries the current machineTemplate.metadata onto
// the Machines that already exist, so editing a label on the control plane
// reaches a running cluster instead of waiting for a rollout, and removes
// leaveBookkeepingAnnotations from them, in the same patch.
//
// The merge is additive on purpose. This controller does not own the whole
// metadata of a Machine: the etcd-leave pre-terminate hook lives in the
// annotations, CAPI stamps its own, and a MachineHealthCheck or an operator
// may have added more. Replacing the maps wholesale, which upstream KCP can
// afford because it recomputes a full desired Machine, would strip those.
// Removing a key from the template therefore leaves it on Machines that
// already carry it; docs/API_REFERENCE.md says so. reservedMachineAnnotations
// are never carried (controlPlaneMachineAnnotations drops them).
//
// A Machine being deleted is left exactly as it is, as upstream KCP's
// syncMachines leaves it but for the fields that affect its teardown (here
// reconcileNodeDrainTimeout). Its labels and annotations no longer reach a
// Node that is going away, and carrying them would put back what another
// controller removed to let the deletion go on: its own pre-terminate or
// pre-drain hook, for one (KD-92).
func (r *KairosControlPlaneReconciler) reconcileMachineMetadata(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine) error {
	desiredLabels := controlPlaneMachineLabels(kcp, cluster.Name)
	desiredAnnotations := controlPlaneMachineAnnotations(kcp)

	for _, machine := range machines {
		if !machine.DeletionTimestamp.IsZero() {
			continue
		}
		patchHelper := client.MergeFrom(machine.DeepCopy())

		changed := false
		for k, v := range desiredLabels {
			if existing, ok := machine.Labels[k]; ok && existing == v {
				continue
			}
			if machine.Labels == nil {
				machine.Labels = map[string]string{}
			}
			machine.Labels[k] = v
			changed = true
		}
		for k, v := range desiredAnnotations {
			if existing, ok := machine.Annotations[k]; ok && existing == v {
				continue
			}
			if machine.Annotations == nil {
				machine.Annotations = map[string]string{}
			}
			machine.Annotations[k] = v
			changed = true
		}
		var cleared []string
		for _, k := range leaveBookkeepingAnnotations {
			if _, ok := machine.Annotations[k]; ok {
				delete(machine.Annotations, k)
				cleared = append(cleared, k)
				changed = true
			}
		}

		if !changed {
			continue
		}

		if err := r.Patch(ctx, machine, patchHelper); err != nil {
			return fmt.Errorf("failed to update metadata on machine %s: %w", machine.Name, err)
		}
		if len(cleared) > 0 {
			log.Info("Removed etcd-leave state from a control-plane Machine that is not being deleted; the controller writes it only during a deletion",
				"machine", machine.Name, "annotations", cleared)
		}
		log.Info("Updated control plane machine metadata from machineTemplate", "machine", machine.Name)
	}

	return nil
}

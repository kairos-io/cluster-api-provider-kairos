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

// controlPlaneMachineAnnotations returns the annotations from
// machineTemplate.metadata. Unlike the labels there is nothing to force: the
// etcd-leave pre-terminate hook is stamped by the caller afterwards, so it
// wins over a template that spells the same key.
func controlPlaneMachineAnnotations(kcp *controlplanev1beta2.KairosControlPlane) map[string]string {
	out := map[string]string{}
	if md := kcp.Spec.MachineTemplate.Metadata; md != nil {
		for k, v := range md.Annotations {
			out[k] = v
		}
	}
	return out
}

// reconcileMachineMetadata carries the current machineTemplate.metadata onto
// the Machines that already exist, so editing a label on the control plane
// reaches a running cluster instead of waiting for a rollout.
//
// The merge is additive on purpose. This controller does not own the whole
// metadata of a Machine: the etcd-leave pre-terminate hook lives in the
// annotations, CAPI stamps its own, and a MachineHealthCheck or an operator
// may have added more. Replacing the maps wholesale, which upstream KCP can
// afford because it recomputes a full desired Machine, would strip those.
// Removing a key from the template therefore leaves it on Machines that
// already carry it; docs/API_REFERENCE.md says so.
func (r *KairosControlPlaneReconciler) reconcileMachineMetadata(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine) error {
	desiredLabels := controlPlaneMachineLabels(kcp, cluster.Name)
	desiredAnnotations := controlPlaneMachineAnnotations(kcp)

	for _, machine := range machines {
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

		if !changed {
			continue
		}

		if err := r.Patch(ctx, machine, patchHelper); err != nil {
			return fmt.Errorf("failed to update metadata on machine %s: %w", machine.Name, err)
		}
		log.Info("Updated control plane machine metadata from machineTemplate", "machine", machine.Name)
	}

	return nil
}

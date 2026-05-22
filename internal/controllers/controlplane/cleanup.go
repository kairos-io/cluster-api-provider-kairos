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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// drainOwnedMachines enumerates CAPI Machines owned by this KairosControlPlane,
// issues a Delete for any that are not already being deleted, and returns the
// count of Machines that are still present (including those mid-deletion).
// Callers use the remaining count to decide whether to requeue or proceed
// with finalizer removal.
//
// Selection criteria (both must hold):
//
//  1. Machine has the `cluster.x-k8s.io/cluster-name` label matching the
//     cluster-name label on the KCP. Without a cluster-name on the KCP we
//     cannot scope the drain safely, so we return 0 / nil and let the
//     finalizer be removed -- there is nothing we can act on.
//  2. Machine's controlling OwnerReference Kind=="KairosControlPlane" AND
//     UID matches the KCP's UID. Matching by UID (not just Name) prevents a
//     foreign Machine with the same name from being deleted if the KCP was
//     recreated. Foreign-owned Machines (different UID or different Kind)
//     are deliberately NOT deleted and NOT counted in `remaining` -- they
//     are not ours to drain.
//
// `IsNotFound` from Delete is treated as success (the Machine was already
// reaped between List and Delete). The CAPI Machine controller is
// responsible for cascading to KairosConfig and the infrastructure Machine
// via OwnerReferences, so this function does not delete those directly.
// (KD-4.)
func drainOwnedMachines(ctx context.Context, c client.Client, kcp *controlplanev1beta2.KairosControlPlane) (remaining int, err error) {
	clusterName, ok := kcp.Labels[clusterv1.ClusterNameLabel]
	if !ok || clusterName == "" {
		// We have no cluster-name label to scope the drain. This happens
		// when the KCP was deleted before ever finding its parent Cluster.
		// There are no Machines we can confidently own in this state.
		return 0, nil
	}

	selector := labels.SelectorFromSet(labels.Set{
		clusterv1.ClusterNameLabel: clusterName,
	})
	machineList := &clusterv1.MachineList{}
	if err := c.List(ctx, machineList,
		client.InNamespace(kcp.Namespace),
		client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		return 0, err
	}

	kcpUID := kcp.UID
	for i := range machineList.Items {
		m := &machineList.Items[i]
		if !ownedByKCP(m, kcpUID) {
			continue
		}
		remaining++
		if !m.DeletionTimestamp.IsZero() {
			// Already being deleted; nothing for us to do, but it counts
			// against `remaining` so the caller keeps requeuing.
			continue
		}
		if err := c.Delete(ctx, m); err != nil {
			if apierrors.IsNotFound(err) {
				// Reaped between List and Delete; not really remaining.
				remaining--
				continue
			}
			return remaining, err
		}
	}
	return remaining, nil
}

// ownedByKCP reports whether machine is owned (controller=true) by a
// KairosControlPlane with the given UID. Matching by UID prevents a foreign
// Machine that happens to share a name (e.g. across a delete+recreate) from
// being misidentified as ours.
func ownedByKCP(machine *clusterv1.Machine, kcpUID types.UID) bool {
	for _, ref := range machine.OwnerReferences {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		if ref.Kind != "KairosControlPlane" {
			continue
		}
		if ref.UID == kcpUID {
			return true
		}
	}
	return false
}

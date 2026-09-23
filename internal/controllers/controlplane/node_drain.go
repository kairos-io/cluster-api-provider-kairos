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
	"math"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// nodeDrainTimeoutSeconds converts the machine template's nodeDrainTimeout into
// the unit the CAPI Machine contract uses. A nil timeout stays nil.
//
// Cluster API reads the result in nodeDrainTimeoutExceeded, which treats every
// value <= 0 the same as an unset one and drains with no deadline. So zero does
// not bound the drain, and a negative duration never meant "never drain"
// either: both already land on CAPI's default.
//
// The value is nonetheless clamped into [0, math.MaxInt32], because the Machine
// CRD declares nodeDrainTimeoutSeconds with minimum: 0. Out of that range the
// API server rejects the object, which costs every create and patch of the
// Machine rather than merely mis-timing a drain.
//
// Bounding the int64 before it is narrowed is what closes the overflow.
// metav1.Duration carries int64 nanoseconds, so a timeout past roughly 68
// years exceeds math.MaxInt32 seconds, and int32 would wrap it into a short
// deadline or a rejected negative one: the one input that inverts the field's
// meaning instead of just rounding it.
func nodeDrainTimeoutSeconds(d *metav1.Duration) *int32 {
	if d == nil {
		return nil
	}

	seconds := int64(d.Duration.Seconds())
	switch {
	case seconds < 0:
		seconds = 0
	case seconds > math.MaxInt32:
		seconds = math.MaxInt32
	}

	out := int32(seconds)
	return &out
}

// applyNodeDrainTimeout stamps the desired drain deadline on a Machine spec.
func applyNodeDrainTimeout(spec *clusterv1.MachineSpec, kcp *controlplanev1beta2.KairosControlPlane) {
	spec.Deletion.NodeDrainTimeoutSeconds = nodeDrainTimeoutSeconds(kcp.Spec.MachineTemplate.NodeDrainTimeout)
}

// reconcileNodeDrainTimeout brings already-created Machines in line with the
// current machineTemplate.nodeDrainTimeout.
//
// The field only has an effect at deletion time, and the moment an operator
// reaches for it is usually a rollout that is already stalling on an
// undrainable pod. Stamping it at creation alone would make the fix useless in
// exactly that case, so the value is reconciled on every pass, including on
// Machines that are already terminating -- CAPI's Machine controller re-reads
// the deadline while it drains, so raising or lowering it unblocks a drain in
// flight.
func (r *KairosControlPlaneReconciler) reconcileNodeDrainTimeout(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, machines []*clusterv1.Machine) error {
	desired := nodeDrainTimeoutSeconds(kcp.Spec.MachineTemplate.NodeDrainTimeout)

	for _, machine := range machines {
		if equalInt32Ptr(machine.Spec.Deletion.NodeDrainTimeoutSeconds, desired) {
			continue
		}

		patch := client.MergeFrom(machine.DeepCopy())
		machine.Spec.Deletion.NodeDrainTimeoutSeconds = desired
		if err := r.Patch(ctx, machine, patch); err != nil {
			return fmt.Errorf("failed to patch nodeDrainTimeout on machine %s: %w", machine.Name, err)
		}
		log.Info("Updated control-plane machine drain deadline", "machine", machine.Name, "nodeDrainTimeoutSeconds", desired)
	}

	return nil
}

func equalInt32Ptr(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

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

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// leaveGuardMachines returns the Machines the etcd leave's guards check a
// leaving Machine against: machines, the KairosControlPlane's own, together
// with every other control-plane Machine of cluster, whoever owns it (KD-93).
//
// The guards keep the leave off a member that may be another Machine's: an
// EtcdMember whose peer address another Machine reports
// (etcdMemberPeerOwner), or a node name another Machine's node may go by
// (otherMachineWithNodeName). The leave-requested sentinel is keyed by name
// alone and the node of that name acts on it by itself, so a control-plane
// Machine this KCP does not own (left behind by another control plane, or
// orphaned when its owner was removed) is just as possible an owner, and
// expectedEtcdMembers already counts it. machines are kept as given, so the
// guards never check against fewer Machines than they did before.
func (r *KairosControlPlaneReconciler) leaveGuardMachines(ctx context.Context, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine) ([]*clusterv1.Machine, error) {
	all, err := r.clusterControlPlaneMachines(ctx, kcp, cluster)
	if err != nil {
		return nil, err
	}
	out := append(make([]*clusterv1.Machine, 0, len(machines)+len(all)), machines...)
	seen := make(map[string]bool, len(machines))
	for _, m := range machines {
		seen[m.Name] = true
	}
	for _, m := range all {
		if !seen[m.Name] {
			out = append(out, m)
		}
	}
	return out, nil
}

// leaveGuardList returns leaveGuardMachines, listed at most once per pass of
// reconcileMemberLeave, so every guard of a pass checks against the same
// snapshot.
type leaveGuardList func() ([]*clusterv1.Machine, error)

// leaveGuardListFor returns the leaveGuardList of one pass. Nothing is listed
// until a guard asks: a pass that runs no guard, such as a registered
// Machine's ack, never depends on the List.
func (r *KairosControlPlaneReconciler) leaveGuardListFor(ctx context.Context, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine) leaveGuardList {
	var (
		listed bool
		out    []*clusterv1.Machine
		err    error
	)
	return func() ([]*clusterv1.Machine, error) {
		if !listed {
			out, err = r.leaveGuardMachines(ctx, kcp, cluster, machines)
			listed = true
		}
		return out, err
	}
}

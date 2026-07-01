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
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// etcdStatusSecretName returns the name of the per-cluster HA etcd-status Secret.
func etcdStatusSecretName(clusterName string) string {
	return bootstrapv1beta2.EtcdStatusSecretName(clusterName)
}

// ensureEtcdStatusSecret guarantees the per-cluster HA etcd-status Secret exists,
// owner-ref'd to the *Cluster* (ADR 0005 §E.1) and labeled with the cluster-name
// label (KD-15). It is idempotent and created EMPTY: every control-plane node
// PATCHes its own member key (member id, voting, healthy, member-list count)
// over the node-push channel — the same channel as the join token — and the
// controller reads it to compute etcd health (EtcdHealthyCondition) and the
// quorum-safe-replacement guard (canRemoveMember).
//
// OWNERSHIP — the Cluster, NOT the KairosControlPlane (contrast
// ensureJoinTokenSecret): this is a *multi-writer* per-cluster shared object —
// every control-plane node's SA writes its own member key. It must GC with the
// Cluster and must present the SAME controller owner to every writer's
// CreateOrUpdate. Owning it by a per-Machine object would re-open the multi-node
// "already owned by another controller" bug fixed in ADR 0005 §D.2; owning it by
// the KCP would strand it across a KCP replace. The mutate func NEVER clears
// existing Data — that would wipe node-reported health on every reconcile.
//
// Gated by the caller on desiredReplicas > 1 (single-node clusters have no etcd
// quorum to guard and no peers to report about).
func (r *KairosControlPlaneReconciler) ensureEtcdStatusSecret(ctx context.Context, log logr.Logger, cluster *clusterv1.Cluster) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      etcdStatusSecretName(cluster.Name),
			Namespace: cluster.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels[clusterv1.ClusterNameLabel] = cluster.Name
		secret.Labels[bootstrapv1beta2.EtcdStatusSecretTypeLabel] = bootstrapv1beta2.EtcdStatusSecretTypeValue

		// Created empty; per-node reporters populate their own member keys. Do
		// NOT touch secret.Data here — the node-authored keys must survive every
		// controller reconcile.

		// Owner-ref to the Cluster so the Secret cascades on Cluster delete and
		// every CP node's writer sees the same, stable controller owner.
		return controllerutil.SetControllerReference(cluster, secret, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensure etcd-status secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	log.V(4).Info("Ensured HA etcd-status secret",
		"secret", etcdStatusSecretName(cluster.Name), "namespace", cluster.Namespace)
	return nil
}

// etcdMemberStatus is the per-member health record a control-plane node reports
// into the etcd-status Secret (ADR 0005 §E.1). It is non-secret cluster
// metadata. The wire form is the compact JSON the node-side reporter builds; the
// controller only READS it (it is never authored controller-side).
type etcdMemberStatus struct {
	Name       string `json:"name"`
	Healthy    bool   `json:"healthy"`
	Voting     bool   `json:"voting"`
	Members    int    `json:"members"`
	ReportedAt string `json:"reportedAt"`
}

// readEtcdStatus loads the per-cluster etcd-status Secret and parses each
// member's reported status, keyed by member key (the reporting node's sanitized
// hostname == its Node name). A missing Secret returns an empty map (the cluster
// is pre-HA or no node has reported yet), not an error. Malformed entries (a
// node wrote garbage, or a future schema) are skipped rather than failing the
// whole read — a single bad key must not blind the controller to healthy peers.
func (r *KairosControlPlaneReconciler) readEtcdStatus(ctx context.Context, cluster *clusterv1.Cluster) (map[string]etcdMemberStatus, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: etcdStatusSecretName(cluster.Name), Namespace: cluster.Namespace}
	if err := r.Client.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return map[string]etcdMemberStatus{}, nil
		}
		return nil, fmt.Errorf("read etcd-status secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	out := make(map[string]etcdMemberStatus, len(secret.Data))
	for member, raw := range secret.Data {
		var st etcdMemberStatus
		if err := json.Unmarshal(raw, &st); err != nil {
			continue
		}
		out[member] = st
	}
	return out, nil
}

// etcdVotingHealthyCount returns the number of members reporting BOTH healthy and
// voting — the population that counts toward etcd quorum (ADR 0005 §E.2/§E.4).
func etcdVotingHealthyCount(status map[string]etcdMemberStatus) int {
	n := 0
	for _, st := range status {
		if st.Healthy && st.Voting {
			n++
		}
	}
	return n
}

// canRemoveMember reports whether deleting `target` (a control-plane Machine) is
// safe for etcd quorum (ADR 0005 §E.2). It guards every NON-teardown CP-Machine
// delete site (rollout replacement, scale-down); reconcileDelete does NOT call it
// (whole-cluster teardown is the bypass).
//
// SECURITY (ADR 0005 §E.5, security-architect ruling — must-honor constraints):
//   - FAILS CLOSED: a transient read error, or a live Running target with NO
//     etcd-status report, refuses the delete ("cannot prove quorum safety") — a
//     missing report on a live node may be a silent voting member.
//   - BYPASS is EXACT: only whole-cluster teardown (kcp.DeletionTimestamp set)
//     bypasses. A per-Machine deletion timestamp, rollout, or scale-down never
//     reaches the bypass.
//   - The failed-node fast path (allow the delete of a presumed-dead member) is
//     gated on the OBJECTIVE Machine phase / NodeRef — never on the
//     attacker-influenceable etcd self-report.
//   - The quorum population is the healthy+voting REPORTERS (etcdVotingHealthyCount),
//     never the node-reported `members` list length (unvalidated, forgeable).
func (r *KairosControlPlaneReconciler) canRemoveMember(ctx context.Context, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, target *clusterv1.Machine) (bool, string, error) {
	// Whole-cluster teardown must never deadlock on quorum (the single bypass).
	if !kcp.ObjectMeta.DeletionTimestamp.IsZero() {
		return true, "", nil
	}
	desiredReplicas := int32(1)
	if kcp.Spec.Replicas != nil {
		desiredReplicas = *kcp.Spec.Replicas
	}
	// Single-node (or unset) control planes have no etcd quorum to protect.
	if desiredReplicas <= 1 {
		return true, "", nil
	}
	// Failed-node fast path — OBJECTIVE liveness only (Machine phase / NodeRef),
	// NOT the etcd self-report: a target that never registered a Node or is not
	// Running is presumed dead/failed, is not a healthy voting member, and cannot
	// reduce the healthy-voting count — allow. This is the primary replacement
	// case for an unreachable node.
	if target.Status.NodeRef == nil || target.Status.Phase != string(clusterv1.MachinePhaseRunning) {
		return true, "", nil
	}
	// The target is a live, Running control-plane node. Prove that removing it
	// keeps etcd quorum from the node-reported etcd-status Secret.
	status, err := r.readEtcdStatus(ctx, cluster)
	if err != nil {
		return false, "", err // transient read error: caller requeues, delete withheld
	}
	targetKey := target.Status.NodeRef.Name
	st, reported := status[targetKey]
	if !reported {
		// A live CP node with no etcd report may be a silent voting member whose
		// report is stale — cannot prove safety, fail closed.
		return false, "target is a live control-plane node with no etcd-status report; cannot prove quorum safety", nil
	}
	voting := etcdVotingHealthyCount(status)
	postRemoval := voting
	if st.Healthy && st.Voting {
		postRemoval = voting - 1 // removing a member currently counted toward quorum
	}
	quorum := int(desiredReplicas/2 + 1)
	if postRemoval < quorum {
		return false, fmt.Sprintf(
			"removing this member would leave %d healthy voting etcd members, below the quorum minimum ((N/2)+1=%d)",
			postRemoval, quorum), nil
	}
	return true, "", nil
}

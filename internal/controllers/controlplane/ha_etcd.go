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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
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

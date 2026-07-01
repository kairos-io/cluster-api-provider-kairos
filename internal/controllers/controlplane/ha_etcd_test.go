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
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// TestEnsureEtcdStatusSecret_ClusterOwnedEmptyIdempotent asserts the per-cluster
// etcd-status Secret (ADR 0005 §E.1) is:
//   - created EMPTY (nodes populate their own member keys over node-push),
//   - owner-ref'd to the *Cluster* — NOT the KCP: it is a multi-writer shared
//     object, and Cluster ownership is what avoids the §D.2 "already owned by
//     another controller" multi-node bug and GCs it with the cluster,
//   - labeled for the KD-15 label-filtered watch, and
//   - idempotent: a second reconcile must NOT wipe node-reported member data.
func TestEnsureEtcdStatusSecret_ClusterOwnedEmptyIdempotent(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default", UID: "cluster-uid"}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.ensureEtcdStatusSecret(context.Background(), log.Log, cluster)).To(Succeed())

	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: etcdStatusSecretName("c"), Namespace: "default"}
	g.Expect(c.Get(context.Background(), key, secret)).To(Succeed())

	// Empty on create.
	g.Expect(secret.Data).To(BeEmpty(), "etcd-status Secret is filled by the CP nodes, not the controller")
	// Labels for the label-filtered watch (KD-15).
	g.Expect(secret.Labels).To(HaveKeyWithValue(clusterv1.ClusterNameLabel, "c"))
	g.Expect(secret.Labels).To(HaveKeyWithValue(bootstrapv1beta2.EtcdStatusSecretTypeLabel, bootstrapv1beta2.EtcdStatusSecretTypeValue))
	// Owned by the Cluster, NOT the KCP (contrast the join-token Secret).
	g.Expect(secret.OwnerReferences).To(HaveLen(1))
	g.Expect(secret.OwnerReferences[0].Kind).To(Equal("Cluster"))
	g.Expect(secret.OwnerReferences[0].UID).To(Equal(cluster.UID))

	// Simulate a node reporting its member health, then reconcile again: the
	// controller must NOT clobber node-authored data.
	secret.Data = map[string][]byte{"member-cp-0": []byte(`{"healthy":true}`)}
	g.Expect(c.Update(context.Background(), secret)).To(Succeed())

	g.Expect(r.ensureEtcdStatusSecret(context.Background(), log.Log, cluster)).To(Succeed())
	g.Expect(c.Get(context.Background(), key, secret)).To(Succeed())
	g.Expect(secret.Data).To(HaveKeyWithValue("member-cp-0", []byte(`{"healthy":true}`)),
		"node-reported member data must survive controller reconcile")
}

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
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
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

// etcdStatusSecretWith builds a per-cluster etcd-status Secret whose data holds
// `voting` healthy+voting members plus one unhealthy member, for the readers and
// condition tests.
func etcdStatusSecretWith(cluster string, voting int) *corev1.Secret {
	data := map[string][]byte{}
	for i := 0; i < voting; i++ {
		name := "cp-h" + string(rune('0'+i))
		data[name] = []byte(`{"name":"` + name + `","healthy":true,"voting":true,"members":3,"reportedAt":"t"}`)
	}
	data["cp-down"] = []byte(`{"name":"cp-down","healthy":false,"voting":true,"members":3,"reportedAt":"t"}`)
	data["cp-junk"] = []byte(`this is not json`)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: etcdStatusSecretName(cluster), Namespace: "default"},
		Data:       data,
	}
}

// TestReadEtcdStatus_ParsesCountsSkipsMalformed asserts the reader parses valid
// member reports, skips malformed entries (a node wrote garbage), and that the
// voting-healthy count excludes unhealthy members.
func TestReadEtcdStatus_ParsesCountsSkipsMalformed(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(etcdStatusSecretWith("c", 2)).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	status, err := r.readEtcdStatus(context.Background(), cluster)
	g.Expect(err).ToNot(HaveOccurred())
	// 2 healthy + 1 unhealthy parsed; the malformed "cp-junk" entry is skipped.
	g.Expect(status).To(HaveLen(3))
	g.Expect(etcdVotingHealthyCount(status)).To(Equal(2))
}

// TestReadEtcdStatus_MissingSecretEmpty asserts a missing Secret is not an error
// (pre-HA / no node has reported yet).
func TestReadEtcdStatus_MissingSecretEmpty(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	status, err := r.readEtcdStatus(context.Background(), cluster)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(status).To(BeEmpty())
}

// TestSetHAConditions_EtcdHealthy pins the EtcdHealthyCondition mapping
// (ADR 0005 §E.4): True when all desired members are healthy+voting; Info when
// quorum holds with a margin; Warning at or below the (N/2)+1 razor's edge.
func TestSetHAConditions_EtcdHealthy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		desired    int32
		voting     int
		wantStatus corev1.ConditionStatus
		wantReason string
	}{
		{"3-node all healthy", 3, 3, corev1.ConditionTrue, ""},
		{"3-node one down (at quorum)", 3, 2, corev1.ConditionFalse, controlplanev1beta2.EtcdQuorumAtRiskReason},
		{"5-node one down (margin)", 5, 4, corev1.ConditionFalse, controlplanev1beta2.EtcdQuorumDegradedReason},
		{"5-node two down (at quorum)", 5, 3, corev1.ConditionFalse, controlplanev1beta2.EtcdQuorumAtRiskReason},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := haTestScheme(g)
			cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
			kcp := &controlplanev1beta2.KairosControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "kcp", Namespace: "default"},
				Spec: controlplanev1beta2.KairosControlPlaneSpec{
					Replicas: ptr.To(tc.desired),
					HA:       &controlplanev1beta2.HAConfig{VIP: &controlplanev1beta2.KubeVIPConfig{Address: "10.0.0.1", Interface: "eth0", Mode: "ARP"}},
				},
				Status: controlplanev1beta2.KairosControlPlaneStatus{ReadyReplicas: tc.desired},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(etcdStatusSecretWith("c", tc.voting)).Build()
			r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

			r.setHAConditions(context.Background(), kcp, cluster, true)

			cond := conditions.Get(kcp, controlplanev1beta2.EtcdHealthyCondition)
			g.Expect(cond).ToNot(BeNil(), "EtcdHealthyCondition must be set for HA")
			g.Expect(cond.Status).To(Equal(tc.wantStatus))
			// True conditions carry no reason (CAPI convention, like ControlPlaneJoined).
			if tc.wantStatus == corev1.ConditionFalse {
				g.Expect(cond.Reason).To(Equal(tc.wantReason))
			}
		})
	}
}

// TestInitMachineJoinable_GatesOnEtcdReport asserts the ADR 0005 §E.1 joiner-gate
// graduation: with NodeRef + KubeconfigReady + join-token all satisfied, a k0s
// init is still NOT joinable until it reports a healthy voting etcd member, and
// becomes joinable once it does.
func TestInitMachineJoinable_GatesOnEtcdReport(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "kcp", Namespace: "default"},
		Spec:       controlplanev1beta2.KairosControlPlaneSpec{Distribution: "k0s", Replicas: ptr.To(int32(3))},
	}
	conditions.MarkTrue(kcp, controlplanev1beta2.KubeconfigReadyCondition)
	init := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-0", Namespace: "default"},
		Status:     clusterv1.MachineStatus{NodeRef: &corev1.ObjectReference{Name: "cp-0"}},
	}
	jt := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: joinTokenSecretName("c"), Namespace: "default"},
		Data:       map[string][]byte{joinTokenSecretDataKey: []byte("tok")},
	}

	// No etcd report yet: NOT joinable (k0s strict).
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(jt).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}
	joinable, reason, err := r.initMachineJoinable(context.Background(), kcp, cluster, []*clusterv1.Machine{init})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(joinable).To(BeFalse())
	g.Expect(reason).To(ContainSubstring("etcd member"))

	// Init reports healthy+voting: joinable.
	es := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: etcdStatusSecretName("c"), Namespace: "default"},
		Data:       map[string][]byte{"cp-0": []byte(`{"name":"cp-0","healthy":true,"voting":true,"members":1,"reportedAt":"t"}`)},
	}
	c2 := fake.NewClientBuilder().WithScheme(scheme).WithObjects(jt, es).Build()
	r2 := &KairosControlPlaneReconciler{Client: c2, Scheme: scheme}
	joinable, _, err = r2.initMachineJoinable(context.Background(), kcp, cluster, []*clusterv1.Machine{init})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(joinable).To(BeTrue())
}

// TestCanRemoveMember pins the ADR 0005 §E.2/§E.5 quorum guard: exact teardown
// bypass, objective failed-node fast path (Machine phase, not the etcd
// self-report), fail-closed on a live-but-unreported target, and correct quorum
// arithmetic over healthy+voting reporters.
func TestCanRemoveMember(t *testing.T) {
	mkMachine := func(name, node, phase string) *clusterv1.Machine {
		m := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
		m.Status.Phase = phase
		if node != "" {
			m.Status.NodeRef = &corev1.ObjectReference{Name: node}
		}
		return m
	}
	type member struct{ healthy, voting bool }
	running := string(clusterv1.MachinePhaseRunning)
	for _, tc := range []struct {
		name        string
		desired     int32
		members     map[string]member
		target      *clusterv1.Machine
		deleting    bool
		wantAllowed bool
	}{
		{"teardown bypass allows even at zero quorum", 3, nil, mkMachine("cp-0", "cp-0", running), true, true},
		{"single-node has no quorum to guard", 1, nil, mkMachine("cp-0", "cp-0", running), false, true},
		{"failed node (no NodeRef) fast-path allows", 3,
			map[string]member{"cp-1": {true, true}, "cp-2": {true, true}}, mkMachine("cp-0", "", "Provisioning"), false, true},
		{"not-Running node fast-path allows", 3,
			map[string]member{"cp-1": {true, true}, "cp-2": {true, true}}, mkMachine("cp-0", "cp-0", "Deleting"), false, true},
		{"3-node all healthy, remove one keeps quorum", 3,
			map[string]member{"cp-0": {true, true}, "cp-1": {true, true}, "cp-2": {true, true}}, mkMachine("cp-0", "cp-0", running), false, true},
		{"3-node one down, removing a healthy one breaks quorum", 3,
			map[string]member{"cp-0": {true, true}, "cp-1": {true, true}, "cp-2": {false, true}}, mkMachine("cp-0", "cp-0", running), false, false},
		{"live node with no etcd report fails closed", 3,
			map[string]member{"cp-1": {true, true}, "cp-2": {true, true}}, mkMachine("cp-0", "cp-0", running), false, false},
		{"removing an unhealthy member keeps quorum", 3,
			map[string]member{"cp-0": {false, true}, "cp-1": {true, true}, "cp-2": {true, true}}, mkMachine("cp-0", "cp-0", running), false, true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := haTestScheme(g)
			cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
			kcp := &controlplanev1beta2.KairosControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "kcp", Namespace: "default"},
				Spec:       controlplanev1beta2.KairosControlPlaneSpec{Replicas: ptr.To(tc.desired)},
			}
			if tc.deleting {
				now := metav1.Now()
				kcp.DeletionTimestamp = &now
			}
			objs := []client.Object{}
			if tc.members != nil {
				data := map[string][]byte{}
				for name, m := range tc.members {
					data[name] = []byte(fmt.Sprintf(`{"name":%q,"healthy":%t,"voting":%t,"members":3,"reportedAt":"t"}`, name, m.healthy, m.voting))
				}
				objs = append(objs, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: etcdStatusSecretName("c"), Namespace: "default"},
					Data:       data,
				})
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

			allowed, reason, err := r.canRemoveMember(context.Background(), kcp, cluster, tc.target)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(allowed).To(Equal(tc.wantAllowed), "reason=%q", reason)
		})
	}
}

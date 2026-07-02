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
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

const testClusterName = "c"

// TestEtcdLeaveHookAnnotation pins the exact hook key CAPI recognizes — a
// mismatch would silently disable the pause and orphan etcd members.
func TestEtcdLeaveHookAnnotation(t *testing.T) {
	g := NewWithT(t)
	g.Expect(etcdLeaveHookAnnotation()).To(Equal("pre-terminate.delete.hook.machine.cluster.x-k8s.io/kairos-etcd-leave"))
}

// TestShouldStampEtcdLeaveHook: only k0s init/join are hooked. Single-node has no
// etcd cluster; k3s has no supported member-remove (KD-5d).
func TestShouldStampEtcdLeaveHook(t *testing.T) {
	mk := func(dist string) *controlplanev1beta2.KairosControlPlane {
		return &controlplanev1beta2.KairosControlPlane{Spec: controlplanev1beta2.KairosControlPlaneSpec{Distribution: dist}}
	}
	for _, tc := range []struct {
		name string
		kcp  *controlplanev1beta2.KairosControlPlane
		role bootstrapv1beta2.ControlPlaneRole
		want bool
	}{
		{"k0s init", mk("k0s"), bootstrapv1beta2.ControlPlaneRoleInit, true},
		{"k0s join", mk("k0s"), bootstrapv1beta2.ControlPlaneRoleJoin, true},
		{"k0s single", mk("k0s"), bootstrapv1beta2.ControlPlaneRoleSingle, false},
		{"empty dist defaults k0s init", mk(""), bootstrapv1beta2.ControlPlaneRoleInit, true},
		{"empty dist single", mk(""), bootstrapv1beta2.ControlPlaneRoleSingle, false},
		{"k3s init", mk("k3s"), bootstrapv1beta2.ControlPlaneRoleInit, false},
		{"k3s join", mk("k3s"), bootstrapv1beta2.ControlPlaneRoleJoin, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(shouldStampEtcdLeaveHook(tc.kcp, tc.role)).To(Equal(tc.want))
		})
	}
}

// hookedMachine builds a k0s HA control-plane Machine carrying the etcd-leave
// hook, with the given node name (empty → no NodeRef).
func hookedMachine(name, node string, extraAnnotations map[string]string) *clusterv1.Machine {
	ann := map[string]string{etcdLeaveHookAnnotation(): ""}
	for k, v := range extraAnnotations {
		ann[k] = v
	}
	m := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: ann},
	}
	if node != "" {
		m.Status.NodeRef = &corev1.ObjectReference{Name: node}
	}
	return m
}

func k0sKCP() *controlplanev1beta2.KairosControlPlane {
	return &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "kcp", Namespace: "default"},
		Spec:       controlplanev1beta2.KairosControlPlaneSpec{Distribution: "k0s", Replicas: ptr.To(int32(3))},
	}
}

func testCluster() *clusterv1.Cluster {
	return &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: "default"}}
}

// leaveConfigMap builds the workload-cluster kube-system/kairos-etcd-leave
// ConfigMap with the given data.
func leaveConfigMap(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: etcdLeaveConfigMapName, Namespace: etcdLeaveConfigMapNamespace},
		Data:       data,
	}
}

// etcdStatusSecretForMembers builds the per-cluster etcd-status Secret with the
// given member keys present (values are minimal valid JSON).
func etcdStatusSecretForMembers(members ...string) *corev1.Secret {
	data := map[string][]byte{}
	for _, m := range members {
		data[m] = []byte(`{"name":"` + m + `","healthy":true,"voting":true,"members":3,"reportedAt":"t"}`)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: etcdStatusSecretName(testClusterName), Namespace: "default"},
		Data:       data,
	}
}

// TestReconcileMemberLeave_NoNodeRefFastPath: a Machine that never registered a
// Node has no member to leave — hook removed, done, and NO workload client built.
func TestReconcileMemberLeave_NoNodeRefFastPath(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	target := hookedMachine("cp-0", "", nil)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).Build()

	factoryCalled := false
	r := &KairosControlPlaneReconciler{
		Client: c, Scheme: scheme,
		WorkloadClientFactory: func(context.Context, *clusterv1.Cluster) (client.Client, error) {
			factoryCalled = true
			return nil, nil
		},
	}
	done, err := r.reconcileMemberLeave(context.Background(), log.Log, k0sKCP(), testCluster(), target)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeTrue())
	g.Expect(factoryCalled).To(BeFalse(), "no NodeRef must not reach the workload cluster")
	assertHookGone(g, c, "cp-0")
}

// TestReconcileMemberLeave_NonK0sRemovesHook: defensive — a non-k0s Machine that
// somehow carries the hook is unblocked without a k0s-specific leave.
func TestReconcileMemberLeave_NonK0sRemovesHook(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	target := hookedMachine("cp-0", "cp-0", nil)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).Build()

	factoryCalled := false
	r := &KairosControlPlaneReconciler{
		Client: c, Scheme: scheme,
		WorkloadClientFactory: func(context.Context, *clusterv1.Cluster) (client.Client, error) {
			factoryCalled = true
			return nil, nil
		},
	}
	k3s := k0sKCP()
	k3s.Spec.Distribution = "k3s"
	done, err := r.reconcileMemberLeave(context.Background(), log.Log, k3s, testCluster(), target)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeTrue())
	g.Expect(factoryCalled).To(BeFalse())
	assertHookGone(g, c, "cp-0")
}

// TestReconcileMemberLeave_WritesRequestThenWaits: first entry writes the
// leave-requested sentinel to the workload ConfigMap, stamps the request
// timestamp, and requeues (done=false) with the hook retained.
func TestReconcileMemberLeave_WritesRequestThenWaits(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	target := hookedMachine("cp-0", "cp-0", nil)
	mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target, etcdStatusSecretForMembers("cp-0", "cp-1", "cp-2")).Build()
	wc := fake.NewClientBuilder().WithScheme(scheme).Build() // no ConfigMap yet

	r := &KairosControlPlaneReconciler{
		Client: mgmt, Scheme: scheme,
		WorkloadClientFactory: staticWorkloadClient(wc),
	}
	done, err := r.reconcileMemberLeave(context.Background(), log.Log, k0sKCP(), testCluster(), target)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse(), "must wait for the node to ack")

	// leave-requested written to the workload ConfigMap.
	cm := &corev1.ConfigMap{}
	g.Expect(wc.Get(context.Background(), types.NamespacedName{Name: etcdLeaveConfigMapName, Namespace: etcdLeaveConfigMapNamespace}, cm)).To(Succeed())
	g.Expect(cm.Data).To(HaveKeyWithValue("cp-0", etcdLeaveRequestedValue))

	// timestamp stamped, hook retained.
	got := &clusterv1.Machine{}
	g.Expect(mgmt.Get(context.Background(), types.NamespacedName{Name: "cp-0", Namespace: "default"}, got)).To(Succeed())
	g.Expect(got.Annotations).To(HaveKey(etcdLeaveRequestedAtAnnotation))
	_, perr := time.Parse(time.RFC3339, got.Annotations[etcdLeaveRequestedAtAnnotation])
	g.Expect(perr).ToNot(HaveOccurred())
	g.Expect(hasEtcdLeaveHook(got)).To(BeTrue(), "hook must stay until the node acks")
}

// TestReconcileMemberLeave_AckRemovesHook: once the node writes `left`, the hook
// is removed and the delete may proceed.
func TestReconcileMemberLeave_AckRemovesHook(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	target := hookedMachine("cp-0", "cp-0", map[string]string{etcdLeaveRequestedAtAnnotation: time.Now().UTC().Format(time.RFC3339)})
	mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target, etcdStatusSecretForMembers("cp-0", "cp-1", "cp-2")).Build()
	wc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leaveConfigMap(map[string]string{"cp-0": etcdLeftValue})).Build()

	r := &KairosControlPlaneReconciler{Client: mgmt, Scheme: scheme, WorkloadClientFactory: staticWorkloadClient(wc)}
	done, err := r.reconcileMemberLeave(context.Background(), log.Log, k0sKCP(), testCluster(), target)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeTrue())
	assertHookGone(g, mgmt, "cp-0")
}

// TestReconcileMemberLeave_AbsentFromEtcdStatusRemovesHook: after we have asked
// the node to leave, its disappearance from node-reported etcd-status is treated
// as a completed leave (cross-check terminal).
func TestReconcileMemberLeave_AbsentFromEtcdStatusRemovesHook(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	target := hookedMachine("cp-0", "cp-0", map[string]string{etcdLeaveRequestedAtAnnotation: time.Now().UTC().Format(time.RFC3339)})
	// etcd-status no longer lists cp-0.
	mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target, etcdStatusSecretForMembers("cp-1", "cp-2")).Build()
	wc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leaveConfigMap(map[string]string{"cp-0": etcdLeaveRequestedValue})).Build()

	r := &KairosControlPlaneReconciler{Client: mgmt, Scheme: scheme, WorkloadClientFactory: staticWorkloadClient(wc)}
	done, err := r.reconcileMemberLeave(context.Background(), log.Log, k0sKCP(), testCluster(), target)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeTrue())
	assertHookGone(g, mgmt, "cp-0")
}

// TestReconcileMemberLeave_TimeoutRemovesHook: a node that never acks within
// memberLeaveTimeout must not wedge deletion — the hook is removed (member may be
// orphaned) and a Warning event is emitted.
func TestReconcileMemberLeave_TimeoutRemovesHook(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	old := time.Now().Add(-2 * memberLeaveTimeout).UTC().Format(time.RFC3339)
	target := hookedMachine("cp-0", "cp-0", map[string]string{etcdLeaveRequestedAtAnnotation: old})
	// Member still present in etcd-status (so the cross-check does NOT short-circuit)
	// and no `left` ack — only the timeout can fire.
	mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target, etcdStatusSecretForMembers("cp-0", "cp-1", "cp-2")).Build()
	wc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leaveConfigMap(map[string]string{"cp-0": etcdLeaveRequestedValue})).Build()

	rec := record.NewFakeRecorder(4)
	r := &KairosControlPlaneReconciler{Client: mgmt, Scheme: scheme, Recorder: rec, WorkloadClientFactory: staticWorkloadClient(wc)}
	done, err := r.reconcileMemberLeave(context.Background(), log.Log, k0sKCP(), testCluster(), target)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeTrue())
	assertHookGone(g, mgmt, "cp-0")
	g.Expect(rec.Events).To(Receive(ContainSubstring("EtcdMemberLeaveTimedOut")))
}

// TestReconcileMemberLeave_ClientBuildErrorRetainsHook: a workload-client build
// failure is FAIL-SAFE — the error propagates (caller requeues) and the hook
// stays set so the delete remains blocked and quorum is preserved.
func TestReconcileMemberLeave_ClientBuildErrorRetainsHook(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	target := hookedMachine("cp-0", "cp-0", nil)
	mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target, etcdStatusSecretForMembers("cp-0", "cp-1", "cp-2")).Build()

	r := &KairosControlPlaneReconciler{
		Client: mgmt, Scheme: scheme,
		WorkloadClientFactory: func(context.Context, *clusterv1.Cluster) (client.Client, error) {
			return nil, context.DeadlineExceeded
		},
	}
	done, err := r.reconcileMemberLeave(context.Background(), log.Log, k0sKCP(), testCluster(), target)
	g.Expect(err).To(HaveOccurred())
	g.Expect(done).To(BeFalse())
	got := &clusterv1.Machine{}
	g.Expect(mgmt.Get(context.Background(), types.NamespacedName{Name: "cp-0", Namespace: "default"}, got)).To(Succeed())
	g.Expect(hasEtcdLeaveHook(got)).To(BeTrue(), "hook must be retained on a client-build error (fail-safe)")
}

// TestStripEtcdLeaveHooks_Teardown: the teardown strip removes the hook from
// every owned Machine — including one already mid-deletion and paused at the
// hook — while leaving un-owned and un-hooked Machines untouched.
func TestStripEtcdLeaveHooks_Teardown(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kcp", Namespace: "default", UID: "kcp-uid",
			Labels: map[string]string{clusterv1.ClusterNameLabel: testClusterName},
		},
	}
	ownedRef := metav1.OwnerReference{
		APIVersion: controlplanev1beta2.GroupVersion.String(), Kind: "KairosControlPlane",
		Name: "kcp", UID: "kcp-uid", Controller: ptr.To(true),
	}

	// Owned + hooked, live.
	live := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: "cp-0", Namespace: "default",
		Labels:          map[string]string{clusterv1.ClusterNameLabel: testClusterName},
		OwnerReferences: []metav1.OwnerReference{ownedRef},
		Annotations:     map[string]string{etcdLeaveHookAnnotation(): ""},
	}}
	// Owned + hooked, ALREADY DELETING and paused at the hook (needs a finalizer
	// for the fake client to retain a deletionTimestamp).
	now := metav1.Now()
	deleting := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: "cp-1", Namespace: "default",
		Labels:            map[string]string{clusterv1.ClusterNameLabel: testClusterName},
		OwnerReferences:   []metav1.OwnerReference{ownedRef},
		Annotations:       map[string]string{etcdLeaveHookAnnotation(): ""},
		DeletionTimestamp: &now,
		Finalizers:        []string{"test.kairos.io/hold"},
	}}
	// Foreign (different owner UID) — must NOT be touched.
	foreignRef := ownedRef
	foreignRef.UID = "other-uid"
	foreign := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign", Namespace: "default",
		Labels:          map[string]string{clusterv1.ClusterNameLabel: testClusterName},
		OwnerReferences: []metav1.OwnerReference{foreignRef},
		Annotations:     map[string]string{etcdLeaveHookAnnotation(): ""},
	}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp, live, deleting, foreign).Build()
	g.Expect(stripEtcdLeaveHooks(context.Background(), c, kcp)).To(Succeed())

	assertHookGone(g, c, "cp-0")
	assertHookGone(g, c, "cp-1")

	// Foreign Machine keeps its hook.
	gotForeign := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "foreign", Namespace: "default"}, gotForeign)).To(Succeed())
	g.Expect(hasEtcdLeaveHook(gotForeign)).To(BeTrue(), "a Machine owned by a different KCP UID must be left alone")
}

// TestWarnIfK3sEtcdLimitation: the KD-5d warning fires for multi-replica k3s and
// stays silent for k0s and for single-node.
func TestWarnIfK3sEtcdLimitation(t *testing.T) {
	target := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "cp-0", Namespace: "default"}}
	for _, tc := range []struct {
		name     string
		dist     string
		replicas int32
		wantEvt  bool
	}{
		{"k3s HA warns", "k3s", 3, true},
		{"k0s HA silent", "k0s", 3, false},
		{"k3s single silent", "k3s", 1, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			rec := record.NewFakeRecorder(4)
			r := &KairosControlPlaneReconciler{Recorder: rec}
			kcp := &controlplanev1beta2.KairosControlPlane{Spec: controlplanev1beta2.KairosControlPlaneSpec{
				Distribution: tc.dist, Replicas: ptr.To(tc.replicas),
			}}
			r.warnIfK3sEtcdLimitation(kcp, target)
			if tc.wantEvt {
				g.Expect(rec.Events).To(Receive(ContainSubstring("EtcdMemberRemoveUnsupportedForK3s")))
			} else {
				g.Expect(rec.Events).ToNot(Receive())
			}
		})
	}
}

// staticWorkloadClient returns a factory that always yields the given client.
func staticWorkloadClient(wc client.Client) func(context.Context, *clusterv1.Cluster) (client.Client, error) {
	return func(context.Context, *clusterv1.Cluster) (client.Client, error) { return wc, nil }
}

func assertHookGone(g *WithT, c client.Client, name string) {
	g.THelper()
	got := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
	g.Expect(hasEtcdLeaveHook(got)).To(BeFalse(), "hook should have been removed from %s", name)
}

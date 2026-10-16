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
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// testMemberID is the etcd member ID the test EtcdMembers carry by default.
const testMemberID = "8e9e05c52164694d"

// k0sEtcdMember builds an EtcdMember the way k0s writes it: named after the
// node, with its peer address, testMemberID and a Joined condition.
func k0sEtcdMember(name, peer string, leave bool, joined string) *unstructured.Unstructured {
	return k0sEtcdMemberWithID(name, peer, testMemberID, leave, joined)
}

// k0sEtcdMemberWithID is k0sEtcdMember with the given member ID; "" leaves
// status.memberID out.
func k0sEtcdMemberWithID(name, peer, memberID string, leave bool, joined string) *unstructured.Unstructured {
	status := map[string]interface{}{
		"peerAddress": peer,
		"conditions": []interface{}{map[string]interface{}{
			"type":               "Joined",
			"status":             joined,
			"lastTransitionTime": "2026-10-16T09:00:00Z",
		}},
	}
	if memberID != "" {
		status["memberID"] = memberID
	}
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec":   map[string]interface{}{},
		"status": status,
	}}
	u.SetGroupVersionKind(etcdMemberGVK)
	u.SetName(name)
	if leave {
		u.Object["spec"] = map[string]interface{}{"leave": true}
	}
	return u
}

func getEtcdMember(g *WithT, wc client.Client, name string) *unstructured.Unstructured {
	g.THelper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(etcdMemberGVK)
	g.Expect(wc.Get(context.Background(), client.ObjectKey{Name: name}, u)).To(Succeed())
	return u
}

func leaveRequested(u *unstructured.Unstructured) bool {
	leave, _, _ := unstructured.NestedBool(u.Object, "spec", "leave")
	return leave
}

func ago(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }

// leaveFixture is a 3-member k0s control plane removing cp-0 (10.0.0.10),
// whose node never acked. The staying member cp-1 (10.0.0.11) answers; the
// endpoint hangs.
type leaveFixture struct {
	scheme   *runtime.Scheme
	target   *clusterv1.Machine
	machines []*clusterv1.Machine
	mgmt     client.Client
	member   client.Client
	rec      *record.FakeRecorder
	r        *KairosControlPlaneReconciler
}

func newLeaveFixture(g *WithT, annotations map[string]string, workload []client.Object, intercept *interceptor.Funcs) *leaveFixture {
	f := &leaveFixture{scheme: haTestScheme(g)}
	f.target = leavingMachine("cp-0", annotations, "10.0.0.10")
	f.machines = []*clusterv1.Machine{f.target, nodeMachine("cp-1", false, "10.0.0.11"), nodeMachine("cp-2", false, "10.0.0.12")}
	f.mgmt = fake.NewClientBuilder().WithScheme(f.scheme).WithObjects(f.target, etcdStatusSecretForMembers("cp-0", "cp-1", "cp-2")).Build()
	b := fake.NewClientBuilder().WithScheme(f.scheme).
		WithObjects(leaveConfigMap(map[string]string{"cp-0": etcdLeaveRequestedValue})).
		WithObjects(workload...)
	if intercept != nil {
		b = b.WithInterceptorFuncs(*intercept)
	}
	f.member = b.Build()
	members := &fakeMembers{clients: map[string]client.Client{"10.0.0.11": f.member}}
	f.rec = record.NewFakeRecorder(8)
	f.r = &KairosControlPlaneReconciler{Client: f.mgmt, Scheme: f.scheme, Recorder: f.rec,
		WorkloadClientFactory: staticWorkloadClient(hungClient(f.scheme)), MemberClientFactory: members.factory}
	return f
}

func (f *leaveFixture) reconcile() (bool, error) {
	return f.r.reconcileMemberLeave(context.Background(), log.Log, vipKCP(), haCluster(), f.machines, f.target)
}

func (f *leaveFixture) events() []string {
	var out []string
	for {
		select {
		case e := <-f.rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestDelegateMemberLeave_TimeoutSetsLeaveOnTheMember: a node that has not
// acked within memberLeaveTimeout gets its EtcdMember's spec.leave set, the
// delegation stamped, and the hook kept while k0s still reports it joined.
func TestDelegateMemberLeave_TimeoutSetsLeaveOnTheMember(t *testing.T) {
	g := NewWithT(t)
	f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)},
		[]client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "True"), k0sEtcdMember("cp-1", "10.0.0.11", false, "True")}, nil)

	done, err := f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse(), "the hook stays until k0s reports the member gone")
	g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeTrue())
	got := getMachine(g, f.mgmt, "cp-0")
	g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
	g.Expect(got.Annotations).To(HaveKey(etcdMemberLeaveRequestedAtAnnotation))
	g.Expect(f.events()).To(ConsistOf(ContainSubstring("Normal EtcdMemberLeaveDelegated")))

	// A second pass while k0s works on it changes nothing and keeps waiting.
	stamp := got.Annotations[etcdMemberLeaveRequestedAtAnnotation]
	f.target = got
	f.machines[0] = got
	done, err = f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(getMachine(g, f.mgmt, "cp-0").Annotations).To(HaveKeyWithValue(etcdMemberLeaveRequestedAtAnnotation, stamp), "the stamp is written once")
	g.Expect(f.events()).To(BeEmpty(), "spec.leave is set once")
}

// TestDelegateMemberLeave_PatchWritesOnlySpecLeave: the request to k0s is a
// merge patch of spec.leave alone, pinned to the resourceVersion that was
// inspected, so it cannot land on a replaced object or touch anything else.
func TestDelegateMemberLeave_PatchWritesOnlySpecLeave(t *testing.T) {
	g := NewWithT(t)
	var patchType types.PatchType
	var body map[string]interface{}
	capture := &interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == etcdMemberGVK {
			patchType = patch.Type()
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &body); err != nil {
				return err
			}
		}
		return c.Patch(ctx, obj, patch, opts...)
	}}
	f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)},
		[]client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "True")}, capture)
	before := getEtcdMember(g, f.member, "cp-0")

	_, err := f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(patchType).To(Equal(types.MergePatchType))
	g.Expect(body).To(Equal(map[string]interface{}{
		"metadata": map[string]interface{}{"resourceVersion": before.GetResourceVersion()},
		"spec":     map[string]interface{}{"leave": true},
	}))
}

// TestDelegateMemberLeave_JoinedFalseRemovesHook: once k0s reports the member
// is not in etcd, the hook goes, with no warning.
func TestDelegateMemberLeave_JoinedFalseRemovesHook(t *testing.T) {
	g := NewWithT(t)
	f := newLeaveFixture(g, map[string]string{
		etcdLeaveRequestedAtAnnotation:       ago(2 * memberLeaveTimeout),
		etcdMemberLeaveRequestedAtAnnotation: ago(time.Minute),
	}, []client.Object{k0sEtcdMember("cp-0", "10.0.0.10", true, "False")}, nil)

	done, err := f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeTrue())
	assertHookGone(g, f.mgmt, "cp-0")
	g.Expect(f.events()).To(BeEmpty())
}

// TestDelegateMemberLeave_AlreadyGoneOnFirstPass: k0s v1.36 marks a member
// that is no longer in etcd Joined=False by itself (the node left, but its ack
// was lost). The first delegating pass sets spec.leave and removes the hook.
func TestDelegateMemberLeave_AlreadyGoneOnFirstPass(t *testing.T) {
	g := NewWithT(t)
	f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)},
		[]client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "False")}, nil)

	done, err := f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeTrue())
	assertHookGone(g, f.mgmt, "cp-0")
	g.Expect(f.events()).To(ConsistOf(ContainSubstring("EtcdMemberLeaveDelegated")))
}

// TestDelegateMemberLeave_NodeAckStillWins: the node's own ack ends the leave
// even after the KCP has handed it to k0s.
func TestDelegateMemberLeave_NodeAckStillWins(t *testing.T) {
	g := NewWithT(t)
	f := newLeaveFixture(g, map[string]string{
		etcdLeaveRequestedAtAnnotation:       ago(2 * memberLeaveTimeout),
		etcdMemberLeaveRequestedAtAnnotation: ago(time.Minute),
	}, []client.Object{k0sEtcdMember("cp-0", "10.0.0.10", true, "True")}, nil)
	cm := leaveConfigMap(nil)
	g.Expect(f.member.Get(context.Background(), leaveConfigMapKey, cm)).To(Succeed())
	cm.Data["cp-0"] = etcdLeftValue
	g.Expect(f.member.Update(context.Background(), cm)).To(Succeed())

	done, err := f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeTrue())
	assertHookGone(g, f.mgmt, "cp-0")
	g.Expect(f.events()).To(BeEmpty())
}

// TestDelegateMemberLeave_StaleRequestStillGetsTheFallback: a Machine whose
// leave was requested hours ago, by a KCP without the fallback, still gets
// spec.leave and the full etcdMemberLeaveCap, not an immediate warning.
func TestDelegateMemberLeave_StaleRequestStillGetsTheFallback(t *testing.T) {
	g := NewWithT(t)
	f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(6 * time.Hour)},
		[]client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "True")}, nil)

	done, err := f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeTrue())
	g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
	g.Expect(f.events()).ToNot(ContainElement(ContainSubstring("EtcdMemberLeaveTimedOut")))
}

// TestDelegateMemberLeave_CapFallsBackToWarning: when k0s has not reported the
// member gone within etcdMemberLeaveCap of the delegation, the hook goes with
// the old warning.
func TestDelegateMemberLeave_CapFallsBackToWarning(t *testing.T) {
	g := NewWithT(t)
	f := newLeaveFixture(g, map[string]string{
		etcdLeaveRequestedAtAnnotation:       ago(time.Hour),
		etcdMemberLeaveRequestedAtAnnotation: ago(etcdMemberLeaveCap + time.Minute),
	}, []client.Object{k0sEtcdMember("cp-0", "10.0.0.10", true, "True")}, nil)

	done, err := f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeTrue())
	assertHookGone(g, f.mgmt, "cp-0")
	g.Expect(f.events()).To(ConsistOf(And(ContainSubstring("Warning EtcdMemberLeaveTimedOut"), ContainSubstring("within 10m0s"))))
}

// TestDelegateMemberLeave_NoEtcdMemberKeepsTheOldBehaviour: a cluster without
// the EtcdMember API, or without an EtcdMember for the node, gets the old
// timeout: a warning and the hook removed, with nothing written to k0s.
func TestDelegateMemberLeave_NoEtcdMemberKeepsTheOldBehaviour(t *testing.T) {
	noMatch := &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == etcdMemberGVK {
			return &meta.NoKindMatchError{GroupKind: etcdMemberGVK.GroupKind(), SearchedVersions: []string{etcdMemberGVK.Version}}
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	for _, tc := range []struct {
		name      string
		workload  []client.Object
		intercept *interceptor.Funcs
	}{
		{"no EtcdMember API", nil, noMatch},
		{"no EtcdMember for the node", []client.Object{k0sEtcdMember("cp-1", "10.0.0.11", false, "True")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)}, tc.workload, tc.intercept)

			done, err := f.reconcile()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(done).To(BeTrue())
			got := getMachine(g, f.mgmt, "cp-0")
			g.Expect(hasEtcdLeaveHook(got)).To(BeFalse())
			g.Expect(got.Annotations).ToNot(HaveKey(etcdMemberLeaveRequestedAtAnnotation))
			g.Expect(f.events()).To(ConsistOf(ContainSubstring("Warning EtcdMemberLeaveTimedOut")))
			if tc.intercept == nil {
				g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-1"))).To(BeFalse())
			}
		})
	}
}

// TestDelegateMemberLeave_NeverTouchesAnotherMember: the KCP sets spec.leave
// on the leaving node's EtcdMember only. When the EtcdMember named after the
// node carries the peer address of a staying Machine, it describes another
// member: nothing is written, and the old timeout applies.
func TestDelegateMemberLeave_NeverTouchesAnotherMember(t *testing.T) {
	t.Run("only the leaving node's member is patched", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)}, []client.Object{
			k0sEtcdMember("cp-0", "10.0.0.10", false, "True"),
			k0sEtcdMember("cp-1", "10.0.0.11", false, "True"),
			k0sEtcdMember("cp-2", "10.0.0.12", false, "True"),
		}, nil)
		_, err := f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeTrue())
		g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-1"))).To(BeFalse())
		g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-2"))).To(BeFalse())
	})
	t.Run("a member with another Machine's peer address is left alone", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)},
			[]client.Object{k0sEtcdMember("cp-0", "10.0.0.11", false, "True")}, nil)
		done, err := f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeFalse())
		assertHookGone(g, f.mgmt, "cp-0")
		g.Expect(f.events()).To(ConsistOf(And(ContainSubstring("Warning EtcdMemberLeaveTimedOut"), ContainSubstring(`Machine "cp-1"`))))
	})
	t.Run("a peer address the leaving Machine does not report is accepted", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)},
			[]client.Object{k0sEtcdMember("cp-0", "172.16.56.70", false, "True")}, nil)
		done, err := f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeFalse())
		g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeTrue())
	})
}

// TestDelegateMemberLeave_TransientErrorsKeepTheHook: a failed EtcdMember read
// or patch inside the cap keeps the hook, returns the error, and starts the
// cap clock; past the cap it takes the old timeout.
func TestDelegateMemberLeave_TransientErrorsKeepTheHook(t *testing.T) {
	isEtcdMember := func(obj client.Object) bool {
		u, ok := obj.(*unstructured.Unstructured)
		return ok && u.GroupVersionKind() == etcdMemberGVK
	}
	failRead := &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if isEtcdMember(obj) {
			return apierrors.NewServiceUnavailable("etcdserver: request timed out")
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	failPatch := &interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if isEtcdMember(obj) {
			return apierrors.NewServiceUnavailable("etcdserver: request timed out")
		}
		return c.Patch(ctx, obj, patch, opts...)
	}}
	for _, tc := range []struct {
		name      string
		intercept *interceptor.Funcs
		capped    bool
	}{
		{"read fails inside the cap", failRead, false},
		{"patch fails inside the cap", failPatch, false},
		{"read fails past the cap", failRead, true},
		{"patch fails past the cap", failPatch, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ann := map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)}
			if tc.capped {
				ann[etcdMemberLeaveRequestedAtAnnotation] = ago(etcdMemberLeaveCap + time.Minute)
			}
			f := newLeaveFixture(g, ann, []client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "True")}, tc.intercept)

			done, err := f.reconcile()
			got := getMachine(g, f.mgmt, "cp-0")
			if tc.capped {
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(done).To(BeTrue())
				g.Expect(hasEtcdLeaveHook(got)).To(BeFalse())
				g.Expect(f.events()).To(ConsistOf(ContainSubstring("Warning EtcdMemberLeaveTimedOut")))
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(done).To(BeFalse())
			g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
			g.Expect(got.Annotations).To(HaveKey(etcdMemberLeaveRequestedAtAnnotation))
			g.Expect(f.events()).To(BeEmpty())
		})
	}
}

// TestDelegateMemberLeave_PinsTheMemberID: the first inspection records the
// member ID; while the EtcdMember keeps it, spec.leave is set and Joined=False
// ends the leave with no warning.
func TestDelegateMemberLeave_PinsTheMemberID(t *testing.T) {
	g := NewWithT(t)
	f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)},
		[]client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "True")}, nil)

	done, err := f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	got := getMachine(g, f.mgmt, "cp-0")
	g.Expect(got.Annotations).To(HaveKeyWithValue(etcdMemberLeaveMemberIDAnnotation, testMemberID))
	g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeTrue())
	g.Expect(f.events()).To(ConsistOf(ContainSubstring("Normal EtcdMemberLeaveDelegated")))

	// k0s removes that member and reports it, keeping the ID.
	member := getEtcdMember(g, f.member, "cp-0")
	g.Expect(unstructured.SetNestedSlice(member.Object, []interface{}{map[string]interface{}{
		"type": "Joined", "status": "False", "lastTransitionTime": "2026-10-16T09:10:00Z",
	}}, "status", "conditions")).To(Succeed())
	g.Expect(f.member.Update(context.Background(), member)).To(Succeed())
	f.target, f.machines[0] = got, got
	done, err = f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeTrue())
	assertHookGone(g, f.mgmt, "cp-0")
	g.Expect(f.events()).To(BeEmpty(), "a clean end, not the timeout warning")
}

// TestDelegateMemberLeave_ChangedMemberIDIsNotTouched: an EtcdMember whose
// member ID differs from the one recorded at the first inspection describes a
// different etcd member. spec.leave is not set and Joined=False is not taken
// as the end of this leave: the old timeout applies.
func TestDelegateMemberLeave_ChangedMemberIDIsNotTouched(t *testing.T) {
	for _, tc := range []struct {
		name   string
		member *unstructured.Unstructured
	}{
		{"before spec.leave is set", k0sEtcdMemberWithID("cp-0", "10.0.0.10", "1a2b3c4d5e6f7a8b", false, "True")},
		{"when k0s reports Joined=False", k0sEtcdMemberWithID("cp-0", "10.0.0.10", "1a2b3c4d5e6f7a8b", true, "False")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			wasLeave := leaveRequested(tc.member)
			f := newLeaveFixture(g, map[string]string{
				etcdLeaveRequestedAtAnnotation:       ago(2 * memberLeaveTimeout),
				etcdMemberLeaveRequestedAtAnnotation: ago(time.Minute),
				etcdMemberLeaveMemberIDAnnotation:    testMemberID,
			}, []client.Object{tc.member}, nil)

			done, err := f.reconcile()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(done).To(BeTrue())
			assertHookGone(g, f.mgmt, "cp-0")
			g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(Equal(wasLeave), "the EtcdMember is not written")
			g.Expect(getMachine(g, f.mgmt, "cp-0").Annotations).To(HaveKeyWithValue(etcdMemberLeaveMemberIDAnnotation, testMemberID))
			g.Expect(f.events()).To(ConsistOf(And(ContainSubstring("Warning EtcdMemberLeaveTimedOut"), ContainSubstring("different etcd member"))))
		})
	}
}

// TestDelegateMemberLeave_MissingMemberIDIsNotTouched: an EtcdMember without a
// usable member ID cannot be pinned. Whether that is so at the first
// inspection or later, spec.leave is not set, Joined=False is not taken as the
// end of this leave, and the old timeout applies.
func TestDelegateMemberLeave_MissingMemberIDIsNotTouched(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded bool
		member   *unstructured.Unstructured
	}{
		{"missing at the first inspection", false, k0sEtcdMemberWithID("cp-0", "10.0.0.10", "", false, "True")},
		{"not hexadecimal at the first inspection", false, k0sEtcdMemberWithID("cp-0", "10.0.0.10", "not-an-id", false, "True")},
		{"missing later, when k0s reports Joined=False", true, k0sEtcdMemberWithID("cp-0", "10.0.0.10", "", true, "False")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ann := map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)}
			if tc.recorded {
				ann[etcdMemberLeaveRequestedAtAnnotation] = ago(time.Minute)
				ann[etcdMemberLeaveMemberIDAnnotation] = testMemberID
			}
			wasLeave := leaveRequested(tc.member)
			f := newLeaveFixture(g, ann, []client.Object{tc.member}, nil)

			done, err := f.reconcile()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(done).To(BeTrue())
			assertHookGone(g, f.mgmt, "cp-0")
			g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(Equal(wasLeave), "the EtcdMember is not written")
			if !tc.recorded {
				g.Expect(getMachine(g, f.mgmt, "cp-0").Annotations).ToNot(HaveKey(etcdMemberLeaveMemberIDAnnotation))
			}
			g.Expect(f.events()).To(ConsistOf(And(ContainSubstring("Warning EtcdMemberLeaveTimedOut"), ContainSubstring("no member ID"))))
		})
	}
}

// TestEtcdMemberID reads status.memberID in the form k0s writes it.
func TestEtcdMemberID(t *testing.T) {
	g := NewWithT(t)
	id, ok := etcdMemberID(k0sEtcdMemberWithID("cp-0", "10.0.0.10", "8E9E05C52164694D", false, "True"))
	g.Expect(ok).To(BeTrue())
	g.Expect(id).To(Equal(testMemberID))
	for _, bad := range []string{"", "not-an-id", "0", "1ffffffffffffffff"} {
		_, ok := etcdMemberID(k0sEtcdMemberWithID("cp-0", "10.0.0.10", bad, false, "True"))
		g.Expect(ok).To(BeFalse(), "memberID %q", bad)
	}
}

// TestEtcdMemberJoinedStatus reads the Joined condition the way k0s writes it.
func TestEtcdMemberJoinedStatus(t *testing.T) {
	g := NewWithT(t)
	g.Expect(etcdMemberJoinedStatus(k0sEtcdMember("cp-0", "10.0.0.10", false, "True"))).To(Equal("True"))
	g.Expect(etcdMemberJoinedStatus(k0sEtcdMember("cp-0", "10.0.0.10", true, "False"))).To(Equal("False"))
	bare := &unstructured.Unstructured{Object: map[string]interface{}{}}
	g.Expect(etcdMemberJoinedStatus(bare)).To(BeEmpty())
}

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
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

const orphanTestVersion = "v1.34.8+k0s.0"

// orphanFixture is a steady 3-member k0s control plane (cp-0..cp-2,
// 10.0.0.10..12), pinned, every member healthy and voting, whose workload
// cluster also holds the EtcdMembers given to newOrphanFixture.
type orphanFixture struct {
	g        *WithT
	scheme   *runtime.Scheme
	mgmt     client.Client
	workload client.Client
	members  *fakeMembers
	rec      *record.FakeRecorder
	r        *KairosControlPlaneReconciler
	kcp      *controlplanev1beta2.KairosControlPlane
	machines []*clusterv1.Machine
}

// orphanMachine is a registered control-plane Machine of the test cluster,
// owned by no one in particular, at orphanTestVersion.
func orphanMachine(name, addr string) *clusterv1.Machine {
	m := nodeMachine(name, false, addr)
	m.Labels = map[string]string{clusterv1.ClusterNameLabel: testClusterName, clusterv1.MachineControlPlaneLabel: ""}
	m.Spec.Version = orphanTestVersion
	return m
}

func newOrphanFixture(g *WithT, kcpAnnotations map[string]string, etcdMembers ...client.Object) *orphanFixture {
	return newOrphanFixtureWith(g, kcpAnnotations, nil, etcdMembers...)
}

func newOrphanFixtureWith(g *WithT, kcpAnnotations map[string]string, intercept *interceptor.Funcs, etcdMembers ...client.Object) *orphanFixture {
	f := &orphanFixture{g: g, scheme: haTestScheme(g), rec: record.NewFakeRecorder(20)}
	kcp := vipKCP()
	kcp.Spec.Version = orphanTestVersion
	kcp.Annotations = map[string]string{WorkloadClusterUIDAnnotation: uidA}
	for k, v := range kcpAnnotations {
		kcp.Annotations[k] = v
	}
	f.machines = []*clusterv1.Machine{orphanMachine("cp-0", "10.0.0.10"), orphanMachine("cp-1", "10.0.0.11"), orphanMachine("cp-2", "10.0.0.12")}
	objs := []client.Object{kcp, etcdStatusSecretForMembers("cp-0", "cp-1", "cp-2")}
	for _, m := range f.machines {
		objs = append(objs, m)
	}
	f.mgmt = fake.NewClientBuilder().WithScheme(f.scheme).WithObjects(objs...).Build()
	f.kcp = &controlplanev1beta2.KairosControlPlane{}
	g.Expect(f.mgmt.Get(context.Background(), client.ObjectKeyFromObject(kcp), f.kcp)).To(Succeed())

	b := fake.NewClientBuilder().WithScheme(f.scheme).WithObjects(etcdMembers...)
	if !hasKubeSystem(etcdMembers) {
		b = b.WithObjects(kubeSystem(uidA))
	}
	if intercept != nil {
		b = b.WithInterceptorFuncs(*intercept)
	}
	f.workload = b.Build()
	f.members = &fakeMembers{clients: map[string]client.Client{"10.0.0.10": f.workload, "10.0.0.11": f.workload, "10.0.0.12": f.workload}}
	f.r = &KairosControlPlaneReconciler{Client: f.mgmt, Scheme: f.scheme, Recorder: f.rec,
		WorkloadClientFactory: staticWorkloadClient(hungClient(f.scheme)), MemberClientFactory: f.members.factory}
	return f
}

// reconcile runs one orphan pass with the rate limit lifted, as a pass a
// minute later would.
func (f *orphanFixture) reconcile() (ctrl ctrlResult, hold bool, err error) {
	f.r.orphanCheckedAt = nil
	res, hold, err := f.r.reconcileOrphanEtcdMembers(context.Background(), log.Log, f.kcp, haCluster(), f.machines, 3)
	return ctrlResult{requeueAfter: res.RequeueAfter}, hold, err
}

type ctrlResult struct{ requeueAfter time.Duration }

func (f *orphanFixture) stored() *controlplanev1beta2.KairosControlPlane {
	f.g.THelper()
	got := &controlplanev1beta2.KairosControlPlane{}
	f.g.Expect(f.mgmt.Get(context.Background(), client.ObjectKeyFromObject(f.kcp), got)).To(Succeed())
	return got
}

func (f *orphanFixture) leaveSet(name string) bool {
	f.g.THelper()
	return leaveRequested(getEtcdMember(f.g, f.workload, name))
}

func (f *orphanFixture) setJoined(name, joined string) {
	f.g.THelper()
	m := getEtcdMember(f.g, f.workload, name)
	m.Object["status"].(map[string]interface{})["conditions"] = []interface{}{map[string]interface{}{"type": "Joined", "status": joined}}
	f.g.Expect(f.workload.Update(context.Background(), m)).To(Succeed())
}

func (f *orphanFixture) events() []string {
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

// theThree are the EtcdMembers of the three live Machines.
func theThree() []client.Object {
	return []client.Object{
		k0sEtcdMember("cp-0", "10.0.0.10", false, "True"),
		k0sEtcdMember("cp-1", "10.0.0.11", false, "True"),
		k0sEtcdMember("cp-2", "10.0.0.12", false, "True"),
	}
}

func withOrphans(orphans ...client.Object) []client.Object { return append(theThree(), orphans...) }

func TestParseOrphanLeave(t *testing.T) {
	g := NewWithT(t)
	at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	rec := orphanLeave{name: "kcp-7", memberID: testMemberID, at: at}
	got, ok := parseOrphanLeave(rec.String())
	g.Expect(ok).To(BeTrue())
	g.Expect(got.name).To(Equal("kcp-7"))
	g.Expect(got.memberID).To(Equal(testMemberID))
	g.Expect(got.at.Equal(at)).To(BeTrue())
	for _, bad := range []string{
		"", "kcp-7", "kcp-7 " + testMemberID,
		"KCP_7 " + testMemberID + " 2026-10-18T09:00:00Z",          // not a DNS-1123 name
		"kcp-7 not-hex 2026-10-18T09:00:00Z",                       // not a member ID
		"kcp-7 8E9E05C52164694D 2026-10-18T09:00:00Z",              // not canonical
		"kcp-7 " + testMemberID + " yesterday",                     // not a time
		"kcp-7 " + testMemberID + " 2026-10-18T09:00:00Z trailing", // extra field
	} {
		_, ok := parseOrphanLeave(bad)
		g.Expect(ok).To(BeFalse(), "%q must not parse", bad)
	}

	// A time in the future would stretch the cap and the hold: beyond the
	// clock-skew allowance it is rejected.
	soon := orphanLeave{name: "kcp-7", memberID: testMemberID, at: time.Now().Add(time.Minute)}
	_, ok = parseOrphanLeave(soon.String())
	g.Expect(ok).To(BeTrue(), "within the skew")
	later := orphanLeave{name: "kcp-7", memberID: testMemberID, at: time.Now().Add(orphanLeaveClockSkew + time.Minute)}
	_, ok = parseOrphanLeave(later.String())
	g.Expect(ok).To(BeFalse(), "beyond the skew")
}

// TestOrphanEtcdMembers_RemovedOneAtATime: two members have no Machine. The
// KCP asks k0s to remove the first only, records it, and holds every other
// membership change until k0s reports it gone; then the second.
func TestOrphanEtcdMembers_RemovedOneAtATime(t *testing.T) {
	g := NewWithT(t)
	f := newOrphanFixture(g, nil, withOrphans(
		k0sEtcdMemberWithID("kcp-old-a", "10.0.0.7", "a1", false, "True"),
		k0sEtcdMemberWithID("kcp-old-b", "10.0.0.8", "b2", false, "True"),
	)...)

	_, hold, err := f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hold).To(BeTrue(), "a removal in progress holds scale and rollout")
	g.Expect(f.leaveSet("kcp-old-a")).To(BeTrue())
	g.Expect(f.leaveSet("kcp-old-b")).To(BeFalse(), "one at a time")
	for _, n := range []string{"cp-0", "cp-1", "cp-2"} {
		g.Expect(f.leaveSet(n)).To(BeFalse())
	}
	rec, ok := parseOrphanLeave(f.stored().Annotations[etcdOrphanLeaveAnnotation])
	g.Expect(ok).To(BeTrue())
	g.Expect(rec.name).To(Equal("kcp-old-a"))
	g.Expect(rec.memberID).To(Equal("a1"))
	g.Expect(f.events()).To(ConsistOf(ContainSubstring("Normal EtcdOrphanMemberLeaveRequested")))

	// While k0s works on it, nothing else moves.
	_, hold, err = f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hold).To(BeTrue())
	g.Expect(f.leaveSet("kcp-old-b")).To(BeFalse())

	// k0s removes it: the record goes and the hold lifts.
	f.setJoined("kcp-old-a", "False")
	_, hold, err = f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hold).To(BeFalse())
	g.Expect(f.stored().Annotations).ToNot(HaveKey(etcdOrphanLeaveAnnotation))
	g.Expect(f.events()).To(ConsistOf(ContainSubstring("Normal EtcdOrphanMemberRemoved")))

	// Next pass: the second one.
	_, hold, err = f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hold).To(BeTrue())
	g.Expect(f.leaveSet("kcp-old-b")).To(BeTrue())
}

// TestOrphanEtcdMembers_ExpectedMembersAreKept: a member is not an orphan
// when any control-plane Machine renders its name, has it as its Node name,
// or reports its peer address. Members k0s already reports Joined=False are
// not in etcd and are ignored.
func TestOrphanEtcdMembers_ExpectedMembersAreKept(t *testing.T) {
	g := NewWithT(t)
	f := newOrphanFixture(g, nil, withOrphans(
		k0sEtcdMember("renamed", "10.0.0.11:2380", false, "True"),     // cp-1's address
		k0sEtcdMember("gone", "10.0.0.6", false, "False"),             // already out of etcd
		k0sEtcdMember("cp-host", "192.168.50.20:2380", false, "True"), // CAPK: rendered name, bridge peer
	)...)
	// cp-2 renders hostname cp-host (its Node is still named cp-2).
	g.Expect(f.mgmt.Create(context.Background(), kairosConfigWithHostname("cp-2", "cp-host"))).To(Succeed())
	cp2 := withKairosConfigRef(f.machines[2], "cp-2")
	g.Expect(f.mgmt.Update(context.Background(), cp2)).To(Succeed())

	_, hold, err := f.reconcile()

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hold).To(BeFalse())
	for _, n := range []string{"renamed", "gone", "cp-host"} {
		g.Expect(f.leaveSet(n)).To(BeFalse(), "%s is accounted for", n)
	}
	g.Expect(f.stored().Annotations).ToNot(HaveKey(etcdOrphanLeaveAnnotation))
}

// TestOrphanEtcdMembers_MachinesOfAnyOwnerCount: a control-plane Machine of
// the cluster that this KCP does not own still accounts for its member.
func TestOrphanEtcdMembers_MachinesOfAnyOwnerCount(t *testing.T) {
	g := NewWithT(t)
	f := newOrphanFixture(g, nil, withOrphans(k0sEtcdMember("foreign-cp", "10.0.0.30", false, "True"))...)
	g.Expect(f.mgmt.Create(context.Background(), orphanMachine("foreign-cp", "10.0.0.30"))).To(Succeed())

	_, hold, err := f.reconcile()

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hold).To(BeFalse())
	g.Expect(f.leaveSet("foreign-cp")).To(BeFalse())
}

// TestOrphanEtcdMembers_QuorumBlocked: when removing the orphan would leave
// fewer healthy voting members than a majority of those left, nothing is
// written and a Warning says why.
func TestOrphanEtcdMembers_QuorumBlocked(t *testing.T) {
	g := NewWithT(t)
	// 3 healthy members and 4 members without Machines: 6 would be left, 4
	// needed, 3 healthy.
	f := newOrphanFixture(g, nil, withOrphans(
		k0sEtcdMemberWithID("old-a", "10.0.0.5", "a1", false, "True"),
		k0sEtcdMemberWithID("old-b", "10.0.0.6", "b1", false, "True"),
		k0sEtcdMemberWithID("old-c", "10.0.0.7", "c1", false, "True"),
		k0sEtcdMemberWithID("old-d", "10.0.0.8", "d1", false, "True"),
	)...)

	_, hold, err := f.reconcile()

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hold).To(BeFalse())
	for _, n := range []string{"old-a", "old-b", "old-c", "old-d"} {
		g.Expect(f.leaveSet(n)).To(BeFalse())
	}
	g.Expect(f.stored().Annotations).ToNot(HaveKey(etcdOrphanLeaveAnnotation))
	g.Expect(f.events()).To(ConsistOf(ContainSubstring("Warning EtcdOrphanMemberRemovalBlocked")))
}

// TestOrphanEtcdMembers_QuorumCountsOnlyMembersWithAMachine: a member
// reported healthy whose EtcdMember is not in the list is not counted, and
// the desired replicas set a floor too.
func TestHealthyVotingMembers(t *testing.T) {
	g := NewWithT(t)
	joined := []*unstructured.Unstructured{
		k0sEtcdMember("cp-0", "", false, "True"), k0sEtcdMember("cp-1", "", false, "True"),
		k0sEtcdMember("orphan", "", false, "True"), k0sEtcdMember("stray", "", false, "True"),
	}
	machines := []*clusterv1.Machine{nodeMachine("cp-0", false), nodeMachine("cp-1", false), noNodeRef(nodeMachine("stray", false))}
	status := map[string]etcdMemberStatus{
		"cp-0":   {Healthy: true, Voting: true},
		"cp-1":   {Healthy: true, Voting: false},
		"orphan": {Healthy: true, Voting: true},
		"stray":  {Healthy: true, Voting: true},
	}
	g.Expect(healthyVotingMembers(joined, joined[2], machines, status)).To(Equal(1),
		"cp-1 does not vote, the orphan is being removed, stray has no registered Machine")
}

// TestOrphanEtcdMembers_BlockedOutsideASteadyState: nothing is listed or
// written while the control plane scales, rolls out, deletes a Machine, or
// has a joiner that is not registered and healthy, as upstream waits while a
// Machine is provisioning.
func TestOrphanEtcdMembers_BlockedOutsideASteadyState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(f *orphanFixture)
	}{
		{"scaling up", func(f *orphanFixture) { f.machines = f.machines[:2] }},
		{"rolling out", func(f *orphanFixture) { f.machines[1].Spec.Version = "v1.33.0+k0s.0" }},
		{"a Machine is being deleted", func(f *orphanFixture) {
			stored := getMachine(f.g, f.mgmt, "cp-2")
			stored.Finalizers = []string{"test.kairos.io/hold"}
			f.g.Expect(f.mgmt.Update(context.Background(), stored)).To(Succeed())
			f.g.Expect(f.mgmt.Delete(context.Background(), stored)).To(Succeed())
			f.machines[2] = getMachine(f.g, f.mgmt, "cp-2")
		}},
		{"a joiner has no NodeRef", func(f *orphanFixture) {
			stored := noNodeRef(getMachine(f.g, f.mgmt, "cp-2"))
			f.g.Expect(f.mgmt.Update(context.Background(), stored)).To(Succeed())
			f.machines[2] = stored
		}},
		{"a control-plane Machine of another owner has no NodeRef", func(f *orphanFixture) {
			f.g.Expect(f.mgmt.Create(context.Background(), noNodeRef(orphanMachine("foreign-cp", "10.0.0.30")))).To(Succeed())
		}},
		{"a joiner has not reported healthy and voting", func(f *orphanFixture) {
			g := f.g
			g.Expect(f.mgmt.Update(context.Background(), etcdStatusSecretForMembersRV(g, f.mgmt, "cp-0", "cp-1"))).To(Succeed())
		}},
		{"the control plane is being deleted", func(f *orphanFixture) {
			now := metav1.Now()
			f.kcp.DeletionTimestamp = &now
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			f := newOrphanFixture(g, nil, withOrphans(k0sEtcdMember("kcp-old", "10.0.0.7", false, "True"))...)
			tc.mutate(f)

			_, hold, err := f.reconcile()

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(hold).To(BeFalse())
			g.Expect(f.leaveSet("kcp-old")).To(BeFalse())
			g.Expect(f.stored().Annotations).ToNot(HaveKey(etcdOrphanLeaveAnnotation))
		})
	}
}

// etcdStatusSecretForMembersRV is etcdStatusSecretForMembers carrying the
// stored resourceVersion, for an Update.
func etcdStatusSecretForMembersRV(g *WithT, c client.Client, members ...string) client.Object {
	s := etcdStatusSecretForMembers(members...)
	stored := etcdStatusSecretForMembers()
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(s), stored)).To(Succeed())
	s.ResourceVersion = stored.ResourceVersion
	return s
}

// TestOrphanEtcdMembers_BlockedWhileFrozen: a frozen control plane removes
// no member, and does not even read; and a connection that serves another
// cluster freezes it with nothing written.
func TestOrphanEtcdMembers_BlockedWhileFrozen(t *testing.T) {
	t.Run("already frozen", func(t *testing.T) {
		g := NewWithT(t)
		f := newOrphanFixture(g, map[string]string{WorkloadClusterIdentityMismatchAnnotation: frozenAgainst(uidA)},
			withOrphans(k0sEtcdMember("kcp-old", "10.0.0.7", false, "True"))...)

		_, hold, err := f.reconcile()

		g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
		g.Expect(hold).To(BeFalse())
		g.Expect(f.leaveSet("kcp-old")).To(BeFalse())
		g.Expect(f.members.dialed).To(BeEmpty())
	})
	t.Run("another cluster answers", func(t *testing.T) {
		g := NewWithT(t)
		// The other cluster has no Machines' members at all: everything there
		// looks like an orphan.
		f := newOrphanFixture(g, nil, kubeSystem(uidB), k0sEtcdMember("intruder", "10.0.0.7", false, "True"))

		_, hold, err := f.reconcile()

		g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
		g.Expect(hold).To(BeFalse())
		g.Expect(f.leaveSet("intruder")).To(BeFalse())
		g.Expect(f.stored().Annotations).To(HaveKey(WorkloadClusterIdentityMismatchAnnotation))
		g.Expect(f.stored().Annotations).ToNot(HaveKey(etcdOrphanLeaveAnnotation))
	})
	t.Run("not pinned: no connection, nothing removed", func(t *testing.T) {
		g := NewWithT(t)
		f := newOrphanFixture(g, nil, withOrphans(k0sEtcdMember("kcp-old", "10.0.0.7", false, "True"))...)
		f.kcp.Annotations = nil

		_, hold, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(hold).To(BeFalse())
		g.Expect(f.leaveSet("kcp-old")).To(BeFalse())
		g.Expect(f.members.dialed).To(BeEmpty())
	})
}

// TestOrphanEtcdMembers_MemberIDPinned: the leave request is a merge patch of
// spec.leave pinned to the inspected resourceVersion; and once recorded,
// the EtcdMember is acted on only while it carries the recorded member ID.
func TestOrphanEtcdMembers_MemberIDPinned(t *testing.T) {
	t.Run("the patch is pinned to the inspected object", func(t *testing.T) {
		g := NewWithT(t)
		var body map[string]interface{}
		capture := &interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetObjectKind().GroupVersionKind() == etcdMemberGVK {
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
		f := newOrphanFixtureWith(g, nil, capture, withOrphans(k0sEtcdMember("kcp-old", "10.0.0.7", false, "True"))...)
		before := getEtcdMember(g, f.workload, "kcp-old")

		_, _, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(body).To(Equal(map[string]interface{}{
			"metadata": map[string]interface{}{"resourceVersion": before.GetResourceVersion()},
			"spec":     map[string]interface{}{"leave": true},
		}))
	})
	t.Run("a changed member ID is left alone", func(t *testing.T) {
		g := NewWithT(t)
		rec := orphanLeave{name: "kcp-old", memberID: "a1", at: time.Now().Add(-time.Minute)}
		// A new controller took the name over: k0s reset spec.leave and wrote its own ID.
		f := newOrphanFixture(g, map[string]string{etcdOrphanLeaveAnnotation: rec.String()},
			withOrphans(k0sEtcdMemberWithID("kcp-old", "10.0.0.7", "b2", false, "True"))...)

		_, hold, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(hold).To(BeFalse())
		g.Expect(f.leaveSet("kcp-old")).To(BeFalse(), "never re-asked under another member ID")
		g.Expect(f.stored().Annotations).ToNot(HaveKey(etcdOrphanLeaveAnnotation))
		g.Expect(f.events()).To(ConsistOf(ContainSubstring("Warning EtcdOrphanMemberChanged")))
	})
	t.Run("an orphan without a member ID is not touched", func(t *testing.T) {
		g := NewWithT(t)
		f := newOrphanFixture(g, nil, withOrphans(k0sEtcdMemberWithID("kcp-old", "10.0.0.7", "", false, "True"))...)

		_, hold, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(hold).To(BeFalse())
		g.Expect(f.leaveSet("kcp-old")).To(BeFalse())
	})
}

// TestOrphanEtcdMembers_RecordedRemoval covers the rest of a recorded
// removal: a lost spec.leave is set again, the cap ends it with a warning, a
// deleted EtcdMember ends it, and a member a Machine now accounts for gets
// its request taken back.
func TestOrphanEtcdMembers_RecordedRemoval(t *testing.T) {
	recent := orphanLeave{name: "kcp-old", memberID: testMemberID, at: time.Now().Add(-time.Minute)}
	t.Run("spec.leave not set yet: set again, held", func(t *testing.T) {
		g := NewWithT(t)
		f := newOrphanFixture(g, map[string]string{etcdOrphanLeaveAnnotation: recent.String()},
			withOrphans(k0sEtcdMember("kcp-old", "10.0.0.7", false, "True"))...)
		_, hold, err := f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(hold).To(BeTrue())
		g.Expect(f.leaveSet("kcp-old")).To(BeTrue())
	})
	t.Run("past the cap: cleared with a warning", func(t *testing.T) {
		g := NewWithT(t)
		old := orphanLeave{name: "kcp-old", memberID: testMemberID, at: time.Now().Add(-etcdMemberLeaveCap - time.Minute)}
		f := newOrphanFixture(g, map[string]string{etcdOrphanLeaveAnnotation: old.String()},
			withOrphans(k0sEtcdMember("kcp-old", "10.0.0.7", true, "True"))...)
		_, hold, err := f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(hold).To(BeFalse())
		g.Expect(f.stored().Annotations).ToNot(HaveKey(etcdOrphanLeaveAnnotation))
		g.Expect(f.events()).To(ConsistOf(ContainSubstring("Warning EtcdOrphanMemberRemovalTimedOut")))
	})
	t.Run("the EtcdMember is gone: cleared", func(t *testing.T) {
		g := NewWithT(t)
		f := newOrphanFixture(g, map[string]string{etcdOrphanLeaveAnnotation: recent.String()}, theThree()...)
		_, hold, err := f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(hold).To(BeFalse())
		g.Expect(f.stored().Annotations).ToNot(HaveKey(etcdOrphanLeaveAnnotation))
	})
	t.Run("a Machine now accounts for it: the request is taken back", func(t *testing.T) {
		g := NewWithT(t)
		f := newOrphanFixture(g, map[string]string{etcdOrphanLeaveAnnotation: recent.String()},
			withOrphans(k0sEtcdMember("kcp-old", "10.0.0.7", true, "True"))...)
		g.Expect(f.mgmt.Create(context.Background(), orphanMachine("kcp-old", "10.0.0.7"))).To(Succeed())
		_, hold, err := f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(hold).To(BeFalse())
		g.Expect(f.leaveSet("kcp-old")).To(BeFalse())
		g.Expect(f.stored().Annotations).ToNot(HaveKey(etcdOrphanLeaveAnnotation))
	})
	t.Run("nothing answers within the cap: held", func(t *testing.T) {
		g := NewWithT(t)
		f := newOrphanFixture(g, map[string]string{etcdOrphanLeaveAnnotation: recent.String()},
			withOrphans(k0sEtcdMember("kcp-old", "10.0.0.7", true, "True"))...)
		f.r.MemberClientFactory = (&fakeMembers{}).factory
		_, hold, err := f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(hold).To(BeTrue())
		g.Expect(f.stored().Annotations).To(HaveKey(etcdOrphanLeaveAnnotation))
	})
	t.Run("an unparsable record is dropped", func(t *testing.T) {
		g := NewWithT(t)
		f := newOrphanFixture(g, map[string]string{etcdOrphanLeaveAnnotation: "garbage"}, theThree()...)
		_, hold, err := f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(hold).To(BeFalse())
		g.Expect(f.stored().Annotations).ToNot(HaveKey(etcdOrphanLeaveAnnotation))
	})
}

// TestOrphanEtcdMembers_OnlyK0sHA: k3s and single-node control planes have
// no k0s EtcdMembers to reconcile.
func TestOrphanEtcdMembers_OnlyK0sHA(t *testing.T) {
	g := NewWithT(t)
	f := newOrphanFixture(g, nil, withOrphans(k0sEtcdMember("kcp-old", "10.0.0.7", false, "True"))...)
	f.kcp.Spec.Distribution = "k3s"
	_, hold, err := f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hold).To(BeFalse())
	g.Expect(f.members.dialed).To(BeEmpty())

	f.kcp.Spec.Distribution = "k0s"
	_, hold, err = f.r.reconcileOrphanEtcdMembers(context.Background(), log.Log, f.kcp, haCluster(), f.machines[:1], 1)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hold).To(BeFalse())
	g.Expect(f.members.dialed).To(BeEmpty())
}

// TestOrphanEtcdMembers_RateLimited: without a removal in progress the
// EtcdMembers are listed at most once per etcdOrphanCheckInterval.
func TestOrphanEtcdMembers_RateLimited(t *testing.T) {
	g := NewWithT(t)
	f := newOrphanFixture(g, nil, theThree()...)
	_, _, err := f.r.reconcileOrphanEtcdMembers(context.Background(), log.Log, f.kcp, haCluster(), f.machines, 3)
	g.Expect(err).ToNot(HaveOccurred())
	dialed := len(f.members.dialed)
	g.Expect(dialed).ToNot(BeZero())
	_, _, err = f.r.reconcileOrphanEtcdMembers(context.Background(), log.Log, f.kcp, haCluster(), f.machines, 3)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(f.members.dialed).To(HaveLen(dialed), "no second check within the interval")
}

// TestOrphanEtcdMembers_DriftedMemberNameBlocksRemoval is the security
// review's must-fix: three Machines and three members, but cp-2's member goes
// by another name, and (as on CAPK) its peer address is on a network no
// Machine reports. That member is cp-2's live voter, not an orphan. Every
// Machine must be paired with a distinct member before anything is removed,
// so nothing is written and a Warning says why.
func TestOrphanEtcdMembers_DriftedMemberNameBlocksRemoval(t *testing.T) {
	g := NewWithT(t)
	f := newOrphanFixture(g, nil,
		k0sEtcdMember("cp-0", "10.0.0.10", false, "True"),
		k0sEtcdMember("cp-1", "10.0.0.11", false, "True"),
		k0sEtcdMember("cp-2-renamed", "192.168.50.12:2380", false, "True"),
	)

	_, hold, err := f.reconcile()

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hold).To(BeFalse())
	g.Expect(f.leaveSet("cp-2-renamed")).To(BeFalse(), "a live voter must never be asked to leave")
	g.Expect(f.stored().Annotations).ToNot(HaveKey(etcdOrphanLeaveAnnotation))
	g.Expect(f.events()).To(ConsistOf(And(ContainSubstring("Warning EtcdOrphanMemberRemovalBlocked"), ContainSubstring("naming does not match"), ContainSubstring("cp-2"))))
}

// TestOrphanEtcdMembers_PeerAddressPairsARenamedMember: a member whose name
// drifted but whose peer address its Machine reports is paired with that
// Machine; a real orphan next to it is still removed.
func TestOrphanEtcdMembers_PeerAddressPairsARenamedMember(t *testing.T) {
	g := NewWithT(t)
	f := newOrphanFixture(g, nil,
		k0sEtcdMember("cp-0", "10.0.0.10", false, "True"),
		k0sEtcdMember("cp-1", "10.0.0.11", false, "True"),
		k0sEtcdMember("cp-2-renamed", "10.0.0.12:2380", false, "True"),
		k0sEtcdMemberWithID("ghost", "10.0.0.9", "a1", false, "True"),
	)

	_, hold, err := f.reconcile()

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hold).To(BeTrue())
	g.Expect(f.leaveSet("ghost")).To(BeTrue())
	g.Expect(f.leaveSet("cp-2-renamed")).To(BeFalse())
}

func TestUnmatchedMachines(t *testing.T) {
	g := NewWithT(t)
	machines := []*clusterv1.Machine{nodeMachine("cp-0", false, "10.0.0.10"), nodeMachine("cp-1", false, "10.0.0.11")}
	// One member cannot stand for two Machines: cp-1 reports cp-0's address,
	// but cp-0 already took that member by name.
	machines[1].Status.Addresses = append(machines[1].Status.Addresses, clusterv1.MachineAddress{Address: "10.0.0.10"})
	g.Expect(unmatchedMachines(machines, []*unstructured.Unstructured{k0sEtcdMember("cp-0", "10.0.0.10", false, "True")})).To(Equal([]string{"cp-1"}))
	g.Expect(unmatchedMachines(machines, []*unstructured.Unstructured{
		k0sEtcdMember("cp-0", "10.0.0.10", false, "True"), k0sEtcdMember("x", "10.0.0.11:2380", false, "True"),
	})).To(BeEmpty())
}

// TestOrphanEtcdMembers_RecordOutlivesAScaleToOne: a removal recorded before
// the control plane was scaled to one replica is still followed, and still
// holds every other membership change, until it ends.
func TestOrphanEtcdMembers_RecordOutlivesAScaleToOne(t *testing.T) {
	g := NewWithT(t)
	rec := orphanLeave{name: "kcp-old", memberID: testMemberID, at: time.Now().Add(-time.Minute)}
	f := newOrphanFixture(g, map[string]string{etcdOrphanLeaveAnnotation: rec.String()},
		withOrphans(k0sEtcdMember("kcp-old", "10.0.0.7", false, "True"))...)

	_, hold, err := f.r.reconcileOrphanEtcdMembers(context.Background(), log.Log, f.kcp, haCluster(), f.machines, 1)

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hold).To(BeTrue(), "the scale-down waits for the removal in progress")
	g.Expect(f.leaveSet("kcp-old")).To(BeTrue(), "the recorded removal is still driven")

	f.setJoined("kcp-old", "False")
	_, hold, err = f.r.reconcileOrphanEtcdMembers(context.Background(), log.Log, f.kcp, haCluster(), f.machines, 1)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hold).To(BeFalse())
	g.Expect(f.stored().Annotations).ToNot(HaveKey(etcdOrphanLeaveAnnotation))
}

// TestOrphanEtcdMembers_FailedPassKeepsTheSlot: a pass that stops before it
// has listed the members (here, no verified apiserver answers, then a member
// not yet reported healthy) does not use up the once-a-minute check, so the
// next reconcile tries again and removes the orphan. Before, the slot was
// taken first, and a passing failure delayed the cleanup by a full interval.
func TestOrphanEtcdMembers_FailedPassKeepsTheSlot(t *testing.T) {
	g := NewWithT(t)
	f := newOrphanFixture(g, nil, withOrphans(k0sEtcdMember("kcp-old", "10.0.0.7", false, "True"))...)
	pass := func() bool {
		_, hold, err := f.r.reconcileOrphanEtcdMembers(context.Background(), log.Log, f.kcp, haCluster(), f.machines, 3)
		g.Expect(err).ToNot(HaveOccurred())
		return hold
	}

	members := f.r.MemberClientFactory
	f.r.MemberClientFactory = (&fakeMembers{}).factory
	g.Expect(pass()).To(BeFalse())
	g.Expect(f.r.orphanCheckDue(f.kcp)).To(BeTrue(), "no answer: the slot is kept")

	f.r.MemberClientFactory = members
	g.Expect(f.mgmt.Update(context.Background(), etcdStatusSecretForMembersRV(g, f.mgmt, "cp-0", "cp-1"))).To(Succeed())
	g.Expect(pass()).To(BeFalse())
	g.Expect(f.r.orphanCheckDue(f.kcp)).To(BeTrue(), "a member not reported healthy yet: the slot is kept")

	g.Expect(f.mgmt.Update(context.Background(), etcdStatusSecretForMembersRV(g, f.mgmt, "cp-0", "cp-1", "cp-2"))).To(Succeed())
	g.Expect(pass()).To(BeTrue(), "the next reconcile acts at once")
	g.Expect(f.leaveSet("kcp-old")).To(BeTrue())
	g.Expect(f.r.orphanCheckDue(f.kcp)).To(BeFalse(), "an evaluated pass takes the slot")
}

// TestOrphanEtcdMembers_RecordConflictKeepsTheSlot: when the record cannot be
// written because the KairosControlPlane changed (a routine conflict with
// the status update), nothing is asked of k0s, and the slot is given back, so
// the next reconcile records and requests the removal at once. Before, the
// slot stayed taken with no record, and the cleanup waited a full interval:
// the cause of TestHA_K0sOrphanEtcdMemberIsRemoved's flake.
func TestOrphanEtcdMembers_RecordConflictKeepsTheSlot(t *testing.T) {
	g := NewWithT(t)
	f := newOrphanFixture(g, nil, withOrphans(k0sEtcdMember("kcp-old", "10.0.0.7", false, "True"))...)
	conflicts := 1
	f.r.Client = interceptor.NewClient(f.mgmt.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*controlplanev1beta2.KairosControlPlane); ok && conflicts > 0 {
				conflicts--
				return apierrors.NewConflict(controlplanev1beta2.GroupVersion.WithResource("kairoscontrolplanes").GroupResource(), obj.GetName(), errors.New("the object has been modified"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	pass := func() bool {
		_, hold, err := f.r.reconcileOrphanEtcdMembers(context.Background(), log.Log, f.kcp, haCluster(), f.machines, 3)
		g.Expect(err).ToNot(HaveOccurred())
		return hold
	}

	g.Expect(pass()).To(BeTrue(), "this pass holds")
	g.Expect(f.leaveSet("kcp-old")).To(BeFalse(), "nothing is asked of k0s without a record")
	g.Expect(f.r.orphanCheckDue(f.kcp)).To(BeTrue(), "the slot is given back")

	g.Expect(pass()).To(BeTrue())
	g.Expect(f.leaveSet("kcp-old")).To(BeTrue(), "the next reconcile acts at once")
	g.Expect(f.stored().Annotations).To(HaveKey(etcdOrphanLeaveAnnotation))
}

// TestReconcileMachines_OrphanRemovalHoldsMembershipChanges: while an orphan
// etcd-member removal is in progress, reconcileMachines changes no other
// membership (here, a k0s scale-down delete); once it is done, it does.
func TestReconcileMachines_OrphanRemovalHoldsMembershipChanges(t *testing.T) {
	g := NewWithT(t)
	sc := membershipScenarios[1] // scale-down delete
	rec := orphanLeave{name: "kcp-old", memberID: testMemberID, at: time.Now().Add(-time.Minute)}
	run := newMembershipRun(g, sc, map[string]string{
		WorkloadClusterUIDAnnotation: uidA,
		etcdOrphanLeaveAnnotation:    rec.String(),
	}, uidA)
	run.kcp.Spec.Distribution = "k0s"
	// No member answers directly; the endpoint serves the pinned cluster,
	// where k0s still reports the orphan joined.
	run.workload = workloadWithUID(run.r.Scheme, uidA, k0sEtcdMember("kcp-old", "10.0.0.7", true, "True"))
	run.r.MemberClientFactory = (&fakeMembers{}).factory

	requeue, err := run.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(requeue).To(Equal(joinerGateRequeueAfter))
	g.Expect(sc.acted(g, run.c)).To(BeFalse(), "no delete while an orphan removal is in progress")

	// k0s removes the orphan.
	m := getEtcdMember(g, run.workload, "kcp-old")
	m.Object["status"].(map[string]interface{})["conditions"] = []interface{}{map[string]interface{}{"type": "Joined", "status": "False"}}
	g.Expect(run.workload.Update(context.Background(), m)).To(Succeed())

	_, err = run.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(run.storedAnnotations()).ToNot(HaveKey(etcdOrphanLeaveAnnotation))
	g.Expect(sc.acted(g, run.c)).To(BeTrue(), "the scale-down resumes in the same pass")
}

// TestReconcileMachines_OrphanRecordHoldsAScaleToOne: the hold does not
// depend on the control plane still being HA. Scaled to one replica with a
// removal recorded, reconcileMachines deletes nothing until it ends.
func TestReconcileMachines_OrphanRecordHoldsAScaleToOne(t *testing.T) {
	g := NewWithT(t)
	sc := membershipScenarios[1] // scale-down delete of test-kcp-3
	rec := orphanLeave{name: "kcp-old", memberID: testMemberID, at: time.Now().Add(-time.Minute)}
	run := newMembershipRun(g, sc, map[string]string{
		WorkloadClusterUIDAnnotation: uidA,
		etcdOrphanLeaveAnnotation:    rec.String(),
	}, uidA)
	run.kcp.Spec.Distribution = "k0s"
	one := int32(1)
	run.kcp.Spec.Replicas = &one
	run.workload = workloadWithUID(run.r.Scheme, uidA, k0sEtcdMember("kcp-old", "10.0.0.7", true, "True"))
	run.r.MemberClientFactory = (&fakeMembers{}).factory

	requeue, err := run.reconcile()

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(requeue).To(Equal(joinerGateRequeueAfter))
	g.Expect(sc.acted(g, run.c)).To(BeFalse(), "no delete while an orphan removal is in progress, even at one replica")
	g.Expect(run.storedAnnotations()).To(HaveKey(etcdOrphanLeaveAnnotation))
}

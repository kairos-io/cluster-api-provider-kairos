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

// How the identity gate (ADR 0015 PR-2) holds each control-plane membership
// action. Every scenario is first run with a matching identity, to show the
// action does happen, so that "nothing happened" in the frozen runs means the
// freeze held it.

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// frozenAgainst is a mismatch annotation recorded against pin.
func frozenAgainst(pin string) string {
	return uidB + " via endpoint at 2026-10-18T00:00:00Z; pinned " + pin
}

// membershipScenario is a k3s control plane at a point where reconcileMachines
// takes exactly one membership action.
type membershipScenario struct {
	name string
	// build returns the desired replica count and the machines.
	build func(kcp *controlplanev1beta2.KairosControlPlane) (int32, []*clusterv1.Machine)
	// acted reports whether the action happened.
	acted func(g *WithT, c client.Client) bool
}

func createdMachine(name string) func(g *WithT, c client.Client) bool {
	return func(g *WithT, c client.Client) bool {
		names, _ := remainingMachines(g, c)
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
}

func deletedMachine(name string) func(g *WithT, c client.Client) bool {
	return func(g *WithT, c client.Client) bool {
		names, _ := remainingMachines(g, c)
		for _, n := range names {
			if n == name {
				return false
			}
		}
		return true
	}
}

var membershipScenarios = []membershipScenario{
	{
		name: "scale-up joiner",
		build: func(kcp *controlplanev1beta2.KairosControlPlane) (int32, []*clusterv1.Machine) {
			return 3, []*clusterv1.Machine{rolloutMachine(kcp, "test-kcp-0", rolloutNewVersion, time.Hour, true)}
		},
		acted: createdMachine("test-kcp-1"),
	},
	{
		name: "scale-down delete",
		build: func(kcp *controlplanev1beta2.KairosControlPlane) (int32, []*clusterv1.Machine) {
			return 3, []*clusterv1.Machine{
				rolloutMachine(kcp, "test-kcp-0", rolloutNewVersion, 4*time.Hour, true),
				rolloutMachine(kcp, "test-kcp-1", rolloutNewVersion, 3*time.Hour, true),
				rolloutMachine(kcp, "test-kcp-2", rolloutNewVersion, 2*time.Hour, true),
				rolloutMachine(kcp, "test-kcp-3", rolloutNewVersion, time.Hour, true),
			}
		},
		acted: deletedMachine("test-kcp-3"),
	},
	{
		name: "rollout surge",
		build: func(kcp *controlplanev1beta2.KairosControlPlane) (int32, []*clusterv1.Machine) {
			return 3, []*clusterv1.Machine{
				rolloutMachine(kcp, "test-kcp-0", rolloutOldVersion, 4*time.Hour, true),
				rolloutMachine(kcp, "test-kcp-1", rolloutOldVersion, 3*time.Hour, true),
				rolloutMachine(kcp, "test-kcp-2", rolloutOldVersion, 2*time.Hour, true),
			}
		},
		acted: createdMachine("test-kcp-3"),
	},
	{
		name: "rollout delete",
		build: func(kcp *controlplanev1beta2.KairosControlPlane) (int32, []*clusterv1.Machine) {
			return 3, []*clusterv1.Machine{
				rolloutMachine(kcp, "test-kcp-0", rolloutOldVersion, 4*time.Hour, true),
				rolloutMachine(kcp, "test-kcp-1", rolloutOldVersion, 3*time.Hour, true),
				rolloutMachine(kcp, "test-kcp-2", rolloutOldVersion, 2*time.Hour, true),
				rolloutMachine(kcp, "test-kcp-3", rolloutNewVersion, time.Hour, true),
			}
		},
		acted: deletedMachine("test-kcp-0"),
	},
}

// membershipRun is one reconcileMachines pass over a scenario.
type membershipRun struct {
	g        *WithT
	c        client.Client
	r        *KairosControlPlaneReconciler
	kcp      *controlplanev1beta2.KairosControlPlane
	cluster  *clusterv1.Cluster
	rec      *record.FakeRecorder
	workload client.Client
}

// newMembershipRun builds the scenario with annotations on the KCP and a
// workload endpoint whose kube-system UID is workloadUID ("" = unreachable).
func newMembershipRun(g *WithT, sc membershipScenario, annotations map[string]string, workloadUID string) *membershipRun {
	scheme := machineMetaScheme(g)
	kcp := rolloutKCP(1)
	kcp.Annotations = annotations
	conditions.MarkTrue(kcp, controlplanev1beta2.KubeconfigReadyCondition)
	desired, machines := sc.build(kcp)
	kcp.Spec.Replicas = &desired
	cluster := rolloutCluster()

	objs := []client.Object{kcp, machineMetaInfraTemplate(), rolloutEtcdStatus(cluster, machines...)}
	for _, m := range machines {
		objs = append(objs, m)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	run := &membershipRun{g: g, c: c, cluster: cluster, rec: record.NewFakeRecorder(50)}
	run.kcp = &controlplanev1beta2.KairosControlPlane{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(kcp), run.kcp)).To(Succeed())
	// The status the reconcile works with (the fake has no status subresource
	// semantics here, but keep the in-memory conditions explicit).
	conditions.MarkTrue(run.kcp, controlplanev1beta2.KubeconfigReadyCondition)
	if workloadUID != "" {
		run.workload = workloadWithUID(scheme, workloadUID)
	}
	run.r = &KairosControlPlaneReconciler{Client: c, Scheme: scheme, Recorder: run.rec,
		WorkloadClientFactory: func(context.Context, *clusterv1.Cluster) (client.Client, error) {
			if run.workload == nil {
				return nil, errors.New("the control-plane endpoint does not answer")
			}
			return run.workload, nil
		}}
	return run
}

func (m *membershipRun) reconcile() (time.Duration, error) {
	res, err := m.r.reconcileMachines(context.Background(), log.Log, m.kcp, m.cluster)
	return res.RequeueAfter, err
}

func (m *membershipRun) storedAnnotations() map[string]string {
	m.g.THelper()
	got := &controlplanev1beta2.KairosControlPlane{}
	m.g.Expect(m.c.Get(context.Background(), client.ObjectKeyFromObject(m.kcp), got)).To(Succeed())
	return got.Annotations
}

func TestReconcileMachines_IdentityGatesEveryMembershipAction(t *testing.T) {
	for _, sc := range membershipScenarios {
		t.Run(sc.name+": acts when the identity matches", func(t *testing.T) {
			g := NewWithT(t)
			run := newMembershipRun(g, sc, map[string]string{WorkloadClusterUIDAnnotation: uidA}, uidA)
			_, err := run.reconcile()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(sc.acted(g, run.c)).To(BeTrue())
			g.Expect(run.storedAnnotations()).ToNot(HaveKey(WorkloadClusterIdentityMismatchAnnotation))
		})

		t.Run(sc.name+": held while frozen", func(t *testing.T) {
			g := NewWithT(t)
			run := newMembershipRun(g, sc, map[string]string{
				WorkloadClusterUIDAnnotation:              uidA,
				WorkloadClusterIdentityMismatchAnnotation: frozenAgainst(uidA),
			}, uidA)
			requeue, err := run.reconcile()
			g.Expect(err).ToNot(HaveOccurred(), "a freeze holds, it does not fail the reconcile")
			g.Expect(requeue).To(Equal(workloadIdentityFrozenRequeueAfter))
			g.Expect(sc.acted(g, run.c)).To(BeFalse())
			g.Expect(run.storedAnnotations()).To(HaveKey(WorkloadClusterIdentityMismatchAnnotation),
				"answers that match again do not lift the freeze")
		})

		t.Run(sc.name+": the action's own check freezes on a mismatch", func(t *testing.T) {
			g := NewWithT(t)
			run := newMembershipRun(g, sc, map[string]string{WorkloadClusterUIDAnnotation: uidA}, uidB)
			// The periodic recheck has just run, so only the action's own
			// check can see the switch.
			run.r.markIdentityChecked(run.kcp)
			requeue, err := run.reconcile()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(requeue).To(Equal(workloadIdentityFrozenRequeueAfter))
			g.Expect(sc.acted(g, run.c)).To(BeFalse())
			g.Expect(run.storedAnnotations()[WorkloadClusterIdentityMismatchAnnotation]).To(HaveSuffix("; pinned " + uidA))
		})

		t.Run(sc.name+": held while the identity cannot be verified", func(t *testing.T) {
			g := NewWithT(t)
			run := newMembershipRun(g, sc, map[string]string{WorkloadClusterUIDAnnotation: uidA}, "")
			requeue, err := run.reconcile()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(requeue).To(BeNumerically(">", 0))
			g.Expect(sc.acted(g, run.c)).To(BeFalse())
			g.Expect(run.storedAnnotations()).ToNot(HaveKey(WorkloadClusterIdentityMismatchAnnotation),
				"an unreachable workload cluster is a hold, not a freeze")
		})
	}
}

// TestReconcileMachines_PinsBeforeTheFirstMembershipAction: an HA control
// plane with a registered member is pinned before anything else happens; if
// it cannot be pinned, nothing happens.
func TestReconcileMachines_PinsBeforeTheFirstMembershipAction(t *testing.T) {
	sc := membershipScenarios[0] // scale-up joiner

	t.Run("pinned, then acts", func(t *testing.T) {
		g := NewWithT(t)
		run := newMembershipRun(g, sc, nil, uidA)
		_, err := run.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(run.storedAnnotations()).To(HaveKeyWithValue(WorkloadClusterUIDAnnotation, uidA))
		g.Expect(sc.acted(g, run.c)).To(BeTrue())
	})

	t.Run("cannot be pinned, nothing happens", func(t *testing.T) {
		g := NewWithT(t)
		run := newMembershipRun(g, sc, nil, "")
		requeue, err := run.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(requeue).To(Equal(joinerGateRequeueAfter))
		g.Expect(run.storedAnnotations()).To(BeEmpty())
		g.Expect(sc.acted(g, run.c)).To(BeFalse())
	})
}

// TestReconcileMachines_OperatorRepinResumes: after an operator verifies the
// cluster and re-pins the annotation, membership changes resume.
func TestReconcileMachines_OperatorRepinResumes(t *testing.T) {
	g := NewWithT(t)
	sc := membershipScenarios[1] // scale-down delete
	run := newMembershipRun(g, sc, map[string]string{
		WorkloadClusterUIDAnnotation:              uidA,
		WorkloadClusterIdentityMismatchAnnotation: frozenAgainst(uidA),
	}, uidB)

	_, err := run.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(sc.acted(g, run.c)).To(BeFalse())

	// The operator re-pins to the UID the cluster now has.
	cur := &controlplanev1beta2.KairosControlPlane{}
	g.Expect(run.c.Get(context.Background(), client.ObjectKeyFromObject(run.kcp), cur)).To(Succeed())
	base := cur.DeepCopy()
	cur.Annotations[WorkloadClusterUIDAnnotation] = uidB
	g.Expect(run.c.Patch(context.Background(), cur, client.MergeFrom(base))).To(Succeed())
	g.Expect(run.c.Get(context.Background(), client.ObjectKeyFromObject(run.kcp), run.kcp)).To(Succeed())
	conditions.MarkTrue(run.kcp, controlplanev1beta2.KubeconfigReadyCondition)

	_, err = run.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(sc.acted(g, run.c)).To(BeTrue())
	g.Expect(run.storedAnnotations()).ToNot(HaveKey(WorkloadClusterIdentityMismatchAnnotation))
}

// TestCreateAndDeleteControlPlaneMachine_RefuseWhileFrozen: the two choke
// points hold the freeze even for a caller that did not check it.
func TestCreateAndDeleteControlPlaneMachine_RefuseWhileFrozen(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := machineMetaKCP("k3s", nil)
	kcp.Annotations = map[string]string{WorkloadClusterIdentityMismatchAnnotation: frozenAgainst(uidA)}
	victim := machineMetaMachine("test-kcp-0", nil, nil)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineMetaInfraTemplate(), victim).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	err := r.createControlPlaneMachine(context.Background(), log.Log, kcp, machineMetaCluster(), 1, bootstrapv1beta2.ControlPlaneRoleJoin, "")
	g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "test-kcp-1", Namespace: "default"}, &bootstrapv1beta2.KairosConfig{})).ToNot(Succeed())

	err = r.deleteControlPlaneMachine(context.Background(), kcp, victim)
	g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
	got := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(victim), got)).To(Succeed())
	g.Expect(got.DeletionTimestamp.IsZero()).To(BeTrue())
}

// --- etcd-leave hook removal ---------------------------------------------

// kubeSystem is the workload kube-system Namespace with uid.
func kubeSystem(uid string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: metav1.NamespaceSystem, UID: types.UID(uid)}}
}

// leaveKCP is vipKCP with annotations, stored in the fixture's management
// client so the freeze can be recorded on it.
func leaveKCP(g *WithT, f *leaveFixture, annotations map[string]string) *controlplanev1beta2.KairosControlPlane {
	kcp := vipKCP()
	kcp.Annotations = annotations
	g.Expect(f.mgmt.Create(context.Background(), kcp)).To(Succeed())
	return kcp
}

// TestReconcileMemberLeave_IdentityGatesHookRemoval: the leave of cp-0 has
// timed out and its EtcdMember cannot be found. Through the pinned cluster
// that releases the hook with a warning, as before. Through another cluster
// it must not: that is how the original cluster's members were released on
// 2026-10-17.
func TestReconcileMemberLeave_IdentityGatesHookRemoval(t *testing.T) {
	stale := map[string]string{
		etcdLeaveRequestedAtAnnotation:       ago(20 * time.Minute),
		etcdMemberLeaveRequestedAtAnnotation: ago(15 * time.Minute),
	}

	t.Run("the pinned cluster: the hook is released", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixture(g, stale, []client.Object{kubeSystem(uidA)}, nil)
		kcp := leaveKCP(g, f, map[string]string{WorkloadClusterUIDAnnotation: uidA})
		done, err := f.r.reconcileMemberLeave(context.Background(), log.Log, kcp, haCluster(), f.machines, f.target)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
	})

	t.Run("another cluster: frozen, the hook is kept, nothing is written", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixture(g, stale, []client.Object{kubeSystem(uidB)}, nil)
		kcp := leaveKCP(g, f, map[string]string{WorkloadClusterUIDAnnotation: uidA})
		before := leaveCM(g, f.member)

		done, err := f.r.reconcileMemberLeave(context.Background(), log.Log, kcp, haCluster(), f.machines, f.target)

		g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
		g.Expect(done).To(BeFalse())
		g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
		g.Expect(leaveCM(g, f.member)).To(Equal(before))
		stored := &controlplanev1beta2.KairosControlPlane{}
		g.Expect(f.mgmt.Get(context.Background(), client.ObjectKeyFromObject(kcp), stored)).To(Succeed())
		g.Expect(stored.Annotations[WorkloadClusterIdentityMismatchAnnotation]).To(HavePrefix(uidB + " via 10.0.0.11 at "))
		g.Expect(f.events()).To(ContainElement(ContainSubstring("Warning WorkloadClusterIdentityChanged")))
	})

	t.Run("frozen: even an acked leave keeps the hook", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(time.Minute)}, []client.Object{kubeSystem(uidA)}, nil)
		g.Expect(f.member.Update(context.Background(), leaveConfigMapWithRV(g, f.member, map[string]string{"cp-0": etcdLeftValue}))).To(Succeed())
		kcp := leaveKCP(g, f, map[string]string{
			WorkloadClusterUIDAnnotation:              uidA,
			WorkloadClusterIdentityMismatchAnnotation: frozenAgainst(uidA),
		})

		done, err := f.r.reconcileMemberLeave(context.Background(), log.Log, kcp, haCluster(), f.machines, f.target)

		g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
		g.Expect(done).To(BeFalse())
		g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
	})

	t.Run("frozen: a Machine without a Node keeps its hook", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, noNodeRef(leavingMachine("cp-0", nil, "10.0.0.10")), nil, nil)
		kcp := leaveKCP(g, f, map[string]string{
			WorkloadClusterUIDAnnotation:              uidA,
			WorkloadClusterIdentityMismatchAnnotation: frozenAgainst(uidA),
		})
		_, err := f.r.reconcileMemberLeave(context.Background(), log.Log, kcp, haCluster(), f.machines, f.target)
		g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
		g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
	})

	t.Run("removeEtcdLeaveHook refuses while frozen", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixture(g, nil, nil, nil)
		kcp := vipKCP()
		kcp.Annotations = map[string]string{WorkloadClusterIdentityMismatchAnnotation: frozenAgainst(uidA)}
		err := f.r.removeEtcdLeaveHook(context.Background(), kcp, f.target)
		g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
		g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
	})
}

// leaveConfigMapWithRV returns the stored leave ConfigMap with data replaced.
func leaveConfigMapWithRV(g *WithT, wc client.Client, data map[string]string) *corev1.ConfigMap {
	g.THelper()
	cm := &corev1.ConfigMap{}
	g.Expect(wc.Get(context.Background(), leaveConfigMapKey, cm)).To(Succeed())
	cm.Data = data
	return cm
}

// TestReconcileDelete_TeardownIgnoresTheFreeze: deleting the whole cluster
// never deadlocks on the freeze. Hooks are stripped and Machines deleted.
func TestReconcileDelete_TeardownIgnoresTheFreeze(t *testing.T) {
	g := NewWithT(t)
	scheme := newCleanupTestScheme(t)
	kcp := newDrainKCP()
	kcp.Annotations = map[string]string{
		WorkloadClusterUIDAnnotation:              uidA,
		WorkloadClusterIdentityMismatchAnnotation: frozenAgainst(uidA),
	}
	kcp.Finalizers = []string{controlplanev1beta2.KairosControlPlaneFinalizer}
	hooked := newOwnedMachine("cp-0", kcp)
	hooked.Annotations = map[string]string{etcdLeaveHookAnnotation(): ""}
	hooked.Finalizers = []string{"test.kairos.io/hold"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp, hooked).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	_, _, err := r.reconcileDelete(context.Background(), log.Log, kcp)

	g.Expect(err).ToNot(HaveOccurred())
	got := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(hooked), got)).To(Succeed())
	g.Expect(hasEtcdLeaveHook(got)).To(BeFalse(), "teardown strips the hook whatever the identity")
	g.Expect(got.DeletionTimestamp.IsZero()).To(BeFalse(), "teardown deletes the Machine whatever the identity")
}

// --- k0s join tokens -------------------------------------------------------

// pinPolicyEnv adds annotations to the policy env's KCP and reloads it.
func pinPolicyEnv(t *testing.T, e *policyEnv, annotations map[string]string) {
	t.Helper()
	g := NewWithT(t)
	cur := &controlplanev1beta2.KairosControlPlane{}
	g.Expect(e.mgmt.Get(context.Background(), client.ObjectKeyFromObject(e.kcp), cur)).To(Succeed())
	base := cur.DeepCopy()
	if cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		cur.Annotations[k] = v
	}
	g.Expect(e.mgmt.Patch(context.Background(), cur, client.MergeFrom(base))).To(Succeed())
	g.Expect(e.mgmt.Get(context.Background(), client.ObjectKeyFromObject(e.kcp), e.kcp)).To(Succeed())
	conditions.MarkTrue(e.kcp, controlplanev1beta2.KubeconfigReadyCondition)
}

func TestEnsureK0sJoinToken_IdentityGatesTheMint(t *testing.T) {
	t.Run("the pinned cluster: minted", func(t *testing.T) {
		g := NewWithT(t)
		e := newPolicyEnv(t, policyOpts{liveAddrs: []string{"10.0.0.20"}, workload: []client.Object{kubeSystem(uidA)}})
		pinPolicyEnv(t, e, map[string]string{WorkloadClusterUIDAnnotation: uidA})
		id, reason, err := e.ensure()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(reason).To(BeEmpty())
		g.Expect(id).ToNot(BeEmpty())
	})

	t.Run("another cluster: frozen, nothing minted", func(t *testing.T) {
		g := NewWithT(t)
		e := newPolicyEnv(t, policyOpts{liveAddrs: []string{"10.0.0.20"}, workload: []client.Object{kubeSystem(uidB)}})
		pinPolicyEnv(t, e, map[string]string{WorkloadClusterUIDAnnotation: uidA})
		id, _, err := e.ensure()
		g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
		g.Expect(id).To(BeEmpty())
		g.Expect(e.workloadTokenIDs()).To(BeEmpty())
		g.Expect(e.mgmtSecret().Data).ToNot(HaveKey(joinTokenSecretDataKey))
		_, frozen := workloadIdentityFrozen(e.kcp)
		g.Expect(frozen).To(BeTrue())
	})

	t.Run("frozen: nothing read, nothing minted", func(t *testing.T) {
		g := NewWithT(t)
		e := newPolicyEnv(t, policyOpts{liveAddrs: []string{"10.0.0.20"}, workload: []client.Object{kubeSystem(uidA)}})
		pinPolicyEnv(t, e, map[string]string{WorkloadClusterUIDAnnotation: uidA, WorkloadClusterIdentityMismatchAnnotation: frozenAgainst(uidA)})
		_, _, err := e.ensure()
		g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
		g.Expect(e.factoryCalls).To(BeZero())
		g.Expect(e.workloadTokenIDs()).To(BeEmpty())
	})
}

func TestK0sJoinTokenLifetime_IdentityGatesTheExtension(t *testing.T) {
	setup := func(t *testing.T, uid string, annotations map[string]string) (*policyEnv, *corev1.Secret) {
		enc, ws := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(29*time.Minute))
		e := newPolicyEnv(t, policyOpts{
			storedToken: enc, storedAnno: "aaaaaa", workload: []client.Object{ws, kubeSystem(uid)}, liveAddrs: []string{"10.0.0.20"},
			machines: []*clusterv1.Machine{cpMachine("cp-1", "", policyNow.Add(-2*time.Hour), "")},
			configs:  []client.Object{joinConfig("cp-1", "aaaaaa")},
		})
		pinPolicyEnv(t, e, annotations)
		return e, ws
	}

	t.Run("the pinned cluster: extended", func(t *testing.T) {
		g := NewWithT(t)
		e, ws := setup(t, uidA, map[string]string{WorkloadClusterUIDAnnotation: uidA})
		e.lifetime()
		got, err := e.workloadToken("aaaaaa")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got.Data["expiration"]).ToNot(Equal(ws.Data["expiration"]))
	})

	t.Run("another cluster: frozen, not extended", func(t *testing.T) {
		g := NewWithT(t)
		e, ws := setup(t, uidB, map[string]string{WorkloadClusterUIDAnnotation: uidA})
		e.lifetime()
		got, err := e.workloadToken("aaaaaa")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got.Data["expiration"]).To(Equal(ws.Data["expiration"]))
		_, frozen := workloadIdentityFrozen(e.kcp)
		g.Expect(frozen).To(BeTrue())
	})

	t.Run("frozen: not extended, nothing read", func(t *testing.T) {
		g := NewWithT(t)
		e, ws := setup(t, uidA, map[string]string{WorkloadClusterUIDAnnotation: uidA, WorkloadClusterIdentityMismatchAnnotation: frozenAgainst(uidA)})
		e.lifetime()
		got, err := e.workloadToken("aaaaaa")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got.Data["expiration"]).To(Equal(ws.Data["expiration"]))
		g.Expect(e.factoryCalls).To(BeZero())
	})
}

// TestConnectForLeave_UnverifiedMemberIsSkipped: a member whose identity
// cannot be read counts as not answering, and the next one is used.
func TestConnectForLeave_UnverifiedMemberIsSkipped(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	target := leavingMachine("cp-0", nil, "10.0.0.10")
	machines := []*clusterv1.Machine{target, nodeMachine("cp-1", false, "10.0.0.11"), nodeMachine("cp-2", false, "10.0.0.12")}
	mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).Build()
	members := &fakeMembers{clients: map[string]client.Client{
		"10.0.0.11": fake.NewClientBuilder().WithScheme(scheme).Build(), // no kube-system to read
		"10.0.0.12": workloadWithUID(scheme, uidA),
	}}
	r := &KairosControlPlaneReconciler{Client: mgmt, Scheme: scheme,
		WorkloadClientFactory: staticWorkloadClient(hungClient(scheme)), MemberClientFactory: members.factory}
	kcp := vipKCP()
	kcp.Annotations = map[string]string{WorkloadClusterUIDAnnotation: uidA}

	conn, err := r.connectForLeave(context.Background(), log.Log, kcp, haCluster(), machines, target)

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(conn.via).To(Equal("10.0.0.12"))
	g.Expect(members.dialed).To(Equal([]string{"10.0.0.11", "10.0.0.12"}))
}

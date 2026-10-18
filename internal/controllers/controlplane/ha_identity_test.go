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
	"errors"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

const (
	uidA = "11111111-1111-1111-1111-111111111111"
	uidB = "22222222-2222-2222-2222-222222222222"
	uidC = "33333333-3333-3333-3333-333333333333"
)

// workloadWithUID is a workload cluster whose kube-system Namespace has uid.
// The fake client does not assign UIDs, so the test sets it.
func workloadWithUID(scheme *runtime.Scheme, uid string, objs ...client.Object) client.Client {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: metav1.NamespaceSystem, UID: types.UID(uid)}}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{ns}, objs...)...).Build()
}

// withPinnedWorkload pins kcp to uidA, as a KCP running this code has done on
// its first reconcile, and returns a workload factory whose cluster has that
// identity. Tests of the membership logic use it so the identity gate passes.
func withPinnedWorkload(scheme *runtime.Scheme, kcp *controlplanev1beta2.KairosControlPlane, objs ...client.Object) func(context.Context, *clusterv1.Cluster) (client.Client, error) {
	if kcp.Annotations == nil {
		kcp.Annotations = map[string]string{}
	}
	kcp.Annotations[WorkloadClusterUIDAnnotation] = uidA
	return staticWorkloadClient(workloadWithUID(scheme, uidA, objs...))
}

// identityFixture is an HA k0s control plane with KubeconfigReady True, a
// management client holding the KCP, a workload endpoint and two staying
// members reachable directly.
type identityFixture struct {
	g        *WithT
	scheme   *runtime.Scheme
	r        *KairosControlPlaneReconciler
	mgmt     client.Client
	kcp      *controlplanev1beta2.KairosControlPlane
	cluster  *clusterv1.Cluster
	machines []*clusterv1.Machine
	rec      *record.FakeRecorder
	members  *fakeMembers
	// endpoint is what the WorkloadClientFactory returns; nil fails the build.
	endpoint      client.Client
	endpointCalls int
}

func newIdentityFixture(g *WithT, annotations map[string]string) *identityFixture {
	f := &identityFixture{g: g, scheme: haTestScheme(g), cluster: haCluster(), rec: record.NewFakeRecorder(50)}
	kcp := vipKCP()
	kcp.Annotations = annotations
	conditions.MarkTrue(kcp, controlplanev1beta2.KubeconfigReadyCondition)
	f.mgmt = fake.NewClientBuilder().WithScheme(f.scheme).WithObjects(kcp).Build()
	f.kcp = &controlplanev1beta2.KairosControlPlane{}
	g.Expect(f.mgmt.Get(context.Background(), client.ObjectKeyFromObject(kcp), f.kcp)).To(Succeed())
	f.machines = []*clusterv1.Machine{
		nodeMachine("cp-0", false, "10.0.0.10"),
		nodeMachine("cp-1", false, "10.0.0.11"),
	}
	f.endpoint = workloadWithUID(f.scheme, uidA)
	f.members = &fakeMembers{clients: map[string]client.Client{
		"10.0.0.10": workloadWithUID(f.scheme, uidA),
		"10.0.0.11": workloadWithUID(f.scheme, uidA),
	}}
	f.r = &KairosControlPlaneReconciler{
		Client: f.mgmt, Scheme: f.scheme, Recorder: f.rec,
		WorkloadClientFactory: func(context.Context, *clusterv1.Cluster) (client.Client, error) {
			f.endpointCalls++
			if f.endpoint == nil {
				return nil, errors.New("the control-plane endpoint does not answer")
			}
			return f.endpoint, nil
		},
		MemberClientFactory: f.members.factory,
	}
	return f
}

func (f *identityFixture) reconcile() identityGate {
	f.g.THelper()
	gate, err := f.r.reconcileWorkloadIdentity(context.Background(), log.Log, f.kcp, f.cluster, f.machines, 3)
	f.g.Expect(err).ToNot(HaveOccurred())
	return gate
}

// stored is the KCP as the management apiserver has it.
func (f *identityFixture) stored() *controlplanev1beta2.KairosControlPlane {
	f.g.THelper()
	got := &controlplanev1beta2.KairosControlPlane{}
	f.g.Expect(f.mgmt.Get(context.Background(), client.ObjectKeyFromObject(f.kcp), got)).To(Succeed())
	return got
}

// operatorAnnotates plays an operator editing the KCP's annotations, then
// reloads the reconcile's copy, as the next reconcile would.
func (f *identityFixture) operatorAnnotates(set map[string]string, remove ...string) {
	f.g.THelper()
	cur := f.stored()
	base := cur.DeepCopy()
	if cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	for k, v := range set {
		cur.Annotations[k] = v
	}
	for _, k := range remove {
		delete(cur.Annotations, k)
	}
	f.g.Expect(f.mgmt.Patch(context.Background(), cur, client.MergeFrom(base))).To(Succeed())
	f.kcp = f.stored()
}

func (f *identityFixture) events() []string {
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

func TestDisplayUID(t *testing.T) {
	g := NewWithT(t)
	g.Expect(displayUID(uidA)).To(Equal(uidA))
	g.Expect(displayUID("")).To(Equal(identityNone))
	g.Expect(displayUID("a b")).To(Equal(identityMalformed))
	g.Expect(displayUID("x; pinned y")).To(Equal(identityMalformed))
	g.Expect(displayUID(strings.Repeat("a", 65))).To(Equal(identityMalformed))
}

func TestRecordedPin(t *testing.T) {
	g := NewWithT(t)
	pin, ok := recordedPin(uidB + " via endpoint at 2026-10-18T00:00:00Z; pinned " + uidA)
	g.Expect(ok).To(BeTrue())
	g.Expect(pin).To(Equal(uidA))
	pin, ok = recordedPin(uidB + " via 10.0.0.10 at 2026-10-18T00:00:00Z; pinned <none>")
	g.Expect(ok).To(BeTrue())
	g.Expect(pin).To(Equal(identityNone))
	_, ok = recordedPin("edited by hand")
	g.Expect(ok).To(BeFalse())
}

func TestIdentityApplies(t *testing.T) {
	pinned := func() *controlplanev1beta2.KairosControlPlane {
		k := vipKCP()
		k.Annotations = map[string]string{WorkloadClusterUIDAnnotation: uidA}
		return k
	}
	registered := nodeMachine("cp-0", false)
	unregistered := noNodeRef(nodeMachine("cp-1", false))
	for _, tc := range []struct {
		name     string
		kcp      *controlplanev1beta2.KairosControlPlane
		machines []*clusterv1.Machine
		desired  int32
		want     bool
	}{
		{"HA with a registered member", vipKCP(), []*clusterv1.Machine{registered}, 3, true},
		{"HA before any member registered", vipKCP(), []*clusterv1.Machine{unregistered}, 3, false},
		{"HA with no machine yet", vipKCP(), nil, 3, false},
		{"single node", vipKCP(), []*clusterv1.Machine{registered}, 1, false},
		{"scaling down to one, two machines left", vipKCP(), []*clusterv1.Machine{registered, unregistered}, 1, true},
		{"pinned single node is still checked", pinned(), []*clusterv1.Machine{registered}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			NewWithT(t).Expect(identityApplies(tc.kcp, tc.machines, tc.desired)).To(Equal(tc.want))
		})
	}
}

// TestReconcileWorkloadIdentity_PinsWhenEveryPathAgrees: the first contact
// reads the identity through the endpoint and every reachable member, and
// pins it when they agree. This is also how an existing cluster is pinned on
// the first reconcile after an upgrade.
func TestReconcileWorkloadIdentity_PinsWhenEveryPathAgrees(t *testing.T) {
	g := NewWithT(t)
	f := newIdentityFixture(g, nil)

	gate := f.reconcile()

	g.Expect(gate).To(Equal(identityGate{}))
	g.Expect(f.stored().Annotations).To(HaveKeyWithValue(WorkloadClusterUIDAnnotation, uidA))
	g.Expect(f.kcp.Annotations).To(HaveKeyWithValue(WorkloadClusterUIDAnnotation, uidA), "the reconcile's copy carries the pin too")
	g.Expect(f.members.dialed).To(ConsistOf("10.0.0.10", "10.0.0.11"))
	g.Expect(f.events()).To(ConsistOf(ContainSubstring("Normal WorkloadClusterIdentityPinned")))
}

// TestReconcileWorkloadIdentity_UpgradePinsAnExistingCluster: a running
// three-member cluster that a KCP without the pin managed is pinned on the
// first reconcile, and the pin is not rewritten afterwards.
func TestReconcileWorkloadIdentity_UpgradePinsAnExistingCluster(t *testing.T) {
	g := NewWithT(t)
	f := newIdentityFixture(g, nil)
	f.machines = append(f.machines, nodeMachine("cp-2", false, "10.0.0.12"))
	f.members.clients["10.0.0.12"] = workloadWithUID(f.scheme, uidA)

	g.Expect(f.reconcile()).To(Equal(identityGate{}))
	g.Expect(f.stored().Annotations).To(HaveKeyWithValue(WorkloadClusterUIDAnnotation, uidA))
	rv := f.stored().ResourceVersion

	f.r.forgetIdentityCheck(f.kcp)
	g.Expect(f.reconcile()).To(Equal(identityGate{}))
	g.Expect(f.stored().ResourceVersion).To(Equal(rv), "a matching recheck writes nothing")
}

func TestReconcileWorkloadIdentity_DisagreementAtPinFreezes(t *testing.T) {
	g := NewWithT(t)
	f := newIdentityFixture(g, nil)
	f.members.clients["10.0.0.11"] = workloadWithUID(f.scheme, uidB)

	gate := f.reconcile()

	g.Expect(gate.frozen).To(BeTrue())
	stored := f.stored()
	g.Expect(stored.Annotations).ToNot(HaveKey(WorkloadClusterUIDAnnotation), "nothing is pinned when the paths disagree")
	g.Expect(stored.Annotations[WorkloadClusterIdentityMismatchAnnotation]).To(
		MatchRegexp(`^` + uidB + ` via 10\.0\.0\.11 at \S+; pinned <none>$`))
	ev := f.events()
	g.Expect(ev).To(HaveLen(1))
	g.Expect(ev[0]).To(ContainSubstring("Warning WorkloadClusterIdentityChanged"))
	g.Expect(ev[0]).To(ContainSubstring("endpoint=" + uidA))
	g.Expect(ev[0]).To(ContainSubstring("10.0.0.11=" + uidB))
}

// TestReconcileWorkloadIdentity_MalformedAnswerNeverBecomesThePin: an answer
// that is not UID-shaped is still an answer. It is never pinned, even when
// every path returns it, and it freezes the control plane with a sanitized
// value: nothing a workload apiserver returns reaches the annotation, the log
// or the event verbatim.
func TestReconcileWorkloadIdentity_MalformedAnswerNeverBecomesThePin(t *testing.T) {
	for _, tc := range []struct {
		name string
		uid  string
	}{
		{"very long", strings.Repeat("a", 4096)},
		{"newline", "11111111-1111\nWarning forged-line"},
		{"control characters", "\x1b[31m" + uidA + "\x00"},
		{"separator", uidA + "; pinned " + uidB},
	} {
		t.Run(tc.name+", one path", func(t *testing.T) {
			g := NewWithT(t)
			f := newIdentityFixture(g, nil)
			f.members.clients["10.0.0.11"] = workloadWithUID(f.scheme, tc.uid)

			gate := f.reconcile()

			g.Expect(gate.frozen).To(BeTrue())
			stored := f.stored()
			g.Expect(stored.Annotations).ToNot(HaveKey(WorkloadClusterUIDAnnotation))
			g.Expect(stored.Annotations[WorkloadClusterIdentityMismatchAnnotation]).To(
				MatchRegexp(`^<malformed> via 10\.0\.0\.11 at \S+; pinned <none>$`))
			ev := strings.Join(f.events(), "\n")
			g.Expect(ev).To(ContainSubstring("Warning WorkloadClusterIdentityChanged"))
			g.Expect(ev).To(ContainSubstring("10.0.0.11=<malformed>"))
			g.Expect(ev).ToNot(ContainSubstring(tc.uid))
		})

		t.Run(tc.name+", every path agrees", func(t *testing.T) {
			g := NewWithT(t)
			f := newIdentityFixture(g, nil)
			f.endpoint = workloadWithUID(f.scheme, tc.uid)
			for ip := range f.members.clients {
				f.members.clients[ip] = workloadWithUID(f.scheme, tc.uid)
			}

			gate := f.reconcile()

			g.Expect(gate.frozen).To(BeTrue(), "agreement on a malformed UID must not pin it")
			stored := f.stored()
			g.Expect(stored.Annotations).ToNot(HaveKey(WorkloadClusterUIDAnnotation))
			g.Expect(stored.Annotations[WorkloadClusterIdentityMismatchAnnotation]).To(HavePrefix("<malformed> via "))
			for _, e := range f.events() {
				g.Expect(e).ToNot(ContainSubstring("WorkloadClusterIdentityPinned"))
				g.Expect(e).ToNot(ContainSubstring(tc.uid))
			}
		})
	}
}

// TestReconcileWorkloadIdentity_VanishedMarkerRechecksAtOnce: when an
// operator removes the mismatch marker, the next reconcile rechecks every
// path at once, not up to a minute later.
func TestReconcileWorkloadIdentity_VanishedMarkerRechecksAtOnce(t *testing.T) {
	g := NewWithT(t)
	f := newIdentityFixture(g, map[string]string{WorkloadClusterUIDAnnotation: uidA})
	g.Expect(f.reconcile()).To(Equal(identityGate{}))
	g.Expect(f.endpointCalls).To(Equal(1))

	// The freeze lands within the interval (here: recorded by another check).
	f.operatorAnnotates(map[string]string{WorkloadClusterIdentityMismatchAnnotation: frozenAgainst(uidA)})
	g.Expect(f.reconcile().frozen).To(BeTrue())

	f.endpoint = workloadWithUID(f.scheme, uidB)
	f.operatorAnnotates(nil, WorkloadClusterIdentityMismatchAnnotation)
	g.Expect(f.reconcile().frozen).To(BeTrue(), "the cluster still differs: frozen again at once")
	g.Expect(f.endpointCalls).To(Equal(2), "the recheck ran on the first reconcile after the marker went")
}

func TestReconcileWorkloadIdentity_HoldsWhenNothingAnswers(t *testing.T) {
	g := NewWithT(t)
	f := newIdentityFixture(g, nil)
	f.endpoint = nil
	f.members.clients = nil

	gate := f.reconcile()

	g.Expect(gate.frozen).To(BeFalse())
	g.Expect(gate.hold).To(ContainSubstring("no workload apiserver answered"))
	g.Expect(f.stored().Annotations).To(BeEmpty())
}

// TestReconcileWorkloadIdentity_AnswerWithoutUIDIsNoAnswer: a kube-system
// Namespace with no UID is not an identity.
func TestReconcileWorkloadIdentity_AnswerWithoutUIDIsNoAnswer(t *testing.T) {
	g := NewWithT(t)
	f := newIdentityFixture(g, nil)
	f.endpoint = workloadWithUID(f.scheme, "")
	f.members.clients = nil

	gate := f.reconcile()

	g.Expect(gate.hold).To(ContainSubstring("has no UID"))
	g.Expect(f.stored().Annotations).To(BeEmpty())
}

func TestReconcileWorkloadIdentity_WaitsForTheKubeconfig(t *testing.T) {
	g := NewWithT(t)
	f := newIdentityFixture(g, nil)
	conditions.MarkFalse(f.kcp, controlplanev1beta2.KubeconfigReadyCondition, "WaitingForNodePush", clusterv1.ConditionSeverityInfo, "")

	gate := f.reconcile()

	g.Expect(gate.hold).To(ContainSubstring("kubeconfig is not ready"))
	g.Expect(f.endpointCalls).To(BeZero())
	g.Expect(f.stored().Annotations).To(BeEmpty())
}

func TestReconcileWorkloadIdentity_NotApplicableReadsNothing(t *testing.T) {
	g := NewWithT(t)
	f := newIdentityFixture(g, nil)
	f.machines = []*clusterv1.Machine{noNodeRef(nodeMachine("cp-0", false, "10.0.0.10"))}

	g.Expect(f.reconcile()).To(Equal(identityGate{}))
	g.Expect(f.endpointCalls).To(BeZero())
	g.Expect(f.members.dialed).To(BeEmpty())
	g.Expect(f.stored().Annotations).To(BeEmpty())
}

// TestReconcileWorkloadIdentity_RecheckFreezesOnASwitch: with the identity
// pinned and no action pending, the periodic recheck notices that a member
// now serves another cluster.
func TestReconcileWorkloadIdentity_RecheckFreezesOnASwitch(t *testing.T) {
	g := NewWithT(t)
	f := newIdentityFixture(g, map[string]string{WorkloadClusterUIDAnnotation: uidA})
	f.members.clients["10.0.0.11"] = workloadWithUID(f.scheme, uidB)

	gate := f.reconcile()

	g.Expect(gate.frozen).To(BeTrue())
	g.Expect(f.stored().Annotations[WorkloadClusterIdentityMismatchAnnotation]).To(
		MatchRegexp(`^` + uidB + ` via 10\.0\.0\.11 at \S+; pinned ` + uidA + `$`))
	g.Expect(f.stored().Annotations).To(HaveKeyWithValue(WorkloadClusterUIDAnnotation, uidA), "the pin is never rewritten by the KCP")
	ev := f.events()
	g.Expect(ev).To(HaveLen(1))
	g.Expect(ev[0]).To(ContainSubstring("Warning WorkloadClusterIdentityChanged"))
	g.Expect(ev[0]).To(ContainSubstring(uidA))
	g.Expect(ev[0]).To(ContainSubstring(uidB))
	g.Expect(ev[0]).To(ContainSubstring("10.0.0.11"))
}

// TestReconcileWorkloadIdentity_RecheckIsRateLimited: the full recheck runs
// at most once per interval.
func TestReconcileWorkloadIdentity_RecheckIsRateLimited(t *testing.T) {
	g := NewWithT(t)
	f := newIdentityFixture(g, map[string]string{WorkloadClusterUIDAnnotation: uidA})

	g.Expect(f.reconcile()).To(Equal(identityGate{}))
	g.Expect(f.endpointCalls).To(Equal(1))

	f.endpoint = workloadWithUID(f.scheme, uidB)
	g.Expect(f.reconcile()).To(Equal(identityGate{}), "within the interval nothing is read")
	g.Expect(f.endpointCalls).To(Equal(1))

	f.r.forgetIdentityCheck(f.kcp) // the interval has passed
	g.Expect(f.reconcile().frozen).To(BeTrue())
}

// TestReconcileWorkloadIdentity_FreezeIsSticky: once frozen, matching answers
// do not lift the freeze, and nothing is read.
func TestReconcileWorkloadIdentity_FreezeIsSticky(t *testing.T) {
	g := NewWithT(t)
	f := newIdentityFixture(g, map[string]string{
		WorkloadClusterUIDAnnotation:              uidA,
		WorkloadClusterIdentityMismatchAnnotation: uidB + " via endpoint at 2026-10-18T00:00:00Z; pinned " + uidA,
	})

	for i := 0; i < 2; i++ {
		f.r.forgetIdentityCheck(f.kcp)
		gate := f.reconcile()
		g.Expect(gate.frozen).To(BeTrue())
		g.Expect(gate.hold).To(ContainSubstring(WorkloadClusterUIDAnnotation))
	}
	g.Expect(f.endpointCalls).To(BeZero())
	g.Expect(f.stored().Annotations).To(HaveKey(WorkloadClusterIdentityMismatchAnnotation))
}

// TestReconcileWorkloadIdentity_OperatorRepin: only an operator clears the
// freeze. Changing the pin to the verified UID clears it and checks against
// the new pin; removing the pin clears it and pins again; removing the
// mismatch annotation alone lifts it until the next check.
func TestReconcileWorkloadIdentity_OperatorRepin(t *testing.T) {
	frozenAgainstA := map[string]string{
		WorkloadClusterUIDAnnotation:              uidA,
		WorkloadClusterIdentityMismatchAnnotation: uidB + " via endpoint at 2026-10-18T00:00:00Z; pinned " + uidA,
	}

	t.Run("setting the pin to the verified UID clears the freeze", func(t *testing.T) {
		g := NewWithT(t)
		f := newIdentityFixture(g, frozenAgainstA)
		f.endpoint = workloadWithUID(f.scheme, uidB)
		f.members.clients = map[string]client.Client{"10.0.0.10": workloadWithUID(f.scheme, uidB), "10.0.0.11": workloadWithUID(f.scheme, uidB)}
		f.operatorAnnotates(map[string]string{WorkloadClusterUIDAnnotation: uidB})

		g.Expect(f.reconcile()).To(Equal(identityGate{}))
		stored := f.stored()
		g.Expect(stored.Annotations).ToNot(HaveKey(WorkloadClusterIdentityMismatchAnnotation))
		g.Expect(stored.Annotations).To(HaveKeyWithValue(WorkloadClusterUIDAnnotation, uidB))
		g.Expect(f.endpointCalls).To(Equal(1), "the new pin is checked at once")
		g.Expect(f.events()).To(ConsistOf(ContainSubstring("Normal WorkloadClusterIdentityFreezeCleared")))
	})

	t.Run("a new pin that still does not match freezes again", func(t *testing.T) {
		g := NewWithT(t)
		f := newIdentityFixture(g, frozenAgainstA)
		f.endpoint = workloadWithUID(f.scheme, uidC)
		f.operatorAnnotates(map[string]string{WorkloadClusterUIDAnnotation: uidB})

		g.Expect(f.reconcile().frozen).To(BeTrue())
		g.Expect(f.stored().Annotations[WorkloadClusterIdentityMismatchAnnotation]).To(HaveSuffix("; pinned " + uidB))
	})

	t.Run("removing the pin clears the freeze and pins again", func(t *testing.T) {
		g := NewWithT(t)
		f := newIdentityFixture(g, frozenAgainstA)
		f.endpoint = workloadWithUID(f.scheme, uidB)
		f.members.clients = map[string]client.Client{"10.0.0.10": workloadWithUID(f.scheme, uidB), "10.0.0.11": workloadWithUID(f.scheme, uidB)}
		f.operatorAnnotates(nil, WorkloadClusterUIDAnnotation)

		g.Expect(f.reconcile()).To(Equal(identityGate{}))
		stored := f.stored()
		g.Expect(stored.Annotations).ToNot(HaveKey(WorkloadClusterIdentityMismatchAnnotation))
		g.Expect(stored.Annotations).To(HaveKeyWithValue(WorkloadClusterUIDAnnotation, uidB))
	})

	t.Run("removing only the mismatch annotation freezes again on the next check", func(t *testing.T) {
		g := NewWithT(t)
		f := newIdentityFixture(g, frozenAgainstA)
		f.endpoint = workloadWithUID(f.scheme, uidB)
		f.operatorAnnotates(nil, WorkloadClusterIdentityMismatchAnnotation)

		g.Expect(f.reconcile().frozen).To(BeTrue())
		g.Expect(f.stored().Annotations).To(HaveKey(WorkloadClusterIdentityMismatchAnnotation))
	})

	t.Run("removing only the mismatch annotation unfreezes when the cluster matches the pin", func(t *testing.T) {
		g := NewWithT(t)
		f := newIdentityFixture(g, frozenAgainstA)
		f.operatorAnnotates(nil, WorkloadClusterIdentityMismatchAnnotation)

		g.Expect(f.reconcile()).To(Equal(identityGate{}))
		g.Expect(f.stored().Annotations).ToNot(HaveKey(WorkloadClusterIdentityMismatchAnnotation))
	})

	t.Run("a mismatch annotation edited by hand is kept", func(t *testing.T) {
		g := NewWithT(t)
		f := newIdentityFixture(g, map[string]string{
			WorkloadClusterUIDAnnotation:              uidB,
			WorkloadClusterIdentityMismatchAnnotation: "edited by hand",
		})

		g.Expect(f.reconcile().frozen).To(BeTrue())
		g.Expect(f.stored().Annotations).To(HaveKeyWithValue(WorkloadClusterIdentityMismatchAnnotation, "edited by hand"))
	})
}

func TestVerifyWorkloadIdentity(t *testing.T) {
	ctx := context.Background()

	t.Run("matching identity", func(t *testing.T) {
		g := NewWithT(t)
		f := newIdentityFixture(g, map[string]string{WorkloadClusterUIDAnnotation: uidA})
		g.Expect(f.r.verifyWorkloadIdentity(ctx, log.Log, f.kcp, workloadWithUID(f.scheme, uidA), "10.0.0.10")).To(Succeed())
		g.Expect(f.stored().Annotations).ToNot(HaveKey(WorkloadClusterIdentityMismatchAnnotation))
	})

	t.Run("a mismatch freezes", func(t *testing.T) {
		g := NewWithT(t)
		f := newIdentityFixture(g, map[string]string{WorkloadClusterUIDAnnotation: uidA})
		err := f.r.verifyWorkloadIdentity(ctx, log.Log, f.kcp, workloadWithUID(f.scheme, uidB), "10.0.0.10")
		g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
		g.Expect(f.stored().Annotations[WorkloadClusterIdentityMismatchAnnotation]).To(HavePrefix(uidB + " via 10.0.0.10 at "))
		_, frozen := workloadIdentityFrozen(f.kcp)
		g.Expect(frozen).To(BeTrue(), "the reconcile's copy is frozen at once")
	})

	t.Run("a failed read is not a mismatch", func(t *testing.T) {
		g := NewWithT(t)
		f := newIdentityFixture(g, map[string]string{WorkloadClusterUIDAnnotation: uidA})
		err := f.r.verifyWorkloadIdentity(ctx, log.Log, f.kcp, fake.NewClientBuilder().WithScheme(f.scheme).Build(), leaveViaEndpoint)
		g.Expect(err).To(HaveOccurred())
		g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeFalse())
		g.Expect(f.stored().Annotations).ToNot(HaveKey(WorkloadClusterIdentityMismatchAnnotation))
	})

	t.Run("no pin: nothing is read", func(t *testing.T) {
		g := NewWithT(t)
		f := newIdentityFixture(g, nil)
		g.Expect(f.r.verifyWorkloadIdentity(ctx, log.Log, f.kcp, hungClient(f.scheme), leaveViaEndpoint)).To(Succeed())
		g.Expect(f.r.verifyIdentityViaEndpoint(ctx, log.Log, f.kcp, f.cluster)).To(Succeed())
		g.Expect(f.endpointCalls).To(BeZero())
	})

	t.Run("already frozen: refused without a read", func(t *testing.T) {
		g := NewWithT(t)
		f := newIdentityFixture(g, map[string]string{
			WorkloadClusterUIDAnnotation:              uidA,
			WorkloadClusterIdentityMismatchAnnotation: uidB + " via endpoint at 2026-10-18T00:00:00Z; pinned " + uidA,
		})
		err := f.r.verifyWorkloadIdentity(ctx, log.Log, f.kcp, workloadWithUID(f.scheme, uidA), "10.0.0.10")
		g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
		err = f.r.verifyIdentityViaEndpoint(ctx, log.Log, f.kcp, f.cluster)
		g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
		g.Expect(f.endpointCalls).To(BeZero())
	})
}

// TestTripIdentityFreeze_NeverCopiesAnUnexpectedValue: an observed UID that
// is not UID-shaped is recorded as <malformed>, so a workload apiserver
// cannot write arbitrary text into the annotation or the event, nor forge the
// recorded pin.
func TestTripIdentityFreeze_NeverCopiesAnUnexpectedValue(t *testing.T) {
	g := NewWithT(t)
	f := newIdentityFixture(g, map[string]string{WorkloadClusterUIDAnnotation: uidA})
	evil := "x; pinned " + uidB

	err := f.r.verifyWorkloadIdentity(context.Background(), log.Log, f.kcp, workloadWithUID(f.scheme, evil), "10.0.0.10")

	g.Expect(errors.Is(err, errWorkloadIdentityChanged)).To(BeTrue())
	v := f.stored().Annotations[WorkloadClusterIdentityMismatchAnnotation]
	g.Expect(v).To(HavePrefix(identityMalformed + " via 10.0.0.10 at "))
	g.Expect(v).To(HaveSuffix("; pinned " + uidA))
	g.Expect(strings.Join(f.events(), "\n")).ToNot(ContainSubstring(evil))
}

// TestTripIdentityFreeze_KeepsTheFirstObservation: a second mismatch does
// not overwrite the first one.
func TestTripIdentityFreeze_KeepsTheFirstObservation(t *testing.T) {
	g := NewWithT(t)
	f := newIdentityFixture(g, map[string]string{WorkloadClusterUIDAnnotation: uidA})
	ctx := context.Background()

	g.Expect(errors.Is(f.r.verifyWorkloadIdentity(ctx, log.Log, f.kcp, workloadWithUID(f.scheme, uidB), "10.0.0.10"), errWorkloadIdentityChanged)).To(BeTrue())
	first := f.stored().Annotations[WorkloadClusterIdentityMismatchAnnotation]
	g.Expect(errors.Is(f.r.tripIdentityFreeze(ctx, log.Log, f.kcp, uidC, "10.0.0.11", uidA, ""), errWorkloadIdentityChanged)).To(BeTrue())

	g.Expect(f.stored().Annotations[WorkloadClusterIdentityMismatchAnnotation]).To(Equal(first))
	g.Expect(f.events()).To(HaveLen(1))
}

// TestPatchKCPAnnotations_KeepsTheComputedStatus: the annotation patch must
// not replace status the reconcile has computed but not written yet.
func TestPatchKCPAnnotations_KeepsTheComputedStatus(t *testing.T) {
	g := NewWithT(t)
	f := newIdentityFixture(g, nil)
	rv := f.kcp.ResourceVersion
	conditions.MarkFalse(f.kcp, controlplanev1beta2.MachinesUpToDateCondition, controlplanev1beta2.RollingOutReason, clusterv1.ConditionSeverityInfo, "in memory only")

	g.Expect(f.r.patchKCPAnnotations(context.Background(), f.kcp, map[string]string{WorkloadClusterUIDAnnotation: uidA})).To(Succeed())

	g.Expect(conditions.GetReason(f.kcp, controlplanev1beta2.MachinesUpToDateCondition)).To(Equal(controlplanev1beta2.RollingOutReason))
	g.Expect(f.kcp.ResourceVersion).ToNot(Equal(rv))
	g.Expect(f.kcp.Annotations).To(HaveKeyWithValue(WorkloadClusterUIDAnnotation, uidA))
}

func TestMarkWorkloadIdentityConditions(t *testing.T) {
	g := NewWithT(t)
	kcp := vipKCP()
	conditions.MarkTrue(kcp, clusterv1.ReadyCondition)
	conditions.MarkTrue(kcp, controlplanev1beta2.EtcdHealthyCondition)
	conditions.MarkTrue(kcp, controlplanev1beta2.AvailableCondition)

	markWorkloadIdentityConditions(kcp)
	g.Expect(conditions.IsTrue(kcp, controlplanev1beta2.EtcdHealthyCondition)).To(BeTrue(), "no freeze: untouched")
	g.Expect(conditions.IsTrue(kcp, clusterv1.ReadyCondition)).To(BeTrue(), "no freeze: untouched")

	kcp.Annotations = map[string]string{WorkloadClusterIdentityMismatchAnnotation: uidB + " via endpoint at 2026-10-18T00:00:00Z; pinned " + uidA}
	markWorkloadIdentityConditions(kcp)
	for _, ct := range []clusterv1.ConditionType{clusterv1.ReadyCondition, controlplanev1beta2.EtcdHealthyCondition, controlplanev1beta2.AvailableCondition} {
		c := conditions.Get(kcp, ct)
		g.Expect(c).ToNot(BeNil())
		g.Expect(c.Status).To(Equal(corev1.ConditionFalse))
		g.Expect(c.Reason).To(Equal(controlplanev1beta2.WorkloadClusterIdentityChangedReason))
		g.Expect(c.Severity).To(Equal(clusterv1.ConditionSeverityError))
		g.Expect(c.Message).To(ContainSubstring(uidB))
		g.Expect(c.Message).To(ContainSubstring(WorkloadClusterUIDAnnotation))
	}
}

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
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// guardFor is the guard reconcileDistribution returns for a control plane
// whose Machines agree with spec: not held, building new Machines with
// distributionOf(kcp). Tests that drive reconcileMachines or the create and
// delete paths directly, and are not about the distribution, pass it.
func guardFor(kcp *controlplanev1beta2.KairosControlPlane) distributionGuard {
	return guardWith(distributionOf(kcp))
}

// guardWith is an open guard that builds Machines with distribution.
func guardWith(distribution string) distributionGuard {
	return distributionGuard{effective: distribution, spec: distribution}
}

// heldGuard is a guard holding membership changes with reason, as a
// control plane relabelled from k0s to k3s gets (machinesExist reports
// whether any control-plane Machine exists).
func heldGuard(reason string, machinesExist bool) distributionGuard {
	g := distributionGuard{effective: "k3s", spec: "k3s", reason: reason, message: "held for the test"}
	if machinesExist {
		g.observed = summarizeObserved(map[string][]string{"k0s": {"cp-a"}}, nil)
	}
	return g
}

// guardKCP is a KairosControlPlane in "default" named test-kcp, referencing
// KairosConfigTemplate cfg-template, with the given spec.distribution.
func guardKCP(dist string, replicas int32) *controlplanev1beta2.KairosControlPlane {
	kcp := machineMetaKCP("k3s", nil)
	kcp.Spec.Distribution = dist
	kcp.Spec.Replicas = &replicas
	kcp.Spec.KairosConfigTemplate = controlplanev1beta2.KairosConfigTemplateReference{Name: "cfg-template"}
	return kcp
}

// guardMachine is a control-plane Machine owned by kcp whose KairosConfig
// records dist. dist "" returns no KairosConfig: the Machine names one that
// does not exist.
func guardMachine(kcp *controlplanev1beta2.KairosControlPlane, name, dist string, age time.Duration) (*clusterv1.Machine, *bootstrapv1beta2.KairosConfig) {
	m := withKairosConfigRef(rolloutMachine(kcp, name, kcp.Spec.Version, age, false), name)
	if dist == "" {
		return m, nil
	}
	return m, kairosConfigFor(m, dist)
}

// guardObjects returns the Machines and KairosConfigs for dists, one Machine
// per entry, oldest first, as client objects.
func guardObjects(kcp *controlplanev1beta2.KairosControlPlane, dists ...string) []client.Object {
	var out []client.Object
	for i, d := range dists {
		m, kc := guardMachine(kcp, fmt.Sprintf("cp-%d", i), d, time.Duration(len(dists)-i)*time.Hour)
		out = append(out, m)
		if kc != nil {
			out = append(out, kc)
		}
	}
	return out
}

func TestSummarizeObserved(t *testing.T) {
	for _, tc := range []struct {
		name    string
		by      map[string][]string
		unread  []string
		want    observedKind
		wantVal string
		wantStr string
	}{
		{name: "no Machines", want: observedNone, wantStr: "none"},
		{name: "all agree", by: map[string][]string{"k0s": {"b", "a"}}, want: observedAgreed, wantVal: "k0s", wantStr: "k0s"},
		{name: "unreadable Machines get no vote", by: map[string][]string{"k3s": {"a"}}, unread: []string{"b"}, want: observedAgreed, wantVal: "k3s", wantStr: "k3s"},
		{name: "readable configs disagree", by: map[string][]string{"k0s": {"a"}, "k3s": {"b"}}, want: observedMixed, wantStr: "mixed"},
		{name: "none readable", unread: []string{"a", "b"}, want: observedUnknown, wantStr: "unknown"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			o := summarizeObserved(tc.by, tc.unread)
			g.Expect(o.kind).To(Equal(tc.want))
			g.Expect(o.value).To(Equal(tc.wantVal))
			g.Expect(o.String()).To(Equal(tc.wantStr))
			g.Expect(o.machinesExist()).To(Equal(tc.want != observedNone))
		})
	}
}

// TestDescribeMachines: Machines per distribution, sorted, at most five names
// each, and the unreadable ones listed apart.
func TestDescribeMachines(t *testing.T) {
	g := NewWithT(t)
	o := summarizeObserved(map[string][]string{
		"k3s": {"m7", "m6", "m5", "m4", "m3", "m2", "m1"},
		"k0s": {"a"},
	}, []string{"x"})
	g.Expect(o.describeMachines()).To(Equal("k0s (a), k3s (m1, m2, m3, m4, m5, and 2 more), no readable KairosConfig (x)"))
	g.Expect(summarizeObserved(nil, nil).describeMachines()).To(Equal("none"))
}

func TestObserveDistribution(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := guardKCP("k0s", 3)
	objs := guardObjects(kcp, "k0s", "k0s", "")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	machines, err := r.getControlPlaneMachines(context.Background(), kcp, machineMetaCluster())
	g.Expect(err).NotTo(HaveOccurred())
	o, err := r.observeDistribution(context.Background(), machines)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(o.kind).To(Equal(observedAgreed))
	g.Expect(o.value).To(Equal("k0s"))
	g.Expect(o.byDistribution["k0s"]).To(Equal([]string{"cp-0", "cp-1"}))
	g.Expect(o.unread).To(Equal([]string{"cp-2"}))

	// A read error is returned, never guessed around.
	failing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(failKairosConfigGets()).Build()
	r = &KairosControlPlaneReconciler{Client: failing, Scheme: scheme}
	_, err = r.observeDistribution(context.Background(), machines)
	g.Expect(err).To(HaveOccurred())
}

// TestDistributionToWrite is ADR 0016 D2.
func TestDistributionToWrite(t *testing.T) {
	agreed := func(d string) observedDistribution { return summarizeObserved(map[string][]string{d: {"a"}}, nil) }
	for _, tc := range []struct {
		name       string
		template   string
		obs        observedDistribution
		wantValue  string
		wantReason string
	}{
		{"no Machines: the template's value", "k3s", observedDistribution{}, "k3s", distributionInheritedEventReason},
		{"no Machines, no template value: nothing", "", observedDistribution{}, "", ""},
		{"Machines agree: their value, not the template's", "k3s", agreed("k0s"), "k0s", distributionRestoredEventReason},
		{"Machines agree, no template: their value", "", agreed("k3s"), "k3s", distributionRestoredEventReason},
		{"mixed: nothing", "k0s", summarizeObserved(map[string][]string{"k0s": {"a"}, "k3s": {"b"}}, nil), "", ""},
		{"unknown: nothing", "k0s", summarizeObserved(nil, []string{"a"}), "", ""},
		{"kubeadm template, no Machines: nothing", "kubeadm", observedDistribution{}, "", ""},
		{"kubeadm template, Machines agree: nothing", "kubeadm", agreed("k0s"), "", ""},
		{"Machines agree on kubeadm: nothing", "k0s", agreed("kubeadm"), "", ""},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			v, reason := distributionToWrite(tc.template, tc.obs)
			g.Expect(v).To(Equal(tc.wantValue))
			g.Expect(reason).To(Equal(tc.wantReason))
		})
	}
}

// TestDistributionVerdict is ADR 0016 D3: kubeadm first, then Machines that
// are mixed, unknown, or differ from spec.
func TestDistributionVerdict(t *testing.T) {
	agreed := func(d string) observedDistribution { return summarizeObserved(map[string][]string{d: {"a"}}, nil) }
	for _, tc := range []struct {
		name       string
		spec       string
		template   string
		obs        observedDistribution
		wantReason string
		wantMsg    string
	}{
		{name: "no Machines, explicit spec: open", spec: "k3s", template: "k0s"},
		{name: "no Machines, empty spec: open", spec: "", template: ""},
		{name: "spec matches the Machines: open", spec: "k0s", template: "k3s", obs: agreed("k0s")},
		{name: "spec kubeadm", spec: "kubeadm", obs: agreed("k0s"),
			wantReason: controlplanev1beta2.UnsupportedDistributionReason, wantMsg: "spec.distribution is kubeadm"},
		{name: "kubeadm template with explicit k0s and no Machines", spec: "k0s", template: "kubeadm",
			wantReason: controlplanev1beta2.UnsupportedDistributionReason, wantMsg: `KairosConfigTemplate "cfg-template" has distribution kubeadm`},
		{name: "kubeadm template wins over a mismatch", spec: "k3s", template: "kubeadm", obs: agreed("k0s"),
			wantReason: controlplanev1beta2.UnsupportedDistributionReason, wantMsg: "Control-plane Machines are not created or deleted"},
		{name: "relabelled", spec: "k3s", template: "k3s", obs: agreed("k0s"),
			wantReason: controlplanev1beta2.DistributionMismatchReason, wantMsg: "spec.distribution is k3s, but the control-plane Machines' KairosConfigs record k0s"},
		{name: "relabelled: tells the operator to confirm what the nodes run", spec: "k3s", template: "k3s", obs: agreed("k0s"),
			wantReason: controlplanev1beta2.DistributionMismatchReason, wantMsg: "confirm what the nodes actually run before you set spec.distribution"},
		{name: "empty spec the controller could not write", spec: "", obs: agreed("kubeadm"),
			wantReason: controlplanev1beta2.DistributionMismatchReason, wantMsg: "spec.distribution is <unset>"},
		{name: "mixed", spec: "k0s", obs: summarizeObserved(map[string][]string{"k0s": {"a"}, "k3s": {"b"}}, nil),
			wantReason: controlplanev1beta2.DistributionMismatchReason, wantMsg: "KairosConfigs disagree: k0s (a), k3s (b)"},
		{name: "a KairosConfig contradicting its hook", spec: "k3s",
			obs:        summarizeObserved(map[string][]string{"k3s": {"a", "b"}}, nil).withContradictions([]string{"b"}),
			wantReason: controlplanev1beta2.DistributionMismatchReason, wantMsg: "k3s (a, b), KairosConfig contradicts the k0s etcd-leave hook (b)"},
		{name: "unknown", spec: "k0s", obs: summarizeObserved(nil, []string{"a"}),
			wantReason: controlplanev1beta2.DistributionMismatchReason, wantMsg: "No control-plane Machine has a readable KairosConfig (a)"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			reason, msg := distributionVerdict(tc.spec, "cfg-template", tc.template, tc.obs)
			g.Expect(reason).To(Equal(tc.wantReason))
			if tc.wantReason == "" {
				g.Expect(msg).To(BeEmpty())
				return
			}
			g.Expect(msg).To(ContainSubstring(tc.wantMsg))
		})
	}
}

// TestReconcileDistribution drives reconcileDistribution against a fake
// management cluster: what it writes into spec.distribution, which event it
// emits for the write, and the verdict.
func TestReconcileDistribution(t *testing.T) {
	for _, tc := range []struct {
		name         string
		spec         string
		template     string // "" = template object without a distribution; "-" = no template object
		machines     []string
		wantSpec     string
		wantWrote    bool
		wantEvent    string
		wantReason   string
		wantEffectiv string
	}{
		{name: "empty spec, no Machines: inherits the template", template: "k3s",
			wantSpec: "k3s", wantWrote: true, wantEvent: distributionInheritedEventReason, wantEffectiv: "k3s"},
		{name: "empty spec, no Machines, template missing: nothing written, k0s fallback", template: "-",
			wantSpec: "", wantEffectiv: "k0s"},
		{name: "empty spec, Machines run k0s, template says k3s: restores k0s", template: "k3s", machines: []string{"k0s", "k0s", "k0s"},
			wantSpec: "k0s", wantWrote: true, wantEvent: distributionRestoredEventReason, wantEffectiv: "k0s"},
		{name: "explicit spec relabelled: held, nothing written", spec: "k3s", template: "k3s", machines: []string{"k0s", "k0s", "k0s"},
			wantSpec: "k3s", wantReason: controlplanev1beta2.DistributionMismatchReason, wantEffectiv: "k3s"},
		{name: "empty spec, mixed: held, nothing written", template: "k0s", machines: []string{"k0s", "k0s", "k3s"},
			wantSpec: "", wantReason: controlplanev1beta2.DistributionMismatchReason, wantEffectiv: "k0s"},
		{name: "empty spec, unknown: held, nothing written", template: "k0s", machines: []string{"", ""},
			wantSpec: "", wantReason: controlplanev1beta2.DistributionMismatchReason, wantEffectiv: "k0s"},
		{name: "empty spec, kubeadm template, no Machines: held, nothing written", template: "kubeadm",
			wantSpec: "", wantReason: controlplanev1beta2.UnsupportedDistributionReason, wantEffectiv: "k0s"},
		{name: "explicit k0s, kubeadm template, no Machines: held", spec: "k0s", template: "kubeadm",
			wantSpec: "k0s", wantReason: controlplanev1beta2.UnsupportedDistributionReason, wantEffectiv: "k0s"},
		{name: "explicit k0s, kubeadm template, Machines: held", spec: "k0s", template: "kubeadm", machines: []string{"k0s"},
			wantSpec: "k0s", wantReason: controlplanev1beta2.UnsupportedDistributionReason, wantEffectiv: "k0s"},
		{name: "explicit spec overrides a different template: override event, open", spec: "k3s", template: "k0s", machines: []string{"k3s"},
			wantSpec: "k3s", wantEvent: distributionOverrideEventReason, wantEffectiv: "k3s"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := machineMetaScheme(g)
			kcp := guardKCP(tc.spec, 3)
			objs := append([]client.Object{kcp}, guardObjects(kcp, tc.machines...)...)
			if tc.template != "-" {
				objs = append(objs, newDistTemplate("cfg-template", "default", tc.template))
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			rec := record.NewFakeRecorder(10)
			r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme, Recorder: rec}

			live := &controlplanev1beta2.KairosControlPlane{}
			g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(kcp), live)).To(Succeed())
			guard, err := r.reconcileDistribution(context.Background(), log.Log, live, machineMetaCluster())
			g.Expect(err).NotTo(HaveOccurred())

			g.Expect(guard.wroteSpec).To(Equal(tc.wantWrote))
			g.Expect(guard.reason).To(Equal(tc.wantReason))
			g.Expect(guard.effective).To(Equal(tc.wantEffectiv))
			g.Expect(live.Spec.Distribution).To(Equal(tc.wantSpec), "in-memory spec")
			stored := &controlplanev1beta2.KairosControlPlane{}
			g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(kcp), stored)).To(Succeed())
			g.Expect(stored.Spec.Distribution).To(Equal(tc.wantSpec), "stored spec")

			if tc.wantEvent != "" {
				g.Expect(rec.Events).To(Receive(ContainSubstring(tc.wantEvent)))
			}
			g.Expect(rec.Events).NotTo(Receive(), "no other event")
		})
	}
}

// TestReconcileDistribution_OptimisticLock: the spec write is pinned to the
// resourceVersion the reconcile read. An operator edit that landed in between
// is not overwritten; the write fails with a Conflict and the in-memory spec
// is left as it was.
func TestReconcileDistribution_OptimisticLock(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := guardKCP("", 3)
	objs := append([]client.Object{kcp, newDistTemplate("cfg-template", "default", "k3s")}, guardObjects(kcp, "k0s", "k0s", "k0s")...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	stale := &controlplanev1beta2.KairosControlPlane{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(kcp), stale)).To(Succeed())

	// The operator sets k3s after the reconcile read the object.
	edited := stale.DeepCopy()
	edited.Spec.Distribution = "k3s"
	g.Expect(c.Update(context.Background(), edited)).To(Succeed())

	_, err := r.reconcileDistribution(context.Background(), log.Log, stale, machineMetaCluster())
	g.Expect(apierrors.IsConflict(err)).To(BeTrue(), "got %v", err)
	g.Expect(stale.Spec.Distribution).To(BeEmpty())

	stored := &controlplanev1beta2.KairosControlPlane{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(kcp), stored)).To(Succeed())
	g.Expect(stored.Spec.Distribution).To(Equal("k3s"), "the operator's edit wins")
}

// TestMarkDistributionConditions is ADR 0016 D5.
func TestMarkDistributionConditions(t *testing.T) {
	t.Run("held with Machines: Ready and MachinesUpToDate, never Available", func(t *testing.T) {
		g := NewWithT(t)
		kcp := guardKCP("k3s", 3)
		conditions.MarkTrue(kcp, controlplanev1beta2.AvailableCondition)
		markDistributionConditions(kcp, heldGuard(controlplanev1beta2.DistributionMismatchReason, true))

		for _, ct := range []clusterv1.ConditionType{clusterv1.ReadyCondition, controlplanev1beta2.MachinesUpToDateCondition} {
			c := conditions.Get(kcp, ct)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(corev1.ConditionFalse))
			g.Expect(c.Reason).To(Equal(controlplanev1beta2.DistributionMismatchReason))
			g.Expect(c.Severity).To(Equal(clusterv1.ConditionSeverityWarning))
		}
		g.Expect(conditions.IsTrue(kcp, controlplanev1beta2.AvailableCondition)).To(BeTrue(), "a serving control plane is not reported unavailable")
		g.Expect(kcp.Status.FailureReason).To(BeEmpty())
		g.Expect(kcp.Status.FailureMessage).To(BeEmpty())
	})

	t.Run("held without Machines: Ready, Available and the failure fields", func(t *testing.T) {
		g := NewWithT(t)
		kcp := guardKCP("k0s", 1)
		markDistributionConditions(kcp, heldGuard(controlplanev1beta2.UnsupportedDistributionReason, false))

		for _, ct := range []clusterv1.ConditionType{clusterv1.ReadyCondition, controlplanev1beta2.AvailableCondition} {
			c := conditions.Get(kcp, ct)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal(controlplanev1beta2.UnsupportedDistributionReason))
			g.Expect(c.Severity).To(Equal(clusterv1.ConditionSeverityWarning))
		}
		g.Expect(conditions.Has(kcp, controlplanev1beta2.MachinesUpToDateCondition)).To(BeFalse())
		g.Expect(kcp.Status.FailureReason).To(Equal(controlplanev1beta2.UnsupportedDistributionReason))
		g.Expect(kcp.Status.FailureMessage).To(Equal("held for the test"))
	})

	t.Run("not held: drops a MachinesUpToDate the hold left, keeps any other", func(t *testing.T) {
		g := NewWithT(t)
		kcp := guardKCP("k0s", 3)
		conditions.MarkFalse(kcp, controlplanev1beta2.MachinesUpToDateCondition, controlplanev1beta2.DistributionMismatchReason, clusterv1.ConditionSeverityWarning, "old")
		markDistributionConditions(kcp, guardFor(kcp))
		g.Expect(conditions.Has(kcp, controlplanev1beta2.MachinesUpToDateCondition)).To(BeFalse())

		conditions.MarkFalse(kcp, controlplanev1beta2.MachinesUpToDateCondition, controlplanev1beta2.RollingOutReason, clusterv1.ConditionSeverityInfo, "rolling")
		markDistributionConditions(kcp, guardFor(kcp))
		g.Expect(conditions.GetReason(kcp, controlplanev1beta2.MachinesUpToDateCondition)).To(Equal(controlplanev1beta2.RollingOutReason))
	})
}

// TestRecordDistributionEvents: a Warning on entering a hold, naming spec,
// the template and the Machines per distribution; a Normal on leaving it;
// each once, against Ready's previous reason.
func TestRecordDistributionEvents(t *testing.T) {
	held := heldGuard(controlplanev1beta2.DistributionMismatchReason, true)
	held.templateName, held.template = "cfg-template", "k3s"
	for _, tc := range []struct {
		name     string
		guard    distributionGuard
		previous string
		want     string // "" = no event
	}{
		{"entering a hold", held, controlplanev1beta2.WaitingForMachinesReason,
			`Warning DistributionMismatch Holding control-plane Machine creation and deletion: spec.distribution=k3s, KairosConfigTemplate "cfg-template" distribution=k3s, control-plane Machines: k0s (cp-a)`},
		{"still held", held, controlplanev1beta2.DistributionMismatchReason, ""},
		{"moving to another verdict", held, controlplanev1beta2.UnsupportedDistributionReason, "Warning DistributionMismatch"},
		{"leaving a mismatch", guardWith("k0s"), controlplanev1beta2.DistributionMismatchReason, "Normal DistributionMismatchResolved"},
		{"leaving a kubeadm refusal", guardWith("k0s"), controlplanev1beta2.UnsupportedDistributionReason, "Normal DistributionMismatchResolved"},
		{"never held", guardWith("k0s"), controlplanev1beta2.WaitingForMachinesReason, ""},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			rec := record.NewFakeRecorder(4)
			r := &KairosControlPlaneReconciler{Recorder: rec}
			tc.guard.previousReadyReason = tc.previous
			r.recordDistributionEvents(log.Log, guardKCP("k3s", 3), tc.guard)
			if tc.want == "" {
				g.Expect(rec.Events).NotTo(Receive())
				return
			}
			g.Expect(rec.Events).To(Receive(HavePrefix(tc.want)))
			g.Expect(rec.Events).NotTo(Receive())
		})
	}
}

// TestReconcileMachines_DistributionHold: a held guard stops every membership
// change. A scale-up creates nothing and no join-token Secret, a scale-down
// deletes nothing, and the requeue is the 60s hold interval. The etcd-leave
// sweep, which runs before the hold, still releases a Machine whose own
// KairosConfig says it is not k0s.
func TestReconcileMachines_DistributionHold(t *testing.T) {
	t.Run("scale-up", func(t *testing.T) {
		g := NewWithT(t)
		scheme := machineMetaScheme(g)
		kcp := guardKCP("k3s", 3)
		objs := append([]client.Object{machineMetaInfraTemplate(), newDistTemplate("cfg-template", "default", "k3s")}, guardObjects(kcp, "k0s")...)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

		res, err := r.reconcileMachines(context.Background(), log.Log, kcp, rolloutCluster(), heldGuard(controlplanev1beta2.DistributionMismatchReason, true))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(distributionHoldRequeueAfter))
		names, _ := remainingMachines(g, c)
		g.Expect(names).To(ConsistOf("cp-0"), "no Machine may be created while held")
		secret := &corev1.Secret{}
		err = c.Get(context.Background(), types.NamespacedName{Name: joinTokenSecretName("test-cluster"), Namespace: "default"}, secret)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the join-token Secret is not ensured while held")
	})

	t.Run("scale-down", func(t *testing.T) {
		g := NewWithT(t)
		scheme := machineMetaScheme(g)
		kcp := guardKCP("k3s", 1)
		objs := append([]client.Object{machineMetaInfraTemplate()}, guardObjects(kcp, "k0s", "k0s", "k0s")...)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

		res, err := r.reconcileMachines(context.Background(), log.Log, kcp, rolloutCluster(), heldGuard(controlplanev1beta2.DistributionMismatchReason, true))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(distributionHoldRequeueAfter))
		names, deleting := remainingMachines(g, c)
		g.Expect(names).To(HaveLen(3))
		g.Expect(deleting).To(BeZero(), "no Machine may be deleted while held")

		// The same control plane, not held, scales down: the hold is what
		// stopped it.
		_, err = r.reconcileMachines(context.Background(), log.Log, kcp, rolloutCluster(), guardFor(kcp))
		g.Expect(err).NotTo(HaveOccurred())
		names, _ = remainingMachines(g, c)
		g.Expect(names).To(HaveLen(2))
	})

	t.Run("the leave sweep still runs", func(t *testing.T) {
		// A k0s member an operator deleted, held at CAPI's pre-terminate
		// hook, with a pinned workload cluster that answers. The hold must
		// not stop its leave: the request is written before the hold returns.
		g := NewWithT(t)
		scheme := machineMetaScheme(g)
		kcp := pinned(guardKCP("k3s", 3))
		m, kc := guardMachine(kcp, "cp-0", "k0s", time.Hour)
		m.Annotations = map[string]string{etcdLeaveHookAnnotation(): ""}
		m.Finalizers = []string{"test.kairos.io/hold"}
		m.Status.NodeRef = clusterv1.MachineNodeReference{Name: "cp-0"}
		atPreTerminateHook(m)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(m, kc).WithStatusSubresource(&clusterv1.Machine{}).Build()
		g.Expect(c.Delete(context.Background(), m)).To(Succeed())
		wc := pinnedWorkloadBuilder(scheme).Build()
		r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme, WorkloadClientFactory: staticWorkloadClient(wc)}

		_, err := r.reconcileMachines(context.Background(), log.Log, kcp, rolloutCluster(), heldGuard(controlplanev1beta2.DistributionMismatchReason, true))
		g.Expect(err).NotTo(HaveOccurred())
		cm := &corev1.ConfigMap{}
		g.Expect(wc.Get(context.Background(), types.NamespacedName{Name: etcdLeaveConfigMapName, Namespace: etcdLeaveConfigMapNamespace}, cm)).To(Succeed())
		g.Expect(cm.Data).To(HaveKeyWithValue("cp-0", etcdLeaveRequestedValue), "the etcd leave progresses while membership changes are held")
		got := &clusterv1.Machine{}
		g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "cp-0", Namespace: "default"}, got)).To(Succeed())
		g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
	})
}

// TestCreateControlPlaneMachine_DistributionGuard: the create refuses before
// it makes anything while held, when the guard saw a kubeadm template, when
// no distribution was decided, and when the template it reads for the merge
// turned kubeadm since. An open guard builds with its effective value, not
// with spec.distribution.
func TestCreateControlPlaneMachine_DistributionGuard(t *testing.T) {
	kubeadmGuard := guardWith("k0s")
	kubeadmGuard.template, kubeadmGuard.templateName = kcpUnsupportedDistributionKubeadm, "cfg-template"
	for _, tc := range []struct {
		name         string
		guard        distributionGuard
		templateDist string
		wantHeld     bool
		wantErr      bool
	}{
		{name: "held", guard: heldGuard(controlplanev1beta2.DistributionMismatchReason, true), templateDist: "k3s", wantHeld: true, wantErr: true},
		{name: "kubeadm template seen by the guard", guard: kubeadmGuard, templateDist: "k0s", wantHeld: true, wantErr: true},
		{name: "kubeadm template at create time", guard: guardWith("k0s"), templateDist: "kubeadm", wantHeld: true, wantErr: true},
		{name: "no distribution decided", guard: distributionGuard{}, templateDist: "k0s", wantErr: true},
		{name: "open guard builds with its effective value", guard: guardWith("k3s"), templateDist: "k0s"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := machineMetaScheme(g)
			kcp := guardKCP("k0s", 1)
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(machineMetaInfraTemplate(), newDistTemplate("cfg-template", "default", tc.templateDist)).Build()
			r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

			created, err := r.createControlPlaneMachine(context.Background(), log.Log, kcp, machineMetaCluster(), tc.guard,
				bootstrapv1beta2.ControlPlaneRoleSingle, "")
			if !tc.wantErr {
				g.Expect(err).NotTo(HaveOccurred())
				kc := &bootstrapv1beta2.KairosConfig{}
				g.Expect(c.Get(context.Background(), types.NamespacedName{Name: created.Name, Namespace: "default"}, kc)).To(Succeed())
				g.Expect(kc.Spec.Distribution).To(Equal(tc.guard.effective))
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(errors.Is(err, errDistributionHeld)).To(Equal(tc.wantHeld), "got %v", err)
			g.Expect(created).To(BeNil())
			configs := &bootstrapv1beta2.KairosConfigList{}
			g.Expect(c.List(context.Background(), configs)).To(Succeed())
			g.Expect(configs.Items).To(BeEmpty(), "a refused create makes nothing")
			names, _ := remainingMachines(g, c)
			g.Expect(names).To(BeEmpty())
		})
	}
}

// TestDeleteControlPlaneMachine_RefusesWhileHeld: the delete keeps the hold
// even for a caller that did not check it.
func TestDeleteControlPlaneMachine_RefusesWhileHeld(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := guardKCP("k3s", 1)
	m, kc := guardMachine(kcp, "cp-0", "k0s", time.Hour)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(m, kc).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	err := r.deleteControlPlaneMachine(context.Background(), kcp, heldGuard(controlplanev1beta2.DistributionMismatchReason, true), m)
	g.Expect(errors.Is(err, errDistributionHeld)).To(BeTrue(), "got %v", err)
	names, deleting := remainingMachines(g, c)
	g.Expect(names).To(ConsistOf("cp-0"))
	g.Expect(deleting).To(BeZero())
}

// TestObserveDistribution_HookContradiction is ADR 0016 D1/D2 as ruled after
// review: a Machine that is not being deleted, carries the k0s etcd-leave
// hook, and whose KairosConfig records another distribution is conflicting
// evidence. The observed value is mixed even when every KairosConfig agrees,
// so the controller holds and writes nothing. A Machine being deleted votes
// by its KairosConfig only.
func TestObserveDistribution_HookContradiction(t *testing.T) {
	for _, tc := range []struct {
		name      string
		deleting  bool
		wantKind  observedKind
		wantValue string
	}{
		{name: "running Machine with a contradicting KairosConfig: mixed", wantKind: observedMixed},
		{name: "deleting Machine with a contradicting KairosConfig: votes by its KairosConfig", deleting: true,
			wantKind: observedAgreed, wantValue: "k3s"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := machineMetaScheme(g)
			kcp := guardKCP("", 3)
			objs := guardObjects(kcp, "k3s", "k3s")
			hooked, kc := guardMachine(kcp, "cp-hooked", "k3s", 30*time.Minute)
			hooked.Annotations = map[string]string{etcdLeaveHookAnnotation(): ""}
			hooked.Finalizers = []string{"test.kairos.io/hold"}
			objs = append(objs, hooked, kc, newDistTemplate("cfg-template", "default", "k3s"), kcp)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			if tc.deleting {
				g.Expect(c.Delete(context.Background(), hooked)).To(Succeed())
			}
			r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

			live := &controlplanev1beta2.KairosControlPlane{}
			g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(kcp), live)).To(Succeed())
			guard, err := r.reconcileDistribution(context.Background(), log.Log, live, machineMetaCluster())
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(guard.observed.kind).To(Equal(tc.wantKind))
			g.Expect(guard.observed.value).To(Equal(tc.wantValue))
			if tc.wantKind == observedMixed {
				g.Expect(guard.observed.contradicted).To(Equal([]string{"cp-hooked"}))
				g.Expect(guard.reason).To(Equal(controlplanev1beta2.DistributionMismatchReason))
				g.Expect(guard.message).To(ContainSubstring("KairosConfig contradicts the k0s etcd-leave hook (cp-hooked)"))
				g.Expect(guard.wroteSpec).To(BeFalse(), "conflicting evidence: no spec write")
				g.Expect(live.Spec.Distribution).To(BeEmpty())
				return
			}
			g.Expect(guard.wroteSpec).To(BeTrue())
			g.Expect(live.Spec.Distribution).To(Equal("k3s"))
		})
	}
}

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

package envtest

// ADR 0016 end to end: the control plane's distribution follows its
// Machines. The suite has no infrastructure provider, so every control plane
// here is built from fixtures: Machines owned by the KairosControlPlane, each
// with a KairosConfig recording the distribution its node runs, exactly as
// createControlPlaneMachine stamps it. None has a NodeRef, so the workload
// identity gate (ADR 0015) does not apply, and a scale-down needs nothing
// from a workload cluster: without the distribution hold it deletes a
// Machine.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/controllers/controlplane"
)

const (
	distTimeout     = 30 * time.Second
	distPolling     = 500 * time.Millisecond
	distQuiet       = 5 * time.Second
	distVersion     = "v1.30.0+k0s.0"
	distHoldFinal   = "test.kairos.io/hold"
	distTemplate    = "cp-config"
	distFieldOwner  = "capi-topology"
	distTouchMarker = "ha-test/nudge"
)

// distEventRecorder keeps every event with the namespace of the object it is
// about, so subtests sharing one manager each see only their own.
type distEventRecorder struct {
	mu     sync.Mutex
	events map[string][]string
}

func (r *distEventRecorder) Event(o runtime.Object, eventtype, reason, message string) {
	ns := ""
	if m, err := meta.Accessor(o); err == nil {
		ns = m.GetNamespace()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.events == nil {
		r.events = map[string][]string{}
	}
	r.events[ns] = append(r.events[ns], eventtype+" "+reason+" "+message)
}

func (r *distEventRecorder) Eventf(o runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	r.Event(o, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (r *distEventRecorder) AnnotatedEventf(o runtime.Object, _ map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	r.Eventf(o, eventtype, reason, messageFmt, args...)
}

// withPrefix returns the events in ns that start with prefix ("Warning
// DistributionMismatch", "Normal DistributionRestored", ...).
func (r *distEventRecorder) withPrefix(ns, prefix string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events[ns] {
		if strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// distEnv is one envtest, manager and KCP reconciler shared by the subtests
// of TestDistributionGuard, each in its own namespace.
type distEnv struct {
	ctx    context.Context
	c      client.Client
	cfg    *rest.Config
	events *distEventRecorder
}

// distFixture describes one control plane.
type distFixture struct {
	// spec is KairosControlPlane.spec.distribution ("" = unset).
	spec string
	// template is the referenced KairosConfigTemplate's distribution.
	template string
	replicas int32
	// machines lists, per control-plane Machine, the distribution its
	// KairosConfig records, oldest first; "" names a KairosConfig that does
	// not exist.
	machines []string
	// hooked stamps the etcd-leave hook, as k0s HA Machines carry it.
	hooked bool
	// holdFinalizer keeps a deleted Machine around, as CAPI's own finalizer
	// does while the hook is set.
	holdFinalizer bool
	// clusterPaused creates the Cluster with spec.paused.
	clusterPaused bool
	// applyKCP creates the KCP by server-side apply as field manager
	// capi-topology, the way CAPI topology does.
	applyKCP bool
}

// distCluster is a control plane built by build.
type distCluster struct {
	env      *distEnv
	ns       string
	cluster  string
	kcp      types.NamespacedName
	machines []string
}

// TestDistributionGuard covers the ADR 0016 E-cases that envtest can drive.
func TestDistributionGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	events := &distEventRecorder{}
	ctx, c, cfg, _, teardown := startKCPEnvtest(t, func(r *controlplane.KairosControlPlaneReconciler, _ *rest.Config) {
		r.Recorder = events
	})
	defer teardown()
	env := &distEnv{ctx: ctx, c: c, cfg: cfg, events: events}

	// E1: a k0s control plane relabelled k3s holds membership changes: a
	// scale-down deletes nothing, and a member an operator deletes keeps its
	// etcd-leave hook, because the leave follows the Machine's own KairosConfig.
	// Setting spec back clears the hold.
	t.Run("E1 relabel holds scale and keeps the hook", func(t *testing.T) {
		g := NewWithT(t)
		d := env.build(t, "dist-e1", distFixture{spec: "k0s", template: "k0s", replicas: 3,
			machines: []string{"k0s", "k0s", "k0s"}, hooked: true, holdFinalizer: true})
		d.waitSettled(g)

		d.updateKCP(g, func(kcp *controlplanev1beta2.KairosControlPlane) { kcp.Spec.Distribution = "k3s" })
		g.Eventually(d.conditionReason(clusterv1.ReadyCondition)).WithTimeout(distTimeout).WithPolling(distPolling).
			Should(Equal(controlplanev1beta2.DistributionMismatchReason))
		g.Eventually(d.conditionReason(controlplanev1beta2.MachinesUpToDateCondition)).WithTimeout(distTimeout).WithPolling(distPolling).
			Should(Equal(controlplanev1beta2.DistributionMismatchReason))
		g.Expect(d.conditionReason(controlplanev1beta2.AvailableCondition)()).NotTo(Equal(controlplanev1beta2.DistributionMismatchReason),
			"a serving control plane is not reported unavailable")
		g.Eventually(func() []string { return env.events.withPrefix(d.ns, "Warning DistributionMismatch") }).
			WithTimeout(distTimeout).WithPolling(distPolling).Should(ContainElement(SatisfyAll(
			ContainSubstring("spec.distribution=k3s"), ContainSubstring("k0s (")))) // names the Machines per distribution

		// Scale down while held: nothing is deleted.
		d.updateKCP(g, func(kcp *controlplanev1beta2.KairosControlPlane) { kcp.Spec.Replicas = ptr.To(int32(1)) })
		d.touch(g)
		g.Consistently(d.deletingMachines).WithTimeout(distQuiet).WithPolling(distPolling).Should(BeEmpty())

		// An operator deletes a member: its hook stays (D4).
		d.deleteMachine(g, d.machines[0])
		d.touch(g)
		g.Consistently(d.machineAnnotations(d.machines[0])).WithTimeout(distQuiet).WithPolling(distPolling).Should(HaveKey(etcdLeaveHookKey),
			"a k0s member relabelled k3s must not lose its etcd-leave hook without a leave")
		g.Expect(env.events.withPrefix(d.ns, "Warning DistributionMismatch")).To(HaveLen(1), "the Warning is emitted once per hold")

		// Fix: spec back to what the Machines run.
		d.updateKCP(g, func(kcp *controlplanev1beta2.KairosControlPlane) { kcp.Spec.Distribution = "k0s" })
		g.Eventually(d.conditionReason(clusterv1.ReadyCondition)).WithTimeout(distTimeout).WithPolling(distPolling).
			ShouldNot(Equal(controlplanev1beta2.DistributionMismatchReason))
		g.Eventually(func() []string { return env.events.withPrefix(d.ns, "Normal DistributionMismatchResolved") }).
			WithTimeout(distTimeout).WithPolling(distPolling).Should(HaveLen(1))
	})

	// E2: removing spec.distribution from a running control plane writes back
	// what the Machines run, not the KairosConfigTemplate's value.
	t.Run("E2 removed spec restores observed", func(t *testing.T) {
		g := NewWithT(t)
		d := env.build(t, "dist-e2", distFixture{spec: "k0s", template: "k3s", replicas: 1, machines: []string{"k0s"}})
		d.waitSettled(g)

		d.updateKCP(g, func(kcp *controlplanev1beta2.KairosControlPlane) { kcp.Spec.Distribution = "" })
		g.Eventually(d.specDistribution).WithTimeout(distTimeout).WithPolling(distPolling).Should(Equal("k0s"))
		g.Eventually(func() []string { return env.events.withPrefix(d.ns, "Normal DistributionRestored") }).
			WithTimeout(distTimeout).WithPolling(distPolling).Should(HaveLen(1))
		d.touch(g)
		g.Consistently(d.specDistribution).WithTimeout(distQuiet).WithPolling(distPolling).Should(Equal("k0s"))
		g.Expect(d.conditionReason(clusterv1.ReadyCondition)()).NotTo(Equal(controlplanev1beta2.DistributionMismatchReason))
	})

	// E3: CAPI topology owns spec.distribution through server-side apply.
	// When its apply drops the field, the apiserver removes it; the controller
	// writes the Machines' value back under its own field manager, so the
	// next apply without the field leaves it.
	t.Run("E3 topology apply without the field: restored and not removed again", func(t *testing.T) {
		g := NewWithT(t)
		d := env.build(t, "dist-e3", distFixture{spec: "k0s", template: "k3s", replicas: 1, machines: []string{"k0s"}, applyKCP: true})
		d.waitSettled(g)

		d.applyKCP(g, "")
		g.Eventually(func() []string { return env.events.withPrefix(d.ns, "Normal DistributionRestored") }).
			WithTimeout(distTimeout).WithPolling(distPolling).Should(HaveLen(1), "the topology apply removed the field and the controller restored it")
		g.Eventually(d.specDistribution).WithTimeout(distTimeout).WithPolling(distPolling).Should(Equal("k0s"))

		d.applyKCP(g, "")
		d.touch(g)
		g.Consistently(d.specDistribution).WithTimeout(distQuiet).WithPolling(distPolling).Should(Equal("k0s"),
			"capi-topology no longer owns the field, so its apply does not remove it")
		g.Expect(env.events.withPrefix(d.ns, "Normal DistributionRestored")).To(HaveLen(1), "nothing had to be restored a second time")
		g.Expect(d.conditionReason(clusterv1.ReadyCondition)()).NotTo(Equal(controlplanev1beta2.DistributionMismatchReason))
	})

	// E5: a kubeadm KairosConfigTemplate is refused even with an explicit
	// k0s, and with no Machines the refusal is today's: Ready and Available
	// False, failure fields set, and no KairosConfig is ever stamped.
	t.Run("E5 kubeadm template with explicit k0s, no Machines", func(t *testing.T) {
		g := NewWithT(t)
		const ns = "dist-e5"
		recorder := recordKairosConfigs(t, env.ctx, env.cfg, ns)
		d := env.build(t, ns, distFixture{spec: "k0s", template: "kubeadm", replicas: 1})

		g.Eventually(d.conditionReason(controlplanev1beta2.AvailableCondition)).WithTimeout(distTimeout).WithPolling(distPolling).
			Should(Equal(controlplanev1beta2.UnsupportedDistributionReason))
		g.Expect(d.conditionReason(clusterv1.ReadyCondition)()).To(Equal(controlplanev1beta2.UnsupportedDistributionReason))
		kcp := d.get(g)
		g.Expect(kcp.Status.FailureReason).To(Equal(controlplanev1beta2.UnsupportedDistributionReason))
		g.Expect(kcp.Status.FailureMessage).To(ContainSubstring("kubeadm is not supported on a KairosControlPlane"))
		g.Expect(kcp.Spec.Distribution).To(Equal("k0s"))

		d.touch(g)
		g.Consistently(func() int { return len(recorder.withRole(bootstrapv1beta2.ControlPlaneRoleSingle)) }).
			WithTimeout(distQuiet).WithPolling(distPolling).Should(BeZero(), "no control-plane KairosConfig may be built from a kubeadm template")
		g.Expect(d.allMachines(g)).To(BeEmpty())
	})

	// E6: the same template with Machines holds like a mismatch: Ready and
	// MachinesUpToDate False, Available and the failure fields untouched, and
	// a scale-down deletes nothing.
	t.Run("E6 kubeadm template with explicit k0s, with Machines", func(t *testing.T) {
		g := NewWithT(t)
		d := env.build(t, "dist-e6", distFixture{spec: "k0s", template: "kubeadm", replicas: 1, machines: []string{"k0s", "k0s"}})

		g.Eventually(d.conditionReason(controlplanev1beta2.MachinesUpToDateCondition)).WithTimeout(distTimeout).WithPolling(distPolling).
			Should(Equal(controlplanev1beta2.UnsupportedDistributionReason))
		g.Expect(d.conditionReason(clusterv1.ReadyCondition)()).To(Equal(controlplanev1beta2.UnsupportedDistributionReason))
		g.Expect(d.conditionReason(controlplanev1beta2.AvailableCondition)()).NotTo(Equal(controlplanev1beta2.UnsupportedDistributionReason))
		kcp := d.get(g)
		g.Expect(kcp.Status.FailureReason).To(BeEmpty())
		g.Expect(kcp.Status.FailureMessage).To(BeEmpty())

		d.touch(g)
		g.Consistently(d.deletingMachines).WithTimeout(distQuiet).WithPolling(distPolling).Should(BeEmpty())
		g.Expect(d.allMachines(g)).To(HaveLen(2))
		g.Expect(env.events.withPrefix(d.ns, "Warning UnsupportedDistribution")).To(HaveLen(1))
	})

	// E7: a control plane relabelled before the upgrade is held at once, and
	// the controller writes nothing into its spec. Setting spec back lets the
	// held scale-down run, which shows the hold was what stopped it.
	t.Run("E7 pre-relabelled is held with no spec write", func(t *testing.T) {
		g := NewWithT(t)
		d := env.build(t, "dist-e7", distFixture{spec: "k3s", template: "k3s", replicas: 1, machines: []string{"k0s", "k0s", "k0s"}})

		g.Eventually(d.conditionReason(clusterv1.ReadyCondition)).WithTimeout(distTimeout).WithPolling(distPolling).
			Should(Equal(controlplanev1beta2.DistributionMismatchReason))
		generation := d.get(g).Generation
		d.touch(g)
		g.Consistently(func(g Gomega) {
			kcp := d.get(g)
			g.Expect(kcp.Spec.Distribution).To(Equal("k3s"))
			g.Expect(kcp.Generation).To(Equal(generation), "no spec write")
		}).WithTimeout(distQuiet).WithPolling(distPolling).Should(Succeed())
		g.Expect(d.deletingMachines()).To(BeEmpty())
		g.Expect(d.allMachines(g)).To(HaveLen(3))

		d.updateKCP(g, func(kcp *controlplanev1beta2.KairosControlPlane) { kcp.Spec.Distribution = "k0s" })
		// Nothing holds these Machines (no hook, no finalizer), so the
		// scale-down runs one delete per reconcile down to the one replica.
		g.Eventually(func(g Gomega) { g.Expect(d.allMachines(g)).To(HaveLen(1)) }).
			WithTimeout(distTimeout).WithPolling(distPolling).Should(Succeed(), "once the hold clears, the scale-down runs")
	})

	// E8: Machines that disagree hold membership changes; deleting the
	// minority clears the hold. The controller never deletes them itself.
	t.Run("E8 mixed is held, deleting the minority clears it", func(t *testing.T) {
		g := NewWithT(t)
		d := env.build(t, "dist-e8", distFixture{spec: "k0s", template: "k0s", replicas: 3, machines: []string{"k0s", "k0s", "k3s"}})

		g.Eventually(d.conditionMessage(clusterv1.ReadyCondition)).WithTimeout(distTimeout).WithPolling(distPolling).
			Should(ContainSubstring("run different distributions"))
		g.Expect(d.conditionReason(clusterv1.ReadyCondition)()).To(Equal(controlplanev1beta2.DistributionMismatchReason))
		d.touch(g)
		g.Consistently(func(g Gomega) { g.Expect(d.allMachines(g)).To(HaveLen(3)) }).
			WithTimeout(distQuiet).WithPolling(distPolling).Should(Succeed(), "the controller never deletes Machines while they disagree")

		d.deleteMachine(g, d.machines[2])
		g.Eventually(d.conditionReason(clusterv1.ReadyCondition)).WithTimeout(distTimeout).WithPolling(distPolling).
			ShouldNot(Equal(controlplanev1beta2.DistributionMismatchReason))
		g.Eventually(func() []string { return env.events.withPrefix(d.ns, "Normal DistributionMismatchResolved") }).
			WithTimeout(distTimeout).WithPolling(distPolling).Should(HaveLen(1))
		g.Eventually(d.conditionReason(controlplanev1beta2.MachinesUpToDateCondition)).WithTimeout(distTimeout).WithPolling(distPolling).
			ShouldNot(Equal(controlplanev1beta2.DistributionMismatchReason))
	})

	// E9: deleting a held KairosControlPlane completes: reconcileDelete never
	// reaches the hold.
	t.Run("E9 KCP deletion completes while held", func(t *testing.T) {
		g := NewWithT(t)
		d := env.build(t, "dist-e9", distFixture{spec: "k3s", template: "k3s", replicas: 3, machines: []string{"k0s", "k0s", "k0s"}, hooked: true})
		g.Eventually(d.conditionReason(clusterv1.ReadyCondition)).WithTimeout(distTimeout).WithPolling(distPolling).
			Should(Equal(controlplanev1beta2.DistributionMismatchReason))

		g.Expect(env.c.Delete(env.ctx, d.get(g))).To(Succeed())
		g.Eventually(func(g Gomega) { g.Expect(d.allMachines(g)).To(BeEmpty()) }).
			WithTimeout(distTimeout).WithPolling(distPolling).Should(Succeed())
		g.Eventually(func() error {
			err := env.c.Get(env.ctx, d.kcp, &controlplanev1beta2.KairosControlPlane{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			return fmt.Errorf("KairosControlPlane %s still exists", d.kcp)
		}).WithTimeout(distTimeout).WithPolling(distPolling).Should(Succeed())
	})

	// E10: a paused Cluster gets no spec write, even with an empty
	// spec.distribution the Machines would fill. Unpausing writes it.
	t.Run("E10 paused writes nothing", func(t *testing.T) {
		g := NewWithT(t)
		d := env.build(t, "dist-e10", distFixture{spec: "", template: "k3s", replicas: 1, machines: []string{"k0s"}, clusterPaused: true})
		generation := d.get(g).Generation
		d.touch(g)
		g.Consistently(func(g Gomega) {
			kcp := d.get(g)
			g.Expect(kcp.Spec.Distribution).To(BeEmpty())
			g.Expect(kcp.Generation).To(Equal(generation))
		}).WithTimeout(distQuiet).WithPolling(distPolling).Should(Succeed())
		g.Expect(env.events.withPrefix(d.ns, "")).To(BeEmpty(), "a paused control plane emits nothing")

		d.updateCluster(g, func(cl *clusterv1.Cluster) { cl.Spec.Paused = ptr.To(false) })
		g.Eventually(d.specDistribution).WithTimeout(distTimeout).WithPolling(distPolling).Should(Equal("k0s"),
			"unpaused, the empty spec.distribution is written from the Machines, not the template")
	})
}

// build creates the namespace, Cluster, KairosConfigTemplate and
// KairosControlPlane of f, then its Machines and their KairosConfigs. The KCP
// is created with the paused annotation and unpaused only once every fixture
// exists, so the controller never sees a half-built control plane: an empty
// spec.distribution would otherwise be inherited from the template before the
// Machines appeared.
func (e *distEnv) build(t *testing.T, ns string, f distFixture) *distCluster {
	t.Helper()
	g := NewWithT(t)
	d := &distCluster{env: e, ns: ns, cluster: ns + "-cluster", kcp: types.NamespacedName{Namespace: ns, Name: ns + "-kcp"}}

	g.Expect(e.c.Create(e.ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	g.Expect(e.c.Create(e.ctx, &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: d.cluster, Namespace: ns},
		Spec: clusterv1.ClusterSpec{
			Paused: ptr.To(f.clusterPaused),
			ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: controlplanev1beta2.GroupVersion.Group, Kind: "KairosControlPlane", Name: d.kcp.Name,
			},
			ControlPlaneEndpoint: testControlPlaneEndpoint(),
		},
	})).To(Succeed())
	g.Expect(e.c.Create(e.ctx, &bootstrapv1beta2.KairosConfigTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: distTemplate, Namespace: ns},
		Spec: bootstrapv1beta2.KairosConfigTemplateSpec{Template: bootstrapv1beta2.KairosConfigTemplateResource{
			Spec: bootstrapv1beta2.KairosConfigSpec{Role: "control-plane", Distribution: f.template},
		}},
	})).To(Succeed())

	if f.applyKCP {
		d.applyKCPWith(g, f.spec, f.replicas)
	} else {
		g.Expect(e.c.Create(e.ctx, d.kcpObject(f.spec, f.replicas))).To(Succeed())
	}
	// Paused or not, the finalizer goes on first.
	g.Eventually(func(g Gomega) {
		g.Expect(d.get(g).Finalizers).To(ContainElement(controlplanev1beta2.KairosControlPlaneFinalizer))
	}).WithTimeout(distTimeout).WithPolling(distPolling).Should(Succeed())

	kcp := d.get(g)
	for i, dist := range f.machines {
		name := fmt.Sprintf("%s-%d", d.kcp.Name, i)
		d.machines = append(d.machines, name)
		owner := *metav1.NewControllerRef(kcp, controlplanev1beta2.GroupVersion.WithKind("KairosControlPlane"))
		if dist != "" {
			g.Expect(e.c.Create(e.ctx, &bootstrapv1beta2.KairosConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: name, Namespace: ns,
					Labels:          map[string]string{clusterv1.ClusterNameLabel: d.cluster, clusterv1.MachineControlPlaneLabel: ""},
					OwnerReferences: []metav1.OwnerReference{owner},
				},
				Spec: bootstrapv1beta2.KairosConfigSpec{Role: "control-plane", Distribution: dist, KubernetesVersion: distVersion},
			})).To(Succeed())
		}
		m := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns,
				Labels:          map[string]string{clusterv1.ClusterNameLabel: d.cluster, clusterv1.MachineControlPlaneLabel: ""},
				OwnerReferences: []metav1.OwnerReference{owner},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: d.cluster,
				Version:     distVersion,
				Bootstrap: clusterv1.Bootstrap{ConfigRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: bootstrapv1beta2.GroupVersion.Group, Kind: "KairosConfig", Name: name,
				}},
				InfrastructureRef: testMachineInfraRef("infra-" + name),
			},
		}
		if f.hooked {
			m.Annotations = map[string]string{etcdLeaveHookKey: ""}
		}
		if f.holdFinalizer {
			m.Finalizers = []string{distHoldFinal}
		}
		g.Expect(e.c.Create(e.ctx, m)).To(Succeed())
	}
	// The KCP must see every Machine before it is unpaused.
	g.Eventually(func(g Gomega) { g.Expect(d.allMachines(g)).To(HaveLen(len(f.machines))) }).
		WithTimeout(distTimeout).WithPolling(distPolling).Should(Succeed())

	if f.applyKCP {
		// Unpause by a merge patch from the test's own field manager, so the
		// topology applies below touch only the fields they own.
		g.Expect(e.c.Patch(e.ctx, d.get(g), client.RawPatch(types.MergePatchType,
			[]byte(`{"metadata":{"annotations":{"`+clusterv1.PausedAnnotation+`":null}}}`)))).To(Succeed())
	} else {
		d.updateKCP(g, func(kcp *controlplanev1beta2.KairosControlPlane) { delete(kcp.Annotations, clusterv1.PausedAnnotation) })
	}
	return d
}

// kcpObject is the KCP of the fixture, created paused.
func (d *distCluster) kcpObject(spec string, replicas int32) *controlplanev1beta2.KairosControlPlane {
	return &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name: d.kcp.Name, Namespace: d.ns,
			Labels:      map[string]string{clusterv1.ClusterNameLabel: d.cluster},
			Annotations: map[string]string{clusterv1.PausedAnnotation: ""},
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas:     ptr.To(replicas),
			Version:      distVersion,
			Distribution: spec,
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "not-installed",
					Namespace:  d.ns,
				},
			},
			KairosConfigTemplate: controlplanev1beta2.KairosConfigTemplateReference{Name: distTemplate},
		},
	}
}

// applyKCPWith server-side applies the KCP as capi-topology: the fields
// CAPI topology sets from a KairosControlPlaneTemplate, spec.distribution
// only when not empty. The first apply also sets the paused annotation the
// fixture starts with; later applies do not own it.
func (d *distCluster) applyKCPWith(g Gomega, spec string, replicas int32) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(controlplanev1beta2.GroupVersion.WithKind("KairosControlPlane"))
	u.SetName(d.kcp.Name)
	u.SetNamespace(d.ns)
	u.SetLabels(map[string]string{clusterv1.ClusterNameLabel: d.cluster})
	specFields := map[string]interface{}{
		"replicas": int64(replicas),
		"version":  distVersion,
		"machineTemplate": map[string]interface{}{"infrastructureRef": map[string]interface{}{
			"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
			"kind":       "DockerMachineTemplate",
			"name":       "not-installed",
			"namespace":  d.ns,
		}},
		"kairosConfigTemplate": map[string]interface{}{"name": distTemplate},
	}
	if spec != "" {
		specFields["distribution"] = spec
	}
	u.Object["spec"] = specFields
	if err := d.env.c.Get(d.env.ctx, d.kcp, &controlplanev1beta2.KairosControlPlane{}); apierrors.IsNotFound(err) {
		u.SetAnnotations(map[string]string{clusterv1.PausedAnnotation: ""})
	}
	g.Expect(d.env.c.Apply(d.env.ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(distFieldOwner), client.ForceOwnership)).To(Succeed())
}

// applyKCP is a later topology apply with the fixture's replicas.
func (d *distCluster) applyKCP(g Gomega, spec string) {
	d.applyKCPWith(g, spec, ptr.Deref(d.get(g).Spec.Replicas, 1))
}

// waitSettled waits until the control plane has reconciled its Machines with
// nothing held: MachinesUpToDate True, which reconcileMachines sets only past
// every hold.
func (d *distCluster) waitSettled(g Gomega) {
	g.Eventually(func(g Gomega) {
		kcp := d.get(g)
		c := kcpCondition(kcp, controlplanev1beta2.MachinesUpToDateCondition)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(corev1.ConditionTrue))
	}).WithTimeout(distTimeout).WithPolling(distPolling).Should(Succeed())
}

func (d *distCluster) get(g Gomega) *controlplanev1beta2.KairosControlPlane {
	kcp := &controlplanev1beta2.KairosControlPlane{}
	g.Expect(d.env.c.Get(d.env.ctx, d.kcp, kcp)).To(Succeed())
	return kcp
}

// conditionReason returns a poll function for the reason of condition ct.
func (d *distCluster) conditionReason(ct clusterv1.ConditionType) func() (string, error) {
	return func() (string, error) {
		kcp := &controlplanev1beta2.KairosControlPlane{}
		if err := d.env.c.Get(d.env.ctx, d.kcp, kcp); err != nil {
			return "", err
		}
		if c := kcpCondition(kcp, string(ct)); c != nil {
			return c.Reason, nil
		}
		return "", nil
	}
}

// conditionMessage returns a poll function for the message of condition ct.
func (d *distCluster) conditionMessage(ct clusterv1.ConditionType) func() (string, error) {
	return func() (string, error) {
		kcp := &controlplanev1beta2.KairosControlPlane{}
		if err := d.env.c.Get(d.env.ctx, d.kcp, kcp); err != nil {
			return "", err
		}
		if c := kcpCondition(kcp, string(ct)); c != nil {
			return c.Message, nil
		}
		return "", nil
	}
}

func (d *distCluster) specDistribution() (string, error) {
	kcp := &controlplanev1beta2.KairosControlPlane{}
	if err := d.env.c.Get(d.env.ctx, d.kcp, kcp); err != nil {
		return "", err
	}
	return kcp.Spec.Distribution, nil
}

// allMachines lists the control-plane Machines of the fixture's Cluster.
func (d *distCluster) allMachines(g Gomega) []clusterv1.Machine {
	list := &clusterv1.MachineList{}
	g.Expect(d.env.c.List(d.env.ctx, list, client.InNamespace(d.ns),
		client.MatchingLabels{clusterv1.ClusterNameLabel: d.cluster})).To(Succeed())
	return list.Items
}

// deletingMachines returns the names of the Machines being deleted.
func (d *distCluster) deletingMachines() ([]string, error) {
	list := &clusterv1.MachineList{}
	if err := d.env.c.List(d.env.ctx, list, client.InNamespace(d.ns),
		client.MatchingLabels{clusterv1.ClusterNameLabel: d.cluster}); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range list.Items {
		if !m.DeletionTimestamp.IsZero() {
			out = append(out, m.Name)
		}
	}
	return out, nil
}

// machineAnnotations returns a poll function for the annotations of Machine
// name.
func (d *distCluster) machineAnnotations(name string) func() (map[string]string, error) {
	return func() (map[string]string, error) {
		m := &clusterv1.Machine{}
		if err := d.env.c.Get(d.env.ctx, types.NamespacedName{Namespace: d.ns, Name: name}, m); err != nil {
			return nil, err
		}
		return m.Annotations, nil
	}
}

func (d *distCluster) deleteMachine(g Gomega, name string) {
	m := &clusterv1.Machine{}
	g.Expect(d.env.c.Get(d.env.ctx, types.NamespacedName{Namespace: d.ns, Name: name}, m)).To(Succeed())
	g.Expect(d.env.c.Delete(d.env.ctx, m)).To(Succeed())
}

// updateKCP applies mutate to the live KCP, retrying on conflict: the
// controller writes the object concurrently.
func (d *distCluster) updateKCP(g Gomega, mutate func(*controlplanev1beta2.KairosControlPlane)) {
	g.Eventually(func() error {
		kcp := &controlplanev1beta2.KairosControlPlane{}
		if err := d.env.c.Get(d.env.ctx, d.kcp, kcp); err != nil {
			return err
		}
		mutate(kcp)
		return d.env.c.Update(d.env.ctx, kcp)
	}).WithTimeout(10 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

func (d *distCluster) updateCluster(g Gomega, mutate func(*clusterv1.Cluster)) {
	g.Eventually(func() error {
		cl := &clusterv1.Cluster{}
		if err := d.env.c.Get(d.env.ctx, types.NamespacedName{Namespace: d.ns, Name: d.cluster}, cl); err != nil {
			return err
		}
		mutate(cl)
		return d.env.c.Update(d.env.ctx, cl)
	}).WithTimeout(10 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

// touch re-enqueues the KCP by changing an annotation, instead of waiting out
// the 60s hold requeue.
func (d *distCluster) touch(g Gomega) {
	d.updateKCP(g, func(kcp *controlplanev1beta2.KairosControlPlane) {
		if kcp.Annotations == nil {
			kcp.Annotations = map[string]string{}
		}
		kcp.Annotations[distTouchMarker] = time.Now().Format(time.RFC3339Nano)
	})
}

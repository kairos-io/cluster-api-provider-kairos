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

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// TestSSHFallback_DisabledStaysSilent (PR-9, ADR § D.2 negative case):
// when Spec.SSHFallback.Enabled=false (and when SSHFallback is nil entirely),
// the sibling reconciler must never enqueue work to the worker pool — and so
// the KCP's KubeconfigReadyCondition must never show any SSHFallback-prefixed
// Reason, even after the kubeconfigReadyTimeout / ActivateAfter window has
// long elapsed. This pins the "off by default" guarantee at the integration
// layer.
//
// Strategy: set Status.LastNodePushObserved to "long ago" so a buggy
// reconciler that fails to gate on Enabled=false would immediately fire the
// fallback path. Consistently for 5s assert no SSHFallback Reason ever
// appears.
func TestSSHFallback_DisabledStaysSilent(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping envtest in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ssh-disabled-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	clusterName := "ssh-disabled-cluster"
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns.Name},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: &corev1.ObjectReference{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       clusterName + "-kcp",
				Namespace:  ns.Name,
			},
		},
	}
	g.Expect(c.Create(ctx, cluster)).To(Succeed())

	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-kcp",
			Namespace: ns.Name,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas: ptr.To(int32(1)),
			Version:  "v1.30.0+k0s.0",
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "missing-on-purpose",
					Namespace:  ns.Name,
				},
			},
			KairosConfigTemplate: controlplanev1beta2.KairosConfigTemplateReference{
				Name: "missing-on-purpose",
			},
			// SSHFallback intentionally nil — equivalent to Enabled=false.
		},
	}
	g.Expect(c.Create(ctx, kcp)).To(Succeed())

	// Backdate LastNodePushObserved past KubeconfigReadyTimeout so a buggy
	// reconciler with a missing Enabled=true gate would immediately fire.
	g.Eventually(func() error {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return err
		}
		stale := metav1.NewTime(time.Now().Add(-30 * time.Minute))
		got.Status.LastNodePushObserved = &stale
		return c.Status().Update(ctx, got)
	}, 30*time.Second, time.Second).Should(Succeed())

	// Consistently for 5s — no SSHFallback Reason ever appears.
	g.Consistently(func() string {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return "ERR: " + err.Error()
		}
		cond := conditions.Get(got, controlplanev1beta2.KubeconfigReadyCondition)
		if cond == nil {
			return ""
		}
		return cond.Reason
	}, 5*time.Second, 500*time.Millisecond).Should(Or(
		Equal(""),
		Equal(controlplanev1beta2.WaitingForNodePushReason),
	), "Spec.SSHFallback=nil/disabled MUST keep KubeconfigReadyCondition.Reason out of the SSHFallback* vocabulary")
}

// TestSSHFallback_MisconfiguredSurfacesCondition (PR-9, ADR § D.2):
// when Spec.SSHFallback.Enabled=true but the referenced Secrets do not exist,
// the worker MUST surface SSHFallbackMisconfiguredReason on
// KubeconfigReadyCondition. This pins the integration between the reconciler
// (which enqueues the job), the worker (which fails fast on the missing
// Secret and posts a Misconfigured result), the result-drain goroutine
// (which sets the condition), and the eligibility predicate (which lets the
// next reconcile observe the new condition state).
//
// Important: this test uses NO real SSH dialing. The worker exits before
// any dial attempt because loadKnownHosts can't find the Secret. That's the
// design — Misconfigured is the cheapest possible result and the
// fastest-feedback path for operator typos.
func TestSSHFallback_MisconfiguredSurfacesCondition(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping envtest in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ssh-misconfigured-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	clusterName := "ssh-misconfigured-cluster"
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns.Name},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: &corev1.ObjectReference{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       clusterName + "-kcp",
				Namespace:  ns.Name,
			},
		},
	}
	g.Expect(c.Create(ctx, cluster)).To(Succeed())

	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-kcp",
			Namespace: ns.Name,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas: ptr.To(int32(1)),
			Version:  "v1.30.0+k0s.0",
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "missing-on-purpose",
					Namespace:  ns.Name,
				},
			},
			KairosConfigTemplate: controlplanev1beta2.KairosConfigTemplateReference{
				Name: "missing-on-purpose",
			},
			SSHFallback: &controlplanev1beta2.SSHFallback{
				Enabled:             true,
				KnownHostsSecretRef: &controlplanev1beta2.SSHFallbackSecretReference{Name: "missing-known-hosts"},
				IdentitySecretRef:   &controlplanev1beta2.SSHFallbackSecretReference{Name: "missing-identity"},
				User:                "kairos",
				Port:                22,
				ActivateAfter:       &metav1.Duration{Duration: 15 * time.Minute},
			},
		},
	}
	g.Expect(c.Create(ctx, kcp)).To(Succeed())

	// Pre-create a control-plane Machine with an address so the worker's
	// address-resolver doesn't defer. The address won't actually be dialed
	// because the Misconfigured path fails before the dial.
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcp.Name + "-0",
			Namespace: ns.Name,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         clusterName,
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: controlplanev1beta2.GroupVersion.String(),
					Kind:       "KairosControlPlane",
					Name:       kcp.Name,
					UID:        kcp.UID,
					Controller: ptr.To(true),
				},
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: clusterName,
			Bootstrap:   clusterv1.Bootstrap{DataSecretName: ptr.To("placeholder")},
			Version:     ptr.To("v1.30.0+k0s.0"),
		},
	}
	g.Expect(c.Create(ctx, machine)).To(Succeed())
	g.Eventually(func() error {
		got := &clusterv1.Machine{}
		if err := c.Get(ctx, types.NamespacedName{Name: machine.Name, Namespace: machine.Namespace}, got); err != nil {
			return err
		}
		got.Status.Addresses = []clusterv1.MachineAddress{
			{Type: clusterv1.MachineInternalIP, Address: "10.99.99.1"},
		}
		return c.Status().Update(ctx, got)
	}, 30*time.Second, time.Second).Should(Succeed())

	// Backdate LastNodePushObserved past ActivateAfter so the eligibility
	// gate fires on the next reconcile.
	g.Eventually(func() error {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return err
		}
		stale := metav1.NewTime(time.Now().Add(-30 * time.Minute))
		got.Status.LastNodePushObserved = &stale
		return c.Status().Update(ctx, got)
	}, 30*time.Second, time.Second).Should(Succeed())

	// Eventually the worker fires, fails fast on the missing Secrets, and
	// the result drain sets the Reason to SSHFallbackMisconfigured.
	//
	// Determinism: the sibling reconciler's time-based backstop is
	// collapsed to EvalRequeue=2s in startKCPEnvtest, so once the gate is
	// open the eligibility re-check fires within a couple of seconds
	// rather than racing the 1-minute production cadence. The 90s window
	// (well above that 2s cadence) is generous headroom so envtest
	// start-up cost on a slow/contended CI runner cannot eat into it.
	g.Eventually(func() string {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return "ERR: " + err.Error()
		}
		cond := conditions.Get(got, controlplanev1beta2.KubeconfigReadyCondition)
		if cond == nil {
			return ""
		}
		return cond.Reason
	}, 90*time.Second, 2*time.Second).Should(Equal(controlplanev1beta2.SSHFallbackMisconfiguredReason),
		"missing SSHFallback Secrets MUST surface SSHFallbackMisconfigured on KubeconfigReadyCondition")
}

// TestSSHFallback_AnnotationDrivesReason (PR-9, exercises commit 3's
// observeKubeconfigSecret 9-LOC change): when the kubeconfig Secret is
// observed AND its annotation is `kubeconfig-source=ssh-fallback`, KCP must
// transition to KubeconfigReadyViaSSHFallback (not the default
// KubeconfigReady). This isolates and pins the annotation→Reason mapping
// without needing a real SSH dial — the test writes the Secret directly,
// simulating what a successful worker would have produced.
//
// Doubling as a regression guard: the annotation literal is fragile
// (matches both the worker writer side and the observer side); a typo on
// either side breaks the audit trail in production with no other signal.
func TestSSHFallback_AnnotationDrivesReason(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping envtest in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ssh-annotation-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	clusterName := "ssh-annotation-cluster"
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns.Name},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: &corev1.ObjectReference{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       clusterName + "-kcp",
				Namespace:  ns.Name,
			},
		},
	}
	g.Expect(c.Create(ctx, cluster)).To(Succeed())

	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-kcp",
			Namespace: ns.Name,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas: ptr.To(int32(1)),
			Version:  "v1.30.0+k0s.0",
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "missing-on-purpose",
					Namespace:  ns.Name,
				},
			},
			KairosConfigTemplate: controlplanev1beta2.KairosConfigTemplateReference{
				Name: "missing-on-purpose",
			},
		},
	}
	g.Expect(c.Create(ctx, kcp)).To(Succeed())

	// Pre-create a control-plane Machine so reconcileMachines doesn't try to
	// invoke createControlPlaneMachine (which would fail without the infra/
	// config templates that don't exist in envtest). Same pattern as the
	// PR-7 node-push test. Without this, Reconcile errors out before reaching
	// observeKubeconfigSecret and the annotation→Reason wiring never runs.
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcp.Name + "-0",
			Namespace: ns.Name,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         clusterName,
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: controlplanev1beta2.GroupVersion.String(),
					Kind:       "KairosControlPlane",
					Name:       kcp.Name,
					UID:        kcp.UID,
					Controller: ptr.To(true),
				},
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: clusterName,
			Bootstrap:   clusterv1.Bootstrap{DataSecretName: ptr.To("placeholder")},
			Version:     ptr.To("v1.30.0+k0s.0"),
		},
	}
	g.Expect(c.Create(ctx, machine)).To(Succeed())

	// Write the kubeconfig Secret with the ssh-fallback annotation — this is
	// the byte-for-byte shape the worker (ssh_fallback_worker.go) produces on
	// success. Doing it from the test exercises observeKubeconfigSecret's
	// annotation-aware branch without any SSH dialing.
	pushed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-kubeconfig",
			Namespace: ns.Name,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
			Annotations: map[string]string{
				"controllers.cluster.x-k8s.io/kubeconfig-source": "ssh-fallback",
			},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{
			"value": []byte("apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"),
		},
	}
	g.Expect(c.Create(ctx, pushed)).To(Succeed())

	g.Eventually(func() string {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return "ERR: " + err.Error()
		}
		cond := conditions.Get(got, controlplanev1beta2.KubeconfigReadyCondition)
		if cond == nil {
			return ""
		}
		if cond.Status != corev1.ConditionTrue {
			return "not-true: " + string(cond.Status) + "/" + cond.Reason
		}
		return cond.Reason
	}, 30*time.Second, time.Second).Should(Equal(controlplanev1beta2.KubeconfigReadyViaSSHFallbackReason),
		"Secret with ssh-fallback annotation MUST drive KubeconfigReadyCondition.Reason = KubeconfigReadyViaSSHFallback")
}

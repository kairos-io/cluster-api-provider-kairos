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

// TestKD12_KCPDoesNotWriteClusterSpecEndpoint is the KD-12 regression
// guard: per CAPI v1beta2 contract, `Cluster.Spec.ControlPlaneEndpoint` is
// the infrastructure provider's responsibility. The KairosControlPlane
// controller MUST read it, never write it. Before KD-12 (commits
// b5c6307 + fb9eab1 of this PR), the controller polled the LB Service
// (CAPK) or the first Machine's status.addresses (CAPV/CAPD) and copied
// the result into Cluster.Spec. This test pins the "never write"
// invariant so a future refactor can't accidentally reintroduce a write
// path.
//
// The test creates a fully-populated KCP environment where the OLD
// controller would have absolutely written the endpoint (Machine with
// status.addresses, no pre-set endpoint on the Cluster), then asserts
// Consistently that the host stays empty AND the KCP shows the new
// WaitingForInfrastructureControlPlaneEndpoint Reason on its Available
// condition.
func TestKD12_KCPDoesNotWriteClusterSpecEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping envtest in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kd12-no-writes-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	clusterName := "kd12-no-writes-cluster"
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns.Name},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: &corev1.ObjectReference{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       clusterName + "-kcp",
				Namespace:  ns.Name,
			},
			// ControlPlaneEndpoint intentionally left empty — the
			// INVARIANT under test is that the controller does NOT
			// populate it.
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

	// Pre-create a control-plane Machine with a usable IP address. Before
	// KD-12 this would have caused the controller to copy the IP into
	// Cluster.Spec.ControlPlaneEndpoint. After KD-12 it must NOT.
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

	// Populate the Machine's status.addresses — the old write path's
	// preferred trigger.
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

	// Wait for the controller to reconcile + emit the new Reason. The
	// Eventually here is the "controller has run at least once with the
	// Machine in place" gate; the actual regression assertion is the
	// Consistently below.
	g.Eventually(func() string {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return "ERR: " + err.Error()
		}
		cond := conditions.Get(got, controlplanev1beta2.AvailableCondition)
		if cond == nil {
			return ""
		}
		return cond.Reason
	}, 30*time.Second, time.Second).Should(Equal(controlplanev1beta2.WaitingForInfrastructureControlPlaneEndpointReason),
		"with Cluster.Spec.ControlPlaneEndpoint empty, the KCP MUST surface WaitingForInfrastructureControlPlaneEndpoint on Available — not WaitingForMachines")

	// The regression assertion: for 30s, neither the host nor the port
	// of Cluster.Spec.ControlPlaneEndpoint may be written by the
	// controller. A regression that resurrects the auto-write would
	// fail this assertion within the first reconcile after the Machine
	// status was populated.
	g.Consistently(func() string {
		got := &clusterv1.Cluster{}
		if err := c.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns.Name}, got); err != nil {
			return "ERR: " + err.Error()
		}
		// Return a stringified Endpoint so the failure message shows the
		// actual value that leaked in.
		ep := got.Spec.ControlPlaneEndpoint
		if ep.Host == "" && ep.Port == 0 {
			return ""
		}
		return ep.Host + ":" + string(rune(ep.Port))
	}, 30*time.Second, 5*time.Second).Should(BeEmpty(),
		"KairosControlPlane controller MUST NOT write Cluster.Spec.ControlPlaneEndpoint — that field is the infrastructure provider's responsibility (KD-12)")
}

// TestKD12_KCPReadsClusterSpecEndpointAfterInfraSets verifies the
// transition: when the infrastructure provider (or CAPI core copying from
// an InfraCluster) eventually populates `Cluster.Spec.ControlPlaneEndpoint`,
// the KairosControlPlane controller observes it and transitions the
// Available condition's Reason AWAY from WaitingForInfrastructureControlPlaneEndpoint.
// (The next gate is reconcileMachines / kubeconfig readiness, which we
// don't exercise here — the controller will land on either
// WaitingForMachines or WaitingForNodePush depending on what else is
// pending. We only assert the endpoint-wait Reason clears.)
//
// This is the happy-path companion to TestKD12_KCPDoesNotWriteClusterSpecEndpoint
// and pins the read-don't-write data flow that PR-8's PR-9's KD-12's PRs
// all converge on.
func TestKD12_KCPReadsClusterSpecEndpointAfterInfraSets(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping envtest in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kd12-reads-after-set-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	clusterName := "kd12-reads-after-set-cluster"
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

	// Pre-create a control-plane Machine so reconcileMachines doesn't
	// fail before reaching the condition-set block. Same gating pattern
	// as PR-7's TestKD3b_KubeconfigReady_TransitionsOnNodePush — without
	// the Machine, the Reconcile path goes through the failure-set arm
	// and never reaches the WaitingForInfrastructureControlPlaneEndpoint
	// surface we're testing.
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

	// First, observe the KCP settles on WaitingForInfrastructureControlPlaneEndpoint —
	// the precondition for testing the transition.
	g.Eventually(func() string {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return "ERR: " + err.Error()
		}
		cond := conditions.Get(got, controlplanev1beta2.AvailableCondition)
		if cond == nil {
			return ""
		}
		return cond.Reason
	}, 30*time.Second, time.Second).Should(Equal(controlplanev1beta2.WaitingForInfrastructureControlPlaneEndpointReason))

	// Simulate the infrastructure provider populating the endpoint. In
	// production this is CAPI core copying from
	// <Infra>Cluster.Spec.ControlPlaneEndpoint; the effect on the
	// Cluster spec is identical.
	g.Eventually(func() error {
		got := &clusterv1.Cluster{}
		if err := c.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns.Name}, got); err != nil {
			return err
		}
		got.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{
			Host: "10.99.99.1",
			Port: 6443,
		}
		return c.Update(ctx, got)
	}, 30*time.Second, time.Second).Should(Succeed())

	// The Available condition's Reason MUST transition away from
	// WaitingForInfrastructureControlPlaneEndpoint. Where it lands
	// depends on the next gate (no machines / no kubeconfig / etc.),
	// so we only assert "not the endpoint-wait reason anymore".
	g.Eventually(func() string {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return "ERR: " + err.Error()
		}
		cond := conditions.Get(got, controlplanev1beta2.AvailableCondition)
		if cond == nil {
			return ""
		}
		return cond.Reason
	}, 30*time.Second, time.Second).ShouldNot(Equal(controlplanev1beta2.WaitingForInfrastructureControlPlaneEndpointReason),
		"once Cluster.Spec.ControlPlaneEndpoint is populated, the KCP MUST transition away from WaitingForInfrastructureControlPlaneEndpoint")

	// The Cluster's endpoint MUST stay at the value we set (the
	// controller MUST NOT overwrite it).
	g.Consistently(func() string {
		got := &clusterv1.Cluster{}
		if err := c.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns.Name}, got); err != nil {
			return "ERR: " + err.Error()
		}
		return got.Spec.ControlPlaneEndpoint.Host
	}, 15*time.Second, 3*time.Second).Should(Equal("10.99.99.1"),
		"KCP MUST NOT mutate Cluster.Spec.ControlPlaneEndpoint after the infrastructure provider sets it")
}

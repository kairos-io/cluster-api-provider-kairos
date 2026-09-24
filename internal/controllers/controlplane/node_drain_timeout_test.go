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
	"math"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

func drainTimeoutScheme(g *WithT) *runtime.Scheme {
	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	return scheme
}

func drainTimeoutKCP(timeout *metav1.Duration) *controlplanev1beta2.KairosControlPlane {
	replicas := int32(1)
	return &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-kcp",
			Namespace: "default",
			UID:       types.UID("kcp-uid"),
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas:     &replicas,
			Version:      "v1.30.0+k3s1",
			Distribution: "k3s",
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "test-template",
					Namespace:  "default",
				},
				NodeDrainTimeout: timeout,
			},
		},
	}
}

func drainTimeoutInfraTemplate() *unstructured.Unstructured {
	infraTemplate := &unstructured.Unstructured{}
	infraTemplate.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "DockerMachineTemplate",
	})
	infraTemplate.SetName("test-template")
	infraTemplate.SetNamespace("default")
	infraTemplate.Object["spec"] = map[string]interface{}{
		"template": map[string]interface{}{
			"spec": map[string]interface{}{},
		},
	}
	return infraTemplate
}

// TestCreateControlPlaneMachine_PropagatesNodeDrainTimeout proves that a
// nodeDrainTimeout set on the machine template reaches the Machine, which is
// the only place CAPI's Machine controller reads it from when it drains a node.
func TestCreateControlPlaneMachine_PropagatesNodeDrainTimeout(t *testing.T) {
	g := NewWithT(t)
	scheme := drainTimeoutScheme(g)

	kcp := drainTimeoutKCP(&metav1.Duration{Duration: 7 * time.Minute})
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(drainTimeoutInfraTemplate()).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.createControlPlaneMachine(context.Background(), log.Log, kcp, cluster, 0,
		bootstrapv1beta2.ControlPlaneRoleSingle)).To(Succeed())

	machine := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, machine)).To(Succeed())

	g.Expect(machine.Spec.Deletion.NodeDrainTimeoutSeconds).NotTo(BeNil())
	g.Expect(*machine.Spec.Deletion.NodeDrainTimeoutSeconds).To(Equal(int32(420)))
}

// TestCreateControlPlaneMachine_NoNodeDrainTimeoutLeavesFieldUnset keeps the
// CAPI default (drain without a deadline) reachable.
func TestCreateControlPlaneMachine_NoNodeDrainTimeoutLeavesFieldUnset(t *testing.T) {
	g := NewWithT(t)
	scheme := drainTimeoutScheme(g)

	kcp := drainTimeoutKCP(nil)
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(drainTimeoutInfraTemplate()).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.createControlPlaneMachine(context.Background(), log.Log, kcp, cluster, 0,
		bootstrapv1beta2.ControlPlaneRoleSingle)).To(Succeed())

	machine := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, machine)).To(Succeed())

	g.Expect(machine.Spec.Deletion.NodeDrainTimeoutSeconds).To(BeNil())
}

func drainTimeoutMachine(name string, seconds *int32, deleting bool) *clusterv1.Machine {
	m := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         "test-cluster",
				clusterv1.MachineControlPlaneLabel: "",
			},
		},
		Spec: clusterv1.MachineSpec{ClusterName: "test-cluster"},
	}
	m.Spec.Deletion.NodeDrainTimeoutSeconds = seconds
	if deleting {
		now := metav1.Now()
		m.DeletionTimestamp = &now
		m.Finalizers = []string{"test.kairos.io/hold"}
	}
	return m
}

// TestReconcileNodeDrainTimeout_ConvergesExistingMachines is the half that
// matters in practice: the operator raises the deadline because a rollout is
// stalling on an undrainable pod, and the Machines already exist.
func TestReconcileNodeDrainTimeout_ConvergesExistingMachines(t *testing.T) {
	g := NewWithT(t)
	scheme := drainTimeoutScheme(g)

	kcp := drainTimeoutKCP(&metav1.Duration{Duration: 10 * time.Minute})

	stale := drainTimeoutMachine("test-kcp-0", nil, false)
	// A Machine already draining: CAPI re-reads the deadline mid-drain, so this
	// is the one edit that can unblock a stuck rollout.
	draining := drainTimeoutMachine("test-kcp-1", ptrInt32(60), true)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stale, draining).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	machines := []*clusterv1.Machine{stale.DeepCopy(), draining.DeepCopy()}
	g.Expect(r.reconcileNodeDrainTimeout(context.Background(), log.Log, kcp, machines)).To(Succeed())

	for _, name := range []string{"test-kcp-0", "test-kcp-1"} {
		got := &clusterv1.Machine{}
		g.Expect(c.Get(context.Background(),
			types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
		g.Expect(got.Spec.Deletion.NodeDrainTimeoutSeconds).NotTo(BeNil(), name)
		g.Expect(*got.Spec.Deletion.NodeDrainTimeoutSeconds).To(Equal(int32(600)), name)
	}
}

// TestReconcileNodeDrainTimeout_ClearsWhenTemplateUnsets proves the value can
// be taken back off, not only raised.
func TestReconcileNodeDrainTimeout_ClearsWhenTemplateUnsets(t *testing.T) {
	g := NewWithT(t)
	scheme := drainTimeoutScheme(g)

	kcp := drainTimeoutKCP(nil)
	machine := drainTimeoutMachine("test-kcp-0", ptrInt32(300), false)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.reconcileNodeDrainTimeout(context.Background(), log.Log, kcp,
		[]*clusterv1.Machine{machine.DeepCopy()})).To(Succeed())

	got := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, got)).To(Succeed())
	g.Expect(got.Spec.Deletion.NodeDrainTimeoutSeconds).To(BeNil())
}

func TestNodeDrainTimeoutSeconds(t *testing.T) {
	g := NewWithT(t)

	g.Expect(nodeDrainTimeoutSeconds(nil)).To(BeNil())
	g.Expect(*nodeDrainTimeoutSeconds(&metav1.Duration{Duration: 90 * time.Second})).To(Equal(int32(90)))
	// Sub-second precision the Machine contract cannot carry truncates down.
	g.Expect(*nodeDrainTimeoutSeconds(&metav1.Duration{Duration: 1500 * time.Millisecond})).To(Equal(int32(1)))
	// Zero and a negative duration both mean "no deadline" to CAPI, which reads
	// every value <= 0 as unset. The clamp is here so the Machine CRD's
	// minimum: 0 accepts the object, not to bound the drain.
	g.Expect(*nodeDrainTimeoutSeconds(&metav1.Duration{Duration: 0})).To(Equal(int32(0)))
	g.Expect(*nodeDrainTimeoutSeconds(&metav1.Duration{Duration: -5 * time.Minute})).To(Equal(int32(0)))
}

// TestNodeDrainTimeoutSecondsDoesNotOverflow pins the ceiling. Without it,
// int32 narrowing wraps a timeout past math.MaxInt32 seconds, so the longest
// deadline an operator can express becomes either a very short one or a
// negative the API server rejects for minimum: 0. The assertions on the
// unclamped arithmetic are what make the difference visible.
func TestNodeDrainTimeoutSecondsDoesNotOverflow(t *testing.T) {
	g := NewWithT(t)

	// One second past the ceiling: int32 would wrap this to math.MinInt32.
	justOver := time.Duration(math.MaxInt32+1) * time.Second
	g.Expect(int32(int64(justOver.Seconds()))).To(Equal(int32(math.MinInt32)), "precondition: the unclamped narrowing wraps to a negative")
	g.Expect(*nodeDrainTimeoutSeconds(&metav1.Duration{Duration: justOver})).To(Equal(int32(math.MaxInt32)))

	// Twice the ceiling wraps to zero instead, which CAPI reads as "no
	// deadline": an asked-for bound silently becoming an unbounded drain.
	twiceOver := time.Duration(2*(math.MaxInt32+1)) * time.Second
	g.Expect(int32(int64(twiceOver.Seconds()))).To(Equal(int32(0)), "precondition: the unclamped narrowing wraps to zero")
	g.Expect(*nodeDrainTimeoutSeconds(&metav1.Duration{Duration: twiceOver})).To(Equal(int32(math.MaxInt32)))

	// The largest duration metav1.Duration can hold still clamps.
	g.Expect(*nodeDrainTimeoutSeconds(&metav1.Duration{Duration: time.Duration(math.MaxInt64)})).To(Equal(int32(math.MaxInt32)))

	// Exactly the ceiling is passed through untouched.
	g.Expect(*nodeDrainTimeoutSeconds(&metav1.Duration{Duration: time.Duration(math.MaxInt32) * time.Second})).To(Equal(int32(math.MaxInt32)))
}

func ptrInt32(v int32) *int32 { return &v }

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

// TestKD3b_KubeconfigReady_SeverityEscalatesAfterTimeout asserts the Info →
// Warning severity transition once the elapsed since first-miss exceeds
// kubeconfigReadyTimeout. The test simulates the elapsed by patching
// Status.LastNodePushObserved to a past timestamp rather than waiting for
// the real 10-minute clock — the controller's observe helper consults
// time.Since(LastNodePushObserved.Time), so a backdated timestamp produces
// the same condition state without the wall-clock cost.
//
// What this guards:
//
//   - The escalation does not anchor a fresh LastNodePushObserved on each
//     reconcile (we measure elapsed since first observation, not since each
//     reconcile).
//   - The escalation does not transition the condition to True or to a
//     different Reason — only Severity changes.
//   - No terminal state: the controller continues to wait on the Secret
//     watch past the timeout. PR-9 introduces the SSHFallback recovery
//     path; PR-7 deliberately leaves the warning as a visibility-only
//     signal.
func TestKD3b_KubeconfigReady_SeverityEscalatesAfterTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping envtest in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, teardown := startKCPEnvtest(t)
	defer teardown()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "kd3b-timeout-ns"},
	}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	clusterName := "kd3b-timeout-cluster"
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

	// Pre-create a control-plane Machine so reconcileMachines doesn't error
	// on the missing infra/config templates (envtest doesn't load CAPD).
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

	// Wait for the controller to settle on the Info-severity False condition.
	g.Eventually(func() clusterv1.ConditionSeverity {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return ""
		}
		cond := conditions.Get(got, controlplanev1beta2.KubeconfigReadyCondition)
		if cond == nil {
			return ""
		}
		return cond.Severity
	}, 30*time.Second, time.Second).Should(Equal(clusterv1.ConditionSeverityInfo))

	// Backdate Status.LastNodePushObserved past kubeconfigReadyTimeout so
	// the NEXT reconcile flips severity to Warning. The controller patches
	// status on every reconcile, so we Eventually-retry the Status().Update
	// to win against the concurrent writer.
	g.Eventually(func() error {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return err
		}
		stale := metav1.NewTime(time.Now().Add(-30 * time.Minute))
		got.Status.LastNodePushObserved = &stale
		return c.Status().Update(ctx, got)
	}, 15*time.Second, time.Second).Should(Succeed())

	// Re-trigger reconcile by bumping an annotation. Eventually-retry to
	// survive concurrent controller writes.
	g.Eventually(func() error {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return err
		}
		if got.Annotations == nil {
			got.Annotations = map[string]string{}
		}
		got.Annotations["kd3b-test-trigger"] = "1"
		return c.Update(ctx, got)
	}, 15*time.Second, time.Second).Should(Succeed())

	g.Eventually(func() clusterv1.ConditionSeverity {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return ""
		}
		cond := conditions.Get(got, controlplanev1beta2.KubeconfigReadyCondition)
		if cond == nil {
			return ""
		}
		return cond.Severity
	}, 30*time.Second, time.Second).Should(Equal(clusterv1.ConditionSeverityWarning))

	// And the Reason / Status stay the same — only Severity changes.
	final := &controlplanev1beta2.KairosControlPlane{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, final)).To(Succeed())
	cond := conditions.Get(final, controlplanev1beta2.KubeconfigReadyCondition)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(corev1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(controlplanev1beta2.WaitingForNodePushReason))
}

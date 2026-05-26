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

// TestKD3b_KubeconfigReady_TransitionsOnNodePush exercises the KD-3b
// node-push end-to-end loop at the envtest layer:
//
//  1. Create Cluster + KairosControlPlane (no Machines — the gate we're
//     testing is independent of Machine state).
//  2. Observe that the controller settles on
//     KubeconfigReady=False(WaitingForNodePush, Info) once it sees the
//     missing Secret.
//  3. Create the workload-cluster kubeconfig Secret with the
//     `cluster.x-k8s.io/cluster-name` label and a synthetic kubeconfig
//     payload — simulating what a CAPV/CAPK node-side push would do.
//  4. Assert KCP transitions to KubeconfigReady=True via the Secret watch,
//     LastNodePushObserved is cleared, and Initialized flips to true.
//
// The test deliberately writes a SYNTHETIC kubeconfig payload (no
// real API server is needed). The controller does not parse-validate the
// payload — the distribution is the appointed writer and is trusted to
// produce a syntactically valid kubeconfig (see observeKubeconfigSecret
// docstring). The synthetic-payload posture is permanent; the test
// reflects that steady state.
func TestKD3b_KubeconfigReady_TransitionsOnNodePush(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping envtest in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	// Per-test unique namespace so repeat runs against a long-lived envtest
	// don't collide.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kd3b-push-ns",
		},
	}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	clusterName := "kd3b-push-cluster"
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

	// Pre-create a control-plane Machine so reconcileMachines doesn't try
	// to invoke createControlPlaneMachine (which would fail without the
	// infra/config templates that don't exist in envtest). The Machine's
	// labels mark it as owned by our KCP and as a control-plane Machine.
	// We don't need a NodeRef or Bootstrap.DataSecretName — the KD-3b
	// kubeconfig-observe loop is independent of Machine readiness.
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

	// (1) The controller should settle on False(WaitingForNodePush) — even
	// though reconcileMachines will fail (no infra template), Reconcile
	// returns nil on that path after writing FailureReason, so subsequent
	// reconciles still reach the kubeconfig observe step. We use a long
	// Eventually because envtest's first reconcile may race the Cluster
	// owner-reference walk.
	g.Eventually(func() *clusterv1.Condition {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return nil
		}
		return conditions.Get(got, controlplanev1beta2.KubeconfigReadyCondition)
	}, 30*time.Second, time.Second).ShouldNot(BeNil())

	got := &controlplanev1beta2.KairosControlPlane{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got)).To(Succeed())
	cond := conditions.Get(got, controlplanev1beta2.KubeconfigReadyCondition)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(corev1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(controlplanev1beta2.WaitingForNodePushReason))
	g.Expect(cond.Severity).To(Equal(clusterv1.ConditionSeverityInfo))
	g.Expect(got.Status.LastNodePushObserved).NotTo(BeNil())

	// (2) Simulate the node-side curl push: write the kubeconfig Secret with
	// the cluster-name label, the node-push annotation, and a non-empty
	// payload.
	pushedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-kubeconfig",
			Namespace: ns.Name,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
			Annotations: map[string]string{
				"controllers.cluster.x-k8s.io/kubeconfig-source": "node-push",
			},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{
			"value": []byte("apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"),
		},
	}
	g.Expect(c.Create(ctx, pushedSecret)).To(Succeed())

	// (3) Watch should wake KCP and the condition should flip to True.
	g.Eventually(func() corev1.ConditionStatus {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return ""
		}
		cond := conditions.Get(got, controlplanev1beta2.KubeconfigReadyCondition)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, 30*time.Second, time.Second).Should(Equal(corev1.ConditionTrue))

	g.Expect(c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got)).To(Succeed())
	cond = conditions.Get(got, controlplanev1beta2.KubeconfigReadyCondition)
	g.Expect(cond.Reason).To(Equal(controlplanev1beta2.KubeconfigReadyReason))
	g.Expect(got.Status.LastNodePushObserved).To(BeNil(), "Secret-present must clear LastNodePushObserved")
	g.Expect(got.Status.Initialized).To(BeTrue(),
		"Initialized must flip to true when the kubeconfig Secret is observed (no NodeRef required)")
}

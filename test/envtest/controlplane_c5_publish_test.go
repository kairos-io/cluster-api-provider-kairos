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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// TestC5_UnsafeContractNeutralizedEmptyInPlace proves the maintainer decision at
// the apiserver layer: an unsafe (tokenFile) contract kubeconfig is emptied IN
// PLACE via a resourceVersion-preconditioned UPDATE — the Secret object is
// retained, its value cleared — and KubeconfigReady goes
// False(WaitingForNodePush) so recovery fires. The controller issues NO delete
// (it holds no secrets:delete grant).
func TestC5_UnsafeContractNeutralizedEmptyInPlace(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping envtest in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "c5-neutralize-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	clusterName := "c5-neutralize-cluster"
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns.Name},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: controlplanev1beta2.GroupVersion.Group,
				Kind:     "KairosControlPlane",
				Name:     clusterName + "-kcp",
			},
			ControlPlaneEndpoint: testControlPlaneEndpoint(),
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
			KairosConfigTemplate: controlplanev1beta2.KairosConfigTemplateReference{Name: "missing-on-purpose"},
		},
	}
	g.Expect(c.Create(ctx, kcp)).To(Succeed())

	// Pre-create a control-plane Machine so reconcileMachines does not try to
	// createControlPlaneMachine (which needs infra/config templates absent in
	// envtest) and error out before reaching the publish path — same pattern as
	// the KD-3b node-push test.
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcp.Name + "-0",
			Namespace: ns.Name,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         clusterName,
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       kcp.Name,
				UID:        kcp.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName:       clusterName,
			Bootstrap:         clusterv1.Bootstrap{DataSecretName: ptr.To("placeholder")},
			InfrastructureRef: testMachineInfraRef("infra"),
			Version:           "v1.30.0+k0s.0",
		},
	}
	g.Expect(c.Create(ctx, machine)).To(Succeed())

	// A node pushes an UNSAFE kubeconfig (tokenFile — the A1 shape).
	pushed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-kubeconfig",
			Namespace: ns.Name,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": unsafeWorkloadKubeconfig(t)},
	}
	g.Expect(c.Create(ctx, pushed)).To(Succeed())

	// The contract Secret must be emptied in place with the recovery condition.
	g.Eventually(func() string {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return "ERR"
		}
		cond := conditions.Get(got, controlplanev1beta2.KubeconfigReadyCondition)
		if cond == nil {
			return ""
		}
		if cond.Status != corev1.ConditionFalse {
			return "not-false"
		}
		return cond.Reason
	}, 30*time.Second, time.Second).Should(Equal(controlplanev1beta2.WaitingForNodePushReason))

	// The Secret is RETAINED (never deleted), and its value is cleared.
	g.Eventually(func() int {
		got := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: clusterName + "-kubeconfig", Namespace: ns.Name}, got); err != nil {
			return -1
		}
		return len(got.Data["value"])
	}, 30*time.Second, time.Second).Should(Equal(0), "unsafe content must be emptied in place, not deleted")

	// Prove non-deletion explicitly: the object still exists.
	final := &corev1.Secret{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: clusterName + "-kubeconfig", Namespace: ns.Name}, final)).To(Succeed(),
		"the contract Secret must still exist after neutralization (empty-in-place, no delete)")
	g.Expect(apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: clusterName + "-kubeconfig", Namespace: ns.Name}, &corev1.Secret{}))).To(BeFalse())
}

// TestC5_SF1_ForgedOwnerPlantNeutralizedAndRepublished proves ADR 0011 SF1
// (option b) at the apiserver layer: a contract Secret that is neither KCP-owned
// nor adoptable — here a tokenFile plant carrying a FORGED controller ownerRef —
// is neutralized in place (never deleted) and then republished from clean staging
// because Cluster.spec.controlPlaneRef points at this KCP. The forged owner is
// dropped; the KCP takes ownership; KubeconfigReady goes True. This is the A1/A2
// window closure for a same-namespace Secret writer (T4).
func TestC5_SF1_ForgedOwnerPlantNeutralizedAndRepublished(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping envtest in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "c5-sf1-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	clusterName := "c5-sf1-cluster"
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns.Name},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: controlplanev1beta2.GroupVersion.Group,
				Kind:     "KairosControlPlane",
				Name:     clusterName + "-kcp",
			},
			ControlPlaneEndpoint: testControlPlaneEndpoint(),
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
			KairosConfigTemplate: controlplanev1beta2.KairosConfigTemplateReference{Name: "missing-on-purpose"},
		},
	}
	g.Expect(c.Create(ctx, kcp)).To(Succeed())

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcp.Name + "-0",
			Namespace: ns.Name,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         clusterName,
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       kcp.Name,
				UID:        kcp.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName:       clusterName,
			Bootstrap:         clusterv1.Bootstrap{DataSecretName: ptr.To("placeholder")},
			InfrastructureRef: testMachineInfraRef("infra"),
			Version:           "v1.30.0+k0s.0",
		},
	}
	g.Expect(c.Create(ctx, machine)).To(Succeed())

	// Clean staging Secret, KCP-owned, so the SF1 branch can republish.
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-kubeconfig-push",
			Namespace: ns.Name,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       kcp.Name,
				UID:        kcp.UID,
				Controller: ptr.To(true),
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"value": validWorkloadKubeconfig(t)},
	}
	g.Expect(c.Create(ctx, staging)).To(Succeed())

	// A plant: unsafe (tokenFile) content under a FORGED KairosControlPlane
	// controller ownerRef (a UID that is not this KCP), so it is neither KCP-owned
	// nor adoptable.
	plant := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-kubeconfig",
			Namespace: ns.Name,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       kcp.Name,
				UID:        types.UID("00000000-0000-0000-0000-forgedforged"),
				Controller: ptr.To(true),
			}},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": unsafeWorkloadKubeconfig(t)},
	}
	g.Expect(c.Create(ctx, plant)).To(Succeed())

	// The contract must end up republished (KubeconfigReady=True) with the KCP as
	// controller owner, and the unsafe tokenFile content replaced.
	g.Eventually(func() string {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, got); err != nil {
			return "ERR"
		}
		cond := conditions.Get(got, controlplanev1beta2.KubeconfigReadyCondition)
		if cond == nil {
			return ""
		}
		if cond.Status != corev1.ConditionTrue {
			return "false:" + cond.Reason
		}
		return cond.Reason
	}, 30*time.Second, time.Second).Should(Equal(controlplanev1beta2.KubeconfigReadyReason))

	got := &corev1.Secret{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: clusterName + "-kubeconfig", Namespace: ns.Name}, got)).To(Succeed(),
		"the contract Secret is retained (empty-in-place then republished, never deleted)")
	g.Expect(got.Data["value"]).NotTo(BeEmpty(), "republished from clean staging")
	g.Expect(metav1.IsControlledBy(got, kcp)).To(BeTrue(), "the KCP takes ownership, dropping the forged owner")
}

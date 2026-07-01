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
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// haFixture creates a Cluster + KCP (replicas=3) in a fresh namespace and waits
// for the finalizer, giving each HA test an isolated starting point. The infra
// template is intentionally not installed: the controller's HA bookkeeping
// (join-token Secret, role assignment on the KairosConfig) runs BEFORE infra
// cloning, so these tests assert that bookkeeping without needing CAPD CRDs.
func haFixture(t *testing.T, ctx context.Context, c client.Client, nsName, clusterName, kcpName, distribution string) {
	t.Helper()
	g := NewWithT(t)

	g.Expect(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})).To(Succeed())

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: nsName},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: &corev1.ObjectReference{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       kcpName,
				Namespace:  nsName,
			},
		},
	}
	g.Expect(c.Create(ctx, cluster)).To(Succeed())

	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcpName,
			Namespace: nsName,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas:     ptr.To(int32(3)),
			Version:      "v1.30.0+k0s.0",
			Distribution: distribution,
			HA: &controlplanev1beta2.HAConfig{
				VIP: &controlplanev1beta2.KubeVIPConfig{Address: "192.168.1.240", Interface: "eth0", Mode: controlplanev1beta2.KubeVIPModeARP},
			},
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "not-installed",
					Namespace:  nsName,
				},
			},
			// No KairosConfigTemplate: createControlPlaneMachine builds the config
			// inline, so the init KairosConfig is created (with role=init) before
			// infra cloning fails on the missing DockerMachineTemplate CRD.
		},
	}
	g.Expect(c.Create(ctx, kcp)).To(Succeed())

	g.Eventually(func() bool {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcpName, Namespace: nsName}, got); err != nil {
			return false
		}
		for _, f := range got.Finalizers {
			if f == controlplanev1beta2.KairosControlPlaneFinalizer {
				return true
			}
		}
		return false
	}, 15*time.Second, 500*time.Millisecond).Should(BeTrue(), "KCP finalizer must be added")
}

// TestHA_JoinTokenSecret_OwnerRefLabelAndNotLogged asserts (ADR 0005 Phase 3):
// the per-cluster join-token Secret is created, owner-ref'd to the KCP, labeled
// with the cluster-name + secret-type labels, and — for k3s — carries a
// controller-generated token. (No-secrets-in-logs is enforced by the resolver
// code path; here we assert the Secret is present and correctly owned/labeled.)
func TestHA_JoinTokenSecret_OwnerRefLabelAndNotLogged(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	const (
		nsName      = "ha-token"
		clusterName = "ha-token-cluster"
		kcpName     = "ha-token-kcp"
	)
	haFixture(t, ctx, c, nsName, clusterName, kcpName, "k3s")

	secretName := bootstrapv1beta2.ControlPlaneJoinTokenSecretName(clusterName)
	secret := &corev1.Secret{}
	g.Eventually(func() error {
		return c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: nsName}, secret)
	}, 15*time.Second, 500*time.Millisecond).Should(Succeed(), "join-token Secret must be created")

	// Owner-ref'd to the KCP (TOKEN-INV: cascades on KCP delete).
	g.Expect(secret.OwnerReferences).To(HaveLen(1))
	g.Expect(secret.OwnerReferences[0].Kind).To(Equal("KairosControlPlane"))
	g.Expect(secret.OwnerReferences[0].Name).To(Equal(kcpName))

	// Label-filtered watch predicate depends on both labels (KD-15).
	g.Expect(secret.Labels[clusterv1.ClusterNameLabel]).To(Equal(clusterName))
	g.Expect(secret.Labels[bootstrapv1beta2.ControlPlaneJoinTokenSecretTypeLabel]).
		To(Equal(bootstrapv1beta2.ControlPlaneJoinTokenSecretTypeValue))

	// k3s: the controller generates the shared server token up front.
	g.Expect(secret.Data[bootstrapv1beta2.ControlPlaneJoinTokenSecretDataKey]).ToNot(BeEmpty())

	// The token must remain stable across subsequent reconciles (never regenerated).
	first := secret.Data[bootstrapv1beta2.ControlPlaneJoinTokenSecretDataKey]
	g.Consistently(func() []byte {
		s := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: nsName}, s); err != nil {
			return nil
		}
		return s.Data[bootstrapv1beta2.ControlPlaneJoinTokenSecretDataKey]
	}, 3*time.Second, 500*time.Millisecond).Should(Equal(first), "k3s token must not be regenerated")
}

// TestHA_K0sJoinTokenSecret_CreatedEmpty asserts the k0s join-token Secret is
// created empty (the init node fills it via the node-push channel), owner-ref'd
// and labeled the same way.
func TestHA_K0sJoinTokenSecret_CreatedEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	const (
		nsName      = "ha-k0s-token"
		clusterName = "ha-k0s-token-cluster"
		kcpName     = "ha-k0s-token-kcp"
	)
	haFixture(t, ctx, c, nsName, clusterName, kcpName, "k0s")

	secretName := bootstrapv1beta2.ControlPlaneJoinTokenSecretName(clusterName)
	secret := &corev1.Secret{}
	g.Eventually(func() error {
		return c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: nsName}, secret)
	}, 15*time.Second, 500*time.Millisecond).Should(Succeed())

	g.Expect(secret.OwnerReferences).To(HaveLen(1))
	g.Expect(secret.Labels[bootstrapv1beta2.ControlPlaneJoinTokenSecretTypeLabel]).
		To(Equal(bootstrapv1beta2.ControlPlaneJoinTokenSecretTypeValue))
	// k0s: empty until the init node pushes it.
	g.Expect(secret.Data[bootstrapv1beta2.ControlPlaneJoinTokenSecretDataKey]).To(BeEmpty())
}

// TestHA_RoleAssignment_InitFirstThenJoin asserts the sequencing gate + role
// assignment (ADR 0005 Phase 3): the first CP KairosConfig is created with
// role=init, and NO join KairosConfig is created until the init machine is
// joinable (NodeRef + KubeconfigReady + k0s token present). We drive that state
// by hand (pre-create the init Machine with a NodeRef, push the kubeconfig
// Secret, populate the k0s token) and assert a join KairosConfig then appears.
func TestHA_RoleAssignment_InitFirstThenJoin(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	const (
		nsName      = "ha-seq"
		clusterName = "ha-seq-cluster"
		kcpName     = "ha-seq-kcp"
	)
	haFixture(t, ctx, c, nsName, clusterName, kcpName, "k0s")

	// The controller creates the KairosConfig for machine-0 (init) before infra
	// cloning fails, so it should appear with role=init.
	initCfgName := kcpName + "-0"
	initCfg := &bootstrapv1beta2.KairosConfig{}
	g.Eventually(func() (bootstrapv1beta2.ControlPlaneRole, error) {
		if err := c.Get(ctx, types.NamespacedName{Name: initCfgName, Namespace: nsName}, initCfg); err != nil {
			return "", err
		}
		return initCfg.Spec.ControlPlaneRole, nil
	}, 15*time.Second, 500*time.Millisecond).Should(Equal(bootstrapv1beta2.ControlPlaneRoleInit))
	g.Expect(initCfg.Spec.SingleNode).To(BeFalse())
	// VIP copied down onto the config.
	g.Expect(initCfg.Spec.ControlPlaneVIP).ToNot(BeNil())
	g.Expect(initCfg.Spec.ControlPlaneVIP.Address).To(Equal("192.168.1.240"))

	// No join config yet: the init machine is not joinable (no NodeRef, no
	// kubeconfig, no k0s token). Assert machine-1 config does NOT appear.
	joinCfgName := kcpName + "-1"
	g.Consistently(func() bool {
		err := c.Get(ctx, types.NamespacedName{Name: joinCfgName, Namespace: nsName}, &bootstrapv1beta2.KairosConfig{})
		return err != nil // still not created
	}, 4*time.Second, 500*time.Millisecond).Should(BeTrue(), "join config must not be created before init is joinable")

	// Now make the init machine joinable:
	//  (1) pre-create the init Machine with a NodeRef and Running phase,
	//  (2) push the kubeconfig Secret (opens KubeconfigReady),
	//  (3) populate the k0s join-token Secret (init-node push simulation).
	initMachine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcpName + "-0",
			Namespace: nsName,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         clusterName,
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(getKCP(g, ctx, c, nsName, kcpName), controlplanev1beta2.GroupVersion.WithKind("KairosControlPlane")),
			},
		},
		Spec: clusterv1.MachineSpec{ClusterName: clusterName, Version: ptr.To("v1.30.0+k0s.0")},
	}
	g.Expect(c.Create(ctx, initMachine)).To(Succeed())
	initMachine.Status.NodeRef = &corev1.ObjectReference{Kind: "Node", Name: "init-node"}
	initMachine.Status.Phase = string(clusterv1.MachinePhaseRunning)
	g.Expect(c.Status().Update(ctx, initMachine)).To(Succeed())

	// Push kubeconfig Secret (KD-3b) so KubeconfigReady becomes True.
	kubeconfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-kubeconfig",
			Namespace: nsName,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": []byte("apiVersion: v1\nkind: Config\n")},
	}
	g.Expect(c.Create(ctx, kubeconfigSecret)).To(Succeed())

	// Populate the k0s join-token Secret (simulate the init node's push).
	tokenSecret := &corev1.Secret{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: bootstrapv1beta2.ControlPlaneJoinTokenSecretName(clusterName), Namespace: nsName}, tokenSecret)).To(Succeed())
	if tokenSecret.Data == nil {
		tokenSecret.Data = map[string][]byte{}
	}
	tokenSecret.Data[bootstrapv1beta2.ControlPlaneJoinTokenSecretDataKey] = []byte("k0s-controller-join-token")
	g.Expect(c.Update(ctx, tokenSecret)).To(Succeed())

	// The gate should now open and the controller should create the join config.
	g.Eventually(func() (bootstrapv1beta2.ControlPlaneRole, error) {
		cfg := &bootstrapv1beta2.KairosConfig{}
		if err := c.Get(ctx, types.NamespacedName{Name: joinCfgName, Namespace: nsName}, cfg); err != nil {
			return "", err
		}
		return cfg.Spec.ControlPlaneRole, nil
	}, 20*time.Second, 500*time.Millisecond).Should(Equal(bootstrapv1beta2.ControlPlaneRoleJoin),
		"join config must be created once the init machine is joinable")
}

// TestHA_JoinTokenWatch_ReReconcilesJoinConfig asserts BLOCKER-2: writing the
// token into the join-token Secret re-reconciles a waiting join KairosConfig via
// the label-filtered watch. We scaffold a full join config (owning Machine +
// cluster) whose bootstrap render is blocked only on the empty k0s token, so it
// stays NOT-ready; populating the token then lets the render succeed and the
// config becomes Ready — the watch is what wakes it promptly.
func TestHA_JoinTokenWatch_ReReconcilesJoinConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	const (
		nsName      = "ha-watch"
		clusterName = "ha-watch-cluster"
		kcpName     = "ha-watch-kcp"
	)
	haFixture(t, ctx, c, nsName, clusterName, kcpName, "k0s")

	tokenSecretName := bootstrapv1beta2.ControlPlaneJoinTokenSecretName(clusterName)
	g.Eventually(func() error {
		return c.Get(ctx, types.NamespacedName{Name: tokenSecretName, Namespace: nsName}, &corev1.Secret{})
	}, 15*time.Second, 500*time.Millisecond).Should(Succeed())

	// A join KairosConfig owned by a Machine (so the bootstrap reconcile
	// progresses past owner/cluster lookups to token resolution). The token
	// Secret is empty, so the render returns errTokenNotReady → not Ready.
	joinCfg := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pending-join",
			Namespace: nsName,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:              "control-plane",
			Distribution:      "k0s",
			KubernetesVersion: "v1.30.0+k0s.0",
			ControlPlaneRole:  bootstrapv1beta2.ControlPlaneRoleJoin,
			ControlPlaneJoinTokenSecretRef: &bootstrapv1beta2.WorkerTokenSecretReference{
				Name: tokenSecretName, Namespace: nsName, Key: bootstrapv1beta2.ControlPlaneJoinTokenSecretDataKey,
			},
			UserName: "kairos",
		},
	}
	g.Expect(c.Create(ctx, joinCfg)).To(Succeed())

	// Owning Machine (owner-ref + cluster-name label) so GetOwnerMachine resolves.
	joinMachine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pending-join-machine",
			Namespace: nsName,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         clusterName,
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(joinCfg, bootstrapv1beta2.GroupVersion.WithKind("KairosConfig")),
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: clusterName,
			Version:     ptr.To("v1.30.0+k0s.0"),
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: &corev1.ObjectReference{
					APIVersion: bootstrapv1beta2.GroupVersion.String(),
					Kind:       "KairosConfig",
					Name:       "pending-join",
					Namespace:  nsName,
				},
			},
		},
	}
	g.Expect(c.Create(ctx, joinMachine)).To(Succeed())
	// Set the KairosConfig's owner to the Machine (GetOwnerMachine walks owner refs).
	g.Eventually(func() error {
		cur := &bootstrapv1beta2.KairosConfig{}
		if err := c.Get(ctx, types.NamespacedName{Name: "pending-join", Namespace: nsName}, cur); err != nil {
			return err
		}
		cur.OwnerReferences = []metav1.OwnerReference{
			*metav1.NewControllerRef(joinMachine, clusterv1.GroupVersion.WithKind("Machine")),
		}
		return c.Update(ctx, cur)
	}, 10*time.Second, 200*time.Millisecond).Should(Succeed())

	// With the empty token, the config must NOT become Ready.
	g.Consistently(func() bool {
		got := &bootstrapv1beta2.KairosConfig{}
		if err := c.Get(ctx, types.NamespacedName{Name: "pending-join", Namespace: nsName}, got); err != nil {
			return false
		}
		return !got.Status.Ready
	}, 4*time.Second, 500*time.Millisecond).Should(BeTrue(), "join config must not be Ready with an empty token")

	// Populate the token — the label-filtered watch wakes the join config and the
	// render now succeeds, so it becomes Ready.
	tokenSecret := &corev1.Secret{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: tokenSecretName, Namespace: nsName}, tokenSecret)).To(Succeed())
	if tokenSecret.Data == nil {
		tokenSecret.Data = map[string][]byte{}
	}
	tokenSecret.Data[bootstrapv1beta2.ControlPlaneJoinTokenSecretDataKey] = []byte("k0s-controller-join-token")
	g.Expect(c.Update(ctx, tokenSecret)).To(Succeed())

	g.Eventually(func() bool {
		got := &bootstrapv1beta2.KairosConfig{}
		if err := c.Get(ctx, types.NamespacedName{Name: "pending-join", Namespace: nsName}, got); err != nil {
			return false
		}
		return got.Status.Ready
	}, 20*time.Second, 500*time.Millisecond).Should(BeTrue(),
		"join config must become Ready once the token lands (woken by the token watch)")
}

// TestHA_StatusMath_N3 asserts the v1beta2 replica status math across a 3-node
// HA control plane: replicas, readyReplicas, availableReplicas, and the
// ControlPlaneJoined condition progress as machines gain NodeRefs.
func TestHA_StatusMath_N3(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	const (
		nsName      = "ha-status"
		clusterName = "ha-status-cluster"
		kcpName     = "ha-status-kcp"
	)
	haFixture(t, ctx, c, nsName, clusterName, kcpName, "k0s")
	kcp := getKCP(g, ctx, c, nsName, kcpName)

	// Pre-create 3 CP Machines owned by the KCP, all Running with NodeRefs.
	for i := 0; i < 3; i++ {
		m := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      kcpName + "-" + string(rune('0'+i)),
				Namespace: nsName,
				Labels: map[string]string{
					clusterv1.ClusterNameLabel:         clusterName,
					clusterv1.MachineControlPlaneLabel: "",
				},
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(kcp, controlplanev1beta2.GroupVersion.WithKind("KairosControlPlane")),
				},
			},
			Spec: clusterv1.MachineSpec{ClusterName: clusterName, Version: ptr.To("v1.30.0+k0s.0")},
		}
		g.Expect(c.Create(ctx, m)).To(Succeed())
		m.Status.NodeRef = &corev1.ObjectReference{Kind: "Node", Name: "node-" + string(rune('0'+i))}
		m.Status.Phase = string(clusterv1.MachinePhaseRunning)
		g.Expect(c.Status().Update(ctx, m)).To(Succeed())
	}

	// Nudge a reconcile by touching the KCP annotations.
	touchKCP(g, ctx, c, nsName, kcpName)

	g.Eventually(func() bool {
		got := getKCP(g, ctx, c, nsName, kcpName)
		return got.Status.Replicas == 3 &&
			got.Status.ReadyReplicas == 3 &&
			got.Status.AvailableReplicas == 3
	}, 20*time.Second, 500*time.Millisecond).Should(BeTrue(), "N=3 status math: replicas/ready/available all 3")

	// ControlPlaneJoined should be True once all members joined.
	g.Eventually(func() bool {
		got := getKCP(g, ctx, c, nsName, kcpName)
		for _, cond := range got.Status.Conditions {
			if string(cond.Type) == controlplanev1beta2.ControlPlaneJoinedCondition {
				return cond.Status == corev1.ConditionTrue
			}
		}
		return false
	}, 20*time.Second, 500*time.Millisecond).Should(BeTrue(), "ControlPlaneJoined must be True at N=3")
}

// --- helpers ---

func getKCP(g *WithT, ctx context.Context, c client.Client, ns, name string) *controlplanev1beta2.KairosControlPlane {
	kcp := &controlplanev1beta2.KairosControlPlane{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, kcp)).To(Succeed())
	return kcp
}

func touchKCP(g *WithT, ctx context.Context, c client.Client, ns, name string) {
	// Retry on conflict: the controller reconciles the KCP concurrently.
	g.Eventually(func() error {
		kcp := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, kcp); err != nil {
			return err
		}
		if kcp.Annotations == nil {
			kcp.Annotations = map[string]string{}
		}
		kcp.Annotations["ha-test/nudge"] = time.Now().Format(time.RFC3339Nano)
		return c.Update(ctx, kcp)
	}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
}

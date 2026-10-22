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
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/controllers/controlplane"
)

// haFixture creates a Cluster + KCP (replicas=3) in a fresh namespace and waits
// for the finalizer, giving each HA test an isolated starting point. The infra
// template is intentionally not installed: the controller's HA bookkeeping
// (join-token Secret, role assignment on the KairosConfig) runs BEFORE infra
// cloning, so these tests assert that bookkeeping without needing CAPD CRDs.
// The failed create deletes its KairosConfig again (ADR 0015 PR-4); tests that
// inspect it read it through recordKairosConfigs.
func haFixture(t *testing.T, ctx context.Context, c client.Client, nsName, clusterName, kcpName, distribution string) {
	t.Helper()
	g := NewWithT(t)

	g.Expect(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})).To(Succeed())

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: nsName},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: controlplanev1beta2.GroupVersion.Group,
				Kind:     "KairosControlPlane",
				Name:     kcpName,
			},
			ControlPlaneEndpoint: testControlPlaneEndpoint(),
		},
	}
	g.Expect(c.Create(ctx, cluster)).To(Succeed())
	// No infrastructure provider runs in envtest, so stand in for CAPI's cluster
	// controller: the bootstrap controller renders nothing until this is set.
	g.Expect(markClusterInfrastructureProvisioned(ctx, c, cluster)).To(Succeed())

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
			// infra cloning fails on the missing DockerMachineTemplate CRD, and
			// is then deleted by the failed create's cleanup.
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
// created empty, owner-ref'd and labeled the same way. The KCP fills it when it
// admits the first joiner (ADR 0013).
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
	// k0s: empty until the KCP admits a joiner.
	g.Expect(secret.Data[bootstrapv1beta2.ControlPlaneJoinTokenSecretDataKey]).To(BeEmpty())
}

// TestHA_RoleAssignment_InitFirstThenJoin asserts the sequencing gate + role
// assignment (ADR 0005 Phase 3 + Phase-4 §E.1): the first CP KairosConfig is
// created with role=init, and NO join KairosConfig is created until the init
// machine is joinable (NodeRef + KubeconfigReady + the init node has reported a
// healthy voting etcd member) and the KCP has minted a k0s controller-join token
// (ADR 0013). We drive that state by hand (pre-create the init Machine with a
// NodeRef and the live apiserver's address, push the kubeconfig Secret, write
// the init node's etcd-status report) and assert a join KairosConfig then
// appears.
func TestHA_RoleAssignment_InitFirstThenJoin(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	ctx, c, cfg, _, teardown := startKCPEnvtest(t, withEnvtestAsWorkloadCluster(t))
	defer teardown()

	const (
		nsName      = "ha-seq"
		clusterName = "ha-seq-cluster"
		kcpName     = "ha-seq-kcp"
	)
	configs := recordKairosConfigs(t, ctx, cfg, nsName)
	haFixture(t, ctx, c, nsName, clusterName, kcpName, "k0s")
	apiAddr := envtestAPIServerAddress(t, ctx, envtestWorkloadClient(t, cfg))

	// The controller creates the init KairosConfig before infra cloning fails.
	// The failed attempt deletes it again (ADR 0015 PR-4), so read it from the
	// record of what was created.
	g.Eventually(func() []*bootstrapv1beta2.KairosConfig {
		return configs.withRole(bootstrapv1beta2.ControlPlaneRoleInit)
	}, 15*time.Second, 500*time.Millisecond).ShouldNot(BeEmpty())
	initCfg := configs.withRole(bootstrapv1beta2.ControlPlaneRoleInit)[0]
	g.Expect(initCfg.Name).To(MatchRegexp(`^` + kcpName + `-[a-z0-9]{5}$`))
	g.Expect(initCfg.Spec.SingleNode).To(BeFalse())
	// VIP copied down onto the config.
	g.Expect(initCfg.Spec.ControlPlaneVIP).ToNot(BeNil())
	g.Expect(initCfg.Spec.ControlPlaneVIP.Address).To(Equal("192.168.1.240"))
	// The init config carries no join-token ID: only joiners consume a token.
	g.Expect(initCfg.Annotations).ToNot(HaveKey(bootstrapv1beta2.K0sControllerJoinTokenIDAnnotation))
	// A failed create leaves nothing behind, on a real apiserver too.
	g.Eventually(func() bool { return configs.wasDeleted(initCfg.Name) }, 15*time.Second, 200*time.Millisecond).
		Should(BeTrue(), "the KairosConfig of a failed create must be deleted")
	// Every retry uses a new name.
	g.Eventually(func() int { return len(configs.withRole(bootstrapv1beta2.ControlPlaneRoleInit)) }, 30*time.Second, 500*time.Millisecond).
		Should(BeNumerically(">=", 2))
	seen := map[string]bool{}
	for _, kc := range configs.withRole(bootstrapv1beta2.ControlPlaneRoleInit) {
		g.Expect(seen).NotTo(HaveKey(kc.Name), "a retried create reused a name")
		seen[kc.Name] = true
	}

	// No join config yet: the init machine is not joinable (no NodeRef, no
	// kubeconfig, no etcd report).
	g.Consistently(func() []*bootstrapv1beta2.KairosConfig {
		return configs.withRole(bootstrapv1beta2.ControlPlaneRoleJoin)
	}, 4*time.Second, 500*time.Millisecond).Should(BeEmpty(), "join config must not be created before init is joinable")

	// Make the init machine joinable: NodeRef + Running, the address of the live
	// apiserver, the kubeconfig Secret (opens KubeconfigReady), and a healthy
	// voting etcd report (ADR 0005 §E.1). Nothing fills the join-token Secret by
	// hand: the KCP mints the token.
	makeK0sInitJoinable(t, ctx, c, nsName, clusterName, kcpName, k0sWorkloadKubeconfig(t, cfg), apiAddr)

	// The gate should now open and the controller should create a join config.
	g.Eventually(func() []*bootstrapv1beta2.KairosConfig {
		return configs.withRole(bootstrapv1beta2.ControlPlaneRoleJoin)
	}, 30*time.Second, 500*time.Millisecond).ShouldNot(BeEmpty(),
		"join config must be created once the init machine is joinable")
	joinCfg := configs.withRole(bootstrapv1beta2.ControlPlaneRoleJoin)[0]
	g.Expect(joinCfg.Name).To(MatchRegexp(`^` + kcpName + `-[a-z0-9]{5}$`))
	g.Expect(seen).NotTo(HaveKey(joinCfg.Name))

	// ADR 0015 PR-2: the joiner was admitted only after the KCP pinned the
	// identity of the workload cluster it admitted it into.
	g.Expect(getKCP(g, ctx, c, nsName, kcpName).Annotations).To(HaveKeyWithValue(
		controlplane.WorkloadClusterUIDAnnotation, envtestKubeSystemUID(t, ctx, envtestWorkloadClient(t, cfg))))
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
			Version:     "v1.30.0+k0s.0",
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: bootstrapv1beta2.GroupVersion.Group,
					Kind:     "KairosConfig",
					Name:     "pending-join",
				},
			},
			InfrastructureRef: testMachineInfraRef("infra"),
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
			Spec: clusterv1.MachineSpec{
				ClusterName:       clusterName,
				Version:           "v1.30.0+k0s.0",
				Bootstrap:         clusterv1.Bootstrap{DataSecretName: ptr.To("placeholder")},
				InfrastructureRef: testMachineInfraRef("infra"),
			},
		}
		g.Expect(c.Create(ctx, m)).To(Succeed())
		m.Status.NodeRef = clusterv1.MachineNodeReference{Name: "node-" + string(rune('0'+i))}
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

// envtestKubeSystemUID returns the UID of kube-system in the envtest
// apiserver, which plays the workload cluster in these tests.
func envtestKubeSystemUID(t *testing.T, ctx context.Context, wc client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{}
	NewWithT(t).Expect(wc.Get(ctx, types.NamespacedName{Name: metav1.NamespaceSystem}, ns)).To(Succeed())
	return string(ns.UID)
}

// pinEnvtestWorkloadIdentity records the workload identity pin (ADR 0015
// PR-2) on the KCP, as the KCP does at first contact, for tests whose
// cluster is already past that point.
func pinEnvtestWorkloadIdentity(t *testing.T, ctx context.Context, c, wc client.Client, ns, name string) {
	t.Helper()
	uid := envtestKubeSystemUID(t, ctx, wc)
	NewWithT(t).Eventually(func() error {
		kcp := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, kcp); err != nil {
			return err
		}
		if kcp.Annotations == nil {
			kcp.Annotations = map[string]string{}
		}
		kcp.Annotations[controlplane.WorkloadClusterUIDAnnotation] = uid
		return c.Update(ctx, kcp)
	}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
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

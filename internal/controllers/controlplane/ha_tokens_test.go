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
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

func haTestScheme(g *WithT) *runtime.Scheme {
	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	return scheme
}

func machineAt(name string) *clusterv1.Machine {
	return &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
}

// TestControlPlaneRoleForNewMachine is the role-assignment table (ADR 0005
// Phase 3 §1), asserting observed output (the returned role) against the
// (desiredReplicas, existing-machine-count) inputs.
func TestControlPlaneRoleForNewMachine(t *testing.T) {
	r := &KairosControlPlaneReconciler{}

	tests := []struct {
		name            string
		desiredReplicas int32
		existing        []*clusterv1.Machine
		want            bootstrapv1beta2.ControlPlaneRole
	}{
		{"replicas 1 -> single", 1, nil, bootstrapv1beta2.ControlPlaneRoleSingle},
		{"replicas 1 with existing -> single", 1, []*clusterv1.Machine{machineAt("m0")}, bootstrapv1beta2.ControlPlaneRoleSingle},
		{"replicas 3 empty -> init", 3, nil, bootstrapv1beta2.ControlPlaneRoleInit},
		{"replicas 3 with init -> join", 3, []*clusterv1.Machine{machineAt("m0")}, bootstrapv1beta2.ControlPlaneRoleJoin},
		{"replicas 3 with two -> join", 3, []*clusterv1.Machine{machineAt("m0"), machineAt("m1")}, bootstrapv1beta2.ControlPlaneRoleJoin},
		{"replicas 5 empty -> init", 5, nil, bootstrapv1beta2.ControlPlaneRoleInit},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(r.controlPlaneRoleForNewMachine(tc.desiredReplicas, tc.existing)).To(Equal(tc.want))
		})
	}
}

// TestEnsureJoinTokenSecret_K3sIdempotent asserts the k3s shared server token is
// generated once and never regenerated on subsequent reconciles, and that the
// Secret is owner-ref'd to the KCP and carries the cluster-name + type labels
// (TOKEN-INV / KD-15).
func TestEnsureJoinTokenSecret_K3sIdempotent(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "kcp", Namespace: "default", UID: "kcp-uid"},
		Spec:       controlplanev1beta2.KairosControlPlaneSpec{Distribution: "k3s", Replicas: ptr.To(int32(3))},
	}
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.ensureJoinTokenSecret(context.Background(), log.Log, kcp, cluster)).To(Succeed())

	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: joinTokenSecretName("c"), Namespace: "default"}
	g.Expect(c.Get(context.Background(), key, secret)).To(Succeed())

	first := secret.Data[joinTokenSecretDataKey]
	g.Expect(first).ToNot(BeEmpty(), "k3s token must be generated")
	g.Expect(secret.Labels[clusterv1.ClusterNameLabel]).To(Equal("c"))
	g.Expect(secret.Labels[controlPlaneJoinTokenSecretTypeLabel]).To(Equal(controlPlaneJoinTokenSecretTypeValue))
	g.Expect(secret.OwnerReferences).To(HaveLen(1))
	g.Expect(secret.OwnerReferences[0].Kind).To(Equal("KairosControlPlane"))
	g.Expect(secret.OwnerReferences[0].UID).To(Equal(kcp.UID))

	// Second call must NOT regenerate the token.
	g.Expect(r.ensureJoinTokenSecret(context.Background(), log.Log, kcp, cluster)).To(Succeed())
	g.Expect(c.Get(context.Background(), key, secret)).To(Succeed())
	g.Expect(secret.Data[joinTokenSecretDataKey]).To(Equal(first), "k3s token must not be regenerated")
}

// TestEnsureJoinTokenSecret_K0sEmpty asserts the k0s join-token Secret is created
// empty — the init node fills it over the node-push channel, not the controller.
func TestEnsureJoinTokenSecret_K0sEmpty(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "kcp", Namespace: "default", UID: "kcp-uid"},
		Spec:       controlplanev1beta2.KairosControlPlaneSpec{Distribution: "k0s", Replicas: ptr.To(int32(3))},
	}
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.ensureJoinTokenSecret(context.Background(), log.Log, kcp, cluster)).To(Succeed())

	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: joinTokenSecretName("c"), Namespace: "default"}
	g.Expect(c.Get(context.Background(), key, secret)).To(Succeed())
	g.Expect(secret.Data[joinTokenSecretDataKey]).To(BeEmpty(), "k0s token is filled by the init node, not the controller")

	// joinTokenSecretValue reflects the empty state.
	val, err := r.joinTokenSecretValue(context.Background(), cluster)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(val).To(BeEmpty())
}

// TestGenerateJoinToken_StrongAndUnique asserts tokens are non-empty, URL-safe,
// and distinct across calls (crypto/rand entropy).
func TestGenerateJoinToken_StrongAndUnique(t *testing.T) {
	g := NewWithT(t)
	a, err := generateJoinToken()
	g.Expect(err).ToNot(HaveOccurred())
	b, err := generateJoinToken()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(a).ToNot(BeEmpty())
	g.Expect(a).ToNot(Equal(b))
	// base64url(32 bytes) without padding => 43 chars, only [A-Za-z0-9_-].
	g.Expect(a).To(MatchRegexp(`^[A-Za-z0-9_-]{43}$`))
}

// TestApplyControlPlaneHASpec_TokenRefAndVIP asserts the HA spec stamping wires
// the distribution-appropriate *SecretRef (never inline) and copies the VIP.
func TestApplyControlPlaneHASpec_TokenRefAndVIP(t *testing.T) {
	g := NewWithT(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}}
	vip := &controlplanev1beta2.KubeVIPConfig{Address: "192.168.1.240", Interface: "eth0", Mode: controlplanev1beta2.KubeVIPModeARP}

	t.Run("k3s join uses K3sTokenSecretRef + VIP", func(t *testing.T) {
		kcp := &controlplanev1beta2.KairosControlPlane{Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Distribution: "k3s", HA: &controlplanev1beta2.HAConfig{VIP: vip},
		}}
		r := &KairosControlPlaneReconciler{}
		spec := &bootstrapv1beta2.KairosConfigSpec{Distribution: "k3s"}
		r.applyControlPlaneHASpec(spec, kcp, cluster, bootstrapv1beta2.ControlPlaneRoleJoin)

		g.Expect(spec.ControlPlaneRole).To(Equal(bootstrapv1beta2.ControlPlaneRoleJoin))
		g.Expect(spec.SingleNode).To(BeFalse())
		g.Expect(spec.K3sTokenSecretRef).ToNot(BeNil())
		g.Expect(spec.K3sTokenSecretRef.Name).To(Equal(joinTokenSecretName("c")))
		g.Expect(spec.ControlPlaneJoinTokenSecretRef).To(BeNil())
		g.Expect(spec.ControlPlaneVIP).ToNot(BeNil())
		g.Expect(spec.ControlPlaneVIP.Address).To(Equal("192.168.1.240"))
		// TOKEN-INV: no inline token fields are ever set.
		g.Expect(spec.K3sToken).To(BeEmpty())
		g.Expect(spec.WorkerToken).To(BeEmpty())
		g.Expect(spec.Token).To(BeEmpty())
	})

	t.Run("k0s join uses ControlPlaneJoinTokenSecretRef", func(t *testing.T) {
		kcp := &controlplanev1beta2.KairosControlPlane{Spec: controlplanev1beta2.KairosControlPlaneSpec{Distribution: "k0s"}}
		r := &KairosControlPlaneReconciler{}
		spec := &bootstrapv1beta2.KairosConfigSpec{Distribution: "k0s"}
		r.applyControlPlaneHASpec(spec, kcp, cluster, bootstrapv1beta2.ControlPlaneRoleJoin)

		g.Expect(spec.ControlPlaneJoinTokenSecretRef).ToNot(BeNil())
		g.Expect(spec.K3sTokenSecretRef).To(BeNil())
		g.Expect(spec.Token).To(BeEmpty())
	})

	t.Run("single gets no token ref and no VIP", func(t *testing.T) {
		kcp := &controlplanev1beta2.KairosControlPlane{Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Distribution: "k0s", HA: &controlplanev1beta2.HAConfig{VIP: vip},
		}}
		r := &KairosControlPlaneReconciler{}
		spec := &bootstrapv1beta2.KairosConfigSpec{Distribution: "k0s"}
		r.applyControlPlaneHASpec(spec, kcp, cluster, bootstrapv1beta2.ControlPlaneRoleSingle)

		g.Expect(spec.ControlPlaneRole).To(Equal(bootstrapv1beta2.ControlPlaneRoleSingle))
		g.Expect(spec.SingleNode).To(BeTrue())
		g.Expect(spec.ControlPlaneJoinTokenSecretRef).To(BeNil())
		g.Expect(spec.K3sTokenSecretRef).To(BeNil())
		g.Expect(spec.ControlPlaneVIP).To(BeNil())
	})
}

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
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/k0sjointoken"
)

// TestHA_K0sJoinToken_BootstrapRendersOnlyTheIssuedToken: a k0s join
// KairosConfig names the token the KCP issued for it (ADR 0013). While the
// join-token Secret holds a different token, the bootstrap controller requeues
// and renders nothing. Once the Secret holds the named token, the join-token
// watch wakes the config and it renders that token, and only that one.
func TestHA_K0sJoinToken_BootstrapRendersOnlyTheIssuedToken(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	const (
		nsName      = "ha-k0s-idmatch"
		clusterName = "ha-k0s-idmatch-cluster"
		kcpName     = "ha-k0s-idmatch-kcp"
	)
	encode := func(id string) string {
		tok, err := k0sjointoken.NewBootstrapToken(id, "0123456789"+id)
		g.Expect(err).ToNot(HaveOccurred())
		enc, err := k0sjointoken.Encode("https://192.0.2.10:9443", []byte("ca"), tok)
		g.Expect(err).ToNot(HaveOccurred())
		return enc
	}
	staleToken, issuedToken := encode("aaaaaa"), encode("bbbbbb")
	idAnno := bootstrapv1beta2.K0sControllerJoinTokenIDAnnotation
	haFixture(t, ctx, c, nsName, clusterName, kcpName, "k0s")

	// Stand in for the KCP: the Secret holds an earlier token, "aaaaaa". The
	// KCP's join gate never opens in this test, so it writes nothing here.
	tokenSecretName := bootstrapv1beta2.ControlPlaneJoinTokenSecretName(clusterName)
	setIssued := func(id, value string) {
		g.Eventually(func() error {
			s := &corev1.Secret{}
			if err := c.Get(ctx, types.NamespacedName{Name: tokenSecretName, Namespace: nsName}, s); err != nil {
				return err
			}
			if s.Annotations == nil {
				s.Annotations = map[string]string{}
			}
			s.Annotations[idAnno] = id
			if s.Data == nil {
				s.Data = map[string][]byte{}
			}
			s.Data[bootstrapv1beta2.ControlPlaneJoinTokenSecretDataKey] = []byte(value)
			return c.Update(ctx, s)
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())
	}
	setIssued("aaaaaa", staleToken)

	// A join KairosConfig issued token "bbbbbb", owned by a Machine so the
	// bootstrap reconcile gets as far as token resolution.
	joinCfg := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "issued-join",
			Namespace:   nsName,
			Labels:      map[string]string{clusterv1.ClusterNameLabel: clusterName},
			Annotations: map[string]string{idAnno: "bbbbbb"},
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
	joinMachine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "issued-join-machine",
			Namespace: nsName,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         clusterName,
				clusterv1.MachineControlPlaneLabel: "",
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: clusterName,
			Version:     "v1.30.0+k0s.0",
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: bootstrapv1beta2.GroupVersion.Group,
					Kind:     "KairosConfig",
					Name:     joinCfg.Name,
				},
			},
			InfrastructureRef: testMachineInfraRef("infra"),
		},
	}
	g.Expect(c.Create(ctx, joinMachine)).To(Succeed())
	g.Eventually(func() error {
		cur := &bootstrapv1beta2.KairosConfig{}
		if err := c.Get(ctx, types.NamespacedName{Name: joinCfg.Name, Namespace: nsName}, cur); err != nil {
			return err
		}
		cur.OwnerReferences = []metav1.OwnerReference{
			*metav1.NewControllerRef(joinMachine, clusterv1.GroupVersion.WithKind("Machine")),
		}
		return c.Update(ctx, cur)
	}, 10*time.Second, 200*time.Millisecond).Should(Succeed())

	// ID mismatch: the config must not render.
	g.Consistently(func() bool {
		got := &bootstrapv1beta2.KairosConfig{}
		if err := c.Get(ctx, types.NamespacedName{Name: joinCfg.Name, Namespace: nsName}, got); err != nil {
			return false
		}
		return !got.Status.Ready && got.Status.DataSecretName == nil
	}, 5*time.Second, 500*time.Millisecond).Should(BeTrue(), "a join config must not render a token other than the one issued for it")

	// The KCP stores the token it issued for this config.
	setIssued("bbbbbb", issuedToken)

	got := &bootstrapv1beta2.KairosConfig{}
	g.Eventually(func() bool {
		if err := c.Get(ctx, types.NamespacedName{Name: joinCfg.Name, Namespace: nsName}, got); err != nil {
			return false
		}
		return got.Status.Ready && got.Status.DataSecretName != nil
	}, 30*time.Second, 500*time.Millisecond).Should(BeTrue(), "the config renders once the Secret holds its token")

	data := &corev1.Secret{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: *got.Status.DataSecretName, Namespace: nsName}, data)).To(Succeed())
	rendered := string(data.Data["value"])
	g.Expect(rendered).To(ContainSubstring("/etc/k0s/controller-token"))
	g.Expect(strings.Count(rendered, issuedToken)).To(Equal(1), "the issued token is written to the token file")
	g.Expect(rendered).ToNot(ContainSubstring(staleToken))
}

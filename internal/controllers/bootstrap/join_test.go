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

package bootstrap

import (
	"context"
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// fakeJoinSource is an injectable JoinMaterialSource test double.
type fakeJoinSource struct {
	token  string
	called int
}

func (f *fakeJoinSource) WorkerJoin(_ context.Context, _ JoinRequest) (WorkerJoinMaterial, error) {
	f.called++
	return WorkerJoinMaterial{Token: f.token}, nil
}

// TestWorkerJoinMaterial_Redacts asserts the token never leaks through any fmt
// verb (root rule 2).
func TestWorkerJoinMaterial_Redacts(t *testing.T) {
	g := NewWithT(t)
	const secret = "super-secret-token-value"
	mat := WorkerJoinMaterial{Token: secret}
	for _, verb := range []string{"%v", "%s", "%+v", "%#v"} {
		rendered := fmt.Sprintf(verb, mat)
		g.Expect(rendered).NotTo(ContainSubstring(secret), "verb %s leaked the token: %s", verb, rendered)
		g.Expect(rendered).To(ContainSubstring("REDACTED"), "verb %s should mark the field redacted", verb)
	}
	// Also a pointer, which inherits the value-receiver Stringer/GoStringer.
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		rendered := fmt.Sprintf(verb, &mat)
		g.Expect(rendered).NotTo(ContainSubstring(secret), "verb %s (pointer) leaked the token", verb)
	}
}

// TestWorkerJoinMaterial_RedactsKubeadm asserts the kubeadm JoinConfiguration
// (which embeds the minted bootstrap token) never leaks through any fmt verb, while
// the non-secret token ID IS surfaced for audit (ADR 0010 P1 item 8).
func TestWorkerJoinMaterial_RedactsKubeadm(t *testing.T) {
	g := NewWithT(t)
	const secretJoinCfg = "discovery:\n  bootstrapToken:\n    token: abcdef.secrettokenvalue01"
	mat := WorkerJoinMaterial{Kubeadm: &KubeadmJoinMaterial{
		JoinConfiguration: secretJoinCfg,
		KubernetesVersion: "v1.30.0",
		TokenID:           "abcdef",
	}}
	for _, verb := range []string{"%v", "%s", "%+v", "%#v"} {
		rendered := fmt.Sprintf(verb, mat)
		g.Expect(rendered).NotTo(ContainSubstring("secrettokenvalue01"), "verb %s leaked the bootstrap token: %s", verb, rendered)
		g.Expect(rendered).NotTo(ContainSubstring("bootstrapToken"), "verb %s leaked the JoinConfiguration body", verb)
		g.Expect(rendered).To(ContainSubstring("REDACTED"))
		g.Expect(rendered).To(ContainSubstring("abcdef"), "verb %s should surface the non-secret token ID", verb)
	}
}

// TestJoinSourceFor pins source selection: an injected source is used; a nil map
// or a missing key uses the built-in over r.Client.
func TestJoinSourceFor(t *testing.T) {
	g := NewWithT(t)
	scheme := charScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	injected := &fakeJoinSource{token: "x"}
	r := &KairosConfigReconciler{Client: c, Scheme: scheme, JoinSources: map[string]JoinMaterialSource{
		bootstrapv1beta2.DistributionK0s: injected,
	}}
	gotInjected, err := r.joinSourceFor(bootstrapv1beta2.DistributionK0s)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gotInjected).To(BeIdenticalTo(injected), "injected source must be used")
	// Missing key -> built-in k3s.
	k3sSrc, err := r.joinSourceFor(bootstrapv1beta2.DistributionK3s)
	g.Expect(err).NotTo(HaveOccurred())
	_, isK3s := k3sSrc.(k3sJoinSource)
	g.Expect(isK3s).To(BeTrue(), "missing key must fall back to the built-in k3s source")

	// Nil map -> built-in for both.
	rNil := &KairosConfigReconciler{Client: c, Scheme: scheme}
	k0sSrc, err := rNil.joinSourceFor(bootstrapv1beta2.DistributionK0s)
	g.Expect(err).NotTo(HaveOccurred())
	_, isK0s := k0sSrc.(k0sJoinSource)
	g.Expect(isK0s).To(BeTrue(), "nil map must fall back to the built-in k0s source")
}

// TestGenerateCloudConfig_InjectedJoinSourceIsUsed proves an injected source
// supplies the worker token the render embeds, in place of the built-in.
func TestGenerateCloudConfig_InjectedJoinSourceIsUsed(t *testing.T) {
	g := NewWithT(t)
	scheme := charScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	injected := &fakeJoinSource{token: "injected-worker-token"}
	r := &KairosConfigReconciler{Client: c, Scheme: scheme, JoinSources: map[string]JoinMaterialSource{
		bootstrapv1beta2.DistributionK0s: injected,
	}}

	// A k0s worker whose inline token is DIFFERENT, so the injected token winning
	// is observable.
	kc := workerKC("k0s", func(s *bootstrapv1beta2.KairosConfigSpec) {
		s.WorkerToken = "builtin-would-use-this"
	})
	out, err := r.generateCloudConfig(context.Background(), log.Log, kc, charMachine("VSphereMachine", ""), charCluster(true))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(injected.called).To(Equal(1), "the injected source must be invoked")
	g.Expect(out).To(ContainSubstring("injected-worker-token"))
	g.Expect(strings.Contains(out, "builtin-would-use-this")).To(BeFalse(), "the built-in inline token must not be used when a source is injected")
}

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
	g.Expect(r.joinSourceFor(bootstrapv1beta2.DistributionK0s)).To(BeIdenticalTo(injected), "injected source must be used")
	// Missing key -> built-in k3s.
	_, isK3s := r.joinSourceFor(bootstrapv1beta2.DistributionK3s).(k3sJoinSource)
	g.Expect(isK3s).To(BeTrue(), "missing key must fall back to the built-in k3s source")

	// Nil map -> built-in for both.
	rNil := &KairosConfigReconciler{Client: c, Scheme: scheme}
	_, isK0s := rNil.joinSourceFor(bootstrapv1beta2.DistributionK0s).(k0sJoinSource)
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

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// newDistTemplate returns a KairosConfigTemplate whose nested spec sets the
// given distribution ("" leaves it unset).
func newDistTemplate(name, namespace, dist string) *bootstrapv1beta2.KairosConfigTemplate {
	return &bootstrapv1beta2.KairosConfigTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: bootstrapv1beta2.KairosConfigTemplateSpec{
			Template: bootstrapv1beta2.KairosConfigTemplateResource{
				Spec: bootstrapv1beta2.KairosConfigSpec{
					Distribution: dist,
				},
			},
		},
	}
}

func newKCPWithTemplate(dist, templateName string) *controlplanev1beta2.KairosControlPlane {
	return &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-kcp", Namespace: "default"},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Version:      "v1.30.0+k0s.0",
			Distribution: dist,
			KairosConfigTemplate: controlplanev1beta2.KairosConfigTemplateReference{
				Name: templateName,
			},
		},
	}
}

// TestResolveEffectiveDistribution covers the inherit source: it returns the
// referenced template's distribution, "" when the template does not set one,
// and "" (no error) when the template is absent so a later reconcile can
// resolve once it exists.
func TestResolveEffectiveDistribution(t *testing.T) {
	tests := []struct {
		name         string
		templateDist string // distribution set on the template ("" = unset)
		templateName string // KCP's KairosConfigTemplate.Name
		withTemplate bool   // whether the template object exists in the cluster
		want         string
		wantErr      bool
	}{
		{
			name:         "template sets k3s -> inherits k3s",
			templateDist: "k3s",
			templateName: "cfg-template",
			withTemplate: true,
			want:         "k3s",
		},
		{
			name:         "template sets k0s -> inherits k0s",
			templateDist: "k0s",
			templateName: "cfg-template",
			withTemplate: true,
			want:         "k0s",
		},
		{
			name:         "template has no distribution -> empty",
			templateDist: "",
			templateName: "cfg-template",
			withTemplate: true,
			want:         "",
		},
		{
			name:         "template not found -> empty, no error",
			templateDist: "k3s",
			templateName: "cfg-template",
			withTemplate: false,
			want:         "",
		},
		{
			name:         "no template reference -> empty, no error",
			templateDist: "k3s",
			templateName: "",
			withTemplate: false,
			want:         "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := newKCPTestScheme(t)

			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tc.withTemplate {
				builder = builder.WithObjects(newDistTemplate("cfg-template", "default", tc.templateDist))
			}
			r := &KairosControlPlaneReconciler{Client: builder.Build(), Scheme: scheme}

			kcp := newKCPWithTemplate("", tc.templateName)

			got, err := r.resolveEffectiveDistribution(context.Background(), kcp)
			if tc.wantErr {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

// TestDistributionOf_Fallback locks in the k0s fallback used by every
// downstream path (machine create, join gate, etcd leave) when spec.distribution
// is still empty — i.e. the template was not yet resolved this reconcile.
func TestDistributionOf_Fallback(t *testing.T) {
	g := NewWithT(t)

	g.Expect(distributionOf(newKCPWithTemplate("", "cfg-template"))).To(Equal("k0s"),
		"empty spec.distribution must fall back to k0s")
	g.Expect(distributionOf(newKCPWithTemplate("k3s", "cfg-template"))).To(Equal("k3s"),
		"explicit spec.distribution must be returned verbatim")
	g.Expect(distributionOf(newKCPWithTemplate("k0s", "cfg-template"))).To(Equal("k0s"),
		"explicit k0s must be returned verbatim")
}

// TestEffectiveDistribution_Precedence exercises the full resolution precedence
// as the controller composes it: explicit KCP value wins over the template;
// unset + template inherits; unset + no template falls back to k0s via
// distributionOf. The explicit-wins arm asserts that resolveEffectiveDistribution
// (the inherit source) is NOT consulted for the effective value when the KCP is
// explicit — the controller only reads it to detect an override for the warning.
func TestEffectiveDistribution_Precedence(t *testing.T) {
	g := NewWithT(t)
	scheme := newKCPTestScheme(t)

	// Template says k0s; KCP explicitly says k3s -> explicit wins.
	explicitClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(newDistTemplate("cfg-template", "default", "k0s")).Build()
	rExplicit := &KairosControlPlaneReconciler{Client: explicitClient, Scheme: scheme}
	explicitKCP := newKCPWithTemplate("k3s", "cfg-template")
	g.Expect(distributionOf(explicitKCP)).To(Equal("k3s"),
		"explicit spec.distribution must win even when the template differs")
	// The inherit source still reports the template's value (used for override warning).
	tmplDist, err := rExplicit.resolveEffectiveDistribution(context.Background(), explicitKCP)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(tmplDist).To(Equal("k0s"))
	g.Expect(tmplDist).NotTo(Equal(explicitKCP.Spec.Distribution),
		"template distribution differs from the explicit KCP value -> override case")

	// Template says k3s; KCP unset -> inherits k3s.
	inheritClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(newDistTemplate("cfg-template", "default", "k3s")).Build()
	rInherit := &KairosControlPlaneReconciler{Client: inheritClient, Scheme: scheme}
	inheritKCP := newKCPWithTemplate("", "cfg-template")
	inherited, err := rInherit.resolveEffectiveDistribution(context.Background(), inheritKCP)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inherited).To(Equal("k3s"))
	inheritKCP.Spec.Distribution = inherited // controller persists this
	g.Expect(distributionOf(inheritKCP)).To(Equal("k3s"))

	// No template object; KCP unset -> resolve yields "" and distributionOf
	// falls back to k0s (no persist happens for this reconcile).
	noTmplClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	rNoTmpl := &KairosControlPlaneReconciler{Client: noTmplClient, Scheme: scheme}
	fallbackKCP := newKCPWithTemplate("", "cfg-template")
	resolved, err := rNoTmpl.resolveEffectiveDistribution(context.Background(), fallbackKCP)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resolved).To(BeEmpty())
	g.Expect(fallbackKCP.Spec.Distribution).To(BeEmpty(), "no template -> nothing persisted")
	g.Expect(distributionOf(fallbackKCP)).To(Equal("k0s"))
}

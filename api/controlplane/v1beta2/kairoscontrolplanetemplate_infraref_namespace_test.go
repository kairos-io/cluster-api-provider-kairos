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

package v1beta2

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Anything invalid on a KairosControlPlane has to be invalid on the template
// too, or an operator stages a template that only fails when a cluster is
// created from it. A cross-namespace infrastructureRef is the case that was
// missing: the KCP webhook rejects it, and the template admitted it.
func TestKairosControlPlaneTemplate_Validate_RejectsCrossNamespaceInfrastructureRef(t *testing.T) {
	tmpl := newValidKCPTemplate()
	tmpl.Spec.Template.Spec.MachineTemplate.InfrastructureRef.Namespace = "other-tenant"

	err := tmpl.validate()
	if err == nil {
		t.Fatal("an infrastructureRef into another namespace must be rejected on the template")
	}
	if !strings.Contains(err.Error(), "spec.template.spec.machineTemplate.infrastructureRef.namespace") ||
		!strings.Contains(err.Error(), "cross-namespace references are not allowed") {
		t.Fatalf("error must name the nested field and the rule, got: %v", err)
	}
}

// An empty namespace means the object's own, which is what clusterctl cluster
// templates rely on, and naming it explicitly is equally fine.
func TestKairosControlPlaneTemplate_Validate_AcceptsOwnNamespaceInfrastructureRef(t *testing.T) {
	for _, ns := range []string{"", "default"} {
		tmpl := newValidKCPTemplate()
		tmpl.Spec.Template.Spec.MachineTemplate.InfrastructureRef.Namespace = ns
		if err := tmpl.validate(); err != nil {
			t.Fatalf("namespace %q must be accepted, got: %v", ns, err)
		}
	}
}

// The rule reads the template's own namespace, not a hardcoded one, so a
// template outside "default" accepts its own namespace and rejects "default".
func TestKairosControlPlaneTemplate_Validate_InfrastructureRefNamespaceIsTheTemplatesOwn(t *testing.T) {
	tmpl := newValidKCPTemplate()
	tmpl.Namespace = "tenant-a"

	tmpl.Spec.Template.Spec.MachineTemplate.InfrastructureRef.Namespace = "tenant-a"
	if err := tmpl.validate(); err != nil {
		t.Fatalf("a reference into the template's own namespace must be accepted, got: %v", err)
	}

	tmpl.Spec.Template.Spec.MachineTemplate.InfrastructureRef.Namespace = "default"
	if err := tmpl.validate(); err == nil {
		t.Fatal("a reference into another namespace must be rejected, even when that namespace is \"default\"")
	}
}

// The template and the KairosControlPlane it stamps must agree. Whatever one
// of them does with a given namespace, the other has to do as well.
func TestKairosControlPlaneTemplate_Validate_AgreesWithKCPOnInfrastructureRefNamespace(t *testing.T) {
	for _, ns := range []string{"", "default", "other-tenant"} {
		kcp := newValidKCP()
		kcp.Spec.MachineTemplate.InfrastructureRef.Namespace = ns
		kcpErr := kcp.validate()

		tmpl := newValidKCPTemplate()
		tmpl.Spec.Template.Spec.MachineTemplate.InfrastructureRef.Namespace = ns
		tmplErr := tmpl.validate()

		if (kcpErr == nil) != (tmplErr == nil) {
			t.Fatalf("namespace %q: KairosControlPlane error is %v but KairosControlPlaneTemplate error is %v; the two must agree",
				ns, kcpErr, tmplErr)
		}
	}
}

// A VIP on a single-node control plane is a warning on a KairosControlPlane
// and has to be a warning on the template too, for the same reason: staging it
// otherwise hides the warning until a cluster is created.
func TestKairosControlPlaneTemplate_ValidateWithWarnings_VIPOnSingleNode(t *testing.T) {
	tmpl := newValidKCPTemplate()
	tmpl.Spec.Template.Spec.Replicas = ptr(int32(1))
	tmpl.Spec.Template.Spec.HA = &HAConfig{VIP: &KubeVIPConfig{
		Address:   "192.168.1.10",
		Interface: "eth0",
	}}

	warnings, err := tmpl.validateWithWarnings()
	if err != nil {
		t.Fatalf("validateWithWarnings() returned error %v; expected nil (VIP on single-node is a warning, not an error)", err)
	}
	if len(warnings) == 0 {
		t.Fatal("validateWithWarnings() returned no warnings; expected one about the VIP being ignored for single-node")
	}
	if !strings.Contains(warnings[0], "spec.template.spec.ha.vip") {
		t.Errorf("warning[0] = %q; expected it to name the nested path spec.template.spec.ha.vip", warnings[0])
	}
}

// Three replicas with a VIP is the configuration the warning exists to steer
// operators towards, so it must stay silent.
func TestKairosControlPlaneTemplate_ValidateWithWarnings_NoWarningOnThreeReplicas(t *testing.T) {
	tmpl := newValidKCPTemplate()
	tmpl.Spec.Template.Spec.Replicas = ptr(int32(3))
	tmpl.Spec.Template.Spec.HA = &HAConfig{VIP: &KubeVIPConfig{
		Address:   "192.168.1.10",
		Interface: "eth0",
	}}

	warnings, err := tmpl.validateWithWarnings()
	if err != nil {
		t.Fatalf("validateWithWarnings() returned unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("validateWithWarnings() returned %d warnings; expected 0 for a three-node control plane: %v", len(warnings), warnings)
	}
}

// The default shape stays silent, so the change adds no warning to templates
// that were clean before.
func TestKairosControlPlaneTemplate_ValidateWithWarnings_CleanTemplateIsSilent(t *testing.T) {
	tmpl := newValidKCPTemplate()
	warnings, err := tmpl.validateWithWarnings()
	if err != nil {
		t.Fatalf("validateWithWarnings() returned %v; expected nil for the default template shape", err)
	}
	if len(warnings) != 0 {
		t.Errorf("validateWithWarnings() returned %d warnings; expected 0: %v", len(warnings), warnings)
	}
}

// The two rules above are only worth anything if the admission entry points
// carry them, which is where the warning was lost: ValidateCreate and
// ValidateUpdate returned a nil warning list of their own.
func TestKairosControlPlaneTemplateValidator_CarriesErrorsAndWarnings(t *testing.T) {
	v := &kairosControlPlaneTemplateValidator{}

	rejected := newValidKCPTemplate()
	rejected.Spec.Template.Spec.MachineTemplate.InfrastructureRef.Namespace = "other-tenant"
	for _, tc := range []struct {
		name string
		call func(*KairosControlPlaneTemplate) (admission.Warnings, error)
	}{
		{"create", func(r *KairosControlPlaneTemplate) (admission.Warnings, error) {
			return v.ValidateCreate(context.Background(), r)
		}},
		{"update", func(r *KairosControlPlaneTemplate) (admission.Warnings, error) {
			return v.ValidateUpdate(context.Background(), r, r)
		}},
	} {
		t.Run(tc.name+"/rejects-cross-namespace-infrastructure-ref", func(t *testing.T) {
			if _, err := tc.call(rejected); err == nil {
				t.Fatal("the validator must reject a cross-namespace infrastructureRef")
			}
		})

		t.Run(tc.name+"/warns-about-a-vip-on-a-single-node", func(t *testing.T) {
			warned := newValidKCPTemplate()
			warned.Spec.Template.Spec.Replicas = ptr(int32(1))
			warned.Spec.Template.Spec.HA = &HAConfig{VIP: &KubeVIPConfig{
				Address:   "192.168.1.10",
				Interface: "eth0",
			}}
			warnings, err := tc.call(warned)
			if err != nil {
				t.Fatalf("a VIP on a single-node template is a warning, not an error, got: %v", err)
			}
			if len(warnings) == 0 {
				t.Fatal("the validator must carry the warning admission shows the operator")
			}
		})
	}
}

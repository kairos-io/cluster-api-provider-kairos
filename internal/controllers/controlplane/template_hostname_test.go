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
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// hostnameTemplateKCP is a single-node k0s KCP referencing a
// KairosConfigTemplate "cp-template" whose spec sets hostname.
func hostnameTemplateKCP(g *WithT, hostname string) (*KairosControlPlaneReconciler, *controlplanev1beta2.KairosControlPlane, *record.FakeRecorder) {
	scheme := machineMetaScheme(g)
	kcp := machineMetaKCP("k0s", nil)
	kcp.Spec.KairosConfigTemplate = controlplanev1beta2.KairosConfigTemplateReference{Name: "cp-template"}
	tmpl := newDistTemplate("cp-template", "default", "k0s")
	tmpl.Spec.Template.Spec.Hostname = hostname
	tmpl.Spec.Template.Spec.SSHPublicKey = "ssh-ed25519 AAAA test"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp, tmpl, machineMetaInfraTemplate()).Build()
	rec := record.NewFakeRecorder(10)
	return &KairosControlPlaneReconciler{Client: c, Scheme: scheme, Recorder: rec}, kcp, rec
}

// TestControlPlaneTemplateHostname: a KairosConfigTemplate the KCP references
// for its control-plane nodes must follow the control-plane hostname rule.
// The KCP reports a bad one on every reconcile, as soon as it is referenced,
// and creates no control-plane Machine, nor any of its objects, from it.
func TestControlPlaneTemplateHostname(t *testing.T) {
	for _, h := range []string{"CP-Host", "cp.example.com", "metal-{{ trunc 4 .MachineID }}"} {
		t.Run(h+": refused and reported", func(t *testing.T) {
			g := NewWithT(t)
			r, kcp, rec := hostnameTemplateKCP(g, h)

			r.reportControlPlaneTemplateHostname(context.Background(), log.Log, kcp)
			g.Expect(rec.Events).To(Receive(ContainSubstring("Warning " + invalidControlPlaneHostnameReason)))

			created, err := r.createControlPlaneMachine(context.Background(), log.Log, kcp, machineMetaCluster(), bootstrapv1beta2.ControlPlaneRoleSingle, "")
			g.Expect(err).To(MatchError(ContainSubstring("lowercase DNS-1123 label")))
			g.Expect(created).To(BeNil())
			// Names are random (ADR 0015 PR-4): check that nothing at all was
			// created, not that one name is absent.
			configs, infra, machines := objectNames(g, r.Client)
			g.Expect(configs).To(BeEmpty(), "no KairosConfig is created from the template")
			g.Expect(infra).To(BeEmpty())
			g.Expect(machines).To(BeEmpty())
		})
	}
	for _, h := range []string{"", "cp-host"} {
		t.Run("'"+h+"': accepted, nothing reported", func(t *testing.T) {
			g := NewWithT(t)
			r, kcp, rec := hostnameTemplateKCP(g, h)

			problem, err := r.controlPlaneTemplateHostnameProblem(context.Background(), kcp)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(problem).To(BeEmpty())
			r.reportControlPlaneTemplateHostname(context.Background(), log.Log, kcp)
			g.Expect(rec.Events).ToNot(Receive())
		})
	}
	t.Run("no template referenced, or not created yet: nothing to check", func(t *testing.T) {
		g := NewWithT(t)
		r, kcp, _ := hostnameTemplateKCP(g, "CP-Host")
		kcp.Spec.KairosConfigTemplate.Name = "missing"
		problem, err := r.controlPlaneTemplateHostnameProblem(context.Background(), kcp)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(problem).To(BeEmpty())
		kcp.Spec.KairosConfigTemplate.Name = ""
		problem, err = r.controlPlaneTemplateHostnameProblem(context.Background(), kcp)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(problem).To(BeEmpty())
	})
}

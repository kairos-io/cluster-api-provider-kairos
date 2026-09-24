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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Making the Cluster an owner of a template in another namespace is worse than
// a stray reference: Kubernetes treats a cross-namespace owner reference as
// absent, so the garbage collector may delete the other namespace's template.
// The ownership step must leave such a template untouched even if the webhook
// was not in the path.
func TestReconcileTemplateOwnerRefs_LeavesAnotherNamespacesTemplateAlone(t *testing.T) {
	g := NewWithT(t)
	scheme := newKCPTestScheme(t)
	cluster, kcp := ownerTestCluster(), ownerTestKCP()
	kcp.Spec.MachineTemplate.InfrastructureRef.Namespace = "other-tenant"
	kcp.Spec.KairosConfigTemplate.Name = ""

	foreign := infraTemplateObj()
	foreign.SetNamespace("other-tenant")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, kcp, foreign).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.reconcileTemplateOwnerRefs(context.Background(), kcp, cluster)).To(Succeed())

	got := infraTemplateObj()
	got.SetNamespace("other-tenant")
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(got), got)).To(Succeed())
	g.Expect(got.GetOwnerReferences()).To(BeEmpty(),
		"a cross-namespace owner reference would let the garbage collector delete this template")
}

// The clone path refuses too, rather than copying another namespace's machine
// template into this cluster.
func TestCreateInfrastructureMachine_RefusesAnotherNamespacesTemplate(t *testing.T) {
	g := NewWithT(t)
	scheme := newKCPTestScheme(t)
	cluster, kcp := ownerTestCluster(), ownerTestKCP()
	kcp.Spec.MachineTemplate.InfrastructureRef.Namespace = "other-tenant"

	foreign := infraTemplateObj()
	foreign.SetNamespace("other-tenant")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, kcp, foreign).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	_, err := r.createInfrastructureMachine(context.Background(), log.Log, kcp, cluster, "owned-kcp-0")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cross-namespace references are not allowed"))
}

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
	"time"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// These tests pin that the KCP orders and picks control-plane Machines by
// creation time, never by the shape of their names. Index names (<kcp>-0) and
// random names (<kcp>-x7k2p) coexist on a cluster created before ADR 0015
// PR-4 and scaled or rolled out after it, so every name below is chosen to
// sort in the opposite order to the Machines' ages.

// The rollout replaces the oldest outdated member first, even when its name
// sorts last and an index-named member sorts first.
func TestReconcileMachines_RolloutReplacesTheOldestByCreationNotByName(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := rolloutKCP(3)
	cluster := rolloutCluster()

	oldest := rolloutMachine(kcp, "test-kcp-zzzzz", rolloutOldVersion, 4*time.Hour, true)
	old0 := rolloutMachine(kcp, "test-kcp-0", rolloutOldVersion, 3*time.Hour, true)
	old1 := rolloutMachine(kcp, "test-kcp-1", rolloutOldVersion, 2*time.Hour, true)
	joined := rolloutMachine(kcp, "test-kcp-abcde", rolloutNewVersion, time.Minute, true)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(oldest, old0, old1, joined, machineMetaInfraTemplate(),
			rolloutEtcdStatus(cluster, oldest, old0, old1, joined)).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme, WorkloadClientFactory: withPinnedWorkload(scheme, kcp)}

	_, err := r.reconcileMachines(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())

	names, _ := remainingMachines(g, c)
	g.Expect(names).To(ConsistOf("test-kcp-0", "test-kcp-1", "test-kcp-abcde"),
		"the oldest member by creation time is replaced first, whatever its name")
}

// Scale-down removes the newest Machine by creation time. Here the newest one
// carries the lowest index name, and a random name sits in the middle.
func TestReconcileMachines_ScaleDownRemovesTheNewestByCreationNotByName(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := rolloutKCP(3)
	cluster := rolloutCluster()

	m9 := rolloutMachine(kcp, "test-kcp-9", rolloutNewVersion, 4*time.Hour, true)
	mRand := rolloutMachine(kcp, "test-kcp-aaaaa", rolloutNewVersion, 3*time.Hour, true)
	m1 := rolloutMachine(kcp, "test-kcp-1", rolloutNewVersion, 2*time.Hour, true)
	newest := rolloutMachine(kcp, "test-kcp-0", rolloutNewVersion, time.Minute, true)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(m9, mRand, m1, newest, machineMetaInfraTemplate(),
			rolloutEtcdStatus(cluster, m9, mRand, m1, newest)).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme, WorkloadClientFactory: withPinnedWorkload(scheme, kcp)}

	_, err := r.reconcileMachines(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())

	err = c.Get(context.Background(), types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, &clusterv1.Machine{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the newest Machine is the one scaled down")
	names, _ := remainingMachines(g, c)
	g.Expect(names).To(ConsistOf("test-kcp-9", "test-kcp-aaaaa", "test-kcp-1"))
}

// A scale-up creates one Machine, and its KairosConfig and infrastructure
// Machine carry the Machine's own name. The assertion does not assume what
// that name is.
func TestReconcileMachines_ScaleUpCreatesOneMachineNamedLikeItsConfigAndInfra(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := rolloutKCP(1)
	cluster := rolloutCluster()

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machineMetaInfraTemplate()).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	_, err := r.reconcileMachines(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())

	machines := &clusterv1.MachineList{}
	g.Expect(c.List(context.Background(), machines, client.InNamespace("default"))).To(Succeed())
	g.Expect(machines.Items).To(HaveLen(1))
	m := machines.Items[0]
	g.Expect(m.Spec.Bootstrap.ConfigRef.Name).To(Equal(m.Name))
	g.Expect(m.Spec.InfrastructureRef.Name).To(Equal(m.Name))

	kc := &bootstrapv1beta2.KairosConfig{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: m.Name, Namespace: "default"}, kc)).To(Succeed())
	g.Expect(kc.Spec.ControlPlaneRole).To(Equal(bootstrapv1beta2.ControlPlaneRoleSingle))

	infra := &unstructured.Unstructured{}
	infra.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta1")
	infra.SetKind("DockerMachine")
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: m.Name, Namespace: "default"}, infra)).To(Succeed())
}

// The role of a new Machine depends on how many Machines exist, not on their
// names: no index-0 Machine, or a missing one, ever yields a second init.
func TestControlPlaneRoleForNewMachine_IgnoresNames(t *testing.T) {
	g := NewWithT(t)
	r := &KairosControlPlaneReconciler{}
	kcp := rolloutKCP(3)

	g.Expect(r.controlPlaneRoleForNewMachine(3, nil)).To(Equal(bootstrapv1beta2.ControlPlaneRoleInit))
	for _, name := range []string{"test-kcp-0", "test-kcp-x7k2p", "test-kcp-4"} {
		existing := []*clusterv1.Machine{rolloutMachine(kcp, name, rolloutNewVersion, time.Hour, true)}
		g.Expect(r.controlPlaneRoleForNewMachine(3, existing)).To(Equal(bootstrapv1beta2.ControlPlaneRoleJoin), name)
		g.Expect(r.controlPlaneRoleForNewMachine(1, existing)).To(Equal(bootstrapv1beta2.ControlPlaneRoleSingle), name)
	}
}

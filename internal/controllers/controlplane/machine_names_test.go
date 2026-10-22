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
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// ADR 0015 PR-4: control-plane Machine names are <kcp>-<5 random characters>
// and are never handed to a second Machine.

var randomCPName = regexp.MustCompile(`^test-kcp-[a-z0-9]{5}$`)

// withNameSuffixes makes controlPlaneMachineNameSuffix return suffixes in
// order, then fail the test if asked for more. It restores the generator when
// the test ends. Tests that use it must not run in parallel.
func withNameSuffixes(t *testing.T, suffixes ...string) {
	t.Helper()
	orig := controlPlaneMachineNameSuffix
	t.Cleanup(func() { controlPlaneMachineNameSuffix = orig })
	i := 0
	controlPlaneMachineNameSuffix = func() string {
		if i >= len(suffixes) {
			t.Fatalf("asked for name suffix %d, only %d scripted", i+1, len(suffixes))
		}
		i++
		return suffixes[i-1]
	}
}

func dockerMachine(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta1")
	u.SetKind("DockerMachine")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

// objectNames lists the names of the KairosConfigs, DockerMachines and Machines
// in the default namespace.
func objectNames(g *WithT, c client.Client) (configs, infra, machines []string) {
	ctx := context.Background()
	kcs := &bootstrapv1beta2.KairosConfigList{}
	g.Expect(c.List(ctx, kcs, client.InNamespace("default"))).To(Succeed())
	for _, o := range kcs.Items {
		configs = append(configs, o.Name)
	}
	dms := &unstructured.UnstructuredList{}
	dms.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta1")
	dms.SetKind("DockerMachineList")
	g.Expect(c.List(ctx, dms, client.InNamespace("default"))).To(Succeed())
	for _, o := range dms.Items {
		infra = append(infra, o.GetName())
	}
	ms := &clusterv1.MachineList{}
	g.Expect(c.List(ctx, ms, client.InNamespace("default"))).To(Succeed())
	for _, o := range ms.Items {
		machines = append(machines, o.Name)
	}
	return configs, infra, machines
}

func TestNewControlPlaneMachineName_Format(t *testing.T) {
	g := NewWithT(t)
	for i := 0; i < 50; i++ {
		g.Expect(newControlPlaneMachineName("test-kcp")).To(MatchRegexp(randomCPName.String()))
	}

	// A long KCP name is cut so the name, which is the default hostname, fits
	// in 63 characters.
	long := strings.Repeat("a", 80)
	name := newControlPlaneMachineName(long)
	g.Expect(len(name)).To(Equal(63))
	g.Expect(name).To(MatchRegexp(`^a{57}-[a-z0-9]{5}$`))

	// The name is the default hostname, so it must pass the hostname rule
	// PR-3 enforces (a lowercase DNS-1123 label), whatever the KCP name: a
	// KCP name may be a dotted DNS-1123 subdomain of up to 253 characters.
	dotted := strings.Repeat("a", 56) + ".bbbbbbbb"
	g.Expect(newControlPlaneMachineName(dotted)).To(MatchRegexp(`^a{56}-[a-z0-9]{5}$`))
	g.Expect(newControlPlaneMachineName("my.cp.example")).To(MatchRegexp(`^my-cp-example-[a-z0-9]{5}$`))
	for _, kcpName := range []string{
		"test-kcp", "a", "my.cp.example", dotted, long,
		strings.Repeat("a", 56) + "-b", strings.Repeat("ab.", 84) + "c",
	} {
		for i := 0; i < 20; i++ {
			name := newControlPlaneMachineName(kcpName)
			g.Expect(bootstrapv1beta2.NodeHostnameErrors(name)).To(BeEmpty(), "KCP %q gave %q", kcpName, name)
		}
	}
}

// Every create gets a name of the right shape that no earlier create had, and
// the KairosConfig and infrastructure Machine share it.
func TestCreateControlPlaneMachine_NamesAreRandomAndUnique(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineMetaInfraTemplate()).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}
	kcp := machineMetaKCP("k3s", nil)

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		m, err := r.createControlPlaneMachine(context.Background(), log.Log, kcp, machineMetaCluster(), bootstrapv1beta2.ControlPlaneRoleSingle, "")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(m.Name).To(MatchRegexp(randomCPName.String()))
		g.Expect(seen).NotTo(HaveKey(m.Name))
		seen[m.Name] = true
		g.Expect(m.Spec.Bootstrap.ConfigRef.Name).To(Equal(m.Name))
		g.Expect(m.Spec.InfrastructureRef.Name).To(Equal(m.Name))
	}
	configs, infra, machines := objectNames(g, c)
	g.Expect(configs).To(HaveLen(20))
	g.Expect(infra).To(ConsistOf(configs))
	g.Expect(machines).To(ConsistOf(configs))
}

// GT-17: with index names, deleting the newest Machine handed its name to the
// next one. Through the reconcile loop, the replacement now gets a new name.
func TestReconcileMachines_ScaleUpAfterDeletingTheNewestDoesNotReuseItsName(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := rolloutKCP(1)
	cluster := rolloutCluster()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineMetaInfraTemplate()).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	used := map[string]bool{}
	for round := 0; round < 5; round++ {
		_, err := r.reconcileMachines(context.Background(), log.Log, kcp, cluster)
		g.Expect(err).NotTo(HaveOccurred())
		_, _, machines := objectNames(g, c)
		g.Expect(machines).To(HaveLen(1))
		name := machines[0]
		g.Expect(name).To(MatchRegexp(randomCPName.String()))
		g.Expect(used).NotTo(HaveKey(name), "round %d reused a deleted Machine's name", round)
		used[name] = true

		// Delete the (only, so newest) Machine and its objects, as CAPI and
		// the garbage collector would.
		g.Expect(c.Delete(context.Background(), &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})).To(Succeed())
		g.Expect(c.Delete(context.Background(), &bootstrapv1beta2.KairosConfig{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})).To(Succeed())
		g.Expect(c.Delete(context.Background(), dockerMachine(name))).To(Succeed())
	}
}

// A cluster created before PR-4 keeps its index-named Machines. Scaling it up
// adds a random-named joiner, never <kcp>-<max+1>, and the index-named init is
// still the one the join gate checks.
func TestReconcileMachines_MixedNamesScaleUpAddsARandomNamedJoiner(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := rolloutKCP(3)
	conditions.MarkTrue(kcp, controlplanev1beta2.KubeconfigReadyCondition)
	cluster := rolloutCluster()

	init := rolloutMachine(kcp, "test-kcp-0", rolloutNewVersion, 2*time.Hour, true)
	joiner := rolloutMachine(kcp, "test-kcp-1", rolloutNewVersion, time.Hour, true)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(init, joiner, machineMetaInfraTemplate(), rolloutEtcdStatus(cluster, init, joiner)).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme, WorkloadClientFactory: withPinnedWorkload(scheme, kcp)}

	_, err := r.reconcileMachines(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())

	_, _, machines := objectNames(g, c)
	g.Expect(machines).To(HaveLen(3))
	var added string
	for _, n := range machines {
		if n != "test-kcp-0" && n != "test-kcp-1" {
			added = n
		}
	}
	g.Expect(added).To(MatchRegexp(randomCPName.String()))
	g.Expect(added).NotTo(Equal("test-kcp-2"))
	kc := &bootstrapv1beta2.KairosConfig{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: added, Namespace: "default"}, kc)).To(Succeed())
	g.Expect(kc.Spec.ControlPlaneRole).To(Equal(bootstrapv1beta2.ControlPlaneRoleJoin))

	// Asked for 5, the next reconcile holds: the new joiner has not joined, so
	// scale-up stays one at a time with mixed names. The fake client sets no
	// creation time, so stamp the one the apiserver would have.
	m := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: added, Namespace: "default"}, m)).To(Succeed())
	m.CreationTimestamp = metav1.Now()
	g.Expect(c.Update(context.Background(), m)).To(Succeed())
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: added, Namespace: "default"}, m)).To(Succeed())
	g.Expect(m.CreationTimestamp.IsZero()).To(BeFalse())

	kcp.Spec.Replicas = ptrInt32(5)
	res, err := r.reconcileMachines(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(BeNumerically(">", 0))
	_, _, machines = objectNames(g, c)
	g.Expect(machines).To(HaveLen(3))
}

// The rollout surge, like scale-up, adds one random-named Machine and then
// waits for it to join before doing anything else.
func TestReconcileMachines_RolloutSurgeAddsOneRandomNamedMachine(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := rolloutKCP(3)
	conditions.MarkTrue(kcp, controlplanev1beta2.KubeconfigReadyCondition)
	cluster := rolloutCluster()

	old0 := rolloutMachine(kcp, "test-kcp-0", rolloutOldVersion, 4*time.Hour, true)
	old1 := rolloutMachine(kcp, "test-kcp-1", rolloutOldVersion, 3*time.Hour, true)
	old2 := rolloutMachine(kcp, "test-kcp-2", rolloutOldVersion, 2*time.Hour, true)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(old0, old1, old2, machineMetaInfraTemplate(), rolloutEtcdStatus(cluster, old0, old1, old2)).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme, WorkloadClientFactory: withPinnedWorkload(scheme, kcp)}

	_, err := r.reconcileMachines(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	_, _, machines := objectNames(g, c)
	g.Expect(machines).To(HaveLen(4))
	g.Expect(machines).NotTo(ContainElement("test-kcp-3"))

	_, err = r.reconcileMachines(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	_, _, machines = objectNames(g, c)
	g.Expect(machines).To(HaveLen(4), "no second surge and no delete until the first surge machine joins")
}

// A failed infrastructure create deletes the KairosConfig made in that attempt.
func TestCreateControlPlaneMachine_FailedInfraCreateDeletesTheKairosConfig(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineMetaInfraTemplate()).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "DockerMachine" {
				return apierrors.NewInternalError(errors.New("injected infra create failure"))
			}
			return cl.Create(ctx, obj, opts...)
		}}).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	_, err := r.createControlPlaneMachine(context.Background(), log.Log, machineMetaKCP("k3s", nil), machineMetaCluster(), bootstrapv1beta2.ControlPlaneRoleSingle, "")
	g.Expect(err).To(MatchError(ContainSubstring("injected infra create failure")))
	configs, infra, machines := objectNames(g, c)
	g.Expect(configs).To(BeEmpty(), "the KairosConfig of the failed attempt is deleted")
	g.Expect(infra).To(BeEmpty())
	g.Expect(machines).To(BeEmpty())
}

// A failed Machine create deletes both the KairosConfig and the infrastructure
// Machine made in that attempt.
func TestCreateControlPlaneMachine_FailedMachineCreateDeletesConfigAndInfra(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineMetaInfraTemplate()).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*clusterv1.Machine); ok {
				return apierrors.NewInternalError(errors.New("injected Machine create failure"))
			}
			return cl.Create(ctx, obj, opts...)
		}}).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	_, err := r.createControlPlaneMachine(context.Background(), log.Log, machineMetaKCP("k3s", nil), machineMetaCluster(), bootstrapv1beta2.ControlPlaneRoleSingle, "")
	g.Expect(err).To(MatchError(ContainSubstring("injected Machine create failure")))
	configs, infra, machines := objectNames(g, c)
	g.Expect(configs).To(BeEmpty())
	g.Expect(infra).To(BeEmpty())
	g.Expect(machines).To(BeEmpty())
}

// When the cleanup itself fails the error says so, and the create does not go
// on to try another name, which would leave more objects behind.
func TestCreateControlPlaneMachine_FailedCleanupIsReportedAndNotRetried(t *testing.T) {
	g := NewWithT(t)
	withNameSuffixes(t, "aaaaa")
	scheme := machineMetaScheme(g)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineMetaInfraTemplate()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*clusterv1.Machine); ok {
					return apierrors.NewAlreadyExists(clusterv1.GroupVersion.WithResource("machines").GroupResource(), obj.GetName())
				}
				return cl.Create(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*bootstrapv1beta2.KairosConfig); ok {
					return apierrors.NewInternalError(errors.New("injected delete failure"))
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	_, err := r.createControlPlaneMachine(context.Background(), log.Log, machineMetaKCP("k3s", nil), machineMetaCluster(), bootstrapv1beta2.ControlPlaneRoleSingle, "")
	g.Expect(err).To(MatchError(ContainSubstring("cleanup failed")))
	g.Expect(err).To(MatchError(ContainSubstring("injected delete failure")))
	g.Expect(errors.Is(err, errControlPlaneNameTaken)).To(BeFalse())
	configs, infra, _ := objectNames(g, c)
	g.Expect(configs).To(ConsistOf("test-kcp-aaaaa"), "the KairosConfig whose delete failed is the only leftover")
	g.Expect(infra).To(BeEmpty(), "the infrastructure Machine was still deleted")
}

// A generated name that is already taken, by any of the three objects, is
// replaced by a fresh one. The object that held the name is not adopted or
// touched, and nothing of the abandoned attempt is left.
func TestCreateControlPlaneMachine_TakenNameIsRegenerated(t *testing.T) {
	for _, tc := range []struct {
		name    string
		foreign client.Object
	}{
		{"KairosConfig", &bootstrapv1beta2.KairosConfig{ObjectMeta: metav1.ObjectMeta{Name: "test-kcp-aaaaa", Namespace: "default", Labels: map[string]string{"foreign": "true"}}}},
		{"infrastructure Machine", func() client.Object {
			u := dockerMachine("test-kcp-aaaaa")
			u.SetLabels(map[string]string{"foreign": "true"})
			return u
		}()},
		{"Machine", func() client.Object {
			m := machineMetaMachine("test-kcp-aaaaa", map[string]string{"foreign": "true"}, nil)
			// Owned by nobody, so not one of this KCP's Machines.
			return m
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			withNameSuffixes(t, "aaaaa", "bbbbb")
			scheme := machineMetaScheme(g)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineMetaInfraTemplate(), tc.foreign).Build()
			r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

			m, err := r.createControlPlaneMachine(context.Background(), log.Log, machineMetaKCP("k3s", nil), machineMetaCluster(), bootstrapv1beta2.ControlPlaneRoleSingle, "")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(m.Name).To(Equal("test-kcp-bbbbb"))

			configs, infra, machines := objectNames(g, c)
			want := func(kind string) []string {
				if kind == tc.name {
					return []string{"test-kcp-aaaaa", "test-kcp-bbbbb"}
				}
				return []string{"test-kcp-bbbbb"}
			}
			g.Expect(configs).To(ConsistOf(want("KairosConfig")))
			g.Expect(infra).To(ConsistOf(want("infrastructure Machine")))
			g.Expect(machines).To(ConsistOf(want("Machine")))

			// The foreign object is untouched.
			got := tc.foreign.DeepCopyObject().(client.Object)
			g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(tc.foreign), got)).To(Succeed())
			g.Expect(got.GetLabels()).To(HaveKeyWithValue("foreign", "true"))
			g.Expect(got.GetOwnerReferences()).To(BeEmpty())
		})
	}
}

// Taken names are retried a bounded number of times.
func TestCreateControlPlaneMachine_GivesUpAfterBoundedNameAttempts(t *testing.T) {
	g := NewWithT(t)
	suffixes := make([]string, controlPlaneMachineNameAttempts)
	for i := range suffixes {
		suffixes[i] = "aaaaa"
	}
	withNameSuffixes(t, suffixes...)
	scheme := machineMetaScheme(g)
	foreign := &bootstrapv1beta2.KairosConfig{ObjectMeta: metav1.ObjectMeta{Name: "test-kcp-aaaaa", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineMetaInfraTemplate(), foreign).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	_, err := r.createControlPlaneMachine(context.Background(), log.Log, machineMetaKCP("k3s", nil), machineMetaCluster(), bootstrapv1beta2.ControlPlaneRoleSingle, "")
	g.Expect(errors.Is(err, errControlPlaneNameTaken)).To(BeTrue())
	_, infra, machines := objectNames(g, c)
	g.Expect(infra).To(BeEmpty())
	g.Expect(machines).To(BeEmpty())
}

// Machines are ordered by creation time whatever their names; equal
// timestamps fall back to the name so the order is stable.
func TestSortMachinesOldestFirst(t *testing.T) {
	g := NewWithT(t)
	kcp := rolloutKCP(3)
	at := func(name string, ts time.Time) *clusterv1.Machine {
		m := rolloutMachine(kcp, name, rolloutNewVersion, 0, false)
		m.CreationTimestamp = metav1.NewTime(ts)
		return m
	}
	base := time.Now().Truncate(time.Second)
	machines := []*clusterv1.Machine{
		at("test-kcp-0", base.Add(3*time.Hour)),
		at("test-kcp-1", base.Add(time.Hour)),
		at("test-kcp-x7k2p", base),
		at("test-kcp-bbbbb", base.Add(2*time.Hour)),
		at("test-kcp-aaaaa", base.Add(2*time.Hour)),
	}
	sortMachinesOldestFirst(machines)
	var names []string
	for _, m := range machines {
		names = append(names, m.Name)
	}
	g.Expect(names).To(Equal([]string{"test-kcp-x7k2p", "test-kcp-1", "test-kcp-aaaaa", "test-kcp-bbbbb", "test-kcp-0"}))
}

// The join gate treats the oldest Machine as the init node, and the rest as
// joiners, whatever their names. Here the index-named Machine sorts first by
// name but is the newer one.
func TestInitMachineJoinable_InitIsTheOldestMachineNotTheLowestName(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := rolloutKCP(3)
	conditions.MarkTrue(kcp, controlplanev1beta2.KubeconfigReadyCondition)
	cluster := rolloutCluster()

	init := rolloutMachine(kcp, "test-kcp-zzzzz", rolloutNewVersion, 2*time.Hour, false)
	joiner := rolloutMachine(kcp, "test-kcp-0", rolloutNewVersion, time.Hour, true)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rolloutEtcdStatus(cluster, joiner)).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	machines := []*clusterv1.Machine{joiner, init} // list order: by name
	sortMachinesOldestFirst(machines)
	joinable, reason, err := r.initMachineJoinable(context.Background(), kcp, cluster, machines)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(joinable).To(BeFalse())
	g.Expect(reason).To(Equal("init machine has no NodeRef yet"))

	// Swap who has joined: now the init is up and the joiner is not.
	init = rolloutMachine(kcp, "test-kcp-zzzzz", rolloutNewVersion, 2*time.Hour, true)
	joiner = rolloutMachine(kcp, "test-kcp-0", rolloutNewVersion, time.Hour, false)
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rolloutEtcdStatus(cluster, init)).Build()
	r = &KairosControlPlaneReconciler{Client: c, Scheme: scheme}
	machines = []*clusterv1.Machine{joiner, init}
	sortMachinesOldestFirst(machines)
	joinable, reason, err = r.initMachineJoinable(context.Background(), kcp, cluster, machines)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(joinable).To(BeFalse())
	g.Expect(reason).To(Equal("joiner test-kcp-0 has not registered a Node yet"))
}

// With random names nothing collides with a Machine the cache has not caught
// up with yet, so the create waits until the cache shows the new Machine;
// otherwise the next reconcile would count one Machine too few and create
// another.
func TestCreateControlPlaneMachine_WaitsForTheMachineInTheCache(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	misses := 3
	gets := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineMetaInfraTemplate()).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*clusterv1.Machine); ok {
				gets++
				if gets <= misses {
					return apierrors.NewNotFound(clusterv1.GroupVersion.WithResource("machines").GroupResource(), key.Name)
				}
			}
			return cl.Get(ctx, key, obj, opts...)
		}}).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	m, err := r.createControlPlaneMachine(context.Background(), log.Log, machineMetaKCP("k3s", nil), machineMetaCluster(), bootstrapv1beta2.ControlPlaneRoleSingle, "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(m).NotTo(BeNil())
	g.Expect(gets).To(Equal(misses+1), "the create returns once a read sees the Machine")
}

// If the cache never shows the Machine in time, the error is returned for a
// requeue, but the Machine was created and nothing is deleted.
func TestCreateControlPlaneMachine_CacheWaitTimeoutKeepsTheMachine(t *testing.T) {
	g := NewWithT(t)
	orig := machineCacheWaitBackoff
	t.Cleanup(func() { machineCacheWaitBackoff = orig })
	machineCacheWaitBackoff = wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: 3}

	scheme := machineMetaScheme(g)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineMetaInfraTemplate()).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*clusterv1.Machine); ok {
				return apierrors.NewNotFound(clusterv1.GroupVersion.WithResource("machines").GroupResource(), key.Name)
			}
			return cl.Get(ctx, key, obj, opts...)
		}}).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	m, err := r.createControlPlaneMachine(context.Background(), log.Log, machineMetaKCP("k3s", nil), machineMetaCluster(), bootstrapv1beta2.ControlPlaneRoleSingle, "")
	g.Expect(err).To(MatchError(ContainSubstring("not in the cache yet")))
	g.Expect(m).NotTo(BeNil())
	configs, infra, machines := objectNames(g, c)
	g.Expect(machines).To(ConsistOf(m.Name))
	g.Expect(configs).To(ConsistOf(m.Name))
	g.Expect(infra).To(ConsistOf(m.Name))
}

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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// otherControllersHook is a pre-terminate hook another controller owns. It is
// an ordinary template annotation: only the etcd leave's own key is reserved.
const otherControllersHook = clusterv1.PreTerminateDeleteHookAnnotationPrefix + "/backup-agent"

// leaveStateKeys are the etcd-leave annotations the controller reads back as
// state (KD-92): the hook, and the leave's bookkeeping.
func leaveStateKeys() []string {
	return []string{
		etcdLeaveHookAnnotation(),
		etcdLeaveRequestedAtAnnotation,
		etcdLeaveUnreachableSinceAnnotation,
		etcdMemberLeaveRequestedAtAnnotation,
		etcdMemberLeaveMemberIDAnnotation,
	}
}

// plantingTemplate is machineTemplate.metadata that sets every reserved key,
// with a backdated request so the leave would go straight to delegation, next
// to an ordinary annotation and another controller's hook.
func plantingTemplate() *clusterv1.ObjectMeta {
	return &clusterv1.ObjectMeta{Annotations: map[string]string{
		etcdLeaveHookAnnotation():            "",
		etcdLeaveRequestedAtAnnotation:       "2020-01-01T00:00:00Z",
		etcdLeaveUnreachableSinceAnnotation:  "2020-01-01T00:00:00Z",
		etcdMemberLeaveRequestedAtAnnotation: "2020-01-01T00:00:00Z",
		etcdMemberLeaveMemberIDAnnotation:    "deadbeef",
		renderedNodeNameAnnotation:           "evil",
		otherControllersHook:                 "",
		"example.com/owner":                  "platform-team",
	}}
}

// TestReservedMachineAnnotations_Exact: the reserved set is the record, the
// leave hook and the leave's bookkeeping, nothing more. Widening it to a
// prefix would take keys away from templates that are theirs to set.
func TestReservedMachineAnnotations_Exact(t *testing.T) {
	g := NewWithT(t)
	want := map[string]bool{renderedNodeNameAnnotation: true}
	for _, k := range leaveStateKeys() {
		want[k] = true
	}
	g.Expect(reservedMachineAnnotations).To(Equal(want))
}

// TestControlPlaneMachineAnnotations_DropsTheLeaveState: no reserved key
// survives; another controller's pre-terminate hook and ordinary
// annotations do.
func TestControlPlaneMachineAnnotations_DropsTheLeaveState(t *testing.T) {
	g := NewWithT(t)
	got := controlPlaneMachineAnnotations(machineMetaKCP("k0s", plantingTemplate()))
	g.Expect(got).To(Equal(map[string]string{
		otherControllersHook: "",
		"example.com/owner":  "platform-team",
	}))
}

// TestCreateControlPlaneMachine_TemplateCannotPlantTheLeaveState: a new
// Machine carries the hook only where the controller stamps it (k0s init and
// join), with the empty value, and none of the leave's bookkeeping, whatever
// the template says. Neither does its infrastructure Machine.
func TestCreateControlPlaneMachine_TemplateCannotPlantTheLeaveState(t *testing.T) {
	for _, dist := range []string{"k0s", "k3s"} {
		for _, role := range []bootstrapv1beta2.ControlPlaneRole{bootstrapv1beta2.ControlPlaneRoleSingle, bootstrapv1beta2.ControlPlaneRoleInit, bootstrapv1beta2.ControlPlaneRoleJoin} {
			t.Run(dist+"/"+string(role), func(t *testing.T) {
				g := NewWithT(t)
				scheme := machineMetaScheme(g)
				kcp := machineMetaKCP(dist, plantingTemplate())
				c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineMetaInfraTemplate()).Build()
				r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

				created, err := r.createControlPlaneMachine(context.Background(), log.Log, kcp, machineMetaCluster(), guardFor(kcp), role, "")
				g.Expect(err).ToNot(HaveOccurred())

				machine := &clusterv1.Machine{}
				g.Expect(c.Get(context.Background(), types.NamespacedName{Name: created.Name, Namespace: "default"}, machine)).To(Succeed())
				if shouldStampEtcdLeaveHook(dist, role) {
					g.Expect(machine.Annotations).To(HaveKeyWithValue(etcdLeaveHookAnnotation(), ""))
				} else {
					g.Expect(machine.Annotations).ToNot(HaveKey(etcdLeaveHookAnnotation()), "only k0s HA Machines get a leave")
				}
				for _, k := range leaveStateKeys()[1:] {
					g.Expect(machine.Annotations).ToNot(HaveKey(k))
				}
				g.Expect(machine.Annotations).To(HaveKeyWithValue(renderedNodeNameAnnotation, created.Name))
				g.Expect(machine.Annotations).To(HaveKeyWithValue(otherControllersHook, ""))
				g.Expect(machine.Annotations).To(HaveKeyWithValue("example.com/owner", "platform-team"))

				infra := &unstructured.Unstructured{}
				infra.SetGroupVersionKind(machineMetaInfraTemplate().GroupVersionKind())
				infra.SetKind("DockerMachine")
				g.Expect(c.Get(context.Background(), types.NamespacedName{Name: created.Name, Namespace: "default"}, infra)).To(Succeed())
				for _, k := range leaveStateKeys() {
					g.Expect(infra.GetAnnotations()).ToNot(HaveKey(k))
				}
				g.Expect(infra.GetAnnotations()).To(HaveKeyWithValue("example.com/owner", "platform-team"))
			})
		}
	}
}

// TestReconcileMachineMetadata_NeverPlantsTheLeaveState: the sync onto a
// Machine that is not being deleted adds no reserved key, and keeps the hook
// the controller stamped.
func TestReconcileMachineMetadata_NeverPlantsTheLeaveState(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := machineMetaKCP("k0s", plantingTemplate())
	unhooked := machineMetaMachine("test-kcp-0", nil, nil)
	hooked := machineMetaMachine("test-kcp-1", nil, map[string]string{etcdLeaveHookAnnotation(): "", renderedNodeNameAnnotation: "test-kcp-1"})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(unhooked, hooked).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}
	g.Expect(r.reconcileMachineMetadata(context.Background(), log.Log, kcp, machineMetaCluster(),
		[]*clusterv1.Machine{unhooked.DeepCopy(), hooked.DeepCopy()})).To(Succeed())

	got := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, got)).To(Succeed())
	g.Expect(got.Annotations).To(Equal(map[string]string{otherControllersHook: "", "example.com/owner": "platform-team"}))
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "test-kcp-1", Namespace: "default"}, got)).To(Succeed())
	g.Expect(got.Annotations).To(Equal(map[string]string{
		etcdLeaveHookAnnotation(): "", renderedNodeNameAnnotation: "test-kcp-1",
		otherControllersHook: "", "example.com/owner": "platform-team",
	}))
}

// TestReconcileMachineMetadata_LeavesDeletingMachinesAlone: a Machine being
// deleted gets no metadata change at all.
//   - Another controller removed its own pre-terminate hook to let the
//     deletion go on; the template does not put it back.
//   - The etcd leave's state on it is the leave's own, written by the sweep:
//     it is not removed.
func TestReconcileMachineMetadata_LeavesDeletingMachinesAlone(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := machineMetaKCP("k0s", &clusterv1.ObjectMeta{
		Labels:      map[string]string{"tier": "platform"},
		Annotations: map[string]string{otherControllersHook: "", "example.com/owner": "platform-team"},
	})
	now := metav1.Now()
	released := machineMetaMachine("test-kcp-0", nil, map[string]string{renderedNodeNameAnnotation: "test-kcp-0"})
	released.DeletionTimestamp = &now
	released.Finalizers = []string{"test.kairos.io/hold"}
	leaveState := map[string]string{
		etcdLeaveHookAnnotation():            "",
		etcdLeaveRequestedAtAnnotation:       "2026-11-16T10:00:00Z",
		etcdLeaveUnreachableSinceAnnotation:  "2026-11-16T09:55:00Z",
		etcdMemberLeaveRequestedAtAnnotation: "2026-11-16T10:05:00Z",
		etcdMemberLeaveMemberIDAnnotation:    "abc123",
	}
	leaving := machineMetaMachine("test-kcp-1", nil, leaveState)
	leaving.DeletionTimestamp = &now
	leaving.Finalizers = []string{"test.kairos.io/hold"}

	patches := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(released, leaving).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			patches++
			return c.Patch(ctx, obj, p, opts...)
		}}).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}
	before := map[string]*clusterv1.Machine{}
	for _, m := range []*clusterv1.Machine{released, leaving} {
		stored := &clusterv1.Machine{}
		g.Expect(c.Get(context.Background(), types.NamespacedName{Name: m.Name, Namespace: "default"}, stored)).To(Succeed())
		before[m.Name] = stored
	}
	g.Expect(r.reconcileMachineMetadata(context.Background(), log.Log, kcp, machineMetaCluster(),
		[]*clusterv1.Machine{released.DeepCopy(), leaving.DeepCopy()})).To(Succeed())

	g.Expect(patches).To(BeZero())
	for name, want := range before {
		got := &clusterv1.Machine{}
		g.Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
		g.Expect(got.Labels).To(Equal(want.Labels), name)
		g.Expect(got.Annotations).To(Equal(want.Annotations), name)
	}
}

// TestReconcileMachineMetadata_ClearsLeaveStateOnLiveMachines: only the
// sweep writes the leave's bookkeeping, and only on a Machine being deleted.
// Found on a Machine that is not, each key was planted, and goes, in the
// same single patch as the template sync. The hook, the record and the
// template's own keys stay.
func TestReconcileMachineMetadata_ClearsLeaveStateOnLiveMachines(t *testing.T) {
	for _, key := range leaveBookkeepingAnnotations {
		t.Run(key, func(t *testing.T) {
			g := NewWithT(t)
			scheme := machineMetaScheme(g)
			kcp := machineMetaKCP("k0s", &clusterv1.ObjectMeta{Annotations: map[string]string{"example.com/owner": "platform-team"}})
			m := machineMetaMachine("test-kcp-0", nil, map[string]string{
				etcdLeaveHookAnnotation(): "", renderedNodeNameAnnotation: "test-kcp-0", "example.com/kept": "yes",
				key: "2020-01-01T00:00:00Z",
			})
			patches := 0
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(m).
				WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
					patches++
					return c.Patch(ctx, obj, p, opts...)
				}}).Build()
			r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}
			g.Expect(r.reconcileMachineMetadata(context.Background(), log.Log, kcp, machineMetaCluster(),
				[]*clusterv1.Machine{m.DeepCopy()})).To(Succeed())

			g.Expect(patches).To(Equal(1))
			got := &clusterv1.Machine{}
			g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, got)).To(Succeed())
			g.Expect(got.Annotations).To(Equal(map[string]string{
				etcdLeaveHookAnnotation(): "", renderedNodeNameAnnotation: "test-kcp-0", "example.com/kept": "yes",
				"example.com/owner": "platform-team",
			}))
		})
	}
}

// TestReconcileMachineMetadata_KeepsAPlantedHook: a hook on a live k3s
// Machine can only have been planted, but the hook decides (ADR 0016 D4), so
// the controller does not remove it. docs/UPGRADING.md tells the operator to.
func TestReconcileMachineMetadata_KeepsAPlantedHook(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := machineMetaKCP("k3s", nil)
	m := machineMetaMachine("test-kcp-0", nil, map[string]string{etcdLeaveHookAnnotation(): "", etcdLeaveRequestedAtAnnotation: "2020-01-01T00:00:00Z"})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(m).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}
	g.Expect(r.reconcileMachineMetadata(context.Background(), log.Log, kcp, machineMetaCluster(),
		[]*clusterv1.Machine{m.DeepCopy()})).To(Succeed())

	got := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, got)).To(Succeed())
	g.Expect(got.Annotations).To(Equal(map[string]string{etcdLeaveHookAnnotation(): ""}))
}

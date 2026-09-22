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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

func machineMetaScheme(g *WithT) *runtime.Scheme {
	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	return scheme
}

func machineMetaKCP(distribution string, md *clusterv1.ObjectMeta) *controlplanev1beta2.KairosControlPlane {
	replicas := int32(1)
	version := "v1.30.0+k3s1"
	if distribution == "k0s" {
		version = "v1.30.0+k0s.0"
	}
	return &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-kcp",
			Namespace: "default",
			UID:       types.UID("kcp-uid"),
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas:     &replicas,
			Version:      version,
			Distribution: distribution,
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "test-template",
					Namespace:  "default",
				},
				Metadata: md,
			},
		},
	}
}

func machineMetaInfraTemplate() *unstructured.Unstructured {
	infraTemplate := &unstructured.Unstructured{}
	infraTemplate.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "DockerMachineTemplate",
	})
	infraTemplate.SetName("test-template")
	infraTemplate.SetNamespace("default")
	infraTemplate.Object["spec"] = map[string]interface{}{
		"template": map[string]interface{}{
			"spec": map[string]interface{}{},
		},
	}
	return infraTemplate
}

func machineMetaCluster() *clusterv1.Cluster {
	return &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
	}
}

// TestCreateControlPlaneMachine_PropagatesTemplateMetadata is the bug itself:
// machineTemplate.metadata is documented as "metadata to apply to created
// Machines", and CAPI's Machine controller reads machine.Labels, and nothing
// else, when it decides which labels to stamp onto the Node. A label that
// reaches only the infrastructure Machine never reaches the node.
func TestCreateControlPlaneMachine_PropagatesTemplateMetadata(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)

	kcp := machineMetaKCP("k3s", &clusterv1.ObjectMeta{
		// A label CAPI actually syncs to the Node, so the test names the
		// consumer rather than an arbitrary key.
		Labels:      map[string]string{"node-role.kubernetes.io/control-plane": "", "tier": "platform"},
		Annotations: map[string]string{"example.com/owner": "platform-team"},
	})

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machineMetaInfraTemplate()).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.createControlPlaneMachine(context.Background(), log.Log, kcp, machineMetaCluster(), 0,
		bootstrapv1beta2.ControlPlaneRoleSingle)).To(Succeed())

	machine := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, machine)).To(Succeed())

	g.Expect(machine.Labels).To(HaveKeyWithValue("node-role.kubernetes.io/control-plane", ""))
	g.Expect(machine.Labels).To(HaveKeyWithValue("tier", "platform"))
	g.Expect(machine.Annotations).To(HaveKeyWithValue("example.com/owner", "platform-team"))

	// The two labels the controller selects on are still there.
	g.Expect(machine.Labels).To(HaveKeyWithValue(clusterv1.ClusterNameLabel, "test-cluster"))
	g.Expect(machine.Labels).To(HaveKey(clusterv1.MachineControlPlaneLabel))
}

// TestCreateControlPlaneMachine_InfraMachineKeepsTemplateMetadata guards the
// half that already worked, since both now go through the same two helpers.
func TestCreateControlPlaneMachine_InfraMachineKeepsTemplateMetadata(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)

	kcp := machineMetaKCP("k3s", &clusterv1.ObjectMeta{
		Labels:      map[string]string{"tier": "platform"},
		Annotations: map[string]string{"example.com/owner": "platform-team"},
	})

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machineMetaInfraTemplate()).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.createControlPlaneMachine(context.Background(), log.Log, kcp, machineMetaCluster(), 0,
		bootstrapv1beta2.ControlPlaneRoleSingle)).To(Succeed())

	infraMachine := &unstructured.Unstructured{}
	infraMachine.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "DockerMachine",
	})
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, infraMachine)).To(Succeed())

	g.Expect(infraMachine.GetLabels()).To(HaveKeyWithValue("tier", "platform"))
	g.Expect(infraMachine.GetAnnotations()).To(HaveKeyWithValue("example.com/owner", "platform-team"))
}

// TestControlPlaneMachineLabels_TemplateCannotOverrideSelectorLabels: the
// controller finds its own Machines with exactly these two labels
// (getControlPlaneMachines), so a template that spells either of them must not
// win, or the KCP would lose track of the replica it just created.
func TestControlPlaneMachineLabels_TemplateCannotOverrideSelectorLabels(t *testing.T) {
	g := NewWithT(t)

	kcp := machineMetaKCP("k3s", &clusterv1.ObjectMeta{
		Labels: map[string]string{
			clusterv1.ClusterNameLabel:         "someone-elses-cluster",
			clusterv1.MachineControlPlaneLabel: "nonsense",
		},
	})

	got := controlPlaneMachineLabels(kcp, "test-cluster")
	g.Expect(got).To(HaveKeyWithValue(clusterv1.ClusterNameLabel, "test-cluster"))
	g.Expect(got).To(HaveKeyWithValue(clusterv1.MachineControlPlaneLabel, ""))
}

// TestControlPlaneMachineLabels_NilMetadataStillLabels keeps the pointer's
// unset case (the common one) working.
func TestControlPlaneMachineLabels_NilMetadataStillLabels(t *testing.T) {
	g := NewWithT(t)

	kcp := machineMetaKCP("k3s", nil)

	g.Expect(controlPlaneMachineLabels(kcp, "test-cluster")).To(Equal(map[string]string{
		clusterv1.ClusterNameLabel:         "test-cluster",
		clusterv1.MachineControlPlaneLabel: "",
	}))
	g.Expect(controlPlaneMachineAnnotations(kcp)).To(BeEmpty())
}

// TestControlPlaneMachineLabels_DoesNotMutateTheSpec: the returned map is
// written to by the callers, so it must not alias the KCP's own map.
func TestControlPlaneMachineLabels_DoesNotMutateTheSpec(t *testing.T) {
	g := NewWithT(t)

	kcp := machineMetaKCP("k3s", &clusterv1.ObjectMeta{
		Labels: map[string]string{"tier": "platform"},
	})

	got := controlPlaneMachineLabels(kcp, "test-cluster")
	got["injected"] = "yes"

	g.Expect(kcp.Spec.MachineTemplate.Metadata.Labels).To(Equal(map[string]string{"tier": "platform"}))
}

// TestCreateControlPlaneMachine_EtcdHookWinsOverTemplateAnnotation: the
// pre-terminate hook is stamped after the template metadata, and its empty
// value is the CAPI convention for "awaiting external completion". A template
// that spelled the same key with a value would otherwise make CAPI treat the
// hook as already done.
func TestCreateControlPlaneMachine_EtcdHookWinsOverTemplateAnnotation(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)

	kcp := machineMetaKCP("k0s", &clusterv1.ObjectMeta{
		Annotations: map[string]string{etcdLeaveHookAnnotation(): "hijacked"},
	})

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machineMetaInfraTemplate()).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.createControlPlaneMachine(context.Background(), log.Log, kcp, machineMetaCluster(), 0,
		bootstrapv1beta2.ControlPlaneRoleInit)).To(Succeed())

	machine := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, machine)).To(Succeed())

	g.Expect(machine.Annotations).To(HaveKeyWithValue(etcdLeaveHookAnnotation(), ""))
}

func machineMetaMachine(name string, labels, annotations map[string]string) *clusterv1.Machine {
	base := map[string]string{
		clusterv1.ClusterNameLabel:         "test-cluster",
		clusterv1.MachineControlPlaneLabel: "",
	}
	for k, v := range labels {
		base[k] = v
	}
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Labels:      base,
			Annotations: annotations,
		},
		Spec: clusterv1.MachineSpec{ClusterName: "test-cluster"},
	}
}

// TestReconcileMachineMetadata_ConvergesExistingMachines is the half that
// matters in practice: labeling a control plane is something an operator does
// after the cluster is up, and this controller never rewrites a Machine
// otherwise, so without this the edit would wait for a rollout that may never
// come.
func TestReconcileMachineMetadata_ConvergesExistingMachines(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)

	kcp := machineMetaKCP("k3s", &clusterv1.ObjectMeta{
		Labels:      map[string]string{"node-role.kubernetes.io/ingress": "", "tier": "platform"},
		Annotations: map[string]string{"example.com/owner": "platform-team"},
	})

	// One Machine with nothing, one that carries an older value for the same key.
	fresh := machineMetaMachine("test-kcp-0", nil, nil)
	stale := machineMetaMachine("test-kcp-1", map[string]string{"tier": "old"}, nil)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fresh, stale).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	machines := []*clusterv1.Machine{fresh.DeepCopy(), stale.DeepCopy()}
	g.Expect(r.reconcileMachineMetadata(context.Background(), log.Log, kcp, machineMetaCluster(), machines)).To(Succeed())

	for _, name := range []string{"test-kcp-0", "test-kcp-1"} {
		got := &clusterv1.Machine{}
		g.Expect(c.Get(context.Background(),
			types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
		g.Expect(got.Labels).To(HaveKeyWithValue("node-role.kubernetes.io/ingress", ""), name)
		g.Expect(got.Labels).To(HaveKeyWithValue("tier", "platform"), name)
		g.Expect(got.Annotations).To(HaveKeyWithValue("example.com/owner", "platform-team"), name)
	}
}

// TestReconcileMachineMetadata_KeepsMetadataItDoesNotOwn: the merge is
// additive because the etcd-leave hook and CAPI's own annotations live on the
// same object. Stripping the hook off a terminating Machine would release a
// member CAPI is holding for a clean `k0s etcd leave`.
func TestReconcileMachineMetadata_KeepsMetadataItDoesNotOwn(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)

	kcp := machineMetaKCP("k0s", &clusterv1.ObjectMeta{
		Labels: map[string]string{"tier": "platform"},
	})

	now := metav1.Now()
	terminating := machineMetaMachine("test-kcp-0",
		map[string]string{"set-by-a-machinehealthcheck": "yes"},
		map[string]string{etcdLeaveHookAnnotation(): ""})
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"test.kairos.io/hold"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(terminating).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.reconcileMachineMetadata(context.Background(), log.Log, kcp, machineMetaCluster(),
		[]*clusterv1.Machine{terminating.DeepCopy()})).To(Succeed())

	got := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, got)).To(Succeed())

	g.Expect(got.Annotations).To(HaveKeyWithValue(etcdLeaveHookAnnotation(), ""))
	g.Expect(got.Labels).To(HaveKeyWithValue("set-by-a-machinehealthcheck", "yes"))
	g.Expect(got.Labels).To(HaveKeyWithValue("tier", "platform"))
}

// TestReconcileMachineMetadata_NoTemplateMetadataIsANoOp keeps the common case
// from issuing a patch per Machine per reconcile.
func TestReconcileMachineMetadata_NoTemplateMetadataIsANoOp(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)

	kcp := machineMetaKCP("k3s", nil)
	machine := machineMetaMachine("test-kcp-0", nil, nil)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	before := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, before)).To(Succeed())

	g.Expect(r.reconcileMachineMetadata(context.Background(), log.Log, kcp, machineMetaCluster(),
		[]*clusterv1.Machine{machine.DeepCopy()})).To(Succeed())

	after := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, after)).To(Succeed())

	// No patch issued, so the fake client's resourceVersion did not move.
	g.Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
}

// TestReconcileMachines_SweepsMachineMetadata covers the wiring rather than the
// helper: reconcileMachines is the only thing that runs on a steady-state
// cluster, so if the sweep is not called from there an edited label still never
// lands, however correct the helper is.
func TestReconcileMachines_SweepsMachineMetadata(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)

	kcp := machineMetaKCP("k3s", &clusterv1.ObjectMeta{
		Labels: map[string]string{"tier": "platform"},
	})

	// Owned by the KCP and already at the desired version, so the replica count
	// is satisfied and nothing scales: the sweep is the only thing that acts.
	machine := machineMetaMachine("test-kcp-0", nil, nil)
	machine.Spec.Version = kcp.Spec.Version
	machine.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: controlplanev1beta2.GroupVersion.String(),
		Kind:       "KairosControlPlane",
		Name:       kcp.Name,
		UID:        kcp.UID,
		Controller: ptrBool(true),
	}}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine, machineMetaInfraTemplate()).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	_, err := r.reconcileMachines(context.Background(), log.Log, kcp, machineMetaCluster())
	g.Expect(err).NotTo(HaveOccurred())

	got := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, got)).To(Succeed())
	g.Expect(got.Labels).To(HaveKeyWithValue("tier", "platform"))
}

func ptrBool(v bool) *bool { return &v }

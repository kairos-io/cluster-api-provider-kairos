package controlplane

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// ownerTestCluster is the Cluster every template below should end up owned by.
func ownerTestCluster() *clusterv1.Cluster {
	return &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "owned-cluster", Namespace: "default", UID: types.UID("cluster-uid-1")},
	}
}

func ownerTestKCP() *controlplanev1beta2.KairosControlPlane {
	return &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "owned-kcp",
			Namespace: "default",
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "owned-cluster"},
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas: ptr.To(int32(1)),
			Version:  "v1.30.0+k0s.0",
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "infra-tmpl",
					Namespace:  "default",
				},
			},
			KairosConfigTemplate: controlplanev1beta2.KairosConfigTemplateReference{Name: "config-tmpl"},
		},
	}
}

func infraTemplateObj(owners ...metav1.OwnerReference) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta1")
	u.SetKind("DockerMachineTemplate")
	u.SetName("infra-tmpl")
	u.SetNamespace("default")
	if len(owners) > 0 {
		u.SetOwnerReferences(owners)
	}
	return u
}

// clusterctl move carries an object only if it is linked to the Cluster through
// an OwnerReference chain. A real move between two management clusters left both
// templates behind and the moved control plane referenced objects that did not
// exist on the target.
func TestReconcileTemplateOwnerRefs_AddsClusterOwnerToBothTemplates(t *testing.T) {
	g := NewWithT(t)
	scheme := newKCPTestScheme(t)

	cluster, kcp := ownerTestCluster(), ownerTestKCP()
	configTmpl := &bootstrapv1beta2.KairosConfigTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "config-tmpl", Namespace: "default"},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster, kcp, configTmpl, infraTemplateObj()).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.reconcileTemplateOwnerRefs(context.Background(), kcp, cluster)).To(Succeed())

	gotInfra := infraTemplateObj()
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(gotInfra), gotInfra)).To(Succeed())
	g.Expect(gotInfra.GetOwnerReferences()).To(HaveLen(1))
	g.Expect(gotInfra.GetOwnerReferences()[0].Kind).To(Equal("Cluster"))
	g.Expect(gotInfra.GetOwnerReferences()[0].Name).To(Equal("owned-cluster"))
	g.Expect(gotInfra.GetOwnerReferences()[0].UID).To(Equal(types.UID("cluster-uid-1")))
	g.Expect(gotInfra.GetOwnerReferences()[0].Controller).To(BeNil(),
		"a template is shared by design; an exclusive controller reference would make a second referrer fail")

	gotCfg := &bootstrapv1beta2.KairosConfigTemplate{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "config-tmpl", Namespace: "default"}, gotCfg)).To(Succeed())
	g.Expect(gotCfg.GetOwnerReferences()).To(HaveLen(1),
		"the bootstrap template carries only a name, so its group/kind must be defaulted and it must be owned too")
	g.Expect(gotCfg.GetOwnerReferences()[0].Name).To(Equal("owned-cluster"))
}

// Kubernetes collects an object only once every owner is gone, so a template
// shared between clusters must accumulate owners rather than have one replace
// another. This is why the reference is an ordinary owner ref.
func TestReconcileTemplateOwnerRefs_SharedTemplateAccumulatesOwners(t *testing.T) {
	g := NewWithT(t)
	scheme := newKCPTestScheme(t)

	first := metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Cluster",
		Name:       "other-cluster",
		UID:        types.UID("cluster-uid-0"),
	}
	cluster, kcp := ownerTestCluster(), ownerTestKCP()
	kcp.Spec.KairosConfigTemplate.Name = ""

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster, kcp, infraTemplateObj(first)).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.reconcileTemplateOwnerRefs(context.Background(), kcp, cluster)).To(Succeed())

	got := infraTemplateObj()
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(got), got)).To(Succeed())
	g.Expect(got.GetOwnerReferences()).To(HaveLen(2),
		"the existing owner must survive: a template shared by two clusters must not be collected when the first is deleted")
	names := []string{got.GetOwnerReferences()[0].Name, got.GetOwnerReferences()[1].Name}
	g.Expect(names).To(ConsistOf("other-cluster", "owned-cluster"))
}

// Running twice must not duplicate the reference.
func TestReconcileTemplateOwnerRefs_IsIdempotent(t *testing.T) {
	g := NewWithT(t)
	scheme := newKCPTestScheme(t)

	cluster, kcp := ownerTestCluster(), ownerTestKCP()
	kcp.Spec.KairosConfigTemplate.Name = ""

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster, kcp, infraTemplateObj()).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	for range 3 {
		g.Expect(r.reconcileTemplateOwnerRefs(context.Background(), kcp, cluster)).To(Succeed())
	}

	got := infraTemplateObj()
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(got), got)).To(Succeed())
	g.Expect(got.GetOwnerReferences()).To(HaveLen(1))
}

// A missing template is the clone path's error to report, with far better
// context than this helper could give. It must not fail the reconcile here.
func TestReconcileTemplateOwnerRefs_ToleratesMissingTemplate(t *testing.T) {
	g := NewWithT(t)
	scheme := newKCPTestScheme(t)

	cluster, kcp := ownerTestCluster(), ownerTestKCP()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, kcp).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.reconcileTemplateOwnerRefs(context.Background(), kcp, cluster)).To(Succeed())
}

// The same field can name a concrete InfraMachine rather than a template; that
// object is owned by its Machine, not the Cluster, so it must be left alone.
func TestReconcileTemplateOwnerRefs_IgnoresNonTemplateKinds(t *testing.T) {
	g := NewWithT(t)
	scheme := newKCPTestScheme(t)

	cluster, kcp := ownerTestCluster(), ownerTestKCP()
	kcp.Spec.MachineTemplate.InfrastructureRef.Kind = "DockerMachine"
	kcp.Spec.MachineTemplate.InfrastructureRef.Name = "a-machine"
	kcp.Spec.KairosConfigTemplate.Name = ""

	machine := &unstructured.Unstructured{}
	machine.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta1")
	machine.SetKind("DockerMachine")
	machine.SetName("a-machine")
	machine.SetNamespace("default")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, kcp, machine).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.reconcileTemplateOwnerRefs(context.Background(), kcp, cluster)).To(Succeed())

	got := machine.DeepCopy()
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(got), got)).To(Succeed())
	g.Expect(got.GetOwnerReferences()).To(BeEmpty(), "a concrete InfraMachine is owned by its Machine, not the Cluster")
}

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
	"reflect"
	"testing"

	. "github.com/onsi/gomega"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// legacyBroadNodePushRules is the pre-PR-3 node Role: an unscoped create rule,
// get/update/patch on the CONTRACT + join-token + etcd-status Secrets, and the
// VMI get.
func legacyBroadNodePushRules(clusterName string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}},
		{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			ResourceNames: []string{
				clusterName + "-kubeconfig", // the CONTRACT name (dropped by migration)
				bootstrapv1beta2.ControlPlaneJoinTokenSecretName(clusterName),
				bootstrapv1beta2.EtcdStatusSecretName(clusterName),
			},
			Verbs: []string{"get", "update", "patch"},
		},
		{APIGroups: []string{"kubevirt.io"}, Resources: []string{"virtualmachineinstances"}, Verbs: []string{"get"}},
	}
}

// TestIntersectNodePushRules_NarrowsLegacyBroadRole is the core intersection
// proof: create is dropped, the contract name is dropped, get/update are dropped,
// the join-token grant is dropped (ADR 0013), staging is NOT added (old never
// granted it), and patch on etcd-status and the VMI get survive.
func TestIntersectNodePushRules_NarrowsLegacyBroadRole(t *testing.T) {
	g := NewWithT(t)
	got := intersectNodePushRules(legacyBroadNodePushRules("c"), "c")

	g.Expect(ruleWithVerbOnName(got, "create", "")).To(BeFalse(), "create must be dropped")
	g.Expect(anyRuleGrants(got, "secrets", "c-kubeconfig", "patch")).To(BeFalse(), "the contract name must be dropped")
	g.Expect(anyRuleGrants(got, "secrets", bootstrapv1beta2.KubeconfigPushSecretName("c"), "patch")).
		To(BeFalse(), "staging must NOT be added (old did not grant patch on it)")
	g.Expect(anyRuleGrants(got, "secrets", bootstrapv1beta2.ControlPlaneJoinTokenSecretName("c"), "patch")).To(BeFalse(), "join-token grant must be dropped (ADR 0013)")
	g.Expect(anyRuleGrants(got, "secrets", bootstrapv1beta2.EtcdStatusSecretName("c"), "patch")).To(BeTrue())
	g.Expect(anyRuleGrants(got, "secrets", bootstrapv1beta2.EtcdStatusSecretName("c"), "get")).To(BeFalse(), "get must be dropped")
	g.Expect(anyRuleGrants(got, "secrets", bootstrapv1beta2.EtcdStatusSecretName("c"), "update")).To(BeFalse(), "update must be dropped")
	g.Expect(roleGrantsVMIGet(&rbacv1.Role{Rules: got})).To(BeTrue(), "VMI get must survive")
}

// TestIntersectNodePushRules_NeverBroadens: a minimal Role (patch on the
// etcd-status Secret only) is not expanded to staging, join-token, or VMI.
func TestIntersectNodePushRules_NeverBroadens(t *testing.T) {
	g := NewWithT(t)
	minimal := []rbacv1.PolicyRule{{
		APIGroups:     []string{""},
		Resources:     []string{"secrets"},
		ResourceNames: []string{bootstrapv1beta2.EtcdStatusSecretName("c")},
		Verbs:         []string{"patch"},
	}}
	got := intersectNodePushRules(minimal, "c")

	g.Expect(anyRuleGrants(got, "secrets", bootstrapv1beta2.EtcdStatusSecretName("c"), "patch")).To(BeTrue())
	g.Expect(anyRuleGrants(got, "secrets", bootstrapv1beta2.KubeconfigPushSecretName("c"), "patch")).To(BeFalse(), "staging must not be added")
	g.Expect(anyRuleGrants(got, "secrets", bootstrapv1beta2.ControlPlaneJoinTokenSecretName("c"), "patch")).To(BeFalse(), "join-token must not be added")
	g.Expect(roleGrantsVMIGet(&rbacv1.Role{Rules: got})).To(BeFalse(), "VMI must not be added")
}

// TestIntersectNodePushRules_DropsJoinTokenOnlyRole: a Role that granted only
// the join-token Secret (the pre-ADR-0013 shape) intersects to nothing.
func TestIntersectNodePushRules_DropsJoinTokenOnlyRole(t *testing.T) {
	g := NewWithT(t)
	joinOnly := []rbacv1.PolicyRule{{
		APIGroups:     []string{""},
		Resources:     []string{"secrets"},
		ResourceNames: []string{bootstrapv1beta2.ControlPlaneJoinTokenSecretName("c")},
		Verbs:         []string{"get", "update", "patch"},
	}}
	g.Expect(intersectNodePushRules(joinOnly, "c")).To(BeEmpty())
}

// TestIntersectNodePushRules_AlreadyMigratedIsNoOp: intersecting the reduced set
// (what the resolver now emits) with the allowed set returns it unchanged, so the
// migrator issues no patch for already-migrated Roles.
func TestIntersectNodePushRules_AlreadyMigratedIsNoOp(t *testing.T) {
	g := NewWithT(t)
	reduced := nodePushAllowedRules("c")
	got := intersectNodePushRules(reduced, "c")
	g.Expect(reflect.DeepEqual(got, reduced)).To(BeTrue(), "the reduced set is a fixed point of the intersection")
}

// TestTargetNodePushCluster covers the owner + name match gate.
func TestTargetNodePushCluster(t *testing.T) {
	g := NewWithT(t)
	ctlTrue := true
	writerName := kubeconfigWriterName("c")

	clusterOwned := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
		Name: writerName, Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster", Name: "c", UID: "u", Controller: &ctlTrue}},
	}}
	name, ok := targetNodePushCluster(clusterOwned)
	g.Expect(ok).To(BeTrue())
	g.Expect(name).To(Equal("c"))

	// Cluster owner, wrong Role name → not a target.
	wrongName := clusterOwned.DeepCopy()
	wrongName.Name = "kairos-kubeconfig-writer-other"
	_, ok = targetNodePushCluster(wrongName)
	g.Expect(ok).To(BeFalse())

	// Foreign owner kind → not a target.
	foreign := clusterOwned.DeepCopy()
	foreign.OwnerReferences[0].Kind = "Deployment"
	foreign.OwnerReferences[0].APIVersion = "apps/v1"
	_, ok = targetNodePushCluster(foreign)
	g.Expect(ok).To(BeFalse())

	// No controller owner → not a target.
	noOwner := clusterOwned.DeepCopy()
	noOwner.OwnerReferences = nil
	_, ok = targetNodePushCluster(noOwner)
	g.Expect(ok).To(BeFalse())

	// Legacy KairosConfig owner + cluster-name label + matching name → target.
	legacy := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
		Name: writerName, Namespace: "ns",
		Labels:          map[string]string{clusterv1.ClusterNameLabel: "c"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: bootstrapv1beta2.GroupVersion.String(), Kind: "KairosConfig", Name: "c-init", UID: "u2", Controller: &ctlTrue}},
	}}
	name, ok = targetNodePushCluster(legacy)
	g.Expect(ok).To(BeTrue())
	g.Expect(name).To(Equal("c"))

	// Legacy KairosConfig owner without the cluster-name label → not a target.
	legacyNoLabel := legacy.DeepCopy()
	legacyNoLabel.Labels = nil
	_, ok = targetNodePushCluster(legacyNoLabel)
	g.Expect(ok).To(BeFalse())
}

// TestNodePushRoleMigrator_Start drives the run-once migrator over a fake client:
// an owner+name-matched broad Role is narrowed; a foreign-owner Role and a
// name-mismatched Role are left untouched.
func TestNodePushRoleMigrator_Start(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(rbacv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	ctlTrue := true

	clusterOwner := metav1.OwnerReference{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster", Name: "c", UID: "u", Controller: &ctlTrue}
	deployOwner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "ud", Controller: &ctlTrue}

	target := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: kubeconfigWriterName("c"), Namespace: "ns", Labels: map[string]string{clusterv1.ClusterNameLabel: "c"}, OwnerReferences: []metav1.OwnerReference{clusterOwner}},
		Rules:      legacyBroadNodePushRules("c"),
	}
	foreignOwned := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: kubeconfigWriterName("d"), Namespace: "ns", OwnerReferences: []metav1.OwnerReference{deployOwner}},
		Rules:      legacyBroadNodePushRules("d"),
	}
	wrongName := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-role", Namespace: "ns", OwnerReferences: []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster", Name: "e", UID: "ue", Controller: &ctlTrue}}},
		Rules:      legacyBroadNodePushRules("e"),
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target, foreignOwned, wrongName).Build()
	m := NewNodePushRoleMigrator(c, c)
	g.Expect(m.Start(context.Background())).To(Succeed())

	// Target narrowed.
	gotTarget := &rbacv1.Role{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: kubeconfigWriterName("c"), Namespace: "ns"}, gotTarget)).To(Succeed())
	g.Expect(roleGrantsSecretCreate(gotTarget)).To(BeFalse(), "create must be removed from the target Role")
	g.Expect(anyRuleGrants(gotTarget.Rules, "secrets", "c-kubeconfig", "patch")).To(BeFalse(), "contract name removed")
	g.Expect(anyRuleGrants(gotTarget.Rules, "secrets", bootstrapv1beta2.ControlPlaneJoinTokenSecretName("c"), "patch")).To(BeFalse(), "join-token grant removed (ADR 0013)")
	g.Expect(anyRuleGrants(gotTarget.Rules, "secrets", bootstrapv1beta2.EtcdStatusSecretName("c"), "patch")).To(BeTrue())
	g.Expect(anyRuleGrants(gotTarget.Rules, "secrets", bootstrapv1beta2.EtcdStatusSecretName("c"), "get")).To(BeFalse())

	// Foreign-owner Role untouched (still has create).
	gotForeign := &rbacv1.Role{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: kubeconfigWriterName("d"), Namespace: "ns"}, gotForeign)).To(Succeed())
	g.Expect(reflect.DeepEqual(gotForeign.Rules, legacyBroadNodePushRules("d"))).To(BeTrue(), "a foreign-owner Role must be untouched")

	// Name-mismatched Role untouched.
	gotWrong := &rbacv1.Role{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "unrelated-role", Namespace: "ns"}, gotWrong)).To(Succeed())
	g.Expect(reflect.DeepEqual(gotWrong.Rules, legacyBroadNodePushRules("e"))).To(BeTrue(), "a name-mismatched Role must be untouched")
}

// anyRuleGrants reports whether any rule in the set grants verb on the named core
// Secret (resourceNames-scoped).
func anyRuleGrants(rules []rbacv1.PolicyRule, resource, name, verb string) bool {
	return roleGrantsNamedResource(&rbacv1.Role{Rules: rules}, resource, name, verb)
}

func roleGrantsNamedResource(role *rbacv1.Role, resource, name, verb string) bool {
	for _, rule := range role.Rules {
		if !contains(rule.APIGroups, "") {
			continue
		}
		if !contains(rule.Resources, resource) {
			continue
		}
		if !contains(rule.ResourceNames, name) {
			continue
		}
		if contains(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

// ruleWithVerbOnName reports whether any rule carries verb with no resourceNames
// (name == "") — used to assert the create rule is gone.
func ruleWithVerbOnName(rules []rbacv1.PolicyRule, verb, name string) bool {
	for _, rule := range rules {
		if name == "" && len(rule.ResourceNames) != 0 {
			continue
		}
		if contains(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

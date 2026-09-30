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
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// NodePushRoleMigrator narrows pre-PR-3 node-push Roles to the reduced rule set at
// bootstrap-manager startup, so KD-60/A1 closes for already-running clusters the
// instant the new manager takes leadership — without waiting for each cluster to
// re-render (which the resolver would otherwise do lazily).
//
// It is a leader-elected, run-once Runnable in the bootstrap manager only. It
// reads Roles through an UNCACHED reader (the manager cache may be namespace
// scoped, and we list cluster-wide) and writes through the cached client. It
// ignores Cluster pause because it only REMOVES permissions (ADR 0011 OQ-7).
//
// Safety (KD-65, confused-deputy): it touches a Role only when
//   - the Role has a controller owner that is a Cluster (or the legacy
//     pre-ADR-0005-D.2 KairosConfig) of the expected API group, AND
//   - the Role name equals kubeconfigWriterName(<cluster>),
//
// and it sets rules to old-INTERSECT-allowed, never adding a rule/verb/name. The
// manager can create and bind Roles, so the apiserver's escalation check would not
// stop an additive bug; the intersection is the guard, so it must never broaden.
type NodePushRoleMigrator struct {
	reader client.Reader
	writer client.Client
}

// NewNodePushRoleMigrator constructs the migrator. main.go wires
// mgr.GetAPIReader() (uncached, cluster-wide reads) and mgr.GetClient().
func NewNodePushRoleMigrator(reader client.Reader, writer client.Client) *NodePushRoleMigrator {
	return &NodePushRoleMigrator{reader: reader, writer: writer}
}

// NeedLeaderElection makes the migrator run only on the elected leader (and, when
// leader election is disabled, unconditionally like every other runnable).
func (m *NodePushRoleMigrator) NeedLeaderElection() bool { return true }

// Start runs the one-shot migration and returns. A per-Role patch failure is
// logged and skipped; it never fails manager startup (best-effort hardening).
func (m *NodePushRoleMigrator) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("node-push-role-migrator")
	roleList := &rbacv1.RoleList{}
	if err := m.reader.List(ctx, roleList); err != nil {
		// A list failure at startup is not fatal: the resolver still rewrites each
		// Role to the reduced set on the next reconcile. Log and move on.
		log.Error(err, "Failed to list Roles for node-push migration; the reduced rule set will still be applied lazily on the next reconcile")
		return nil
	}
	migrated := 0
	for i := range roleList.Items {
		role := &roleList.Items[i]
		clusterName, ok := targetNodePushCluster(role)
		if !ok {
			continue
		}
		newRules := intersectNodePushRules(role.Rules, clusterName)
		if reflect.DeepEqual(role.Rules, newRules) {
			continue
		}
		base := role.DeepCopy()
		role.Rules = newRules
		if err := m.writer.Patch(ctx, role, client.MergeFrom(base)); err != nil {
			log.Error(err, "Failed to narrow legacy node-push Role", "role", role.Namespace+"/"+role.Name)
			continue
		}
		migrated++
		log.Info("Narrowed legacy node-push Role to the reduced rule set", "role", role.Namespace+"/"+role.Name, "cluster", clusterName)
	}
	if migrated > 0 {
		log.Info("Node-push Role migration complete", "narrowed", migrated)
	}
	return nil
}

// targetNodePushCluster reports the cluster name a Role belongs to, and whether
// the Role is an owner-and-name-matched node-push Role this migrator may touch.
func targetNodePushCluster(role *rbacv1.Role) (string, bool) {
	owner := metav1.GetControllerOfNoCopy(role)
	if owner == nil {
		return "", false
	}
	switch owner.Kind {
	case "Cluster":
		if owner.APIVersion != "" && !groupMatches(owner.APIVersion, clusterv1.GroupVersion.Group) {
			return "", false
		}
		clusterName := owner.Name
		if clusterName == "" || role.Name != kubeconfigWriterName(clusterName) {
			return "", false
		}
		return clusterName, true
	case "KairosConfig":
		// Pre-ADR-0005-D.2 legacy owner: the init KairosConfig owned the Role. The
		// cluster name is not the owner name, so derive it from the cluster-name
		// label the resolver stamps, and require the name to still match.
		if owner.APIVersion != "" && !groupMatches(owner.APIVersion, bootstrapv1beta2.GroupVersion.Group) {
			return "", false
		}
		clusterName := role.Labels[clusterv1.ClusterNameLabel]
		if clusterName == "" || role.Name != kubeconfigWriterName(clusterName) {
			return "", false
		}
		return clusterName, true
	default:
		return "", false
	}
}

// nodePushAllowedRules is the ceiling the migrator intersects against: it is a
// superset of every grant the resolver emits (secrets:patch on the two named
// Secrets, plus the conditional VMI get). The join-token Secret is deliberately
// absent: the KairosControlPlane is its only writer (ADR 0013), so an older
// Role's join-token grant is dropped. Intersecting an existing Role with this
// set can only ever remove rules/verbs/names, never add them.
func nodePushAllowedRules(clusterName string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			ResourceNames: []string{
				bootstrapv1beta2.KubeconfigPushSecretName(clusterName),
				bootstrapv1beta2.EtcdStatusSecretName(clusterName),
			},
			Verbs: []string{"patch"},
		},
		{
			APIGroups: []string{"kubevirt.io"},
			Resources: []string{"virtualmachineinstances"},
			Verbs:     []string{"get"},
		},
	}
}

// intersectNodePushRules returns old ∩ allowed: for each allowed rule it keeps
// only the (resourceName, verb) tuples the old rules actually grant. The output is
// a subset of nodePushAllowedRules, so it can never broaden; it is deterministic
// (allowed order, allowed resourceName order).
func intersectNodePushRules(old []rbacv1.PolicyRule, clusterName string) []rbacv1.PolicyRule {
	var out []rbacv1.PolicyRule
	for _, a := range nodePushAllowedRules(clusterName) {
		group := a.APIGroups[0]
		resource := a.Resources[0]

		if len(a.ResourceNames) == 0 {
			// Cluster-wide / all-names grant (VMI get).
			kept := intersectVerbs(a.Verbs, grantedVerbs(old, group, resource, ""))
			if len(kept) > 0 {
				out = append(out, rbacv1.PolicyRule{
					APIGroups: []string{group},
					Resources: []string{resource},
					Verbs:     kept,
				})
			}
			continue
		}

		// Named grant (secrets patch): bucket resourceNames by their surviving verb
		// set so distinct verb sets do not silently merge (defensive — the allowed
		// verb set here is homogeneous).
		buckets := map[string][]string{}
		var order []string
		for _, name := range a.ResourceNames {
			kept := intersectVerbs(a.Verbs, grantedVerbs(old, group, resource, name))
			if len(kept) == 0 {
				continue
			}
			key := strings.Join(kept, ",")
			if _, ok := buckets[key]; !ok {
				order = append(order, key)
			}
			buckets[key] = append(buckets[key], name)
		}
		for _, key := range order {
			out = append(out, rbacv1.PolicyRule{
				APIGroups:     []string{group},
				Resources:     []string{resource},
				ResourceNames: buckets[key],
				Verbs:         strings.Split(key, ","),
			})
		}
	}
	return out
}

// grantedVerbs returns the set of verbs the old rules grant for
// (group, resource, name). A rule with no resourceNames grants on all names; a
// query for name=="" asks only about such all-names rules.
func grantedVerbs(old []rbacv1.PolicyRule, group, resource, name string) map[string]bool {
	out := map[string]bool{}
	for _, r := range old {
		if !ruleHas(r.APIGroups, group) {
			continue
		}
		if !ruleHas(r.Resources, resource) {
			continue
		}
		if name == "" {
			if len(r.ResourceNames) != 0 {
				continue
			}
		} else if len(r.ResourceNames) != 0 && !contains(r.ResourceNames, name) {
			continue
		}
		for _, v := range r.Verbs {
			out[v] = true
		}
	}
	return out
}

// intersectVerbs keeps the allowed verbs that granted (or a "*" grant) covers.
func intersectVerbs(allowed []string, granted map[string]bool) []string {
	var out []string
	for _, v := range allowed {
		if granted[v] || granted["*"] {
			out = append(out, v)
		}
	}
	return out
}

// ruleHas reports membership with the RBAC wildcard "*" honored. For the group
// slot, "" is the core group and matches only "" (or "*").
func ruleHas(list []string, want string) bool {
	for _, e := range list {
		if e == want || e == "*" {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, e := range list {
		if e == want {
			return true
		}
	}
	return false
}

// groupMatches reports whether an ownerReference APIVersion ("<group>/<version>"
// or "<version>") belongs to the wanted API group.
func groupMatches(apiVersion, group string) bool {
	slash := strings.IndexByte(apiVersion, '/')
	if slash < 0 {
		// No group segment (core group ""): matches only the empty group.
		return group == ""
	}
	return apiVersion[:slash] == group
}

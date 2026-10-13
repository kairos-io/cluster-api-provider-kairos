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
	"os"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// rbacRole is the minimal shape of a generated ClusterRole we need to assert on.
type rbacRole struct {
	Rules []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	} `yaml:"rules"`
}

// TestBootstrapRoleGrantsControlPlaneRead guards the envtest-masks-RBAC gap (the
// security review's REQUIRED 1): the kubeadm trust check does a get on the Cluster's
// control-plane object, but envtest runs with an admin client so a missing grant is
// invisible there. A bootstrap-only (--controllers=bootstrap) Deployment has none of
// the control-plane manager's RBAC, so the grant MUST be present in the bootstrap
// split role or the trust-check Get returns Forbidden and no kubeadm worker joins.
//
// This asserts the generated bootstrap ClusterRole carries get on
// kamajicontrolplanes/kubeadmcontrolplanes/kairoscontrolplanes in the
// controlplane.cluster.x-k8s.io group. It fails if a future `make manifests` drops
// the marker.
func TestBootstrapRoleGrantsControlPlaneRead(t *testing.T) {
	const rolePath = "../../../config/rbac/bootstrap/role.yaml"
	data, err := os.ReadFile(rolePath)
	if err != nil {
		t.Fatalf("read bootstrap role %s: %v", rolePath, err)
	}
	var role rbacRole
	if err := yaml.Unmarshal(data, &role); err != nil {
		t.Fatalf("unmarshal bootstrap role: %v", err)
	}

	want := []string{"kamajicontrolplanes", "kubeadmcontrolplanes", "kairoscontrolplanes"}
	for _, res := range want {
		if !roleAllows(role, "controlplane.cluster.x-k8s.io", res, "get") {
			t.Errorf("bootstrap role is missing get on controlplane.cluster.x-k8s.io/%s — "+
				"the kubeadm trust check will be Forbidden on a --controllers=bootstrap Deployment", res)
		}
	}
}

// roleAllows reports whether role has a rule granting verb on group/resource.
func roleAllows(role rbacRole, group, resource, verb string) bool {
	for _, r := range role.Rules {
		if !contains(r.APIGroups, group) || !contains(r.Resources, resource) {
			continue
		}
		if contains(r.Verbs, verb) {
			return true
		}
	}
	return false
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

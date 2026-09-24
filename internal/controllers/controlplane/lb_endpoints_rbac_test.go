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
	"os"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/gomega"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// Under clusterctl each provider runs with only the RBAC generated from its own
// package's markers. The control-plane controller reads KubeVirt VMIs to fill
// the CAPK API load balancer, but only the bootstrap package declared that
// rule. The flat install merges both roles, which hid the gap, so this checks
// the generated control-plane role itself: dropping the marker regenerates the
// role without the rule and fails here, not in a lab.
func TestControlPlaneRoleCanReadVirtualMachineInstances(t *testing.T) {
	g := NewWithT(t)

	raw, err := os.ReadFile("../../../config/rbac/control-plane/role.yaml")
	g.Expect(err).NotTo(HaveOccurred())
	role := &rbacv1.ClusterRole{}
	g.Expect(yaml.Unmarshal(raw, role)).To(Succeed())

	granted := false
	for _, rule := range role.Rules {
		if contains(rule.APIGroups, "kubevirt.io") &&
			contains(rule.Resources, "virtualmachineinstances") &&
			contains(rule.Verbs, "get") {
			granted = true
		}
	}
	g.Expect(granted).To(BeTrue(),
		"control-plane-manager-role must allow get on kubevirt.io/virtualmachineinstances (getKubevirtVMIIP)")
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// A forbidden VMI read is not "no IP yet": the backend will never be added and
// the load balancer stays empty. It used to be logged only at V(4), which is
// why the missing rule went unnoticed, so it must now show at the default level.
func TestEnsureControlPlaneLBEndpoints_ReportsAForbiddenVMIRead(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := machineMetaKCP("k0s", nil)

	machine := machineMetaMachine("test-kcp-0", nil, nil)
	machine.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
		APIGroup: "infrastructure.cluster.x-k8s.io",
		Kind:     "KubevirtMachine",
		Name:     "test-kcp-0",
	}
	machine.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: controlplanev1beta2.GroupVersion.String(),
		Kind:       "KairosControlPlane",
		Name:       kcp.Name,
		UID:        kcp.UID,
		Controller: ptrBool(true),
	}}

	vmi := schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachineinstances"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == "VirtualMachineInstance" {
					return apierrors.NewForbidden(vmi, key.Name, errors.New("RBAC: access denied"))
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	// Default verbosity: V(4) lines are dropped, as they are in a real deployment.
	var logged []string
	logger := funcr.New(func(prefix, args string) { logged = append(logged, args) }, funcr.Options{})

	g.Expect(r.ensureControlPlaneLBEndpoints(context.Background(), logger, kcp, machineMetaCluster(), "test-cluster-control-plane-lb")).
		To(Succeed(), "an unreadable VMI skips that backend; it does not fail the reconcile")

	g.Expect(strings.Join(logged, "\n")).To(ContainSubstring("check the control-plane manager's RBAC"),
		"a forbidden VMI read must be visible at the default log level")
}

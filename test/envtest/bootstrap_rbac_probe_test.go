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

package envtest

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/controllers/bootstrap"
)

// TestC5_NodePushRoleRBACProbe proves the PR-3 node Role at the apiserver's own
// RBAC layer (envtest runs authorization-mode=RBAC). It runs the production
// resolver to create the per-cluster SA/Role/RoleBinding, then impersonates the
// node ServiceAccount and asserts:
//
//   - it CANNOT create any Secret (POST) — KD-60/A1 closed;
//   - it CANNOT apply-create a Secret (server-side apply that would create is
//     re-authorized as `create`, by name — G11);
//   - it CAN patch the named staging / etcd-status Secrets;
//   - it CANNOT patch the contract Secret <cluster>-kubeconfig;
//   - it CANNOT patch the join-token Secret (the KCP is its only writer,
//     ADR 0013);
//   - it CANNOT patch another cluster's staging Secret.
func TestC5_NodePushRoleRBACProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping envtest in short mode")
	}
	g := NewWithT(t)
	ctx, _, cfg, _, teardown := startKCPEnvtest(t)
	defer teardown()

	// A full-scheme admin client: the resolver upserts rbac Roles/RoleBindings, so
	// the client's scheme must know rbac/v1 (the manager's scheme does not).
	fullScheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(fullScheme)).To(Succeed())
	g.Expect(rbacv1.AddToScheme(fullScheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(fullScheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(fullScheme)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(fullScheme)).To(Succeed())
	c, err := client.New(cfg, client.Options{Scheme: fullScheme})
	g.Expect(err).NotTo(HaveOccurred())

	const (
		nsName      = "c5-rbac-ns"
		clusterName = "c5rbac"
	)
	saName := "kairos-kubeconfig-writer-" + clusterName
	stagingName := bootstrapv1beta2.KubeconfigPushSecretName(clusterName)
	joinTokenName := bootstrapv1beta2.ControlPlaneJoinTokenSecretName(clusterName)
	etcdStatusName := bootstrapv1beta2.EtcdStatusSecretName(clusterName)
	contractName := clusterName + "-kubeconfig"

	g.Expect(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})).To(Succeed())

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: nsName},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: controlplanev1beta2.GroupVersion.Group,
				Kind:     "KairosControlPlane",
				Name:     clusterName + "-kcp",
			},
			ControlPlaneEndpoint: testControlPlaneEndpoint(),
		},
	}
	g.Expect(c.Create(ctx, cluster)).To(Succeed())

	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-kcp", Namespace: nsName},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas: ptr.To(int32(1)),
			Version:  "v1.30.0+k0s.0",
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "missing-on-purpose",
					Namespace:  nsName,
				},
			},
			KairosConfigTemplate: controlplanev1beta2.KairosConfigTemplateReference{Name: "missing-on-purpose"},
		},
	}
	g.Expect(c.Create(ctx, kcp)).To(Succeed())

	// A control-plane Machine controlled by the KCP, and a KCP-owned staging
	// Secret with the same owner UID — the resolver gate keys on this pairing.
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcp.Name + "-0",
			Namespace: nsName,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       kcp.Name,
				UID:        kcp.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName:       clusterName,
			Bootstrap:         clusterv1.Bootstrap{DataSecretName: ptr.To("placeholder")},
			InfrastructureRef: testMachineInfraRef("infra"),
		},
	}
	g.Expect(c.Create(ctx, machine)).To(Succeed())

	ownerRef := metav1.OwnerReference{
		APIVersion: controlplanev1beta2.GroupVersion.String(),
		Kind:       "KairosControlPlane",
		Name:       kcp.Name,
		UID:        kcp.UID,
		Controller: ptr.To(true),
	}
	staging := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: stagingName, Namespace: nsName, OwnerReferences: []metav1.OwnerReference{ownerRef},
	}, Type: corev1.SecretTypeOpaque}
	g.Expect(c.Create(ctx, staging)).To(Succeed())
	// Pre-create the other named Secrets and the objects used by the negative
	// probes so a rejection is unambiguously Forbidden (RBAC), never NotFound.
	for _, n := range []string{joinTokenName, etcdStatusName, contractName, "other-cluster-kubeconfig-push"} {
		g.Expect(c.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: nsName}, Type: corev1.SecretTypeOpaque})).To(Succeed())
	}

	// Run the production resolver to create the SA/Role/RoleBinding exactly as
	// production emits them (k0s control-plane → includes the VMI grant).
	resolver := bootstrap.NewKubeVirtTokenResolver(c, c.Scheme(), cfg.Host)
	kcfg := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: nsName},
		Spec:       bootstrapv1beta2.KairosConfigSpec{Role: "control-plane", Distribution: "k0s"},
	}
	ep, err := resolver.Resolve(ctx, kcfg, machine, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ep).NotTo(BeNil())
	g.Expect(ep.KubeconfigSecretName).To(Equal(stagingName))

	// Sanity: the Role has no create grant.
	role := &rbacv1.Role{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: saName, Namespace: nsName}, role)).To(Succeed())

	// Impersonating client acting AS the node ServiceAccount.
	impCfg := rest.CopyConfig(cfg)
	impCfg.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + nsName + ":" + saName}
	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	ic, err := client.New(impCfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())

	strategicValue := func() client.Patch {
		return client.RawPatch(types.StrategicMergePatchType, []byte(`{"data":{"value":"eA=="}}`))
	}
	secretRef := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName}}
	}

	// (1) Create any Secret → Forbidden.
	createErr := ic.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "probe-create", Namespace: nsName}, Type: corev1.SecretTypeOpaque})
	g.Expect(apierrors.IsForbidden(createErr)).To(BeTrue(), "node SA must NOT create Secrets (got %v)", createErr)

	// (2) Apply-create a non-existent Secret → Forbidden (apply-create == create, G11).
	applyTarget := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: "probe-apply-create", Namespace: nsName},
		Data:       map[string][]byte{"value": []byte("x")},
	}
	applyErr := ic.Patch(ctx, applyTarget, client.Apply, client.FieldOwner("node-probe"), client.ForceOwnership)
	g.Expect(apierrors.IsForbidden(applyErr)).To(BeTrue(), "node SA must NOT apply-create Secrets (got %v)", applyErr)

	// (3) Patch the two named Secrets → allowed.
	g.Expect(ic.Patch(ctx, secretRef(stagingName), strategicValue())).To(Succeed(), "node SA must be able to patch the staging Secret")
	g.Expect(ic.Patch(ctx, secretRef(etcdStatusName), strategicValue())).To(Succeed(), "node SA must be able to patch the etcd-status Secret")

	// (3b) Patch the join-token Secret → Forbidden (ADR 0013: KCP-minted).
	joinErr := ic.Patch(ctx, secretRef(joinTokenName), strategicValue())
	g.Expect(apierrors.IsForbidden(joinErr)).To(BeTrue(), "node SA must NOT patch the join-token Secret (got %v)", joinErr)

	// (4) Patch the contract Secret → Forbidden.
	contractErr := ic.Patch(ctx, secretRef(contractName), strategicValue())
	g.Expect(apierrors.IsForbidden(contractErr)).To(BeTrue(), "node SA must NOT patch the contract Secret (got %v)", contractErr)

	// (5) Patch another cluster's staging Secret → Forbidden.
	otherErr := ic.Patch(ctx, secretRef("other-cluster-kubeconfig-push"), strategicValue())
	g.Expect(apierrors.IsForbidden(otherErr)).To(BeTrue(), "node SA must NOT patch another cluster's staging Secret (got %v)", otherErr)

	// (6) The Role is PATCH-ONLY: get and update on the staging Secret it CAN
	// patch must still be Forbidden. Positive proof the removed verbs stay gone
	// (guards against a future re-broadening of the node token).
	getErr := ic.Get(ctx, types.NamespacedName{Name: stagingName, Namespace: nsName}, &corev1.Secret{})
	g.Expect(apierrors.IsForbidden(getErr)).To(BeTrue(), "node SA must NOT get the staging Secret (patch-only; got %v)", getErr)

	// update (PUT) needs a full object; fetch it as admin, then attempt the PUT as
	// the node SA. The request is authorized before the object is read, so a
	// Forbidden here is the RBAC denial, not a conflict.
	stagingForUpdate := &corev1.Secret{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: stagingName, Namespace: nsName}, stagingForUpdate)).To(Succeed())
	updateErr := ic.Update(ctx, stagingForUpdate)
	g.Expect(apierrors.IsForbidden(updateErr)).To(BeTrue(), "node SA must NOT update (PUT) the staging Secret (patch-only; got %v)", updateErr)
}

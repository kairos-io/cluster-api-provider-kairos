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
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// testKCPUID is the shared controller-owner UID for the fixture's Machine and
// its staging kubeconfig Secret. The PR-3 resolver gate (ADR 0011) passes only
// when the staging Secret's controller-owner UID equals the Machine's — both the
// KCP — so the two fixtures below stamp this same UID.
const testKCPUID = types.UID("00000000-0000-0000-0000-0000000000cp")

// fakeSubResourceClient is the minimum surface tests need to drive
// kubeVirtTokenResolver — just the Create path against the "token"
// subresource. Get/Update/Patch are not exercised by the resolver, so they
// remain unimplemented (calling them in a test would surface the omission as
// a panic, which is the desired failure mode).
type fakeSubResourceClient struct {
	createCalls int
	// token is the token string the fake returns on Create. Empty means the
	// fake returns success but with an empty token, exercising the
	// "TokenRequest returned empty token" error path.
	token string
	// err, if non-nil, is returned from Create — exercising the
	// SubResource("token").Create failure path.
	err error
}

func (f *fakeSubResourceClient) Get(_ context.Context, _ client.Object, _ client.Object, _ ...client.SubResourceGetOption) error {
	return errors.New("fakeSubResourceClient.Get not implemented")
}

func (f *fakeSubResourceClient) Create(_ context.Context, _ client.Object, sub client.Object, _ ...client.SubResourceCreateOption) error {
	f.createCalls++
	if f.err != nil {
		return f.err
	}
	tr, ok := sub.(*authenticationv1.TokenRequest)
	if !ok {
		return errors.New("fake: Create called with non-TokenRequest subResource")
	}
	tr.Status.Token = f.token
	return nil
}

func (f *fakeSubResourceClient) Update(_ context.Context, _ client.Object, _ ...client.SubResourceUpdateOption) error {
	return errors.New("fakeSubResourceClient.Update not implemented")
}

func (f *fakeSubResourceClient) Patch(_ context.Context, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
	return errors.New("fakeSubResourceClient.Patch not implemented")
}

func (f *fakeSubResourceClient) Apply(_ context.Context, _ runtime.ApplyConfiguration, _ ...client.SubResourceApplyOption) error {
	return errors.New("fakeSubResourceClient.Apply not implemented")
}

func newResolverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(rbacv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(authenticationv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	return scheme
}

// resolverMachine is a control-plane Machine controlled by a KairosControlPlane
// (UID testKCPUID) — the shape the resolver gate keys on.
func resolverMachine(clusterName, ns string) *clusterv1.Machine {
	ctlTrue := true
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-cp-0",
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       clusterName + "-kcp",
				UID:        testKCPUID,
				Controller: &ctlTrue,
			}},
		},
	}
}

// ownedStagingSecret is the per-cluster staging kubeconfig Secret controller-owned
// by the same KCP (UID testKCPUID). Seeding it lets the resolver gate pass.
func ownedStagingSecret(clusterName, ns string) *corev1.Secret {
	ctlTrue := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bootstrapv1beta2.KubeconfigPushSecretName(clusterName),
			Namespace: ns,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:                     clusterName,
				bootstrapv1beta2.KubeconfigPushSecretTypeLabel: bootstrapv1beta2.KubeconfigPushSecretTypeValue,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       clusterName + "-kcp",
				UID:        testKCPUID,
				Controller: &ctlTrue,
			}},
		},
		Type: corev1.SecretTypeOpaque,
	}
}

// newResolverFixture builds a resolver over a fake client seeded with a
// KairosConfig, a Cluster named "test-cluster", a KCP-controlled control-plane
// Machine, and (by default) the KCP-owned staging Secret so the PR-3 resolver
// gate passes. Additional seed objects can be appended; pass a staging Secret in
// seed to override the default owned one for gate-failure cases.
func newResolverFixture(scheme *runtime.Scheme, sub *fakeSubResourceClient, mgmtAPI string, seed ...client.Object) (*kubeVirtTokenResolver, *bootstrapv1beta2.KairosConfig, *clusterv1.Machine, *clusterv1.Cluster) {
	kc := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
			UID:       "00000000-0000-0000-0000-000000000001",
		},
	}
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}
	machine := resolverMachine("test-cluster", "default")
	objects := append([]client.Object{kc, cluster, machine, ownedStagingSecret("test-cluster", "default")}, seed...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	r := &kubeVirtTokenResolver{
		Client:              c,
		SubResource:         func(string) client.SubResourceClient { return sub },
		Scheme:              scheme,
		ManagementAPIServer: mgmtAPI,
	}
	return r, kc, machine, cluster
}

// TestResolve_HappyPath_ReturnsFullEndpoint exercises the success path: the
// SA/Role/RoleBinding upserts, the fake TokenRequest returns a token, and the
// resolver assembles the ManagementEndpoint pointing at the STAGING Secret.
func TestResolve_HappyPath_ReturnsFullEndpoint(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{token: "tok-abc"}
	r, kc, machine, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443")
	// k0s control-plane is the config shape that exercises every rule the
	// resolver can grant (incl. the conditional VMI/get for SAN detection).
	kc.Spec.Role = "control-plane"
	kc.Spec.Distribution = "k0s"

	got, err := r.Resolve(context.Background(), kc, machine, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).NotTo(BeNil())
	g.Expect(got.Token).To(Equal("tok-abc"))
	g.Expect(got.APIServer).To(Equal("https://mgmt:6443"))
	// PR-3: the node's push target is the STAGING Secret, never the contract one.
	g.Expect(got.KubeconfigSecretName).To(Equal("test-cluster-kubeconfig-push"))
	g.Expect(got.KubeconfigSecretNamespace).To(Equal("default"))

	// SA/Role/RoleBinding were created with the cluster-name label.
	saName := kubeconfigWriterName("test-cluster")
	sa := &corev1.ServiceAccount{}
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "default"}, sa)).To(Succeed())
	g.Expect(sa.Labels).To(HaveKeyWithValue(clusterv1.ClusterNameLabel, "test-cluster"))
	role := &rbacv1.Role{}
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "default"}, role)).To(Succeed())
	// PR-3 minimization: a k0s control-plane (CAPK) SA gets exactly 2 rules:
	//   1. secrets/patch on the named staging/etcd-status Secrets
	//   2. kubevirt.io VMI/get (k0s CAPK control-plane SAN detection only)
	// The former create rule and the get/update verbs are removed (KD-60/A1).
	g.Expect(role.Rules).To(HaveLen(2))
	g.Expect(roleGrantsVMIGet(role)).To(BeTrue(), "k0s control-plane SA must retain virtualmachineinstances:get for SAN detection")
	g.Expect(roleGrantsServicesGet(role)).To(BeFalse(), "services:get is a dead grant and must be removed (KD-46)")
	g.Expect(roleGrantsSecretCreate(role)).To(BeFalse(), "PR-3: the node SA must NOT have secrets:create (KD-60/A1)")
	// Staging is patch-only; get/update are gone.
	stagingName := bootstrapv1beta2.KubeconfigPushSecretName("test-cluster")
	g.Expect(roleGrantsNamedSecret(role, stagingName, "patch")).To(BeTrue(), "node SA must patch the staging Secret")
	g.Expect(roleGrantsNamedSecret(role, stagingName, "get")).To(BeFalse(), "PR-3: node SA must NOT get the staging Secret")
	g.Expect(roleGrantsNamedSecret(role, stagingName, "update")).To(BeFalse(), "PR-3: node SA must NOT update the staging Secret")
	// The contract Secret is never in the Role's resourceNames.
	g.Expect(roleGrantsNamedSecret(role, "test-cluster-kubeconfig", "patch")).To(BeFalse(), "PR-3: node SA must NOT touch the contract Secret")
	rb := &rbacv1.RoleBinding{}
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "default"}, rb)).To(Succeed())
	g.Expect(rb.RoleRef.Name).To(Equal(saName))
	g.Expect(sub.createCalls).To(Equal(1))
}

// roleGrantsVMIGet reports whether the Role grants get on kubevirt.io
// virtualmachineinstances.
func roleGrantsVMIGet(role *rbacv1.Role) bool {
	for _, rule := range role.Rules {
		for _, g := range rule.APIGroups {
			if g != "kubevirt.io" {
				continue
			}
			for _, res := range rule.Resources {
				if res == "virtualmachineinstances" {
					return true
				}
			}
		}
	}
	return false
}

// roleGrantsServicesGet reports whether the Role grants any verb on core
// Services — used to assert the KD-46 removal of the dead services:get rule.
func roleGrantsServicesGet(role *rbacv1.Role) bool {
	for _, rule := range role.Rules {
		coreGroup := false
		for _, g := range rule.APIGroups {
			if g == "" {
				coreGroup = true
				break
			}
		}
		if !coreGroup {
			continue
		}
		for _, res := range rule.Resources {
			if res == "services" {
				return true
			}
		}
	}
	return false
}

// roleGrantsSecretCreate reports whether the Role grants create on core Secrets
// via any rule — used to assert the PR-3 removal of the node create grant.
func roleGrantsSecretCreate(role *rbacv1.Role) bool {
	for _, rule := range role.Rules {
		coreGroup := false
		for _, g := range rule.APIGroups {
			if g == "" || g == "*" {
				coreGroup = true
				break
			}
		}
		if !coreGroup {
			continue
		}
		hasSecret := false
		for _, res := range rule.Resources {
			if res == "secrets" || res == "*" {
				hasSecret = true
				break
			}
		}
		if !hasSecret {
			continue
		}
		for _, v := range rule.Verbs {
			if v == "create" || v == "*" {
				return true
			}
		}
	}
	return false
}

// TestResolve_RBACMinimization_VMIGetScopedToK0sControlPlane verifies the
// KD-46 conditional grant: only a k0s control-plane config receives
// virtualmachineinstances:get. k3s control-planes and all workers get the
// minimal single-rule patch-only Secret Role.
func TestResolve_RBACMinimization_VMIGetScopedToK0sControlPlane(t *testing.T) {
	cases := []struct {
		name         string
		role         string
		distribution string
		wantVMIGet   bool
		wantRuleLen  int
	}{
		{"k0s-control-plane-gets-vmi", "control-plane", "k0s", true, 2},
		{"k0s-empty-distribution-defaults-k0s", "control-plane", "", true, 2},
		{"k3s-control-plane-no-vmi", "control-plane", "k3s", false, 1},
		{"k0s-worker-no-vmi", "worker", "k0s", false, 1},
		{"k3s-worker-no-vmi", "worker", "k3s", false, 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := newResolverScheme(t)
			sub := &fakeSubResourceClient{token: "tok"}
			r, kc, machine, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443")
			kc.Spec.Role = tc.role
			kc.Spec.Distribution = tc.distribution

			_, err := r.Resolve(context.Background(), kc, machine, cluster)
			g.Expect(err).NotTo(HaveOccurred())

			role := &rbacv1.Role{}
			saName := kubeconfigWriterName("test-cluster")
			g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "default"}, role)).To(Succeed())
			g.Expect(role.Rules).To(HaveLen(tc.wantRuleLen))
			g.Expect(roleGrantsVMIGet(role)).To(Equal(tc.wantVMIGet))
			// services:get and secrets:create must never appear regardless of shape.
			g.Expect(roleGrantsServicesGet(role)).To(BeFalse())
			g.Expect(roleGrantsSecretCreate(role)).To(BeFalse())
		})
	}
}

// TestResolve_SubResourceError_ReturnsError asserts that a TokenRequest
// failure from the SubResource client propagates as an error and the
// resolver returns nil for the endpoint.
func TestResolve_SubResourceError_ReturnsError(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{err: errors.New("boom")}
	r, kc, machine, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443")

	got, err := r.Resolve(context.Background(), kc, machine, cluster)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("failed to create serviceaccount token"))
	g.Expect(got).To(BeNil())
}

// TestResolve_EmptyToken_ReturnsError asserts that a successful TokenRequest
// with an empty .Status.Token is detected and returned as an error rather than
// silently producing a useless ManagementEndpoint.
func TestResolve_EmptyToken_ReturnsError(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{token: ""}
	r, kc, machine, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443")

	got, err := r.Resolve(context.Background(), kc, machine, cluster)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("empty token"))
	g.Expect(got).To(BeNil())
}

// TestResolve_EmptyManagementAPIServer_ReturnsNilNil verifies the documented
// "disabled" signal: when ManagementAPIServer is empty the resolver returns
// (nil, nil) and skips all SA/Role/RoleBinding/TokenRequest work — before the
// staging gate is even consulted.
func TestResolve_EmptyManagementAPIServer_ReturnsNilNil(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{token: "ignored"}
	r, kc, machine, cluster := newResolverFixture(scheme, sub, "")

	got, err := r.Resolve(context.Background(), kc, machine, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(BeNil())
	g.Expect(sub.createCalls).To(Equal(0))
	// The SA must not have been touched either.
	saName := kubeconfigWriterName("test-cluster")
	sa := &corev1.ServiceAccount{}
	err = r.Client.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "default"}, sa)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

// TestResolve_StagingMissing_ReturnsNotReady covers the PR-3 resolver gate: when
// the staging Secret does not exist, Resolve returns ErrKubeconfigPushTargetNotReady
// and mints NO token / creates NO SA (the gate runs before that work).
func TestResolve_StagingMissing_ReturnsNotReady(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{token: "tok"}
	kc := &bootstrapv1beta2.KairosConfig{ObjectMeta: metav1.ObjectMeta{Name: "test-config", Namespace: "default"}}
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"}}
	machine := resolverMachine("test-cluster", "default")
	// No staging Secret seeded.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kc, cluster, machine).Build()
	r := &kubeVirtTokenResolver{Client: c, SubResource: func(string) client.SubResourceClient { return sub }, Scheme: scheme, ManagementAPIServer: "https://mgmt:6443"}

	got, err := r.Resolve(context.Background(), kc, machine, cluster)
	g.Expect(errors.Is(err, ErrKubeconfigPushTargetNotReady)).To(BeTrue())
	g.Expect(got).To(BeNil())
	g.Expect(sub.createCalls).To(Equal(0), "no token minted when the gate fails")
	saName := kubeconfigWriterName("test-cluster")
	sa := &corev1.ServiceAccount{}
	g.Expect(apierrors.IsNotFound(c.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "default"}, sa))).To(BeTrue(),
		"no SA created when the gate fails")
}

// TestResolve_StagingForeignOwner_ReturnsNotReady covers the gate's owner-UID
// check: a staging Secret owned by a DIFFERENT KCP (forged / another cluster)
// fails the gate.
func TestResolve_StagingForeignOwner_ReturnsNotReady(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{token: "tok"}
	kc := &bootstrapv1beta2.KairosConfig{ObjectMeta: metav1.ObjectMeta{Name: "test-config", Namespace: "default"}}
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"}}
	machine := resolverMachine("test-cluster", "default")
	foreign := ownedStagingSecret("test-cluster", "default")
	ctlTrue := true
	foreign.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: controlplanev1beta2.GroupVersion.String(),
		Kind:       "KairosControlPlane",
		Name:       "test-cluster-kcp",
		UID:        types.UID("FORGED-different-uid"),
		Controller: &ctlTrue,
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kc, cluster, machine, foreign).Build()
	r := &kubeVirtTokenResolver{Client: c, SubResource: func(string) client.SubResourceClient { return sub }, Scheme: scheme, ManagementAPIServer: "https://mgmt:6443"}

	got, err := r.Resolve(context.Background(), kc, machine, cluster)
	g.Expect(errors.Is(err, ErrKubeconfigPushTargetNotReady)).To(BeTrue())
	g.Expect(got).To(BeNil())
	g.Expect(sub.createCalls).To(Equal(0))
}

// TestResolve_Idempotent_TwoCallsNoConflict asserts that calling Resolve
// twice in a row against the same fixture does not error on the second call.
func TestResolve_Idempotent_TwoCallsNoConflict(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{token: "tok-abc"}
	r, kc, machine, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443")

	first, err := r.Resolve(context.Background(), kc, machine, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(first).NotTo(BeNil())

	second, err := r.Resolve(context.Background(), kc, machine, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(second).NotTo(BeNil())
	g.Expect(second.Token).To(Equal("tok-abc"))
	// Both calls produced a TokenRequest — no in-resolver caching today.
	g.Expect(sub.createCalls).To(Equal(2))
}

// TestResolve_HappyPath_CAPVCluster asserts the resolver returns the same
// ManagementEndpoint shape regardless of the underlying infrastructure provider,
// pointing at that cluster's staging Secret.
func TestResolve_HappyPath_CAPVCluster(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{token: "tok-capv"}
	kc := &bootstrapv1beta2.KairosConfig{ObjectMeta: metav1.ObjectMeta{Name: "test-config", Namespace: "default"}}
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "capv-cluster", Namespace: "default"}}
	machine := resolverMachine("capv-cluster", "default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kc, cluster, machine, ownedStagingSecret("capv-cluster", "default")).Build()
	r := &kubeVirtTokenResolver{Client: c, SubResource: func(string) client.SubResourceClient { return sub }, Scheme: scheme, ManagementAPIServer: "https://mgmt:6443"}

	got, err := r.Resolve(context.Background(), kc, machine, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).NotTo(BeNil())
	g.Expect(got.Token).To(Equal("tok-capv"))
	g.Expect(got.KubeconfigSecretName).To(Equal("capv-cluster-kubeconfig-push"))
	g.Expect(got.KubeconfigSecretNamespace).To(Equal("default"))
	g.Expect(got.APIServer).To(Equal("https://mgmt:6443"))
}

// TestResolve_PreExistingServiceAccount_StillSucceeds covers the case where
// a previous reconcile already created the SA, but not the Role or RoleBinding.
func TestResolve_PreExistingServiceAccount_StillSucceeds(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	saName := kubeconfigWriterName("test-cluster")
	preExisting := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: "default",
		},
	}
	sub := &fakeSubResourceClient{token: "tok-xyz"}
	r, kc, machine, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443", preExisting)

	got, err := r.Resolve(context.Background(), kc, machine, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).NotTo(BeNil())
	g.Expect(got.Token).To(Equal("tok-xyz"))

	// The SA's label was added on the update path.
	sa := &corev1.ServiceAccount{}
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "default"}, sa)).To(Succeed())
	g.Expect(sa.Labels).To(HaveKeyWithValue(clusterv1.ClusterNameLabel, "test-cluster"))
}

// TestResolve_MultipleConfigsSameCluster_OwnedByCluster is the regression guard
// for the Phase-4 multi-node bring-up blocker (ADR 0005 § D.2): the per-cluster
// SA/Role/RoleBinding are Cluster-owned, so two distinct KairosConfigs in the
// same cluster both resolve without an AlreadyOwnedError.
func TestResolve_MultipleConfigsSameCluster_OwnedByCluster(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{token: "tok"}
	r, kc1, machine, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443")
	kc1.Spec.Role = "control-plane"
	kc1.Spec.Distribution = "k3s"

	// Init node.
	_, err := r.Resolve(context.Background(), kc1, machine, cluster)
	g.Expect(err).NotTo(HaveOccurred())

	// A second, distinct KairosConfig in the SAME cluster (a joiner). Different
	// name + UID so a controller-ref owned by kc1 would conflict.
	kc2 := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config-2",
			Namespace: "default",
			UID:       "00000000-0000-0000-0000-000000000002",
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{Role: "control-plane", Distribution: "k3s"},
	}
	_, err = r.Resolve(context.Background(), kc2, machine, cluster)
	g.Expect(err).NotTo(HaveOccurred(), "a second KairosConfig in the same cluster must not conflict on the shared RBAC objects")

	// All three shared objects must be controller-owned by the Cluster.
	saName := kubeconfigWriterName("test-cluster")
	nn := types.NamespacedName{Name: saName, Namespace: "default"}
	sa := &corev1.ServiceAccount{}
	g.Expect(r.Client.Get(context.Background(), nn, sa)).To(Succeed())
	role := &rbacv1.Role{}
	g.Expect(r.Client.Get(context.Background(), nn, role)).To(Succeed())
	rb := &rbacv1.RoleBinding{}
	g.Expect(r.Client.Get(context.Background(), nn, rb)).To(Succeed())

	for _, obj := range []client.Object{sa, role, rb} {
		owner := metav1.GetControllerOf(obj)
		g.Expect(owner).NotTo(BeNil(), "shared object must have a controller owner")
		g.Expect(owner.Kind).To(Equal("Cluster"))
		g.Expect(owner.Name).To(Equal("test-cluster"))
	}
}

// TestResolve_GrantsEtcdStatusSecret asserts the node SA's Role is granted
// patch (and NOT create/get/update) on the per-cluster etcd-status Secret
// (ADR 0005 §E.1), on the same resourceNames-scoped patch rule as the staging
// kubeconfig Secret. The join-token Secret is granted nothing (ADR 0013).
func TestResolve_GrantsEtcdStatusSecret(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{token: "tok"}
	r, kc, machine, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443")
	kc.Spec.Role = "control-plane"
	kc.Spec.Distribution = "k0s"

	_, err := r.Resolve(context.Background(), kc, machine, cluster)
	g.Expect(err).NotTo(HaveOccurred())

	role := &rbacv1.Role{}
	saName := kubeconfigWriterName("test-cluster")
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "default"}, role)).To(Succeed())

	etcdName := bootstrapv1beta2.EtcdStatusSecretName("test-cluster")
	g.Expect(roleGrantsNamedSecret(role, etcdName, "patch")).To(BeTrue(), "node SA must patch the etcd-status Secret")
	g.Expect(roleGrantsNamedSecret(role, etcdName, "get")).To(BeFalse(), "PR-3: node SA must NOT get the etcd-status Secret")
	g.Expect(roleGrantsNamedSecret(role, etcdName, "update")).To(BeFalse(), "PR-3: node SA must NOT update the etcd-status Secret")
	g.Expect(roleGrantsNamedSecret(role, etcdName, "create")).To(BeFalse(), "node SA must NOT create the etcd-status Secret (controller pre-creates it)")

	joinTokenName := bootstrapv1beta2.ControlPlaneJoinTokenSecretName("test-cluster")
	g.Expect(roleGrantsNamedSecret(role, joinTokenName, "patch")).To(BeFalse(), "ADR 0013: the KCP is the join-token Secret's only writer")
	g.Expect(roleGrantsNamedSecret(role, joinTokenName, "get")).To(BeFalse())
}

// TestResolve_NoJoinTokenSecretGrant pins ADR 0013: the KairosControlPlane is
// the only writer of the k0s join-token Secret, so the per-cluster node
// ServiceAccount's Role grants nothing on it, for any role or distribution. A
// Role created by an older release, which did grant it, is narrowed by the
// next Resolve. The node keeps only patch on the staging kubeconfig and
// etcd-status Secrets, and loses every grant on the contract kubeconfig.
func TestResolve_NoJoinTokenSecretGrant(t *testing.T) {
	joinToken := bootstrapv1beta2.ControlPlaneJoinTokenSecretName("test-cluster")
	etcdStatus := bootstrapv1beta2.EtcdStatusSecretName("test-cluster")
	kubeconfig := "test-cluster-kubeconfig"
	staging := bootstrapv1beta2.KubeconfigPushSecretName("test-cluster")

	for _, tc := range []struct{ name, role, distribution string }{
		{"k0s control plane", "control-plane", "k0s"},
		{"k3s control plane", "control-plane", "k3s"},
		{"k0s worker", "worker", "k0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := newResolverScheme(t)
			// The Role an older release created: the join-token Secret in the
			// named get/update/patch rule.
			stale := &rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: kubeconfigWriterName("test-cluster"), Namespace: "default"},
				Rules: []rbacv1.PolicyRule{
					{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}},
					{APIGroups: []string{""}, Resources: []string{"secrets"},
						ResourceNames: []string{kubeconfig, joinToken, etcdStatus}, Verbs: []string{"get", "update", "patch"}},
				},
			}
			r, kc, machine, cluster := newResolverFixture(scheme, &fakeSubResourceClient{token: "tok"}, "https://mgmt:6443", stale)
			kc.Spec.Role = tc.role
			kc.Spec.Distribution = tc.distribution

			_, err := r.Resolve(context.Background(), kc, machine, cluster)
			g.Expect(err).NotTo(HaveOccurred())

			role := &rbacv1.Role{}
			g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: kubeconfigWriterName("test-cluster"), Namespace: "default"}, role)).To(Succeed())
			for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
				g.Expect(roleGrantsNamedSecret(role, joinToken, verb)).To(BeFalse(), "node SA must not %s the join-token Secret", verb)
			}
			for _, rule := range role.Rules {
				g.Expect(rule.ResourceNames).ToNot(ContainElement(joinToken))
			}
			g.Expect(roleGrantsNamedSecret(role, staging, "patch")).To(BeTrue(), "staging kubeconfig push grant")
			g.Expect(roleGrantsNamedSecret(role, etcdStatus, "patch")).To(BeTrue(), "etcd-status grant kept")
			for _, verb := range []string{"get", "update", "create"} {
				g.Expect(roleGrantsNamedSecret(role, staging, verb)).To(BeFalse(), "staging: no %s", verb)
				g.Expect(roleGrantsNamedSecret(role, etcdStatus, verb)).To(BeFalse(), "etcd-status: no %s", verb)
			}
			for _, verb := range []string{"get", "update", "patch", "create"} {
				g.Expect(roleGrantsNamedSecret(role, kubeconfig, verb)).To(BeFalse(), "contract kubeconfig: no %s", verb)
			}
		})
	}
}

// roleGrantsNamedSecret reports whether the Role grants the given verb on the
// named core Secret via a resourceNames-scoped rule.
func roleGrantsNamedSecret(role *rbacv1.Role, name, verb string) bool {
	for _, rule := range role.Rules {
		coreGroup := false
		for _, gp := range rule.APIGroups {
			if gp == "" {
				coreGroup = true
				break
			}
		}
		if !coreGroup {
			continue
		}
		hasSecret := false
		for _, res := range rule.Resources {
			if res == "secrets" {
				hasSecret = true
				break
			}
		}
		if !hasSecret {
			continue
		}
		hasName := false
		for _, rn := range rule.ResourceNames {
			if rn == name {
				hasName = true
				break
			}
		}
		if !hasName {
			continue
		}
		for _, v := range rule.Verbs {
			if v == verb {
				return true
			}
		}
	}
	return false
}

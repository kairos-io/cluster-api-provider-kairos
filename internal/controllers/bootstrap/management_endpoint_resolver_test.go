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
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

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

func newResolverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(rbacv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(authenticationv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	return scheme
}

func newResolverFixture(scheme *runtime.Scheme, sub *fakeSubResourceClient, mgmtAPI string, seed ...client.Object) (*kubeVirtTokenResolver, *bootstrapv1beta2.KairosConfig, *clusterv1.Cluster) {
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
	objects := append([]client.Object{kc, cluster}, seed...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	r := &kubeVirtTokenResolver{
		Client:              c,
		SubResource:         func(string) client.SubResourceClient { return sub },
		Scheme:              scheme,
		ManagementAPIServer: mgmtAPI,
	}
	return r, kc, cluster
}

// TestResolve_HappyPath_ReturnsFullEndpoint exercises the success path: an
// empty management cluster receives the SA/Role/RoleBinding upserts, the
// fake TokenRequest returns a token, and the resolver assembles the
// ManagementEndpoint with all four fields populated.
func TestResolve_HappyPath_ReturnsFullEndpoint(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{token: "tok-abc"}
	r, kc, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443")
	// k0s control-plane is the config shape that exercises every rule the
	// resolver can grant (incl. the conditional VMI/get for SAN detection).
	kc.Spec.Role = "control-plane"
	kc.Spec.Distribution = "k0s"

	got, err := r.Resolve(context.Background(), kc, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).NotTo(BeNil())
	g.Expect(got.Token).To(Equal("tok-abc"))
	g.Expect(got.APIServer).To(Equal("https://mgmt:6443"))
	g.Expect(got.KubeconfigSecretName).To(Equal("test-cluster-kubeconfig"))
	g.Expect(got.KubeconfigSecretNamespace).To(Equal("default"))

	// SA/Role/RoleBinding were created with the cluster-name label.
	saName := kubeconfigWriterName("test-cluster")
	sa := &corev1.ServiceAccount{}
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "default"}, sa)).To(Succeed())
	g.Expect(sa.Labels).To(HaveKeyWithValue(clusterv1.ClusterNameLabel, "test-cluster"))
	role := &rbacv1.Role{}
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "default"}, role)).To(Succeed())
	// KD-46 minimization: a k0s control-plane (CAPK) SA gets exactly 3 rules:
	//   1. secrets/create (no resourceNames — K8s RBAC silent-fail workaround)
	//   2. secrets/get,update,patch on the named kubeconfig Secret
	//   3. kubevirt.io VMI/get (k0s CAPK control-plane SAN detection only)
	// services/get was removed (dead grant — no template reads Services
	// with the node SA token).
	g.Expect(role.Rules).To(HaveLen(3))
	g.Expect(roleGrantsVMIGet(role)).To(BeTrue(), "k0s control-plane SA must retain virtualmachineinstances:get for SAN detection")
	g.Expect(roleGrantsServicesGet(role)).To(BeFalse(), "services:get is a dead grant and must be removed (KD-46)")
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

// TestResolve_RBACMinimization_VMIGetScopedToK0sControlPlane verifies the
// KD-46 conditional grant: only a k0s control-plane config receives
// virtualmachineinstances:get. k3s control-planes and all workers get the
// minimal 2-rule Secret-only Role.
func TestResolve_RBACMinimization_VMIGetScopedToK0sControlPlane(t *testing.T) {
	cases := []struct {
		name         string
		role         string
		distribution string
		wantVMIGet   bool
		wantRuleLen  int
	}{
		{"k0s-control-plane-gets-vmi", "control-plane", "k0s", true, 3},
		{"k0s-empty-distribution-defaults-k0s", "control-plane", "", true, 3},
		{"k3s-control-plane-no-vmi", "control-plane", "k3s", false, 2},
		{"k0s-worker-no-vmi", "worker", "k0s", false, 2},
		{"k3s-worker-no-vmi", "worker", "k3s", false, 2},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := newResolverScheme(t)
			sub := &fakeSubResourceClient{token: "tok"}
			r, kc, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443")
			kc.Spec.Role = tc.role
			kc.Spec.Distribution = tc.distribution

			_, err := r.Resolve(context.Background(), kc, cluster)
			g.Expect(err).NotTo(HaveOccurred())

			role := &rbacv1.Role{}
			saName := kubeconfigWriterName("test-cluster")
			g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "default"}, role)).To(Succeed())
			g.Expect(role.Rules).To(HaveLen(tc.wantRuleLen))
			g.Expect(roleGrantsVMIGet(role)).To(Equal(tc.wantVMIGet))
			// services:get must never appear regardless of config shape.
			g.Expect(roleGrantsServicesGet(role)).To(BeFalse())
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
	r, kc, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443")

	got, err := r.Resolve(context.Background(), kc, cluster)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("failed to create serviceaccount token"))
	g.Expect(got).To(BeNil())
}

// TestResolve_EmptyToken_ReturnsError asserts that a successful TokenRequest
// with an empty .Status.Token (which the apiserver can legitimately return
// under some auth-plugin misconfigurations) is detected and returned as an
// error rather than silently producing a useless ManagementEndpoint.
func TestResolve_EmptyToken_ReturnsError(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{token: ""}
	r, kc, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443")

	got, err := r.Resolve(context.Background(), kc, cluster)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("empty token"))
	g.Expect(got).To(BeNil())
}

// TestResolve_EmptyManagementAPIServer_ReturnsNilNil verifies the documented
// "disabled" signal: when ManagementAPIServer is empty (envtest, `go run`
// outside a cluster), the resolver returns (nil, nil) and skips all
// SA/Role/RoleBinding/TokenRequest work. This contract lets the reconciler
// continue rendering without the in-node push block.
func TestResolve_EmptyManagementAPIServer_ReturnsNilNil(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{token: "ignored"}
	r, kc, cluster := newResolverFixture(scheme, sub, "")

	got, err := r.Resolve(context.Background(), kc, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(BeNil())
	g.Expect(sub.createCalls).To(Equal(0))
	// The SA must not have been touched either.
	saName := kubeconfigWriterName("test-cluster")
	sa := &corev1.ServiceAccount{}
	err = r.Client.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "default"}, sa)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

// TestResolve_Idempotent_TwoCallsNoConflict asserts that calling Resolve
// twice in a row against the same fixture does not error on the second call.
// The first call creates the SA/Role/RoleBinding; the second goes through
// controllerutil.CreateOrUpdate's update path. This is the property the
// legacy ensureKubeconfigPushConfig depended on (Reconcile re-renders on
// every Generation bump and we cannot afford a second-call conflict).
func TestResolve_Idempotent_TwoCallsNoConflict(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{token: "tok-abc"}
	r, kc, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443")

	first, err := r.Resolve(context.Background(), kc, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(first).NotTo(BeNil())

	second, err := r.Resolve(context.Background(), kc, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(second).NotTo(BeNil())
	g.Expect(second.Token).To(Equal("tok-abc"))
	// Both calls produced a TokenRequest — no in-resolver caching today.
	// (KD-33b will add caching; this assertion is the regression guard for
	// the "remove caching by accident" reverse direction.)
	g.Expect(sub.createCalls).To(Equal(2))
}

// TestResolve_HappyPath_CAPVCluster asserts the resolver returns the same
// ManagementEndpoint shape regardless of the underlying infrastructure
// provider — for KD-3b, the resolver is called for CAPV control planes too
// (gate broadened from CAPK-only). The test deliberately constructs no CAPK
// or CAPV-specific objects: the resolver is infra-agnostic on its happy path
// (the per-infra differentiation lives in supportsManagementEndpoint, not
// here). What this test asserts is that nothing in the resolver implicitly
// depends on a KubeVirt-only object existing.
func TestResolve_HappyPath_CAPVCluster(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	sub := &fakeSubResourceClient{token: "tok-capv"}
	r, kc, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443")
	cluster.Name = "capv-cluster" // override the default name to make assertions sharper

	got, err := r.Resolve(context.Background(), kc, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).NotTo(BeNil())
	g.Expect(got.Token).To(Equal("tok-capv"))
	g.Expect(got.KubeconfigSecretName).To(Equal("capv-cluster-kubeconfig"))
	g.Expect(got.KubeconfigSecretNamespace).To(Equal("default"))
	g.Expect(got.APIServer).To(Equal("https://mgmt:6443"))
}

// TestResolve_PreExistingServiceAccount_StillSucceeds covers the case where
// a previous reconcile (or another controller) already created the SA, but
// not the Role or RoleBinding. The CreateOrUpdate path must adopt the
// existing SA (setting our owner-ref + label on update) rather than fail.
func TestResolve_PreExistingServiceAccount_StillSucceeds(t *testing.T) {
	g := NewWithT(t)
	scheme := newResolverScheme(t)
	saName := kubeconfigWriterName("test-cluster")
	preExisting := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: "default",
			// No labels, no owner-ref — the resolver must add them via the
			// CreateOrUpdate mutate function.
		},
	}
	sub := &fakeSubResourceClient{token: "tok-xyz"}
	r, kc, cluster := newResolverFixture(scheme, sub, "https://mgmt:6443", preExisting)

	got, err := r.Resolve(context.Background(), kc, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).NotTo(BeNil())
	g.Expect(got.Token).To(Equal("tok-xyz"))

	// The SA's label was added on the update path.
	sa := &corev1.ServiceAccount{}
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "default"}, sa)).To(Succeed())
	g.Expect(sa.Labels).To(HaveKeyWithValue(clusterv1.ClusterNameLabel, "test-cluster"))
}

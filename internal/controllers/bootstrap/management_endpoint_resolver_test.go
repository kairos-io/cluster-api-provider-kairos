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
	g.Expect(role.Rules).To(HaveLen(3))
	rb := &rbacv1.RoleBinding{}
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "default"}, rb)).To(Succeed())
	g.Expect(rb.RoleRef.Name).To(Equal(saName))
	g.Expect(sub.createCalls).To(Equal(1))
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

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// foreignReadCounter builds a reconciler over a fake client that holds a Secret
// in another tenant's namespace, and counts every read of that namespace. The
// point is not only that the value is not returned but that the controller
// never reads it: the webhook is not always in the path, so the controller has
// to refuse on its own.
func foreignReadCounter(g *WithT) (*KairosConfigReconciler, *int) {
	scheme := tokenTestScheme(g)
	foreignReads := 0
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(tokenSecret("stolen", "other-tenant", "password", "not-yours")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Namespace == "other-tenant" {
					foreignReads++
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	return &KairosConfigReconciler{Client: c, Scheme: scheme}, &foreignReads
}

func TestResolveUserPassword_RefusesAnotherNamespacesSecret(t *testing.T) {
	g := NewWithT(t)
	r, foreignReads := foreignReadCounter(g)

	kc := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "kc", Namespace: "default"},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			UserPasswordSecretRef: &bootstrapv1beta2.UserPasswordSecretReference{Name: "stolen", Namespace: "other-tenant"},
		},
	}

	pw, err := r.resolveUserPassword(context.Background(), kc)
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, errCrossNamespaceSecretRef)).To(BeTrue(), "got: %v", err)
	g.Expect(pw).To(BeEmpty())
	g.Expect(*foreignReads).To(BeZero(), "the other namespace's Secret must not even be read")
}

func TestTokenFromWorkerRef_RefusesAnotherNamespacesSecret(t *testing.T) {
	g := NewWithT(t)
	r, foreignReads := foreignReadCounter(g)

	tok, err := r.tokenFromWorkerRef(context.Background(), "default",
		&bootstrapv1beta2.WorkerTokenSecretReference{Name: "stolen", Namespace: "other-tenant", Key: "password"}, "worker token")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, errCrossNamespaceSecretRef)).To(BeTrue(), "got: %v", err)
	g.Expect(tok).To(BeEmpty())
	g.Expect(*foreignReads).To(BeZero(), "the other namespace's Secret must not even be read")
}

// The KairosConfig's own namespace keeps working, whether left empty or named.
func TestSecretRefKey_OwnNamespace(t *testing.T) {
	g := NewWithT(t)
	for _, ns := range []string{"", "default"} {
		key, err := secretRefKey("default", ns, "pw")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(key.Namespace).To(Equal("default"))
		g.Expect(key.Name).To(Equal("pw"))
	}
}

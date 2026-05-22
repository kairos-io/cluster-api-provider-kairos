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
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

func newCleanupTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	return scheme
}

// TestDeleteBootstrapSecret_NoDataSecretName covers the "nothing to delete"
// path: KairosConfig.Status.DataSecretName is nil. Returns gone=true so the
// caller can immediately remove the finalizer.
func TestDeleteBootstrapSecret_NoDataSecretName(t *testing.T) {
	g := NewWithT(t)
	scheme := newCleanupTestScheme(t)

	kc := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kc",
			Namespace: "default",
		},
		// Status.DataSecretName intentionally nil.
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kc).Build()

	gone, err := deleteBootstrapSecret(context.Background(), c, kc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gone).To(BeTrue())
}

// TestDeleteBootstrapSecret_AlreadyAbsent covers the "Secret was already
// reaped" path: Status.DataSecretName points at a name that doesn't exist.
// Returns gone=true.
func TestDeleteBootstrapSecret_AlreadyAbsent(t *testing.T) {
	g := NewWithT(t)
	scheme := newCleanupTestScheme(t)

	secretName := "kc-bootstrap"
	kc := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kc",
			Namespace: "default",
		},
		Status: bootstrapv1beta2.KairosConfigStatus{
			DataSecretName: &secretName,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kc).Build()

	gone, err := deleteBootstrapSecret(context.Background(), c, kc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gone).To(BeTrue())
}

// TestDeleteBootstrapSecret_DeletesPresent covers the happy path: the Secret
// exists and we issue Delete. Returns gone=false so the caller requeues
// once to let the apiserver finish GC; a follow-up call observes IsNotFound
// and returns gone=true.
func TestDeleteBootstrapSecret_DeletesPresent(t *testing.T) {
	g := NewWithT(t)
	scheme := newCleanupTestScheme(t)

	secretName := "kc-bootstrap"
	kc := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kc",
			Namespace: "default",
		},
		Status: bootstrapv1beta2.KairosConfigStatus{
			DataSecretName: &secretName,
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: "default",
		},
		Data: map[string][]byte{
			"value": []byte("#cloud-config\nfake"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kc, secret).Build()

	gone, err := deleteBootstrapSecret(context.Background(), c, kc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gone).To(BeFalse(), "first pass returns gone=false to trigger a requeue and confirm GC")

	// Follow-up pass: the fake client GC'd it synchronously; we should now
	// observe IsNotFound and return gone=true.
	gone, err = deleteBootstrapSecret(context.Background(), c, kc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gone).To(BeTrue(), "follow-up pass observes IsNotFound and returns gone=true")

	// Sanity: the Secret really is gone.
	got := &corev1.Secret{}
	getErr := c.Get(context.Background(), types.NamespacedName{Name: secretName, Namespace: "default"}, got)
	g.Expect(getErr).To(HaveOccurred())
}

// TestDeleteBootstrapSecret_EmptyDataSecretName covers the edge case of an
// empty (zero-value pointer-to-empty-string) DataSecretName, which the spec
// is careful to treat as "no secret".
func TestDeleteBootstrapSecret_EmptyDataSecretName(t *testing.T) {
	g := NewWithT(t)
	scheme := newCleanupTestScheme(t)

	empty := ""
	kc := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kc",
			Namespace: "default",
		},
		Status: bootstrapv1beta2.KairosConfigStatus{
			DataSecretName: &empty,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kc).Build()

	gone, err := deleteBootstrapSecret(context.Background(), c, kc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gone).To(BeTrue())
}

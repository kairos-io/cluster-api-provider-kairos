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
	"context"
	"errors"
	"os"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/controllers/bootstrap"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/controllers/controlplane"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/secretcache"
)

// TestManagerSecretCache_NoSecretBodyIsCached is the end-to-end guard for
// kairos-io/kairos#5465 and #5499. It starts a manager with the Secret
// settings main.go applies (secretcache.Apply, the same function, not a copy),
// registers the real KairosConfig and KairosControlPlane controllers, so their
// Secret watches are set up exactly as in production, and then asserts:
//
//   - a Secret read through the manager's client is served live;
//   - the metadata-only watch cache holds the Secret without managedFields or
//     the last-applied-configuration annotation;
//   - there is no typed Secret informer anywhere in the cache, so no Secret
//     data is resident.
//
// ReaderFailOnMissingInformer makes the last point observable: a typed cache
// read returns ErrResourceNotCached instead of silently starting an informer.
// Turning any Secret watch back into a typed watch, or dropping DisableFor,
// fails this test.
//
// The non-Secret manager options (metrics, webhook, leader election) are the
// test harness's own; only the Secret settings are under test here.
func TestManagerSecretCache_NoSecretBodyIsCached(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)

	crdPaths := []string{"../../config/crd/bases"}
	if _, err := os.Stat("../../test/crd/capi/cluster-api-components.yaml"); err == nil {
		crdPaths = append(crdPaths, "../../test/crd/capi")
	}
	testEnv := &envtest.Environment{CRDDirectoryPaths: crdPaths}
	cfg, err := testEnv.Start()
	g.Expect(err).NotTo(HaveOccurred())
	defer func() {
		g.Expect(testEnv.Stop()).To(Succeed())
	}()

	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())

	opts := ctrl.Options{
		Scheme:  scheme,
		Logger:  log.Log,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// Multiple managers share this process; see bootstrap_integration_test.go.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
		Cache:      cache.Options{ReaderFailOnMissingInformer: true},
	}
	secretcache.Apply(&opts)

	mgr, err := manager.New(cfg, opts)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect((&bootstrap.KairosConfigReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)).To(Succeed())
	g.Expect((&controlplane.KairosControlPlaneReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)).To(Succeed())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = mgr.Start(ctx)
	}()
	g.Eventually(func() bool {
		return mgr.GetCache().WaitForCacheSync(ctx)
	}, 20*time.Second).Should(BeTrue())

	c := mgr.GetClient()
	g.Expect(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-tenant"}})).To(Succeed())
	key := types.NamespacedName{Namespace: "unrelated-tenant", Name: "other-kubeconfig"}
	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "other"},
			Annotations: map[string]string{
				secretcache.LastAppliedConfigAnnotation: `{"apiVersion":"v1","kind":"Secret","data":{"value":"Ym9keQ=="}}`,
			},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": []byte("body")},
	})).To(Succeed())

	// The client read is live and returns the data.
	got := &corev1.Secret{}
	g.Expect(c.Get(ctx, key, got)).To(Succeed())
	g.Expect(string(got.Data["value"])).To(Equal("body"))

	// The metadata informer behind the Secret watches sees the object, stripped.
	pom := &metav1.PartialObjectMetadata{}
	pom.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	g.Eventually(func() error {
		return mgr.GetCache().Get(ctx, key, pom)
	}, 10*time.Second).Should(Succeed())
	g.Expect(pom.GetManagedFields()).To(BeNil(), "managedFields must be stripped from cached Secret metadata")
	g.Expect(pom.GetAnnotations()).NotTo(HaveKey(secretcache.LastAppliedConfigAnnotation),
		"last-applied-configuration carries the Secret body and must be stripped")

	// No typed Secret informer exists, so no Secret body is resident.
	err = mgr.GetCache().Get(ctx, key, &corev1.Secret{})
	var notCached *cache.ErrResourceNotCached
	g.Expect(errors.As(err, &notCached)).To(BeTrue(),
		"a typed Secret informer exists in the manager cache (got %v); some path caches Secret bodies", err)
}

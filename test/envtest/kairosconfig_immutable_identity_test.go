/*
Copyright 2026 The Kairos CAPI Authors.

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
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// TestKairosConfigIdentityIsImmutable drives the CRD's transition rules on
// KairosConfig.spec through a real apiserver (KD-88): role and distribution
// cannot change once set, and a control-plane KairosConfig's hostname cannot
// be set, changed or cleared after creation. envtest installs no webhooks, so
// only CRD defaulting and CEL run here, which is where the rules live.
func TestKairosConfigIdentityIsImmutable(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	g.Expect(err).NotTo(HaveOccurred())
	defer func() {
		g.Expect(testEnv.Stop()).To(Succeed())
	}()

	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const ns = "kd88"
	g.Expect(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

	n := 0
	create := func(t *testing.T, spec bootstrapv1beta2.KairosConfigSpec, finalizers ...string) *bootstrapv1beta2.KairosConfig {
		t.Helper()
		n++
		spec.KubernetesVersion = "v1.36.1"
		kc := &bootstrapv1beta2.KairosConfig{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: fmt.Sprintf("kc-%d", n), Finalizers: finalizers},
			Spec:       spec,
		}
		NewWithT(t).Expect(c.Create(ctx, kc)).To(Succeed())
		return kc
	}
	controlPlane := func(distribution, hostname string) bootstrapv1beta2.KairosConfigSpec {
		return bootstrapv1beta2.KairosConfigSpec{Role: "control-plane", Distribution: distribution, Hostname: hostname}
	}
	worker := func(distribution, hostname string) bootstrapv1beta2.KairosConfigSpec {
		return bootstrapv1beta2.KairosConfigSpec{Role: "worker", Distribution: distribution, Hostname: hostname}
	}
	update := func(kc *bootstrapv1beta2.KairosConfig, mutate func(*bootstrapv1beta2.KairosConfig)) error {
		mutate(kc)
		return c.Update(ctx, kc)
	}
	stored := func(t *testing.T, kc *bootstrapv1beta2.KairosConfig) *bootstrapv1beta2.KairosConfig {
		t.Helper()
		live := &bootstrapv1beta2.KairosConfig{}
		NewWithT(t).Expect(c.Get(ctx, client.ObjectKeyFromObject(kc), live)).To(Succeed())
		return live
	}
	// expectForbidden asserts err is the apiserver's Invalid error with a
	// FieldValueForbidden cause on path.
	expectForbidden := func(t *testing.T, err error, path string) {
		t.Helper()
		g := NewWithT(t)
		g.Expect(err).To(HaveOccurred())
		g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want Invalid, got %v", err)
		var status apierrors.APIStatus
		g.Expect(errors.As(err, &status)).To(BeTrue())
		details := status.Status().Details
		g.Expect(details).NotTo(BeNil())
		g.Expect(details.Causes).To(ContainElement(SatisfyAll(
			HaveField("Type", metav1.CauseType(field.ErrorTypeForbidden)),
			HaveField("Field", path),
		)), "causes: %+v", details.Causes)
	}

	// Rejected.

	t.Run("distribution k3s to k0s", func(t *testing.T) {
		kc := create(t, worker("k3s", ""))
		expectForbidden(t, update(kc, func(kc *bootstrapv1beta2.KairosConfig) { kc.Spec.Distribution = "k0s" }), "spec.distribution")
		NewWithT(t).Expect(stored(t, kc).Spec.Distribution).To(Equal("k3s"))
	})

	t.Run("distribution omitted on a k3s config defaults to k0s and is refused", func(t *testing.T) {
		kc := create(t, controlPlane("k3s", ""))
		expectForbidden(t, update(kc, func(kc *bootstrapv1beta2.KairosConfig) { kc.Spec.Distribution = "" }), "spec.distribution")
	})

	t.Run("role control-plane to worker", func(t *testing.T) {
		kc := create(t, controlPlane("k0s", ""))
		expectForbidden(t, update(kc, func(kc *bootstrapv1beta2.KairosConfig) { kc.Spec.Role = "worker" }), "spec.role")
	})

	t.Run("role worker to control-plane", func(t *testing.T) {
		kc := create(t, worker("k0s", "Worker.Example"))
		expectForbidden(t, update(kc, func(kc *bootstrapv1beta2.KairosConfig) { kc.Spec.Role = "control-plane" }), "spec.role")
	})

	t.Run("role omitted on a control-plane config defaults to worker and is refused", func(t *testing.T) {
		kc := create(t, controlPlane("k0s", ""))
		expectForbidden(t, update(kc, func(kc *bootstrapv1beta2.KairosConfig) { kc.Spec.Role = "" }), "spec.role")
	})

	t.Run("control-plane hostname changed", func(t *testing.T) {
		kc := create(t, controlPlane("k0s", "cp-a"))
		expectForbidden(t, update(kc, func(kc *bootstrapv1beta2.KairosConfig) { kc.Spec.Hostname = "cp-b" }), "spec.hostname")
		NewWithT(t).Expect(stored(t, kc).Spec.Hostname).To(Equal("cp-a"))
	})

	t.Run("control-plane hostname set after creation", func(t *testing.T) {
		kc := create(t, controlPlane("k0s", ""))
		expectForbidden(t, update(kc, func(kc *bootstrapv1beta2.KairosConfig) { kc.Spec.Hostname = "cp-a" }), "spec.hostname")
	})

	t.Run("control-plane hostname cleared", func(t *testing.T) {
		kc := create(t, controlPlane("k0s", "cp-a"))
		expectForbidden(t, update(kc, func(kc *bootstrapv1beta2.KairosConfig) { kc.Spec.Hostname = "" }), "spec.hostname")
	})

	t.Run("control-plane hostname changed by merge patch", func(t *testing.T) {
		kc := create(t, controlPlane("k0s", "cp-a"))
		patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"hostname":"cp-b"}}`))
		expectForbidden(t, c.Patch(ctx, kc, patch), "spec.hostname")
	})

	t.Run("demoting a control-plane config to reach its hostname is refused at the first step", func(t *testing.T) {
		kc := create(t, controlPlane("k0s", "cp-a"))
		expectForbidden(t, update(kc, func(kc *bootstrapv1beta2.KairosConfig) { kc.Spec.Role = "worker" }), "spec.role")
		live := stored(t, kc)
		NewWithT(t).Expect(live.Spec.Role).To(Equal("control-plane"))
		expectForbidden(t, update(live, func(kc *bootstrapv1beta2.KairosConfig) { kc.Spec.Hostname = "cp-b" }), "spec.hostname")
	})

	// Allowed.

	t.Run("worker hostname changed", func(t *testing.T) {
		kc := create(t, worker("k0s", "w-a"))
		NewWithT(t).Expect(update(kc, func(kc *bootstrapv1beta2.KairosConfig) { kc.Spec.Hostname = "W-B.example" })).To(Succeed())
	})

	t.Run("control-plane config takes other spec, metadata and finalizer writes", func(t *testing.T) {
		g := NewWithT(t)
		kc := create(t, controlPlane("k3s", "cp-a"))
		g.Expect(update(kc, func(kc *bootstrapv1beta2.KairosConfig) { kc.Spec.DNSServers = []string{"10.0.0.53"} })).To(Succeed())
		// The KCP's removeK0sJoinToken shape: an annotation merge patch.
		g.Expect(c.Patch(ctx, kc, client.RawPatch(types.MergePatchType,
			[]byte(`{"metadata":{"labels":{"a":"b"},"annotations":{"c":"d"}}}`)))).To(Succeed())
		// The bootstrap controller's patch.Helper shape: a finalizer.
		g.Expect(c.Patch(ctx, kc, client.RawPatch(types.MergePatchType,
			[]byte(`{"metadata":{"finalizers":["kairosconfig.bootstrap.cluster.x-k8s.io"]}}`)))).To(Succeed())
		live := stored(t, kc)
		g.Expect(live.Spec.DNSServers).To(Equal([]string{"10.0.0.53"}))
		g.Expect(live.Finalizers).To(ContainElement("kairosconfig.bootstrap.cluster.x-k8s.io"))
		g.Expect(update(live, func(kc *bootstrapv1beta2.KairosConfig) { kc.Finalizers = nil })).To(Succeed())
	})

	t.Run("status subresource update", func(t *testing.T) {
		g := NewWithT(t)
		kc := create(t, controlPlane("k0s", "cp-a"))
		kc.Status.Ready = true
		kc.Status.DataSecretName = ptr.To("kc-data")
		g.Expect(c.Status().Update(ctx, kc)).To(Succeed())
		g.Expect(stored(t, kc).Status.Ready).To(BeTrue())
	})

	t.Run("a stored hostname the webhook would refuse stays updatable", func(t *testing.T) {
		kc := create(t, controlPlane("k0s", "CP-Legacy.Example"))
		NewWithT(t).Expect(update(kc, func(kc *bootstrapv1beta2.KairosConfig) { kc.Spec.DNSServers = []string{"10.0.0.53"} })).To(Succeed())
	})

	t.Run("finalizer removed by Update on a deleting control-plane config", func(t *testing.T) {
		g := NewWithT(t)
		kc := create(t, controlPlane("k3s", "cp-a"), "kairosconfig.bootstrap.cluster.x-k8s.io")
		g.Expect(c.Delete(ctx, kc)).To(Succeed())
		live := stored(t, kc)
		g.Expect(live.DeletionTimestamp.IsZero()).To(BeFalse())
		// The bootstrap controller's reconcileDelete shape: the cached object
		// sent back whole, without the finalizer.
		g.Expect(update(live, func(kc *bootstrapv1beta2.KairosConfig) { kc.Finalizers = nil })).To(Succeed())
		g.Eventually(func() bool {
			return apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(kc), &bootstrapv1beta2.KairosConfig{}))
		}, 10*time.Second, 200*time.Millisecond).Should(BeTrue())
	})

	t.Run("KairosConfigTemplate is not restricted", func(t *testing.T) {
		g := NewWithT(t)
		tpl := &bootstrapv1beta2.KairosConfigTemplate{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "tpl"},
			Spec: bootstrapv1beta2.KairosConfigTemplateSpec{Template: bootstrapv1beta2.KairosConfigTemplateResource{
				Spec: bootstrapv1beta2.KairosConfigSpec{Role: "control-plane", Distribution: "k0s", Hostname: "cp-a", KubernetesVersion: "v1.36.1"},
			}},
		}
		g.Expect(c.Create(ctx, tpl)).To(Succeed())
		tpl.Spec.Template.Spec.Role = "worker"
		tpl.Spec.Template.Spec.Distribution = "k3s"
		tpl.Spec.Template.Spec.Hostname = "w-b"
		g.Expect(c.Update(ctx, tpl)).To(Succeed())
	})

	t.Run("a control-plane config deleted and recreated with another hostname", func(t *testing.T) {
		g := NewWithT(t)
		kc := create(t, controlPlane("k0s", "cp-a"))
		key := client.ObjectKeyFromObject(kc)
		g.Expect(c.Delete(ctx, kc)).To(Succeed())
		g.Eventually(func() bool {
			return apierrors.IsNotFound(c.Get(ctx, key, &bootstrapv1beta2.KairosConfig{}))
		}, 10*time.Second, 200*time.Millisecond).Should(BeTrue())
		g.Expect(c.Create(ctx, &bootstrapv1beta2.KairosConfig{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
			Spec:       bootstrapv1beta2.KairosConfigSpec{Role: "control-plane", Distribution: "k0s", Hostname: "cp-b", KubernetesVersion: "v1.36.1"},
		})).To(Succeed())
		g.Expect(stored(t, kc).Spec.Hostname).To(Equal("cp-b"))
	})
}

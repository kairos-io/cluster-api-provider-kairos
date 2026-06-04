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
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"os"
	"strings"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/controllers/bootstrap"
)

func TestBootstrapIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)

	// Setup envtest environment
	crdPaths := []string{
		"../../config/crd/bases",
	}
	// Add CAPI CRDs if available (downloaded by make test-envtest)
	if _, err := os.Stat("../../test/crd/capi/cluster-api-components.yaml"); err == nil {
		crdPaths = append(crdPaths, "../../test/crd/capi")
	}
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     crdPaths,
		ErrorIfCRDPathMissing: false, // Allow missing CAPI CRDs for now
	}

	cfg, err := testEnv.Start()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg).NotTo(BeNil())
	defer func() {
		g.Expect(testEnv.Stop()).To(Succeed())
	}()

	// Create scheme
	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())

	// Create manager
	mgr, err := manager.New(cfg, manager.Options{
		Scheme: scheme,
		Logger: log.Log,
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Setup bootstrap controller
	bootstrapReconciler := &bootstrap.KairosConfigReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
	g.Expect(bootstrapReconciler.SetupWithManager(mgr)).To(Succeed())

	// Start manager
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		g.Expect(mgr.Start(ctx)).To(Succeed())
	}()

	// Wait for manager to be ready
	g.Eventually(func() bool {
		return mgr.GetCache().WaitForCacheSync(ctx)
	}, 10*time.Second).Should(BeTrue())

	// Create test namespace
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-namespace",
		},
	}
	g.Expect(mgr.GetClient().Create(ctx, ns)).To(Succeed())

	// Create Cluster
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "test-namespace",
		},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: &corev1.ObjectReference{
				APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
				Kind:       "DockerCluster",
				Name:       "test-cluster",
			},
		},
	}
	g.Expect(mgr.GetClient().Create(ctx, cluster)).To(Succeed())

	// Create Machine
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "test-namespace",
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: "test-cluster",
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: "test-cluster",
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: &corev1.ObjectReference{
					APIVersion: bootstrapv1beta2.GroupVersion.String(),
					Kind:       "KairosConfig",
					Name:       "test-kairos-config",
					Namespace:  "test-namespace",
				},
			},
		},
	}
	g.Expect(mgr.GetClient().Create(ctx, machine)).To(Succeed())

	// Create KairosConfig
	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-kairos-config",
			Namespace: "test-namespace",
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(machine, clusterv1.GroupVersion.WithKind("Machine")),
			},
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:              "control-plane",
			Distribution:      "k0s",
			KubernetesVersion: "v1.30.0+k0s.0",
			SingleNode:        true,
			UserName:          "kairos",
			UserPassword:      "kairos",
			UserGroups:        []string{"admin"},
		},
	}
	g.Expect(mgr.GetClient().Create(ctx, kairosConfig)).To(Succeed())

	// Wait for bootstrap data to be generated
	g.Eventually(func() bool {
		config := &bootstrapv1beta2.KairosConfig{}
		if err := mgr.GetClient().Get(ctx, types.NamespacedName{
			Name:      "test-kairos-config",
			Namespace: "test-namespace",
		}, config); err != nil {
			return false
		}
		return config.Status.DataSecretName != nil && *config.Status.DataSecretName != ""
	}, 30*time.Second, 1*time.Second).Should(BeTrue())

	// Verify Secret exists
	var secretName string
	g.Eventually(func() bool {
		config := &bootstrapv1beta2.KairosConfig{}
		if err := mgr.GetClient().Get(ctx, types.NamespacedName{
			Name:      "test-kairos-config",
			Namespace: "test-namespace",
		}, config); err != nil {
			return false
		}
		if config.Status.DataSecretName == nil {
			return false
		}
		secretName = *config.Status.DataSecretName
		return true
	}, 10*time.Second).Should(BeTrue())

	secret := &corev1.Secret{}
	g.Eventually(func() bool {
		return mgr.GetClient().Get(ctx, types.NamespacedName{
			Name:      secretName,
			Namespace: "test-namespace",
		}, secret) == nil
	}, 10*time.Second).Should(BeTrue())

	// KD-48: the bootstrap Secret name is deterministic == kairosConfig.Name
	// (no random suffix). This is required for infrastructure providers — notably
	// CAPM3, which derives the userData Secret name from the Machine name — and it
	// prevents the orphaned/duplicate Secrets a fresh random suffix produced on
	// every regeneration. The Machine here carries no spec.bootstrap.dataSecretName,
	// so this exercises the default naming path.
	g.Expect(secretName).To(Equal("test-kairos-config"),
		"bootstrap Secret must be named deterministically after the KairosConfig")

	// And no random-suffixed duplicates accumulate for this KairosConfig: the only
	// Secret whose name is prefixed with the KairosConfig name is the deterministic
	// one. Consistently guards against churn that recreates the Secret under a new
	// name on subsequent reconciles (KD-9 footgun).
	g.Consistently(func() []string {
		list := &corev1.SecretList{}
		if err := mgr.GetClient().List(ctx, list, client.InNamespace("test-namespace")); err != nil {
			return []string{"(list error)"}
		}
		var names []string
		for i := range list.Items {
			if strings.HasPrefix(list.Items[i].Name, "test-kairos-config") {
				names = append(names, list.Items[i].Name)
			}
		}
		return names
	}, 5*time.Second, 1*time.Second).Should(ConsistOf("test-kairos-config"),
		"exactly one bootstrap Secret, named deterministically, must exist (no random-suffixed duplicates)")

	// Verify Secret contains cloud-config with k0s configuration
	g.Expect(secret.Data).To(HaveKey("value"))
	cloudConfig := string(secret.Data["value"])
	g.Expect(cloudConfig).To(ContainSubstring("#cloud-config"))
	g.Expect(cloudConfig).To(ContainSubstring("k0s:"))
	g.Expect(cloudConfig).To(ContainSubstring("enabled: true"))
	g.Expect(cloudConfig).To(ContainSubstring("--single"))
}

// TestBootstrapIntegration_LatchedFailureClearsOnRecovery verifies the KD-14
// flow end-to-end against a real apiserver:
//
//  1. Create a KairosConfig with userPasswordSecretRef pointing at a
//     non-existent Secret.
//  2. Assert Status.FailureReason and Status.FailureMessage get set.
//  3. Create the missing Secret.
//  4. Assert FailureReason/FailureMessage get cleared on the next Reconcile.
//
// Prior to PR-2 the failure fields latched permanently and the CAPI Machine
// controller refused to clone the infra Machine. The KCP-side companion
// test for KD-4 + KD-14 lives in controlplane_integration_test.go.
func TestBootstrapIntegration_LatchedFailureClearsOnRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)

	// Setup envtest environment
	crdPaths := []string{
		"../../config/crd/bases",
	}
	if _, err := os.Stat("../../test/crd/capi/cluster-api-components.yaml"); err == nil {
		crdPaths = append(crdPaths, "../../test/crd/capi")
	}
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     crdPaths,
		ErrorIfCRDPathMissing: false,
	}
	cfg, err := testEnv.Start()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg).NotTo(BeNil())
	defer func() {
		g.Expect(testEnv.Stop()).To(Succeed())
	}()

	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())

	mgr, err := manager.New(cfg, manager.Options{Scheme: scheme, Logger: log.Log})
	g.Expect(err).NotTo(HaveOccurred())

	reconciler := &bootstrap.KairosConfigReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
	g.Expect(reconciler.SetupWithManager(mgr)).To(Succeed())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgrErrCh := make(chan error, 1)
	go func() {
		mgrErrCh <- mgr.Start(ctx)
	}()

	g.Eventually(func() bool {
		return mgr.GetCache().WaitForCacheSync(ctx)
	}, 10*time.Second).Should(BeTrue())

	const (
		nsName            = "kd14-recover"
		clusterName       = "kd14-cluster"
		machineName       = "kd14-machine"
		kcName            = "kd14-kc"
		missingSecretName = "kd14-user-password"
	)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	g.Expect(mgr.GetClient().Create(ctx, ns)).To(Succeed())

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: nsName},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: &corev1.ObjectReference{
				APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
				Kind:       "DockerCluster",
				Name:       clusterName,
			},
		},
	}
	g.Expect(mgr.GetClient().Create(ctx, cluster)).To(Succeed())

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineName,
			Namespace: nsName,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: clusterName,
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: &corev1.ObjectReference{
					APIVersion: bootstrapv1beta2.GroupVersion.String(),
					Kind:       "KairosConfig",
					Name:       kcName,
					Namespace:  nsName,
				},
			},
		},
	}
	g.Expect(mgr.GetClient().Create(ctx, machine)).To(Succeed())

	// KairosConfig with a userPasswordSecretRef pointing at a Secret that
	// does NOT exist yet. The controller's first reconcile will fail when
	// resolveUserPassword tries to fetch the Secret and set the failure
	// fields.
	kc := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcName,
			Namespace: nsName,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(machine, clusterv1.GroupVersion.WithKind("Machine")),
			},
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:              "control-plane",
			Distribution:      "k0s",
			KubernetesVersion: "v1.30.0+k0s.0",
			SingleNode:        true,
			UserName:          "kairos",
			UserPasswordSecretRef: &bootstrapv1beta2.UserPasswordSecretReference{
				Name: missingSecretName,
			},
			UserGroups: []string{"admin"},
		},
	}
	g.Expect(mgr.GetClient().Create(ctx, kc)).To(Succeed())

	// Assert FailureReason / FailureMessage get set.
	g.Eventually(func() string {
		got := &bootstrapv1beta2.KairosConfig{}
		if err := mgr.GetClient().Get(ctx, types.NamespacedName{Name: kcName, Namespace: nsName}, got); err != nil {
			return ""
		}
		return got.Status.FailureReason
	}, 30*time.Second, 1*time.Second).ShouldNot(BeEmpty(), "FailureReason should be set after Secret-missing failure")

	got := &bootstrapv1beta2.KairosConfig{}
	g.Expect(mgr.GetClient().Get(ctx, types.NamespacedName{Name: kcName, Namespace: nsName}, got)).To(Succeed())
	g.Expect(got.Status.FailureMessage).NotTo(BeEmpty(), "FailureMessage should be set alongside FailureReason")

	// Create the missing Secret to unblock resolveUserPassword.
	pwSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: missingSecretName, Namespace: nsName},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	}
	g.Expect(mgr.GetClient().Create(ctx, pwSecret)).To(Succeed())

	// Assert FailureReason / FailureMessage get cleared on the next
	// successful reconcile (KD-14). Bump the KairosConfig generation so the
	// controller reconciles promptly rather than waiting for the next
	// rate-limited requeue.
	g.Expect(mgr.GetClient().Get(ctx, types.NamespacedName{Name: kcName, Namespace: nsName}, got)).To(Succeed())
	if got.Annotations == nil {
		got.Annotations = map[string]string{}
	}
	got.Annotations["kairos.io/kd14-poke"] = "1"
	g.Expect(mgr.GetClient().Update(ctx, got)).To(Succeed())

	g.Eventually(func() string {
		got := &bootstrapv1beta2.KairosConfig{}
		if err := mgr.GetClient().Get(ctx, types.NamespacedName{Name: kcName, Namespace: nsName}, got); err != nil {
			return "(get error)"
		}
		return got.Status.FailureReason
	}, 30*time.Second, 1*time.Second).Should(BeEmpty(), "FailureReason should be cleared after successful reconcile (KD-14)")

	g.Expect(mgr.GetClient().Get(ctx, types.NamespacedName{Name: kcName, Namespace: nsName}, got)).To(Succeed())
	g.Expect(got.Status.FailureMessage).To(BeEmpty(), "FailureMessage should be cleared alongside FailureReason")

	cancel()
	// Drain manager goroutine; ignore Canceled.
	<-mgrErrCh
}

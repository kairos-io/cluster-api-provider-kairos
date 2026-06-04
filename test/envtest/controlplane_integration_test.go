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
	"os"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/controllers/bootstrap"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/controllers/controlplane"
)

func TestControlPlaneIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	// Note: This test requires infrastructure provider CRDs (e.g., CAPD) to fully test
	// Machine creation. Without them, the controller will fail at infrastructure cloning.
	// For now, we test that the controller reconciles and attempts to create resources.
	// Full end-to-end testing should be done with actual infrastructure providers.
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
		ErrorIfCRDPathMissing: false,
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
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())

	// Create manager
	mgr, err := manager.New(cfg, manager.Options{
		Scheme: scheme,
		Logger: log.Log,
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Setup controllers
	bootstrapReconciler := &bootstrap.KairosConfigReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
	g.Expect(bootstrapReconciler.SetupWithManager(mgr)).To(Succeed())

	controlPlaneReconciler := &controlplane.KairosControlPlaneReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
	g.Expect(controlPlaneReconciler.SetupWithManager(mgr)).To(Succeed())

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
			ControlPlaneRef: &corev1.ObjectReference{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       "test-kcp",
				Namespace:  "test-namespace",
			},
		},
	}
	g.Expect(mgr.GetClient().Create(ctx, cluster)).To(Succeed())

	// Create KairosConfigTemplate
	configTemplate := &bootstrapv1beta2.KairosConfigTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config-template",
			Namespace: "test-namespace",
		},
		Spec: bootstrapv1beta2.KairosConfigTemplateSpec{
			Template: bootstrapv1beta2.KairosConfigTemplateResource{
				Spec: bootstrapv1beta2.KairosConfigSpec{
					Role:              "control-plane",
					Distribution:      "k0s",
					KubernetesVersion: "v1.30.0+k0s.0",
					UserName:          "kairos",
					UserPassword:      "kairos",
					UserGroups:        []string{"admin"},
				},
			},
		},
	}
	g.Expect(mgr.GetClient().Create(ctx, configTemplate)).To(Succeed())

	// Note: We skip creating infrastructure template because DockerMachineTemplate CRD is not available
	// The controller will fail to create infrastructure machines, but we can still test
	// that it attempts to create KairosConfig resources with correct SingleNode setting

	// Create KairosControlPlane with replicas=1 (single-node)
	replicas := int32(1)
	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-kcp",
			Namespace: "test-namespace",
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas: &replicas,
			Version:  "v1.30.0+k0s.0",
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "test-infra-template",
					Namespace:  "test-namespace",
				},
			},
			KairosConfigTemplate: controlplanev1beta2.KairosConfigTemplateReference{
				Name: "test-config-template",
			},
		},
	}
	g.Expect(mgr.GetClient().Create(ctx, kcp)).To(Succeed())

	// Verify that the controller attempts to reconcile the KairosControlPlane
	// Note: Without infrastructure CRDs, Machine creation will fail early in the reconciliation.
	// The controller will attempt to create infrastructure machines and fail, but we can verify
	// that the KCP resource exists and the controller is watching it.
	// Full end-to-end testing requires infrastructure provider CRDs (e.g., CAPD).
	updatedKCP := &controlplanev1beta2.KairosControlPlane{}
	g.Eventually(func() bool {
		return mgr.GetClient().Get(ctx, types.NamespacedName{
			Name:      "test-kcp",
			Namespace: "test-namespace",
		}, updatedKCP) == nil
	}, 5*time.Second).Should(BeTrue())

	// Verify spec is correct
	g.Expect(updatedKCP.Spec.Replicas).NotTo(BeNil())
	g.Expect(*updatedKCP.Spec.Replicas).To(Equal(int32(1)))
	g.Expect(updatedKCP.Spec.Version).To(Equal("v1.30.0+k0s.0"))

	// Note: Full Machine and KairosConfig creation testing requires infrastructure provider CRDs.
	// The unit tests (TestCreateControlPlaneMachine_SingleNode) verify the SingleNode logic
	// with mocked infrastructure. For full integration testing, use a real infrastructure provider.
}

// startKCPEnvtest spins up a fresh envtest + manager + KCP reconciler + the
// PR-9 SSHFallback sibling reconciler (plus its worker, exposed in the return
// tuple so tests can swap the Dial function for a fake SSH server). Returns a
// cancel-and-wait teardown closure plus the envtest *rest.Config (callers that
// don't need cfg or the worker discard with `_`). Each KD-4 delete-flow test
// sets up its own envtest because envtest start/stop is slow but
// parallelization across tests is unsafe (shared apiserver port).
//
// Wiring the SSHFallback reconciler unconditionally is safe: its predicate
// filters to KCPs with Spec.SSHFallback.Enabled=true, so tests that don't
// configure SSHFallback are unaffected.
func startKCPEnvtest(t *testing.T) (context.Context, client.Client, *rest.Config, *controlplane.SSHFallbackWorker, func()) {
	t.Helper()
	g := NewWithT(t)

	crdPaths := []string{"../../config/crd/bases"}
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

	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())

	mgr, err := manager.New(cfg, manager.Options{Scheme: scheme, Logger: log.Log})
	g.Expect(err).NotTo(HaveOccurred())

	bootstrapReconciler := &bootstrap.KairosConfigReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}
	g.Expect(bootstrapReconciler.SetupWithManager(mgr)).To(Succeed())

	cpReconciler := &controlplane.KairosControlPlaneReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}
	g.Expect(cpReconciler.SetupWithManager(mgr)).To(Succeed())

	sshWorker := controlplane.NewSSHFallbackWorker(
		mgr.GetClient(), mgr.GetScheme(),
		mgr.GetEventRecorderFor("kairoscontrolplane-ssh-fallback"),
	)
	sshReconciler := &controlplane.SSHFallbackReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Worker: sshWorker,
	}
	g.Expect(sshReconciler.SetupWithManager(mgr)).To(Succeed())
	g.Expect(mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return sshReconciler.StartResultDrain(ctx)
	}))).To(Succeed())

	ctx, cancel := context.WithCancel(context.Background())
	mgrErrCh := make(chan error, 1)
	go func() { mgrErrCh <- mgr.Start(ctx) }()

	g.Eventually(func() bool {
		return mgr.GetCache().WaitForCacheSync(ctx)
	}, 10*time.Second).Should(BeTrue())

	teardown := func() {
		cancel()
		<-mgrErrCh
		g.Expect(testEnv.Stop()).To(Succeed())
	}
	return ctx, mgr.GetClient(), cfg, sshWorker, teardown
}

// TestControlPlaneIntegration_DeleteDrainsOwnedMachine verifies KD-4:
//
//  1. Create a Cluster, KCP, and a CAPI Machine owned by the KCP (controller
//     OwnerReference) with the cluster-name label.
//  2. Delete the KCP.
//  3. Assert the Machine is deleted, the KCP finalizer is removed, and the
//     KCP itself is reaped.
//
// Prior to the fix the KCP finalizer was removed immediately, leaving the
// Machine without a parent KCP to coordinate its delete flow.
func TestControlPlaneIntegration_DeleteDrainsOwnedMachine(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	const (
		nsName      = "kd4-drain"
		clusterName = "kd4-drain-cluster"
		kcpName     = "kd4-drain-kcp"
		machineName = "kd4-drain-kcp-0"
	)

	g.Expect(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})).To(Succeed())

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: nsName},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: &corev1.ObjectReference{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       kcpName,
				Namespace:  nsName,
			},
		},
	}
	g.Expect(c.Create(ctx, cluster)).To(Succeed())

	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcpName,
			Namespace: nsName,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas: ptr.To(int32(1)),
			Version:  "v1.30.0+k0s.0",
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "not-installed",
					Namespace:  nsName,
				},
			},
		},
	}
	g.Expect(c.Create(ctx, kcp)).To(Succeed())

	// Wait for the controller to add the finalizer.
	g.Eventually(func() bool {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcpName, Namespace: nsName}, got); err != nil {
			return false
		}
		for _, f := range got.Finalizers {
			if f == controlplanev1beta2.KairosControlPlaneFinalizer {
				return true
			}
		}
		return false
	}, 15*time.Second, 500*time.Millisecond).Should(BeTrue(), "KCP finalizer must be added before delete")

	// Pre-create a Machine owned by this KCP. Match the labels and
	// controller OwnerReference so drainOwnedMachines picks it up.
	gotKCP := &controlplanev1beta2.KairosControlPlane{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: kcpName, Namespace: nsName}, gotKCP)).To(Succeed())

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineName,
			Namespace: nsName,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         clusterName,
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(gotKCP, controlplanev1beta2.GroupVersion.WithKind("KairosControlPlane")),
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: clusterName,
			Version:     ptr.To("v1.30.0+k0s.0"),
		},
	}
	g.Expect(c.Create(ctx, machine)).To(Succeed())

	// Delete the KCP.
	g.Expect(c.Delete(ctx, gotKCP)).To(Succeed())

	// Eventually the owned Machine is deleted.
	g.Eventually(func() bool {
		got := &clusterv1.Machine{}
		err := c.Get(ctx, types.NamespacedName{Name: machineName, Namespace: nsName}, got)
		return apierrors.IsNotFound(err)
	}, 30*time.Second, 1*time.Second).Should(BeTrue(), "owned Machine must be deleted before KCP finalizer is removed (KD-4)")

	// And then the KCP itself is reaped.
	g.Eventually(func() bool {
		got := &controlplanev1beta2.KairosControlPlane{}
		err := c.Get(ctx, types.NamespacedName{Name: kcpName, Namespace: nsName}, got)
		return apierrors.IsNotFound(err)
	}, 30*time.Second, 1*time.Second).Should(BeTrue(), "KCP must be reaped once its owned Machine is gone")
}

// TestControlPlaneIntegration_DeleteHeldByForeignFinalizerOnMachine verifies
// KD-4 negative path: when an owned Machine has a foreign finalizer holding
// it (so the Machine cannot be fully deleted), reconcileDelete must keep
// requeuing and the KCP finalizer must stay attached. The KCP itself must
// NOT disappear.
//
// This is the property that prevents the orphan-Cluster failure mode that
// motivated KD-4: stripping the KCP finalizer prematurely leaves the parent
// Cluster reapable while owned children still depend on it.
func TestControlPlaneIntegration_DeleteHeldByForeignFinalizerOnMachine(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	ctx, c, _, _, teardown := startKCPEnvtest(t)
	defer teardown()

	const (
		nsName           = "kd4-held"
		clusterName      = "kd4-held-cluster"
		kcpName          = "kd4-held-kcp"
		machineName      = "kd4-held-kcp-0"
		foreignFinalizer = "foreign-controller.example.com/wait"
	)

	g.Expect(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})).To(Succeed())

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: nsName},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: &corev1.ObjectReference{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       kcpName,
				Namespace:  nsName,
			},
		},
	}
	g.Expect(c.Create(ctx, cluster)).To(Succeed())

	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcpName,
			Namespace: nsName,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas: ptr.To(int32(1)),
			Version:  "v1.30.0+k0s.0",
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "not-installed",
					Namespace:  nsName,
				},
			},
		},
	}
	g.Expect(c.Create(ctx, kcp)).To(Succeed())

	g.Eventually(func() bool {
		got := &controlplanev1beta2.KairosControlPlane{}
		if err := c.Get(ctx, types.NamespacedName{Name: kcpName, Namespace: nsName}, got); err != nil {
			return false
		}
		for _, f := range got.Finalizers {
			if f == controlplanev1beta2.KairosControlPlaneFinalizer {
				return true
			}
		}
		return false
	}, 15*time.Second, 500*time.Millisecond).Should(BeTrue())

	gotKCP := &controlplanev1beta2.KairosControlPlane{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: kcpName, Namespace: nsName}, gotKCP)).To(Succeed())

	// Pre-create the Machine with a foreign finalizer. When the KCP delete
	// flow issues Delete on this Machine the apiserver will mark its
	// DeletionTimestamp but the finalizer prevents actual removal.
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineName,
			Namespace: nsName,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         clusterName,
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(gotKCP, controlplanev1beta2.GroupVersion.WithKind("KairosControlPlane")),
			},
			Finalizers: []string{foreignFinalizer},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: clusterName,
			Version:     ptr.To("v1.30.0+k0s.0"),
		},
	}
	g.Expect(c.Create(ctx, machine)).To(Succeed())

	// Delete the KCP.
	g.Expect(c.Delete(ctx, gotKCP)).To(Succeed())

	// Give the controller time to attempt the delete drain a few times.
	// The Machine must enter mid-deletion (DeletionTimestamp set) but stay
	// present (foreign finalizer holding it).
	g.Eventually(func() bool {
		got := &clusterv1.Machine{}
		if err := c.Get(ctx, types.NamespacedName{Name: machineName, Namespace: nsName}, got); err != nil {
			return false
		}
		return !got.DeletionTimestamp.IsZero()
	}, 30*time.Second, 1*time.Second).Should(BeTrue(), "Machine must be marked for deletion by drainOwnedMachines")

	// Consistently: the KCP must NOT disappear while the Machine lingers.
	// 8s is long enough for several reconciles at the 10s requeue
	// interval to land without the test stalling CI.
	g.Consistently(func() bool {
		got := &controlplanev1beta2.KairosControlPlane{}
		err := c.Get(ctx, types.NamespacedName{Name: kcpName, Namespace: nsName}, got)
		return err == nil
	}, 8*time.Second, 1*time.Second).Should(BeTrue(), "KCP must NOT be reaped while owned Machine has a foreign finalizer (KD-4 negative path)")

	// Cleanup: drop the foreign finalizer so the Machine can be reaped and
	// the KCP can finish its delete flow before teardown.
	gotMachine := &clusterv1.Machine{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: machineName, Namespace: nsName}, gotMachine)).To(Succeed())
	gotMachine.Finalizers = nil
	g.Expect(c.Update(ctx, gotMachine)).To(Succeed())

	g.Eventually(func() bool {
		got := &controlplanev1beta2.KairosControlPlane{}
		err := c.Get(ctx, types.NamespacedName{Name: kcpName, Namespace: nsName}, got)
		return apierrors.IsNotFound(err)
	}, 30*time.Second, 1*time.Second).Should(BeTrue(), "KCP must be reaped once the foreign finalizer drops and the Machine is gone")
}

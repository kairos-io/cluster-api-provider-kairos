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

package controlplane

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

func TestCreateControlPlaneMachine_SingleNode(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	replicas := int32(1)
	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-kcp",
			Namespace: "default",
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas:     &replicas,
			Version:      "v1.30.0+k0s.0",
			Distribution: "k3s",
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "test-template",
					Namespace:  "default",
				},
			},
			KairosConfigTemplate: controlplanev1beta2.KairosConfigTemplateReference{
				Name: "test-config-template",
			},
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}

	template := &bootstrapv1beta2.KairosConfigTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config-template",
			Namespace: "default",
		},
		Spec: bootstrapv1beta2.KairosConfigTemplateSpec{
			Template: bootstrapv1beta2.KairosConfigTemplateResource{
				Spec: bootstrapv1beta2.KairosConfigSpec{
					Role:              "control-plane",
					Distribution:      "k0s",
					KubernetesVersion: "v1.30.0+k0s.0",
				},
			},
		},
	}

	// Create a mock infrastructure template (DockerMachineTemplate)
	infraTemplate := &unstructured.Unstructured{}
	infraTemplate.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "DockerMachineTemplate",
	})
	infraTemplate.SetName("test-template")
	infraTemplate.SetNamespace("default")
	infraTemplate.Object["spec"] = map[string]interface{}{
		"template": map[string]interface{}{
			"spec": map[string]interface{}{},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(template, infraTemplate).Build()
	reconciler := &KairosControlPlaneReconciler{
		Client: client,
		Scheme: scheme,
	}

	err := reconciler.createControlPlaneMachine(
		context.Background(),
		log.Log,
		kcp,
		cluster,
		0,
	)

	g.Expect(err).NotTo(HaveOccurred())

	// Verify KairosConfig was created with SingleNode = true
	kairosConfig := &bootstrapv1beta2.KairosConfig{}
	err = client.Get(context.Background(), types.NamespacedName{
		Name:      "test-kcp-0",
		Namespace: "default",
	}, kairosConfig)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kairosConfig.Spec.SingleNode).To(BeTrue())
	g.Expect(kairosConfig.Spec.Role).To(Equal("control-plane"))
	g.Expect(kairosConfig.Spec.Distribution).To(Equal("k3s"))
}

func TestCreateControlPlaneMachine_MultiNode(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	replicas := int32(3)
	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-kcp",
			Namespace: "default",
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas: &replicas,
			Version:  "v1.30.0+k0s.0",
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "test-template",
					Namespace:  "default",
				},
			},
			KairosConfigTemplate: controlplanev1beta2.KairosConfigTemplateReference{
				Name: "test-config-template",
			},
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}

	template := &bootstrapv1beta2.KairosConfigTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config-template",
			Namespace: "default",
		},
		Spec: bootstrapv1beta2.KairosConfigTemplateSpec{
			Template: bootstrapv1beta2.KairosConfigTemplateResource{
				Spec: bootstrapv1beta2.KairosConfigSpec{
					Role:              "control-plane",
					Distribution:      "k0s",
					KubernetesVersion: "v1.30.0+k0s.0",
				},
			},
		},
	}

	// Create a mock infrastructure template (DockerMachineTemplate)
	infraTemplate := &unstructured.Unstructured{}
	infraTemplate.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "DockerMachineTemplate",
	})
	infraTemplate.SetName("test-template")
	infraTemplate.SetNamespace("default")
	infraTemplate.Object["spec"] = map[string]interface{}{
		"template": map[string]interface{}{
			"spec": map[string]interface{}{},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(template, infraTemplate).Build()
	reconciler := &KairosControlPlaneReconciler{
		Client: client,
		Scheme: scheme,
	}

	err := reconciler.createControlPlaneMachine(
		context.Background(),
		log.Log,
		kcp,
		cluster,
		0,
	)

	g.Expect(err).NotTo(HaveOccurred())

	// Verify KairosConfig was created with SingleNode = false
	kairosConfig := &bootstrapv1beta2.KairosConfig{}
	err = client.Get(context.Background(), types.NamespacedName{
		Name:      "test-kcp-0",
		Namespace: "default",
	}, kairosConfig)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kairosConfig.Spec.SingleNode).To(BeFalse())
	g.Expect(kairosConfig.Spec.Role).To(Equal("control-plane"))
}

func TestResolveSSHHost_KubevirtFallback(t *testing.T) {
	g := NewWithT(t)

	machine := &clusterv1.Machine{
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: corev1.ObjectReference{
				Kind: "KubevirtMachine",
			},
		},
	}
	cluster := &clusterv1.Cluster{
		Spec: clusterv1.ClusterSpec{
			ControlPlaneEndpoint: clusterv1.APIEndpoint{
				Host: "10.111.124.223",
			},
		},
	}

	host, err := resolveSSHHost(machine, cluster, "", errors.New("no ip in status"), log.Log)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(host).To(Equal("10.111.124.223"))
}

func TestResolveSSHHost_NoFallbackForVsphere(t *testing.T) {
	g := NewWithT(t)

	expectedErr := errors.New("no ip in status")
	machine := &clusterv1.Machine{
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: corev1.ObjectReference{
				Kind: "VSphereMachine",
			},
		},
	}
	cluster := &clusterv1.Cluster{
		Spec: clusterv1.ClusterSpec{
			ControlPlaneEndpoint: clusterv1.APIEndpoint{
				Host: "10.111.124.223",
			},
		},
	}

	_, err := resolveSSHHost(machine, cluster, "", expectedErr, log.Log)
	g.Expect(err).To(MatchError(expectedErr))
}

func TestGetNodeIP_KubevirtVMIFallback(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	kubevirtMachine := &unstructured.Unstructured{}
	kubevirtMachine.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "KubevirtMachine",
	})
	kubevirtMachine.SetName("test-km")
	kubevirtMachine.SetNamespace("default")

	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "kubevirt.io",
		Version: "v1",
		Kind:    "VirtualMachineInstance",
	})
	vmi.SetName("test-km")
	vmi.SetNamespace("default")
	_ = unstructured.SetNestedSlice(vmi.Object, []interface{}{
		map[string]interface{}{
			"name":      "default",
			"ipAddress": "192.168.100.10",
		},
	}, "status", "interfaces")

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
		},
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: corev1.ObjectReference{
				APIVersion: "infrastructure.cluster.x-k8s.io/v1alpha1",
				Kind:       "KubevirtMachine",
				Name:       "test-km",
				Namespace:  "default",
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kubevirtMachine, vmi).Build()
	reconciler := &KairosControlPlaneReconciler{
		Client: client,
		Scheme: scheme,
	}

	ip, err := reconciler.getNodeIP(context.Background(), log.Log, machine)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ip).To(Equal("192.168.100.10"))
}

// newKCPTestScheme registers the schemes the KCP Reconcile flow depends on
// for these unit tests.
func newKCPTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	return scheme
}

// TestReconcile_NoClusterFlushesObservedGeneration verifies KD-14: when a KCP
// has no associated Cluster yet (cluster-name label missing and no Cluster
// references it via ControlPlaneRef), the deferred patch.Helper still flushes
// observedGeneration. Prior to the fix the patch helper was created after the
// no-cluster early-return and observedGeneration drifted permanently.
func TestReconcile_NoClusterFlushesObservedGeneration(t *testing.T) {
	g := NewWithT(t)
	scheme := newKCPTestScheme(t)

	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "orphan-kcp",
			Namespace:  "default",
			Generation: 4,
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas: ptr.To(int32(1)),
			Version:  "v1.30.0+k0s.0",
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "test-template",
					Namespace:  "default",
				},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(kcp).
		WithStatusSubresource(&controlplanev1beta2.KairosControlPlane{}).
		Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "orphan-kcp", Namespace: "default"}})
	g.Expect(err).NotTo(HaveOccurred())

	got := &controlplanev1beta2.KairosControlPlane{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "orphan-kcp", Namespace: "default"}, got)).To(Succeed())
	g.Expect(got.Status.ObservedGeneration).To(Equal(int64(4)))
	// Finalizer should be added even though the Cluster wasn't found.
	g.Expect(got.Finalizers).To(ContainElement(controlplanev1beta2.KairosControlPlaneFinalizer))
}

// TestReconcile_FailureFieldsClearedOnSuccess verifies KD-14 maintainer-
// confirmed decision #3: when reconcileMachines returns nil, latched
// FailureReason/FailureMessage are cleared unconditionally (no
// ReadyReplicas > 0 gate). Prior to the fix a transient failure during
// reconcileMachines would set FailureReason and a follow-up successful
// reconcile would not clear it unless at least one machine became ready --
// operators had to manually patch the status.
func TestReconcile_FailureFieldsClearedOnSuccess(t *testing.T) {
	g := NewWithT(t)
	scheme := newKCPTestScheme(t)

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "recover-cluster",
			Namespace: "default",
		},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: &corev1.ObjectReference{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       "recover-kcp",
				Namespace:  "default",
			},
		},
	}
	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "recover-kcp",
			Namespace:  "default",
			Generation: 9,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: "recover-cluster",
			},
		},
		Spec: controlplanev1beta2.KairosControlPlaneSpec{
			Replicas: ptr.To(int32(1)),
			Version:  "v1.30.0+k0s.0",
			MachineTemplate: controlplanev1beta2.KairosControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "DockerMachineTemplate",
					Name:       "test-template",
					Namespace:  "default",
				},
			},
		},
		Status: controlplanev1beta2.KairosControlPlaneStatus{
			FailureReason:  controlplanev1beta2.ControlPlaneInitializationFailedReason,
			FailureMessage: "transient: KairosConfigTemplate not found on first attempt",
		},
	}
	// Pre-create a matching control-plane Machine so reconcileMachines is a no-op.
	// Machine has no NodeRef -- ReadyReplicas stays 0. Under the OLD code path
	// the FailureReason/FailureMessage gate (ReadyReplicas > 0) would keep
	// failureReason latched; under the NEW code path it MUST be cleared.
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "recover-kcp-0",
			Namespace: "default",
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         "recover-cluster",
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kcp, controlplanev1beta2.GroupVersion.WithKind("KairosControlPlane")),
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: "recover-cluster",
			Version:     ptr.To("v1.30.0+k0s.0"),
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster, kcp, machine).
		WithStatusSubresource(&controlplanev1beta2.KairosControlPlane{}, &clusterv1.Cluster{}).
		Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "recover-kcp", Namespace: "default"}})
	g.Expect(err).NotTo(HaveOccurred())

	got := &controlplanev1beta2.KairosControlPlane{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "recover-kcp", Namespace: "default"}, got)).To(Succeed())
	g.Expect(got.Status.FailureReason).To(BeEmpty(), "FailureReason should be cleared on successful reconcileMachines, regardless of ReadyReplicas (KD-14)")
	g.Expect(got.Status.FailureMessage).To(BeEmpty(), "FailureMessage should be cleared on successful reconcileMachines, regardless of ReadyReplicas (KD-14)")
	g.Expect(got.Status.ReadyReplicas).To(Equal(int32(0)), "ReadyReplicas should still be 0 since the Machine has no NodeRef -- proving the clear is unconditional")
	g.Expect(got.Status.ObservedGeneration).To(Equal(int64(9)))
}

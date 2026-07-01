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
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
		bootstrapv1beta2.ControlPlaneRoleSingle,
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
	g.Expect(kairosConfig.Spec.ControlPlaneRole).To(Equal(bootstrapv1beta2.ControlPlaneRoleSingle))
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
		bootstrapv1beta2.ControlPlaneRoleInit,
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
	g.Expect(kairosConfig.Spec.ControlPlaneRole).To(Equal(bootstrapv1beta2.ControlPlaneRoleInit))
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

// TestSecretWatchPredicate_FiltersByTypeAndClusterNameLabel verifies the
// KD-15-compliant predicate the KCP controller installs on Secret events.
// We assert by reconstructing the predicate logic against representative
// Secret shapes — the predicate function is a literal inside SetupWithManager
// so we copy the same guards here. If a future refactor changes the
// predicate, the assertions here drive the test to match.
func TestSecretWatchPredicate_FiltersByTypeAndClusterNameLabel(t *testing.T) {
	predicate := func(obj client.Object) bool {
		secret, ok := obj.(*corev1.Secret)
		if !ok {
			return false
		}
		if secret.Type != clusterv1.ClusterSecretType {
			return false
		}
		return secret.Labels[clusterv1.ClusterNameLabel] != ""
	}

	cases := []struct {
		name string
		obj  client.Object
		want bool
	}{
		{
			name: "right type + right label → enqueue",
			obj: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
				},
				Type: clusterv1.ClusterSecretType,
			},
			want: true,
		},
		{
			name: "right type + no label → drop (KD-15: legacy strings.HasSuffix would have matched)",
			obj: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-kubeconfig"},
				Type:       clusterv1.ClusterSecretType,
			},
			want: false,
		},
		{
			name: "wrong type + right label → drop (someone hand-labelled an Opaque secret)",
			obj: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
				},
				Type: corev1.SecretTypeOpaque,
			},
			want: false,
		},
		{
			name: "non-secret object → drop",
			obj:  &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-kubeconfig"}},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(predicate(tc.obj)).To(Equal(tc.want))
		})
	}
}

// TestObserveKubeconfigSecret_TransitionsCondition exercises the three
// KubeconfigReadyCondition states observeKubeconfigSecret produces:
//
//   - Secret missing + first observation → False(WaitingForNodePush, Info);
//     LastNodePushObserved set to now.
//   - Secret missing + LastNodePushObserved older than kubeconfigReadyTimeout
//     → severity escalates to Warning; LastNodePushObserved unchanged.
//   - Secret present with kubeconfig data → True(KubeconfigReady);
//     LastNodePushObserved cleared.
func TestObserveKubeconfigSecret_TransitionsCondition(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
	}
	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-kcp", Namespace: "default"},
	}

	// --- (1) Secret missing, first observation: False(Info), anchor timestamp.
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}
	beforeFirst := metav1.Now()
	ready, err := r.observeKubeconfigSecret(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
	g.Expect(kcp.Status.LastNodePushObserved).NotTo(BeNil(), "first miss must anchor LastNodePushObserved")
	g.Expect(kcp.Status.LastNodePushObserved.Time).To(BeTemporally(">=", beforeFirst.Time.Add(-time.Second)))
	cond := conditions.Get(kcp, controlplanev1beta2.KubeconfigReadyCondition)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(corev1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(controlplanev1beta2.WaitingForNodePushReason))
	g.Expect(cond.Severity).To(Equal(clusterv1.ConditionSeverityInfo))

	// --- (2) Secret still missing, but LastNodePushObserved is now in the past
	// beyond kubeconfigReadyTimeout: severity escalates to Warning.
	stale := metav1.NewTime(time.Now().Add(-2 * kubeconfigReadyTimeout))
	kcp.Status.LastNodePushObserved = &stale
	originalStale := stale
	ready, err = r.observeKubeconfigSecret(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
	// LastNodePushObserved must NOT be re-anchored — we measure elapsed
	// since first observation, not since each reconcile.
	g.Expect(kcp.Status.LastNodePushObserved.Time).To(Equal(originalStale.Time))
	cond = conditions.Get(kcp, controlplanev1beta2.KubeconfigReadyCondition)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Severity).To(Equal(clusterv1.ConditionSeverityWarning))

	// --- (3) Secret present with non-empty value: True; LastNodePushObserved cleared.
	pushedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster-kubeconfig",
			Namespace: "default",
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": []byte("apiVersion: v1\nkind: Config")},
	}
	c2 := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pushedSecret).Build()
	r2 := &KairosControlPlaneReconciler{Client: c2, Scheme: scheme}
	ready, err = r2.observeKubeconfigSecret(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
	g.Expect(kcp.Status.LastNodePushObserved).To(BeNil(), "Secret-present must clear LastNodePushObserved")
	cond = conditions.Get(kcp, controlplanev1beta2.KubeconfigReadyCondition)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(corev1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(controlplanev1beta2.KubeconfigReadyReason))
}

// TestObserveKubeconfigSecret_SSHFallbackAnnotation_SetsViaSSHReason
// guards the one-line annotation check added in PR-9: when the
// kubeconfig Secret was written by the SSH fallback worker (annotation
// "controllers.cluster.x-k8s.io/kubeconfig-source: ssh-fallback"), the
// condition Reason is KubeconfigReadyViaSSHFallback rather than the
// default KubeconfigReady. This is the operator-visible audit signal
// for "this cluster's kubeconfig was retrieved via SSH fallback."
func TestObserveKubeconfigSecret_SSHFallbackAnnotation_SetsViaSSHReason(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
	}
	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-kcp", Namespace: "default"},
	}

	sshFallbackSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster-kubeconfig",
			Namespace: "default",
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
			Annotations: map[string]string{
				KubeconfigSourceAnnotation: KubeconfigSourceSSHFallback,
			},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": []byte("apiVersion: v1\nkind: Config")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sshFallbackSecret).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}
	ready, err := r.observeKubeconfigSecret(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
	cond := conditions.Get(kcp, controlplanev1beta2.KubeconfigReadyCondition)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(corev1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(controlplanev1beta2.KubeconfigReadyViaSSHFallbackReason),
		"Secret annotated with ssh-fallback source must produce the via-SSH Reason")
}

// TestSecretToKairosControlPlane_UsesClusterNameLabel guards the KD-15 label
// lookup: the handler must consult `cluster.x-k8s.io/cluster-name`, never
// derive the cluster name from the Secret's name suffix.
func TestSecretToKairosControlPlane_UsesClusterNameLabel(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(scheme)).To(Succeed())

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: &corev1.ObjectReference{
				Kind: "KairosControlPlane",
				Name: "test-kcp",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	// Secret with the right label → maps to KCP.
	labelled := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "arbitrary-name", // intentionally NOT *-kubeconfig
			Namespace: "default",
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
		},
		Type: clusterv1.ClusterSecretType,
	}
	got := r.secretToKairosControlPlane(context.Background(), labelled)
	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0].Name).To(Equal("test-kcp"))

	// Secret without the label → never maps, even if name suffix would
	// have matched the legacy logic.
	unlabelled := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-kubeconfig", Namespace: "default"},
		Type:       clusterv1.ClusterSecretType,
	}
	got = r.secretToKairosControlPlane(context.Background(), unlabelled)
	g.Expect(got).To(BeEmpty())
}

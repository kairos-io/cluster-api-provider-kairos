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
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/kairos-capi/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/kairos-capi/api/controlplane/v1beta2"
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

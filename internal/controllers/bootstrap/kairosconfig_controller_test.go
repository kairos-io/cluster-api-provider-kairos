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
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

func TestGenerateK0sCloudConfig_ControlPlaneSingleNode(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	lbService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster-control-plane-lb",
			Namespace: "default",
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{
					{IP: "192.0.2.10"},
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lbService).Build()
	reconciler := &KairosConfigReconciler{
		Client: client,
		Scheme: scheme,
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
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

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}

	cloudConfig, err := reconciler.generateK0sCloudConfig(
		context.Background(),
		log.Log,
		kairosConfig,
		machine,
		cluster,
		"control-plane",
		"",
	)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cloudConfig).To(ContainSubstring("#cloud-config"))
	g.Expect(cloudConfig).To(ContainSubstring("k0s:"))
	g.Expect(cloudConfig).To(ContainSubstring("enabled: true"))
	g.Expect(cloudConfig).To(ContainSubstring("--single"))
	g.Expect(cloudConfig).NotTo(ContainSubstring("k0s-worker:"))
}

func TestGenerateK0sCloudConfig_ControlPlaneWithCIDRs(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	lbService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster-control-plane-lb",
			Namespace: "default",
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{
					{IP: "192.0.2.10"},
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lbService).Build()
	reconciler := &KairosConfigReconciler{
		Client: client,
		Scheme: scheme,
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:              "control-plane",
			Distribution:      "k0s",
			KubernetesVersion: "v1.30.0+k0s.0",
			SingleNode:        true,
			UserName:          "kairos",
			UserPassword:      "kairos",
			UserGroups:        []string{"admin"},
			PodCIDR:           "10.244.0.0/16",
			ServiceCIDR:       "10.96.0.0/12",
		},
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}

	cloudConfig, err := reconciler.generateK0sCloudConfig(
		context.Background(),
		log.Log,
		kairosConfig,
		machine,
		cluster,
		"control-plane",
		"",
	)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cloudConfig).To(ContainSubstring("--config /etc/k0s/k0s.yaml"))
	g.Expect(cloudConfig).To(ContainSubstring("podCIDR: 10.244.0.0/16"))
	g.Expect(cloudConfig).To(ContainSubstring("serviceCIDR: 10.96.0.0/12"))
}

func TestGenerateK0sCloudConfig_ControlPlaneKubeVirtBootstrapTrap(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	lbService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster-control-plane-lb",
			Namespace: "default",
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{
					{IP: "192.0.2.10"},
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lbService).Build()
	reconciler := &KairosConfigReconciler{
		Client: client,
		Scheme: scheme,
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
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

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
		},
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: corev1.ObjectReference{
				Kind:      "KubevirtMachine",
				Name:      "test-kubevirt-machine",
				Namespace: "default",
			},
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}

	cloudConfig, err := reconciler.generateK0sCloudConfig(
		context.Background(),
		log.Log,
		kairosConfig,
		machine,
		cluster,
		"control-plane",
		"",
	)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cloudConfig).To(ContainSubstring("CAPK: always mark bootstrap success on script exit"))
}

func TestGenerateK0sCloudConfig_ControlPlaneMultiNode(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &KairosConfigReconciler{
		Client: client,
		Scheme: scheme,
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:              "control-plane",
			Distribution:      "k0s",
			KubernetesVersion: "v1.30.0+k0s.0",
			SingleNode:        false,
			UserName:          "kairos",
			UserPassword:      "kairos",
			UserGroups:        []string{"admin"},
		},
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}

	cloudConfig, err := reconciler.generateK0sCloudConfig(
		context.Background(),
		log.Log,
		kairosConfig,
		machine,
		cluster,
		"control-plane",
		"",
	)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cloudConfig).To(ContainSubstring("#cloud-config"))
	g.Expect(cloudConfig).To(ContainSubstring("k0s:"))
	g.Expect(cloudConfig).To(ContainSubstring("enabled: true"))
	g.Expect(cloudConfig).NotTo(ContainSubstring("--single"))
	g.Expect(cloudConfig).NotTo(ContainSubstring("k0s-worker:"))
}

func TestGenerateK0sCloudConfig_WorkerWithToken(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &KairosConfigReconciler{
		Client: client,
		Scheme: scheme,
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:              "worker",
			Distribution:      "k0s",
			KubernetesVersion: "v1.30.0+k0s.0",
			WorkerToken:       "test-token-12345",
			UserName:          "kairos",
			UserPassword:      "kairos",
			UserGroups:        []string{"admin"},
		},
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}

	cloudConfig, err := reconciler.generateK0sCloudConfig(
		context.Background(),
		log.Log,
		kairosConfig,
		machine,
		cluster,
		"worker",
		"https://control-plane:6443",
	)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cloudConfig).To(ContainSubstring("#cloud-config"))
	g.Expect(cloudConfig).To(ContainSubstring("k0s-worker:"))
	g.Expect(cloudConfig).To(ContainSubstring("enabled: true"))
	g.Expect(cloudConfig).To(ContainSubstring("--token-file /etc/k0s/token"))
	g.Expect(cloudConfig).To(ContainSubstring("path: /etc/k0s/token"))
	g.Expect(cloudConfig).To(ContainSubstring("test-token-12345"))
	g.Expect(cloudConfig).NotTo(ContainSubstring("k0s:"))
}

func TestSanitizeCapkUserdata(t *testing.T) {
	g := NewWithT(t)

	input := `#cloud-config
users:
- name: kairos
  passwd: kairos
  groups:
    - admin
- name: capk
  gecos: CAPK User
  sudo: ALL=(ALL) NOPASSWD:ALL
  groups: users, admin
  ssh_authorized_keys:
    - ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7
k0s:
  enabled: true`

	output, changed := sanitizeCapkUserdata(input)

	g.Expect(changed).To(BeTrue())
	g.Expect(output).To(ContainSubstring("gecos: CAPK User"))
	g.Expect(output).To(ContainSubstring("groups: [users, admin]"))
	g.Expect(output).NotTo(ContainSubstring("sudo: ALL=(ALL) NOPASSWD:ALL"))
	g.Expect(output).To(ContainSubstring("ssh_authorized_keys:"))
	g.Expect(output).To(ContainSubstring("- \"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7\""))
}

func TestSanitizeCapkUserdata_NormalizesListGroups(t *testing.T) {
	g := NewWithT(t)

	input := `#cloud-config
users:
- name: capk
  groups:
    - users
    - admin
  ssh_authorized_keys:
    - ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7
`

	output, changed := sanitizeCapkUserdata(input)

	g.Expect(changed).To(BeTrue())
	g.Expect(output).To(ContainSubstring("groups: [users, admin]"))
	g.Expect(output).To(ContainSubstring("ssh_authorized_keys:"))
	g.Expect(output).To(ContainSubstring("- \"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7\""))
}

func TestSanitizeCapkUserdataSecret_UsesMachineSecretName(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	userdata := `#cloud-config
users:
- name: capk
  groups: users, admin
  ssh_authorized_keys:
    - ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7
`

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "machine-secret-userdata",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"userdata": []byte(userdata),
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	reconciler := &KairosConfigReconciler{
		Client: client,
		Scheme: scheme,
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Status: bootstrapv1beta2.KairosConfigStatus{
			DataSecretName: ptr.To("status-secret"),
		},
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
		},
		Spec: clusterv1.MachineSpec{
			Bootstrap: clusterv1.Bootstrap{
				DataSecretName: ptr.To("machine-secret"),
			},
		},
	}

	updated, found, err := reconciler.sanitizeCapkUserdataSecret(context.Background(), log.Log, kairosConfig, machine)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(updated).To(BeTrue())

	updatedSecret := &corev1.Secret{}
	g.Expect(client.Get(context.Background(), types.NamespacedName{Name: "machine-secret-userdata", Namespace: "default"}, updatedSecret)).To(Succeed())
	g.Expect(string(updatedSecret.Data["userdata"])).To(ContainSubstring("groups: [users, admin]"))
	g.Expect(string(updatedSecret.Data["userdata"])).To(ContainSubstring("ssh_authorized_keys:"))
	g.Expect(string(updatedSecret.Data["userdata"])).To(ContainSubstring("- \"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7\""))
}

func TestGenerateK0sCloudConfig_WorkerWithTokenSecretRef(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-token",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"token": []byte("secret-token-67890"),
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tokenSecret).Build()
	reconciler := &KairosConfigReconciler{
		Client: client,
		Scheme: scheme,
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:              "worker",
			Distribution:      "k0s",
			KubernetesVersion: "v1.30.0+k0s.0",
			WorkerTokenSecretRef: &bootstrapv1beta2.WorkerTokenSecretReference{
				Name: "worker-token",
				Key:  "token",
			},
			UserName:     "kairos",
			UserPassword: "kairos",
			UserGroups:   []string{"admin"},
		},
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}

	cloudConfig, err := reconciler.generateK0sCloudConfig(
		context.Background(),
		log.Log,
		kairosConfig,
		machine,
		cluster,
		"worker",
		"https://control-plane:6443",
	)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cloudConfig).To(ContainSubstring("k0s-worker:"))
	g.Expect(cloudConfig).To(ContainSubstring("secret-token-67890"))
}

func TestGenerateK0sCloudConfig_WorkerTokenPrecedence(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-token",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"token": []byte("secret-token-takes-precedence"),
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tokenSecret).Build()
	reconciler := &KairosConfigReconciler{
		Client: client,
		Scheme: scheme,
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:              "worker",
			Distribution:      "k0s",
			KubernetesVersion: "v1.30.0+k0s.0",
			WorkerToken:       "inline-token-should-be-ignored",
			WorkerTokenSecretRef: &bootstrapv1beta2.WorkerTokenSecretReference{
				Name: "worker-token",
				Key:  "token",
			},
			UserName:     "kairos",
			UserPassword: "kairos",
			UserGroups:   []string{"admin"},
		},
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}

	cloudConfig, err := reconciler.generateK0sCloudConfig(
		context.Background(),
		log.Log,
		kairosConfig,
		machine,
		cluster,
		"worker",
		"https://control-plane:6443",
	)

	g.Expect(err).NotTo(HaveOccurred())
	// WorkerTokenSecretRef should take precedence over WorkerToken
	g.Expect(cloudConfig).To(ContainSubstring("secret-token-takes-precedence"))
	g.Expect(cloudConfig).NotTo(ContainSubstring("inline-token-should-be-ignored"))
}

func TestGenerateK0sCloudConfig_WorkerMissingToken(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &KairosConfigReconciler{
		Client: client,
		Scheme: scheme,
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:              "worker",
			Distribution:      "k0s",
			KubernetesVersion: "v1.30.0+k0s.0",
			// No token provided
			UserName:     "kairos",
			UserPassword: "kairos",
			UserGroups:   []string{"admin"},
		},
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}

	_, err := reconciler.generateK0sCloudConfig(
		context.Background(),
		log.Log,
		kairosConfig,
		machine,
		cluster,
		"worker",
		"https://control-plane:6443",
	)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("worker token is required"))
}

func TestGenerateK0sCloudConfig_HostnameTemplating(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &KairosConfigReconciler{
		Client: client,
		Scheme: scheme,
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
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

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}

	cloudConfig, err := reconciler.generateK0sCloudConfig(
		context.Background(),
		log.Log,
		kairosConfig,
		machine,
		cluster,
		"control-plane",
		"",
	)

	g.Expect(err).NotTo(HaveOccurred())
	// Verify hostname defaults to Machine name when no explicit hostname is set
	g.Expect(cloudConfig).To(ContainSubstring("hostname: test-machine"))
	// Should NOT contain Go template syntax
	g.Expect(cloudConfig).NotTo(ContainSubstring("{{.MachineID}}"))
}

func TestGenerateK3sCloudConfig_WorkerTokenSecretRef(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "k3s-token-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"token": []byte("k3s-secret-token"),
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	reconciler := &KairosConfigReconciler{
		Client: client,
		Scheme: scheme,
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:              "worker",
			Distribution:      "k3s",
			KubernetesVersion: "v1.30.0+k3s.0",
			K3sTokenSecretRef: &bootstrapv1beta2.WorkerTokenSecretReference{
				Name: "k3s-token-secret",
				Key:  "token",
			},
			UserName:     "kairos",
			UserPassword: "kairos",
			UserGroups:   []string{"admin"},
		},
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}

	cloudConfig, err := reconciler.generateK3sCloudConfig(
		context.Background(),
		log.Log,
		kairosConfig,
		machine,
		cluster,
		"worker",
		"https://control-plane:6443",
	)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cloudConfig).To(ContainSubstring("k3s-agent:"))
	g.Expect(cloudConfig).To(ContainSubstring("k3s-secret-token"))
}

func TestGenerateK3sCloudConfig_WorkerTokenSecretMissing(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &KairosConfigReconciler{
		Client: client,
		Scheme: scheme,
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:              "worker",
			Distribution:      "k3s",
			KubernetesVersion: "v1.30.0+k3s.0",
			K3sTokenSecretRef: &bootstrapv1beta2.WorkerTokenSecretReference{
				Name: "k3s-token-secret",
				Key:  "token",
			},
			UserName:     "kairos",
			UserPassword: "kairos",
			UserGroups:   []string{"admin"},
		},
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}

	_, err := reconciler.generateK3sCloudConfig(
		context.Background(),
		log.Log,
		kairosConfig,
		machine,
		cluster,
		"worker",
		"https://control-plane:6443",
	)

	g.Expect(err).To(Equal(errK3sTokenNotReady))
}

func TestGenerateK3sCloudConfig_ControlPlaneKubeVirtCapk(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	lbService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster-control-plane-lb",
			Namespace: "default",
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{
					{IP: "192.0.2.10"},
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lbService).Build()
	reconciler := &KairosConfigReconciler{
		Client: client,
		Scheme: scheme,
		// MgmtEndpointResolver nil: the CAPK gate in generate*CloudConfig
		// short-circuits to "no push block" but the LB endpoint + CAPK
		// template selection still apply.
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:              "control-plane",
			Distribution:      "k3s",
			KubernetesVersion: "v1.30.0+k3s.0",
			SingleNode:        true,
			UserName:          "kairos",
			UserPassword:      "kairos",
			UserGroups:        []string{"admin"},
		},
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
		},
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: corev1.ObjectReference{
				Kind:      "KubevirtMachine",
				Name:      "test-kubevirt-machine",
				Namespace: "default",
			},
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}

	cloudConfig, err := reconciler.generateK3sCloudConfig(
		context.Background(),
		log.Log,
		kairosConfig,
		machine,
		cluster,
		"control-plane",
		"",
	)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cloudConfig).To(ContainSubstring("CAPK: always mark bootstrap success on script exit"))
	g.Expect(cloudConfig).To(ContainSubstring("--tls-san=192.0.2.10"))
	g.Expect(cloudConfig).To(ContainSubstring("k3s:"))
}

// newBootstrapTestScheme registers the schemes that the bootstrap Reconcile
// flow depends on for these unit tests.
func newBootstrapTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	return scheme
}

// TestReconcile_PausedPatchesObservedGeneration verifies KD-14: even when the
// KairosConfig is paused, Reconcile must flush observedGeneration via the
// deferred patch helper. Prior to the fix the patch helper was created after
// the paused early-return and observedGeneration drifted permanently.
func TestReconcile_PausedPatchesObservedGeneration(t *testing.T) {
	g := NewWithT(t)
	scheme := newBootstrapTestScheme(t)

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "paused-config",
			Namespace:  "default",
			Generation: 7,
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:         "control-plane",
			Distribution: "k0s",
			Pause:        true,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(kairosConfig).
		WithStatusSubresource(&bootstrapv1beta2.KairosConfig{}).
		Build()
	r := &KairosConfigReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "paused-config", Namespace: "default"}})
	g.Expect(err).NotTo(HaveOccurred())

	got := &bootstrapv1beta2.KairosConfig{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "paused-config", Namespace: "default"}, got)).To(Succeed())
	g.Expect(got.Status.ObservedGeneration).To(Equal(int64(7)))
}

// TestReconcile_NoOwnerMachinePatchesObservedGeneration verifies KD-14 for the
// "Machine controller has not yet set OwnerRef" early return. observedGeneration
// must still be flushed.
func TestReconcile_NoOwnerMachinePatchesObservedGeneration(t *testing.T) {
	g := NewWithT(t)
	scheme := newBootstrapTestScheme(t)

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "orphan-config",
			Namespace:  "default",
			Generation: 3,
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:         "control-plane",
			Distribution: "k0s",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(kairosConfig).
		WithStatusSubresource(&bootstrapv1beta2.KairosConfig{}).
		Build()
	r := &KairosConfigReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "orphan-config", Namespace: "default"}})
	g.Expect(err).NotTo(HaveOccurred())

	got := &bootstrapv1beta2.KairosConfig{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "orphan-config", Namespace: "default"}, got)).To(Succeed())
	g.Expect(got.Status.ObservedGeneration).To(Equal(int64(3)))
	// Finalizer should be added even though we early-return on missing owner Machine.
	g.Expect(got.Finalizers).To(ContainElement(bootstrapv1beta2.KairosConfigFinalizer))
}

// TestReconcile_PausedDoesNotClearFailureFields verifies the maintainer-confirmed
// decision #2: latched FailureReason/FailureMessage must NOT be cleared on the
// paused path. Clearing happens naturally on the first successful post-resume
// Reconcile.
func TestReconcile_PausedDoesNotClearFailureFields(t *testing.T) {
	g := NewWithT(t)
	scheme := newBootstrapTestScheme(t)

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "paused-failed-config",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:         "control-plane",
			Distribution: "k0s",
			Pause:        true,
		},
		Status: bootstrapv1beta2.KairosConfigStatus{
			FailureReason:  "SomePriorFailure",
			FailureMessage: "rendered userdata before user paused us",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(kairosConfig).
		WithStatusSubresource(&bootstrapv1beta2.KairosConfig{}).
		Build()
	r := &KairosConfigReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "paused-failed-config", Namespace: "default"}})
	g.Expect(err).NotTo(HaveOccurred())

	got := &bootstrapv1beta2.KairosConfig{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "paused-failed-config", Namespace: "default"}, got)).To(Succeed())
	g.Expect(got.Status.FailureReason).To(Equal("SomePriorFailure"))
	g.Expect(got.Status.FailureMessage).To(Equal("rendered userdata before user paused us"))
}

// TestReconcile_SuccessClearsFailureFields verifies KD-14: when a previous
// Reconcile latched FailureReason/FailureMessage (e.g. transient missing
// userPasswordSecretRef target) and the next Reconcile succeeds, the failure
// fields are cleared. Prior to the fix they latched forever and CAPI Machine
// controller refused to clone the infra Machine.
func TestReconcile_SuccessClearsFailureFields(t *testing.T) {
	g := NewWithT(t)
	scheme := newBootstrapTestScheme(t)

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
	}
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: "test-cluster",
		},
	}
	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "recovered-config",
			Namespace:  "default",
			Generation: 5,
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
		Status: bootstrapv1beta2.KairosConfigStatus{
			FailureReason:  bootstrapv1beta2.BootstrapDataSecretGenerationFailedReason,
			FailureMessage: "user password secret default/missing not found",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster, machine, kairosConfig).
		WithStatusSubresource(&bootstrapv1beta2.KairosConfig{}).
		Build()
	r := &KairosConfigReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "recovered-config", Namespace: "default"}})
	g.Expect(err).NotTo(HaveOccurred())

	got := &bootstrapv1beta2.KairosConfig{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "recovered-config", Namespace: "default"}, got)).To(Succeed())
	g.Expect(got.Status.FailureReason).To(BeEmpty(), "FailureReason should be cleared on success (KD-14)")
	g.Expect(got.Status.FailureMessage).To(BeEmpty(), "FailureMessage should be cleared on success (KD-14)")
	g.Expect(got.Status.ObservedGeneration).To(Equal(int64(5)))
}

// TestSupportsManagementEndpoint exhaustively pins the truth table for the
// gate that decides whether a control-plane Machine's render gets a
// kubeconfig-push block. The matrix is small and explicit on purpose: a new
// infrastructure kind needs an entry here as well as in the templates, and
// the test surfaces the omission.
func TestSupportsManagementEndpoint(t *testing.T) {
	mkMachine := func(kind string) *clusterv1.Machine {
		return &clusterv1.Machine{
			Spec: clusterv1.MachineSpec{
				InfrastructureRef: corev1.ObjectReference{Kind: kind},
			},
		}
	}
	cases := []struct {
		name    string
		machine *clusterv1.Machine
		want    bool
	}{
		{name: "nil machine", machine: nil, want: false},
		{name: "missing kind", machine: mkMachine(""), want: false},
		{name: "KubevirtMachine (CAPK lowercase v)", machine: mkMachine("KubevirtMachine"), want: true},
		{name: "KubeVirtMachine (CAPK uppercase V)", machine: mkMachine("KubeVirtMachine"), want: true},
		{name: "VSphereMachine (CAPV)", machine: mkMachine("VSphereMachine"), want: true},
		{name: "DockerMachine (unsupported today)", machine: mkMachine("DockerMachine"), want: false},
		{name: "AWSMachine (unsupported today)", machine: mkMachine("AWSMachine"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(supportsManagementEndpoint(tc.machine)).To(Equal(tc.want))
		})
	}
}

// stubResolver is a tiny test double for ManagementEndpointResolver that
// returns a canned endpoint (or nil) without touching the API server. The
// reconciler tests for the KD-3b gate use this to assert routing decisions
// without spinning up a full kubeVirtTokenResolver.
type stubResolver struct {
	endpoint *ManagementEndpoint
	err      error
	calls    int
}

func (s *stubResolver) Resolve(_ context.Context, _ *bootstrapv1beta2.KairosConfig, _ *clusterv1.Cluster) (*ManagementEndpoint, error) {
	s.calls++
	return s.endpoint, s.err
}

// TestGenerateK0sCloudConfig_CapvControlPlane_RendersPushBlock asserts the
// KD-3b gate extension: a VSphereMachine control plane Machine now triggers
// the resolver path and renders the push_kubeconfig block. Pre-KD-3b the
// gate was CAPK-only.
func TestGenerateK0sCloudConfig_CapvControlPlane_RendersPushBlock(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	resolver := &stubResolver{endpoint: &ManagementEndpoint{
		APIServer:                 "https://mgmt.example.com:6443",
		Token:                     "test-token",
		KubeconfigSecretName:      "test-cluster-kubeconfig",
		KubeconfigSecretNamespace: "default",
	}}
	reconciler := &KairosConfigReconciler{
		Client:               client,
		Scheme:               scheme,
		MgmtEndpointResolver: resolver,
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-config", Namespace: "default"},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:         "control-plane",
			Distribution: "k0s",
			SingleNode:   true,
			UserName:     "kairos",
			UserPassword: "kairos",
			UserGroups:   []string{"admin"},
		},
	}
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Namespace: "default"},
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: corev1.ObjectReference{Kind: "VSphereMachine"},
		},
	}
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneEndpoint: clusterv1.APIEndpoint{Host: "10.0.0.42", Port: 6443},
		},
	}

	out, err := reconciler.generateK0sCloudConfig(context.Background(), log.Log, kairosConfig, machine, cluster, "control-plane", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resolver.calls).To(Equal(1), "resolver must be invoked for CAPV control-plane")
	g.Expect(out).To(ContainSubstring("push_kubeconfig()"), "CAPV control-plane render must include push block")
	// Cluster-name and ControlPlaneEndpointHost must be stamped from the
	// Cluster (not the resolver output), per the call-site convention.
	g.Expect(out).To(ContainSubstring("local cluster_name='test-cluster'"))
	g.Expect(out).To(ContainSubstring("local cp_endpoint_host='10.0.0.42'"))
}

// TestGenerateK0sCloudConfig_CapkWorker_NoPushBlock guards the role gate: a
// CAPK worker (not a control-plane) must NOT trigger the resolver, and the
// rendered output must not include a push block.
func TestGenerateK0sCloudConfig_CapkWorker_NoPushBlock(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	resolver := &stubResolver{endpoint: &ManagementEndpoint{
		APIServer: "https://mgmt.example.com:6443",
		Token:     "test-token",
	}}
	reconciler := &KairosConfigReconciler{
		Client:               client,
		Scheme:               scheme,
		MgmtEndpointResolver: resolver,
	}

	kairosConfig := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-config", Namespace: "default"},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:         "worker",
			Distribution: "k0s",
			UserName:     "kairos",
			UserPassword: "kairos",
			UserGroups:   []string{"admin"},
			WorkerToken:  "tok",
		},
	}
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Namespace: "default"},
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: corev1.ObjectReference{Kind: "KubevirtMachine"},
		},
	}
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
	}

	out, err := reconciler.generateK0sCloudConfig(context.Background(), log.Log, kairosConfig, machine, cluster, "worker", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resolver.calls).To(Equal(0), "resolver must NOT be invoked for worker role")
	g.Expect(out).NotTo(ContainSubstring("push_kubeconfig"), "worker render must not include push block")
}

// TestBootstrapSecretBelongsTo guards the KD-48b ownership check that gates the
// in-place overwrite of the deterministically-named bootstrap Secret. A Secret
// is "ours" iff it carries an owner reference with the KairosConfig's UID, or
// the cluster-name label this controller always stamps; anything else is foreign
// and must NOT be adopted.
func TestBootstrapSecretBelongsTo(t *testing.T) {
	g := NewWithT(t)

	kc := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-0", Namespace: "ns", UID: "uid-1234"},
	}
	const clusterName = "demo"

	secretWith := func(ownerUID string, label string) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cp-0", Namespace: "ns"}}
		if ownerUID != "" {
			s.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: bootstrapv1beta2.GroupVersion.String(),
				Kind:       "KairosConfig",
				Name:       "cp-0",
				UID:        types.UID(ownerUID),
				Controller: ptr.To(true),
			}}
		}
		if label != "" {
			s.Labels = map[string]string{clusterv1.ClusterNameLabel: label}
		}
		return s
	}

	cases := []struct {
		name    string
		secret  *corev1.Secret
		cluster string
		want    bool
	}{
		{"owned by UID (well-formed ref)", secretWith("uid-1234", ""), clusterName, true},
		{"owned by UID even with empty Kind (pre-KD-48a ref)", func() *corev1.Secret {
			s := secretWith("uid-1234", "")
			s.OwnerReferences[0].APIVersion = ""
			s.OwnerReferences[0].Kind = ""
			return s
		}(), clusterName, true},
		{"cluster-name label fallback (no owner ref)", secretWith("", clusterName), clusterName, true},
		{"foreign: no owner ref, no label", secretWith("", ""), clusterName, false},
		{"foreign: different owner UID, different label", secretWith("uid-evil", "other"), clusterName, false},
		{"foreign: matching label but wrong cluster name arg", secretWith("", clusterName), "different", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g.Expect(bootstrapSecretBelongsTo(tc.secret, kc, tc.cluster)).To(Equal(tc.want))
		})
	}

	// A KairosConfig with an empty UID must not match a Secret by UID (avoids a
	// "" == "" false-positive); only the label path can prove ownership then.
	kcNoUID := &bootstrapv1beta2.KairosConfig{ObjectMeta: metav1.ObjectMeta{Name: "cp-0", Namespace: "ns"}}
	g.Expect(bootstrapSecretBelongsTo(secretWith("", ""), kcNoUID, clusterName)).To(BeFalse())
	g.Expect(bootstrapSecretBelongsTo(secretWith("", clusterName), kcNoUID, clusterName)).To(BeTrue())
}

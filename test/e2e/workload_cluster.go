/*
Copyright 2026 The Kairos CAPI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and limitations
under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/kairos-io/cluster-api-provider-kairos/internal/kubevirtenv"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var (
	clusterGVR = schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "clusters"}
	kcpGVR     = schema.GroupVersionResource{Group: "controlplane.cluster.x-k8s.io", Version: "v1beta2", Resource: "kairoscontrolplanes"}
	machineGVR = schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "machines"}
)

// applyWorkloadClusterManifests creates a single-node k3s CAPI cluster backed by KubeVirt.
// The VM boots directly from the Kairos cloud image (kairos-kubevirt DataVolume) — no ISO
// install step is needed since the cloud image is a pre-built bootable disk.
func applyWorkloadClusterManifests(env *kubevirtenv.Environment, dc dynamic.Interface, cfg *rest.Config, clusterName, namespace string) {
	yaml := fmt.Sprintf(`apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: KubevirtCluster
    name: %[1]s
  controlPlaneRef:
    apiGroup: controlplane.cluster.x-k8s.io
    kind: KairosControlPlane
    name: %[1]s-cp
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: KubevirtCluster
metadata:
  name: %[1]s
  namespace: %[2]s
spec: {}
---
apiVersion: controlplane.cluster.x-k8s.io/v1beta2
kind: KairosControlPlane
metadata:
  name: %[1]s-cp
  namespace: %[2]s
spec:
  replicas: 1
  version: "v1.30.0+k3s.0"
  distribution: k3s
  machineTemplate:
    infrastructureRef:
      apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
      kind: KubevirtMachineTemplate
      name: %[1]s-mt
      namespace: %[2]s
  kairosConfigTemplate:
    name: %[1]s-config
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: KubevirtMachineTemplate
metadata:
  name: %[1]s-mt
  namespace: %[2]s
spec:
  template:
    spec:
      virtualMachineBootstrapCheck:
        checkStrategy: none
      virtualMachineTemplate:
        spec:
          runStrategy: Always
          template:
            metadata:
              labels:
                kubevirt.io/domain: %[1]s
            spec:
              domain:
                cpu:
                  cores: 2
                resources:
                  requests:
                    memory: 4Gi
                devices:
                  disks:
                  - name: rootdisk
                    disk:
                      bus: virtio
                    bootOrder: 1
                  interfaces:
                  - name: default
                    masquerade: {}
                features:
                  smm:
                    enabled: true
                  acpi:
                    enabled: true
                firmware:
                  bootloader:
                    efi:
                      secureBoot: false
              networks:
              - name: default
                pod: {}
              volumes:
              - name: rootdisk
                dataVolume:
                  name: kairos-kubevirt
---
apiVersion: bootstrap.cluster.x-k8s.io/v1beta2
kind: KairosConfigTemplate
metadata:
  name: %[1]s-config
  namespace: %[2]s
spec:
  template:
    spec:
      role: control-plane
      distribution: k3s
      kubernetesVersion: "v1.30.0+k3s.0"
      dnsServers:
        - "8.8.8.8"
      userName: kairos
      userPassword: kairos
      userGroups:
        - admin
`, clusterName, namespace)

	Expect(env.ApplyManifestContent(dc, cfg, []byte(yaml))).To(Succeed())
}

// waitForClusterProvisioned polls until the CAPI Cluster reaches the "Provisioned" phase.
func waitForClusterProvisioned(ctx context.Context, dc dynamic.Interface, namespace, name string, timeout time.Duration) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := wait.PollUntilContextCancel(waitCtx, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		obj, getErr := dc.Resource(clusterGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return false, nil
		}
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		_, _ = fmt.Fprintf(GinkgoWriter, "  Cluster phase: %s\n", phase)
		return phase == "Provisioned", nil
	})
	Expect(err).NotTo(HaveOccurred(), "Cluster %s/%s did not reach Provisioned phase within %s", namespace, name, timeout)
}

// waitForControlPlaneReady polls until the KairosControlPlane is initialized with at least one ready replica.
func waitForControlPlaneReady(ctx context.Context, dc dynamic.Interface, namespace, name string, timeout time.Duration) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := wait.PollUntilContextCancel(waitCtx, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		obj, getErr := dc.Resource(kcpGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return false, nil
		}
		initialized, _, _ := unstructured.NestedBool(obj.Object, "status", "initialized")
		readyReplicas, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
		_, _ = fmt.Fprintf(GinkgoWriter, "  KairosControlPlane initialized=%v readyReplicas=%d\n", initialized, readyReplicas)
		return initialized && readyReplicas >= 1, nil
	})
	Expect(err).NotTo(HaveOccurred(), "KairosControlPlane %s/%s did not become ready within %s", namespace, name, timeout)
}

// expectMachinesRunning asserts that all CAPI Machines for the given cluster are in the "Running" phase.
func expectMachinesRunning(ctx context.Context, dc dynamic.Interface, namespace, clusterName string) {
	machineList, err := dc.Resource(machineGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("cluster.x-k8s.io/cluster-name=%s", clusterName),
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(machineList.Items).NotTo(BeEmpty(), "expected at least one Machine for cluster %s", clusterName)

	for _, machine := range machineList.Items {
		phase, _, _ := unstructured.NestedString(machine.Object, "status", "phase")
		_, _ = fmt.Fprintf(GinkgoWriter, "  Machine %s phase: %s\n", machine.GetName(), phase)
		Expect(phase).To(Equal("Running"), "Machine %s is not Running", machine.GetName())
	}
}

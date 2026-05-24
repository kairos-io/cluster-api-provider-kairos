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
	"os/exec"
	"strings"
	"time"

	"github.com/kairos-io/cluster-api-provider-kairos/internal/kubevirtenv"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	clusterGVR              = schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "clusters"}
	kcpGVR                  = schema.GroupVersionResource{Group: "controlplane.cluster.x-k8s.io", Version: "v1beta2", Resource: "kairoscontrolplanes"}
	machineGVR              = schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "machines"}
	kvMachineGVR            = schema.GroupVersionResource{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Resource: "kubevirtmachines"}
	kvMachineTemplateGVR    = schema.GroupVersionResource{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Resource: "kubevirtmachinetemplates"}
	kubevirtClusterGVR      = schema.GroupVersionResource{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Resource: "kubevirtclusters"}
	kairosConfigTemplateGVR = schema.GroupVersionResource{Group: "bootstrap.cluster.x-k8s.io", Version: "v1beta2", Resource: "kairosconfigtemplates"}
	vmiGVR                  = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
)

// applyWorkloadClusterManifests creates a single-node k3s CAPI cluster backed by KubeVirt.
// The VM boots directly from the Kairos cloud image (kairos-kubevirt DataVolume) — no ISO
// install step is needed since the cloud image is a pre-built bootable disk.
func applyWorkloadClusterManifests(env *kubevirtenv.Environment, dc dynamic.Interface, cfg *rest.Config, clusterName, namespace string) {
	yaml := fmt.Sprintf(`apiVersion: cluster.x-k8s.io/v1beta1
kind: Cluster
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  infrastructureRef:
    apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
    kind: KubevirtCluster
    name: %[1]s
  controlPlaneRef:
    apiVersion: controlplane.cluster.x-k8s.io/v1beta2
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
  version: "v1.35.4+k3s1"
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
      kubernetesVersion: "v1.35.4+k3s1"
      dnsServers:
        - "8.8.8.8"
      userName: kairos
      userPassword: kairos
      userGroups:
        - admin
`, clusterName, namespace)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	Expect(env.ApplyManifestContent(ctx, dc, cfg, []byte(yaml))).To(Succeed())

	// Apply is silent on unknown GVKs / partial failures — verify each top-level object actually exists.
	checks := []struct {
		gvr  schema.GroupVersionResource
		name string
	}{
		{clusterGVR, clusterName},
		{kcpGVR, clusterName + "-cp"},
		{kvMachineTemplateGVR, clusterName + "-mt"},
		{kubevirtClusterGVR, clusterName},
		{kairosConfigTemplateGVR, clusterName + "-config"},
	}
	for _, c := range checks {
		_, err := dc.Resource(c.gvr).Namespace(namespace).Get(ctx, c.name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "expected %s %s/%s to exist after apply", c.gvr.Resource, namespace, c.name)
	}
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
// On failure (timeout, fatal pod state, or context cancel) it dumps diagnostics from the workload namespace
// (KCP, KubevirtMachine, VMI, virt-launcher pod logs) before failing the spec.
func waitForControlPlaneReady(ctx context.Context, env *kubevirtenv.Environment, dc dynamic.Interface, namespace, clusterName, name string, timeout time.Duration) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var fatalReason string
	err := wait.PollUntilContextCancel(waitCtx, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		obj, getErr := dc.Resource(kcpGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr == nil {
			initialized, _, _ := unstructured.NestedBool(obj.Object, "status", "initialized")
			readyReplicas, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
			_, _ = fmt.Fprintf(GinkgoWriter, "  KairosControlPlane initialized=%v readyReplicas=%d\n", initialized, readyReplicas)
			if initialized && readyReplicas >= 1 {
				return true, nil
			}
		}
		if reason := detectFatalVirtLauncherState(ctx, env, namespace); reason != "" {
			fatalReason = reason
			return false, fmt.Errorf("fatal virt-launcher state: %s", reason)
		}
		return false, nil
	})
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "\n=== KairosControlPlane did not become ready (%v) ===\n", err)
		if fatalReason != "" {
			_, _ = fmt.Fprintf(GinkgoWriter, "Fatal: %s\n", fatalReason)
		}
		dumpWorkloadDiagnostics(env, dc, namespace, clusterName, name)
		Fail(fmt.Sprintf("KairosControlPlane %s/%s did not become ready within %s: %v", namespace, name, timeout, err))
	}
}

// detectFatalVirtLauncherState returns a non-empty reason if any virt-launcher pod in the namespace is
// in a clearly terminal/broken state (Failed phase, CrashLoopBackOff, or container restartCount >= 3).
func detectFatalVirtLauncherState(ctx context.Context, env *kubevirtenv.Environment, namespace string) string {
	cs, err := env.Clientset()
	if err != nil {
		return ""
	}
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "kubevirt.io=virt-launcher"})
	if err != nil {
		return ""
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodFailed {
			return fmt.Sprintf("pod %s phase=Failed", p.Name)
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.RestartCount >= 3 {
				return fmt.Sprintf("pod %s container %s restartCount=%d", p.Name, cs.Name, cs.RestartCount)
			}
			if w := cs.State.Waiting; w != nil && (w.Reason == "CrashLoopBackOff" || w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull") {
				return fmt.Sprintf("pod %s container %s waiting: %s", p.Name, cs.Name, w.Reason)
			}
		}
	}
	return ""
}

// dumpWorkloadDiagnostics prints status of KCP, KubevirtMachines, VMIs and tail of virt-launcher container logs.
func dumpWorkloadDiagnostics(env *kubevirtenv.Environment, dc dynamic.Interface, namespace, clusterName, kcpName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	w := GinkgoWriter

	_, _ = fmt.Fprintf(w, "\n--- KairosControlPlane %s/%s status ---\n", namespace, kcpName)
	if obj, err := dc.Resource(kcpGVR).Namespace(namespace).Get(ctx, kcpName, metav1.GetOptions{}); err == nil {
		if status, found, _ := unstructured.NestedMap(obj.Object, "status"); found {
			_, _ = fmt.Fprintf(w, "%v\n", status)
		}
	} else {
		_, _ = fmt.Fprintf(w, "get error: %v\n", err)
	}

	_, _ = fmt.Fprintf(w, "\n--- KubevirtMachines in %s ---\n", namespace)
	if list, err := dc.Resource(kvMachineGVR).Namespace(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, m := range list.Items {
			ready, _, _ := unstructured.NestedBool(m.Object, "status", "ready")
			conds, _, _ := unstructured.NestedSlice(m.Object, "status", "conditions")
			_, _ = fmt.Fprintf(w, "  %s ready=%v conditions=%v\n", m.GetName(), ready, conds)
		}
	} else {
		_, _ = fmt.Fprintf(w, "list error: %v\n", err)
	}

	_, _ = fmt.Fprintf(w, "\n--- VirtualMachineInstances in %s ---\n", namespace)
	if list, err := dc.Resource(vmiGVR).Namespace(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, vmi := range list.Items {
			phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase")
			conds, _, _ := unstructured.NestedSlice(vmi.Object, "status", "conditions")
			_, _ = fmt.Fprintf(w, "  %s phase=%s conditions=%v\n", vmi.GetName(), phase, conds)
		}
	} else {
		_, _ = fmt.Fprintf(w, "list error: %v\n", err)
	}

	_, _ = fmt.Fprintf(w, "\n--- virt-launcher pods in %s ---\n", namespace)
	cs, err := env.Clientset()
	if err != nil {
		_, _ = fmt.Fprintf(w, "clientset error: %v\n", err)
		return
	}
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "kubevirt.io=virt-launcher"})
	if err != nil {
		_, _ = fmt.Fprintf(w, "list error: %v\n", err)
		return
	}
	for _, p := range pods.Items {
		_, _ = fmt.Fprintf(w, "\nPod %s phase=%s\n", p.Name, p.Status.Phase)
		for _, cstat := range p.Status.ContainerStatuses {
			_, _ = fmt.Fprintf(w, "  container %s ready=%v restarts=%d state=%+v\n", cstat.Name, cstat.Ready, cstat.RestartCount, cstat.State)
		}
		for _, c := range p.Spec.Containers {
			tail := int64(120)
			req := cs.CoreV1().Pods(namespace).GetLogs(p.Name, &corev1.PodLogOptions{Container: c.Name, TailLines: &tail})
			rc, err := req.Stream(ctx)
			if err != nil {
				_, _ = fmt.Fprintf(w, "  logs(%s) error: %v\n", c.Name, err)
				continue
			}
			_, _ = fmt.Fprintf(w, "  --- logs %s (tail %d) ---\n", c.Name, tail)
			buf := make([]byte, 8192)
			for {
				n, rerr := rc.Read(buf)
				if n > 0 {
					_, _ = w.Write(buf[:n])
				}
				if rerr != nil {
					break
				}
			}
			_ = rc.Close()
			_, _ = fmt.Fprintln(w)
		}
	}

	// Bootstrap-data Secret structural check (KD-3b lab finding: when push doesn't fire,
	// confirm the template actually rendered the unit + wait-loop into the Secret, instead
	// of having to guess. We deliberately do NOT dump the Secret payload — it contains the
	// bearer token and user password. We only assert markers are present.)
	_, _ = fmt.Fprintf(w, "\n--- Bootstrap data Secret structural check ---\n")
	if list, err := cs.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "cluster.x-k8s.io/cluster-name=" + clusterName,
	}); err == nil {
		for _, s := range list.Items {
			if t := s.Type; t != "cluster.x-k8s.io/secret" && string(t) != "Opaque" {
				continue
			}
			payload, ok := s.Data["value"]
			if !ok || len(payload) == 0 {
				continue
			}
			body := string(payload)
			markers := []string{
				"kairos-k0s-post-bootstrap.service",
				"kairos-k3s-post-bootstrap.service",
				"push_kubeconfig()",
				"_wait_budget=300",
				"ConditionKernelCommandLine=!cdroot",
				"systemctl enable --now",
				"bootstrap-success.complete",
			}
			_, _ = fmt.Fprintf(w, "  Secret %s (type=%s, %d bytes):\n", s.Name, s.Type, len(payload))
			for _, m := range markers {
				_, _ = fmt.Fprintf(w, "    %-44s present=%v\n", m, strings.Contains(body, m))
			}
		}
	} else {
		_, _ = fmt.Fprintf(w, "list error: %v\n", err)
	}

	// Guest serial-log tail — KubeVirt writes virtio-serial output to
	// /var/run/kubevirt-private/<vmi-uid>/virt-serial0-log inside the virt-launcher
	// compute container. This is the only way to see what cloud-init / systemd /
	// the kairos-*-post-bootstrap unit actually did inside the guest after CI fails.
	_, _ = fmt.Fprintf(w, "\n--- VMI guest serial log (tail) ---\n")
	if list, err := dc.Resource(vmiGVR).Namespace(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, vmi := range list.Items {
			uid := string(vmi.GetUID())
			vmiName := vmi.GetName()
			podName := ""
			if pods, perr := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: "kubevirt.io=virt-launcher,vm.kubevirt.io/name=" + vmiName,
			}); perr == nil && len(pods.Items) > 0 {
				podName = pods.Items[0].Name
			}
			if podName == "" {
				_, _ = fmt.Fprintf(w, "  vmi=%s no virt-launcher pod found\n", vmiName)
				continue
			}
			serialPath := fmt.Sprintf("/var/run/kubevirt-private/%s/virt-serial0-log", uid)
			// Capture file size first so we know whether we're hitting tail-truncation
			// or whether the VM actually stopped writing serial output.
			sizeCmd := exec.CommandContext(ctx, "kubectl",
				"--kubeconfig", env.KubeconfigPath(), "--context", env.KubectlContext(),
				"-n", namespace, "exec", podName, "-c", "compute", "--",
				"sh", "-c", fmt.Sprintf("wc -c %s 2>&1 || true", serialPath),
			)
			sizeOut, _ := sizeCmd.CombinedOutput()
			_, _ = fmt.Fprintf(w, "  vmi=%s pod=%s path=%s\n", vmiName, podName, serialPath)
			_, _ = fmt.Fprintf(w, "  size: %s", string(sizeOut))
			// Dump up to ~2 MiB tail. For a hung early-boot VM this is the whole file;
			// for a long-running VM it's enough to capture the last few minutes of
			// cloud-init / systemd / k0s|k3s / kairos-*-post-bootstrap output.
			cmd := exec.CommandContext(ctx, "kubectl",
				"--kubeconfig", env.KubeconfigPath(), "--context", env.KubectlContext(),
				"-n", namespace, "exec", podName, "-c", "compute", "--",
				"tail", "-c", "2097152", serialPath,
			)
			out, runErr := cmd.CombinedOutput()
			if runErr != nil {
				_, _ = fmt.Fprintf(w, "  exec error: %v\n  output:\n%s\n", runErr, string(out))
			} else {
				_, _ = w.Write(out)
				_, _ = fmt.Fprintln(w)
			}
		}
	} else {
		_, _ = fmt.Fprintf(w, "list error: %v\n", err)
	}
}

// waitForWorkloadNodeReady fetches the workload cluster kubeconfig from the CAPI-managed
// "<clusterName>-kubeconfig" Secret on the management cluster, then polls the workload API
// until at least one Node reports Ready=True.
func waitForWorkloadNodeReady(ctx context.Context, env *kubevirtenv.Environment, namespace, clusterName string, timeout time.Duration) {
	mgmtCS, err := env.Clientset()
	Expect(err).NotTo(HaveOccurred())

	secretName := clusterName + "-kubeconfig"
	var kubeconfigBytes []byte
	{
		fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		err := wait.PollUntilContextCancel(fetchCtx, 5*time.Second, true, func(c context.Context) (bool, error) {
			s, gerr := mgmtCS.CoreV1().Secrets(namespace).Get(c, secretName, metav1.GetOptions{})
			if gerr != nil {
				return false, nil
			}
			kubeconfigBytes = s.Data["value"]
			return len(kubeconfigBytes) > 0, nil
		})
		Expect(err).NotTo(HaveOccurred(), "kubeconfig secret %s/%s not available", namespace, secretName)
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	Expect(err).NotTo(HaveOccurred())
	wlCS, err := kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err = wait.PollUntilContextCancel(waitCtx, 10*time.Second, true, func(c context.Context) (bool, error) {
		nodes, lerr := wlCS.CoreV1().Nodes().List(c, metav1.ListOptions{})
		if lerr != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "  workload nodes: list error: %v\n", lerr)
			return false, nil
		}
		if len(nodes.Items) == 0 {
			_, _ = fmt.Fprintf(GinkgoWriter, "  workload nodes: none yet\n")
			return false, nil
		}
		for _, n := range nodes.Items {
			ready := corev1.ConditionUnknown
			for _, c := range n.Status.Conditions {
				if c.Type == corev1.NodeReady {
					ready = c.Status
					break
				}
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "  workload node %s Ready=%s\n", n.Name, ready)
			if ready == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
	Expect(err).NotTo(HaveOccurred(), "no workload Node became Ready within %s", timeout)
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

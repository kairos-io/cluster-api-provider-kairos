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
	"os"
	"os/exec"
	"strings"
	"testing"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	"gopkg.in/yaml.v3"
)

func TestRenderK0sCloudConfig_ControlPlaneSingleNode(t *testing.T) {
	data := TemplateData{
		Role:           "control-plane",
		SingleNode:     true,
		Hostname:       "kairos-control-plane-kv-0",
		UserName:       "kairos",
		UserPassword:   "kairos",
		UserGroups:     []string{"admin"},
		GitHubUser:     "testuser",
		HostnamePrefix: "metal-",
	}

	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	// Check for cloud-config header
	if !strings.Contains(result, "#cloud-config") {
		t.Error("Missing cloud-config header")
	}

	// Check for explicit hostname
	if !strings.Contains(result, "hostname: kairos-control-plane-kv-0") {
		t.Error("Missing or incorrect explicit hostname")
	}

	// Check for k0s block
	if !strings.Contains(result, "k0s:") {
		t.Error("Missing k0s block")
	}

	// Check for enabled: true
	if !strings.Contains(result, "enabled: true") {
		t.Error("Missing k0s enabled flag")
	}

	// Check for --single arg
	if !strings.Contains(result, "--single") {
		t.Error("Missing --single arg for single-node mode")
	}

	// Check for user configuration
	if !strings.Contains(result, "name: kairos") {
		t.Error("Missing user name")
	}

	// Check for GitHub user
	if !strings.Contains(result, "github:testuser") {
		t.Error("Missing GitHub user SSH key")
	}

	// Check for capk groups list
	if !strings.Contains(result, "groups: [users, admin]") {
		t.Error("Missing capk groups list")
	}
}

func TestRenderK0sCloudConfig_ControlPlaneMultiNode(t *testing.T) {
	data := TemplateData{
		Role:           "control-plane",
		SingleNode:     false,
		UserName:       "kairos",
		UserPassword:   "kairos",
		UserGroups:     []string{"admin"},
		HostnamePrefix: "metal-",
	}

	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	// Check for k0s block
	if !strings.Contains(result, "k0s:") {
		t.Error("Missing k0s block")
	}

	// Should NOT have --single arg
	if strings.Contains(result, "--single") {
		t.Error("Should not have --single arg for multi-node mode")
	}
}

func TestRenderK0sCloudConfig_Worker(t *testing.T) {
	data := TemplateData{
		Role:           "worker",
		SingleNode:     false,
		UserName:       "kairos",
		UserPassword:   "kairos",
		UserGroups:     []string{"admin"},
		WorkerToken:    "test-token-12345",
		HostnamePrefix: "metal-",
	}

	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	// Check for k0s-worker block
	if !strings.Contains(result, "k0s-worker:") {
		t.Error("Missing k0s-worker block")
	}

	// Check for enabled: true
	if !strings.Contains(result, "enabled: true") {
		t.Error("Missing k0s-worker enabled flag")
	}

	// Check for token file arg
	if !strings.Contains(result, "--token-file /etc/k0s/token") {
		t.Error("Missing --token-file arg")
	}

	// Check for token file write
	if !strings.Contains(result, "path: /etc/k0s/token") {
		t.Error("Missing token file write_files entry")
	}

	// Check for token content
	if !strings.Contains(result, "test-token-12345") {
		t.Error("Missing worker token in file content")
	}

	// Should NOT have k0s block (control-plane)
	if strings.Contains(result, "\nk0s:\n") {
		t.Error("Should not have k0s block for worker")
	}
}

func TestRenderK0sCloudConfig_ControlPlaneWithCIDRs(t *testing.T) {
	data := TemplateData{
		Role:         "control-plane",
		SingleNode:   true,
		UserName:     "kairos",
		UserPassword: "kairos",
		UserGroups:   []string{"admin"},
		PodCIDR:      "10.244.0.0/16",
		ServiceCIDR:  "10.96.0.0/12",
	}

	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if !strings.Contains(result, "--config /etc/k0s/k0s.yaml") {
		t.Error("Missing k0s --config arg for custom CIDRs")
	}
	if !strings.Contains(result, "path: /etc/k0s/k0s.yaml") {
		t.Error("Missing k0s config file write_files entry")
	}
	if !strings.Contains(result, "podCIDR: 10.244.0.0/16") {
		t.Error("Missing podCIDR in k0s config file")
	}
	if !strings.Contains(result, "serviceCIDR: 10.96.0.0/12") {
		t.Error("Missing serviceCIDR in k0s config file")
	}
}

func TestRenderK0sCloudConfig_CapkBootstrapTrap(t *testing.T) {
	data := TemplateData{
		Role:       "control-plane",
		SingleNode: true,
		UserName:   "kairos",
		IsKubeVirt: true,
	}

	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if !strings.Contains(result, "CAPK: always mark bootstrap success on script exit") {
		t.Error("Missing CAPK bootstrap success trap for KubeVirt")
	}
	if !strings.Contains(result, "bootstrap-success.complete") {
		t.Error("Missing bootstrap success file creation for KubeVirt")
	}
}

func TestRenderK3sCloudConfig_ControlPlaneSingleNode(t *testing.T) {
	data := TemplateData{
		Role:           "control-plane",
		SingleNode:     true,
		Hostname:       "kairos-control-plane-k3s-0",
		UserName:       "kairos",
		UserPassword:   "kairos",
		UserGroups:     []string{"admin"},
		HostnamePrefix: "metal-",
	}

	result, err := RenderK3sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if !strings.Contains(result, "#cloud-config") {
		t.Error("Missing cloud-config header")
	}

	if !strings.Contains(result, "hostname: kairos-control-plane-k3s-0") {
		t.Error("Missing or incorrect explicit hostname")
	}

	if !strings.Contains(result, "k3s:") {
		t.Error("Missing k3s block")
	}

	if !strings.Contains(result, "enabled: true") {
		t.Error("Missing k3s enabled flag")
	}

	if strings.Contains(result, "k3s-agent:") {
		t.Error("Should not have k3s-agent block for control-plane")
	}

	// k3s uses top-level write_files + runcmd (like k0s CAPK)
	if !strings.Contains(result, "path: /etc/systemd/system/kairos-k3s-post-bootstrap.service") {
		t.Error("Missing kairos-k3s-post-bootstrap.service in write_files")
	}
	if !strings.Contains(result, "path: /usr/local/bin/kairos-k3s-post-bootstrap.sh") {
		t.Error("Missing kairos-k3s-post-bootstrap.sh in write_files")
	}
	if !strings.Contains(result, "systemctl enable --now kairos-k3s-post-bootstrap.service") {
		t.Error("Missing systemctl enable for k3s post-bootstrap service in runcmd")
	}
}

func TestRenderK3sCloudConfig_Worker(t *testing.T) {
	data := TemplateData{
		Role:         "worker",
		UserName:     "kairos",
		UserPassword: "kairos",
		UserGroups:   []string{"admin"},
		K3sServerURL: "https://10.0.0.10:6443",
		K3sToken:     "test-k3s-token",
	}

	result, err := RenderK3sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if !strings.Contains(result, "k3s-agent:") {
		t.Error("Missing k3s-agent block")
	}

	if !strings.Contains(result, "--server https://10.0.0.10:6443") {
		t.Error("Missing k3s agent --server arg")
	}

	if !strings.Contains(result, "--token-file /etc/rancher/k3s/token") {
		t.Error("Missing k3s agent --token-file arg")
	}

	if !strings.Contains(result, "path: /etc/rancher/k3s/token") {
		t.Error("Missing k3s token file write_files entry")
	}

	if !strings.Contains(result, "test-k3s-token") {
		t.Error("Missing k3s token in file content")
	}

	if strings.Contains(result, "\nk3s:\n") {
		t.Error("Should not have k3s server block for worker")
	}
}

func TestRenderK3sCloudConfig_ControlPlaneWithProviderID(t *testing.T) {
	data := TemplateData{
		Role:         "control-plane",
		SingleNode:   true,
		Hostname:     "kairos-control-plane-k3s-0",
		ProviderID:   "vsphere://422fa74a-5d60-3a4a-af24-1f07be515fcc",
		UserName:     "kairos",
		UserPassword: "kairos",
		UserGroups:   []string{"admin"},
	}

	result, err := RenderK3sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if !strings.Contains(result, "provider-id=vsphere://422fa74a-5d60-3a4a-af24-1f07be515fcc") {
		t.Error("Missing providerID in k3s config/ExecStartPre when ProviderID is set")
	}
	if !strings.Contains(result, "/etc/rancher/k3s/config.yaml.d/90-provider-id.yaml") {
		t.Error("Missing k3s config file drop-in for providerID when ProviderID is set")
	}
	if !strings.Contains(result, "kubectl patch node $(hostname)") {
		t.Error("Missing k0s-style post-bootstrap providerID patch when ProviderID is set")
	}
	if !strings.Contains(result, "ExecStartPre=") {
		t.Error("Missing ExecStartPre in systemd override when ProviderID is set (writes config before k3s)")
	}
	if strings.Contains(result, "kairos-k3s-discover-provider-id.sh") {
		t.Error("Should not have discovery script when ProviderID is set")
	}
}

func TestRenderK3sCloudConfig_ControlPlaneWithoutProviderID(t *testing.T) {
	data := TemplateData{
		Role:           "control-plane",
		SingleNode:     true,
		Hostname:       "kairos-control-plane-k3s-0",
		UserName:       "kairos",
		UserPassword:   "kairos",
		UserGroups:     []string{"admin"},
		HostnamePrefix: "metal-",
	}

	result, err := RenderK3sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if !strings.Contains(result, "kairos-k3s-discover-provider-id.sh") {
		t.Error("Missing providerID discovery script when ProviderID is not set")
	}
	if !strings.Contains(result, "z-provider-id.conf") {
		t.Error("Missing systemd override when ProviderID is not set")
	}
	if !strings.Contains(result, "90-provider-id.yaml") {
		t.Error("Missing k3s config file write in discovery script when ProviderID is not set")
	}
}

func TestRenderK3sCloudConfig_CapkBootstrapTrap(t *testing.T) {
	data := TemplateData{
		Role:       "control-plane",
		SingleNode: true,
		UserName:   "kairos",
		IsKubeVirt: true,
	}

	result, err := RenderK3sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if !strings.Contains(result, "CAPK: always mark bootstrap success on script exit") {
		t.Error("Missing CAPK bootstrap success trap for KubeVirt k3s")
	}
	if !strings.Contains(result, "bootstrap-success.complete") {
		t.Error("Missing bootstrap success file creation for KubeVirt k3s")
	}
}

func TestRenderK3sCloudConfig_CapkKubeconfigPush(t *testing.T) {
	data := TemplateData{
		Role:       "control-plane",
		SingleNode: true,
		UserName:   "kairos",
		IsKubeVirt: true,
		ManagementEndpoint: &ManagementEndpoint{
			Token:                     "test-token",
			KubeconfigSecretName:      "cluster-kubeconfig",
			KubeconfigSecretNamespace: "default",
			APIServer:                 "https://1.2.3.4:6443",
		},
		ControlPlaneLBEndpoint: "10.0.0.1",
	}

	result, err := RenderK3sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if !strings.Contains(result, "Push kubeconfig to management cluster without SSH") {
		t.Error("Missing kubeconfig push block for CAPK k3s")
	}
	if !strings.Contains(result, "/etc/rancher/k3s/k3s.yaml") {
		t.Error("Missing k3s kubeconfig path in push block")
	}
	// ControlPlaneLBEndpoint is routed through a shquote'd shell variable
	// (`local lb_endpoint='10.0.0.1'`) and then referenced as
	// `https://${lb_endpoint}:6443` in the sed substitution, so the literal
	// "https://10.0.0.1:6443" no longer appears verbatim in the rendered
	// output. Assert on the safe-routing observable shape instead.
	if !strings.Contains(result, "local lb_endpoint='10.0.0.1'") {
		t.Error("Missing shquote'd lb_endpoint local var for ControlPlaneLBEndpoint")
	}
	if !strings.Contains(result, "https://${lb_endpoint}:6443") {
		t.Error("Missing ${lb_endpoint} reference in sed URL substitution")
	}
	if !strings.Contains(result, "systemctl enable --now kairos-k3s-post-bootstrap.service") {
		t.Error("Missing systemctl enable for k3s post-bootstrap service in runcmd")
	}
}

func TestRenderK3sCloudConfig_CapkTlsSan(t *testing.T) {
	data := TemplateData{
		Role:                   "control-plane",
		SingleNode:             true,
		UserName:               "kairos",
		IsKubeVirt:             true,
		ControlPlaneLBEndpoint: "10.0.0.1",
	}

	result, err := RenderK3sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if !strings.Contains(result, "--tls-san=10.0.0.1") {
		t.Error("Missing --tls-san for ControlPlaneLBEndpoint in k3s CAPK template")
	}
}

// TestRenderK0sCloudConfig_CapvTemplateExcludesCapkBlocks asserts the
// CAPV-template invariants that hold REGARDLESS of whether the kubeconfig-push
// block is rendered:
//   - The CAPK-only LB SAN service / endpoint wiring never appears (CAPV has
//     no LB Service).
//   - With ManagementEndpoint == nil, no push block renders. KD-3b extends
//     the CAPV templates with a push block gated on a non-nil
//     ManagementEndpoint; the assertions below cover the nil case. The
//     non-nil case has its own test (TestRenderK0sCloudConfig_CapvKubeconfigPush).
func TestRenderK0sCloudConfig_CapvTemplateExcludesCapkBlocks(t *testing.T) {
	data := TemplateData{
		Role:         "control-plane",
		SingleNode:   true,
		UserName:     "kairos",
		UserPassword: "kairos",
		UserGroups:   []string{"admin"},
	}

	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if strings.Contains(result, "kairos-k0s-lb-sans.service") {
		t.Error("CAPV template should not include KubeVirt LB SAN service")
	}
	if strings.Contains(result, "KAIROS_LB_ENDPOINT") {
		t.Error("CAPV template should not include KubeVirt LB endpoint handling")
	}
	if strings.Contains(result, "push_kubeconfig") {
		t.Error("CAPV template should not include push_kubeconfig block when ManagementEndpoint is nil")
	}
}

func TestRenderK0sCloudConfig_WithManifests(t *testing.T) {
	data := TemplateData{
		Role:           "control-plane",
		SingleNode:     true,
		UserName:       "kairos",
		UserPassword:   "kairos",
		UserGroups:     []string{"admin"},
		HostnamePrefix: "metal-",
		Manifests: []bootstrapv1beta2.Manifest{
			{
				Name:    "test",
				File:    "test.yaml",
				Content: "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: test",
			},
		},
	}

	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	// Check for manifest path
	if !strings.Contains(result, "/var/lib/k0s/manifests/test/test.yaml") {
		t.Error("Missing manifest path")
	}

	// Check for manifest content
	if !strings.Contains(result, "kind: Namespace") {
		t.Error("Missing manifest content")
	}
}

func TestRenderK0sCloudConfig_WithSSHPublicKey(t *testing.T) {
	data := TemplateData{
		Role:           "control-plane",
		SingleNode:     true,
		UserName:       "kairos",
		UserPassword:   "kairos",
		UserGroups:     []string{"admin"},
		SSHPublicKey:   "ssh-rsa AAAAB3NzaC1yc2E...",
		HostnamePrefix: "metal-",
	}

	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	// Check for SSH public key
	if !strings.Contains(result, "ssh-rsa AAAAB3NzaC1yc2E...") {
		t.Error("Missing SSH public key")
	}
}

func TestRenderK0sCloudConfig_WithInstallConfig(t *testing.T) {
	installConfig := &InstallConfig{
		Auto:   true,
		Device: "auto",
		Reboot: true,
	}

	data := TemplateData{
		Role:           "control-plane",
		SingleNode:     true,
		UserName:       "kairos",
		UserPassword:   "kairos",
		UserGroups:     []string{"admin"},
		HostnamePrefix: "metal-",
		Install:        installConfig,
	}

	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	// Check for install block
	if !strings.Contains(result, "install:") {
		t.Error("Missing install block")
	}

	// Check for install.auto
	if !strings.Contains(result, "auto: true") {
		t.Error("Missing install.auto: true")
	}

	// Check for install.device — YAML emits unquoted for safe values like "auto".
	// The semantic invariant is that install.device == "auto" after parsing.
	if !strings.Contains(result, "device: auto") {
		t.Error("Missing install.device: auto")
	}

	// Check for install.reboot
	if !strings.Contains(result, "reboot: true") {
		t.Error("Missing install.reboot: true")
	}
}

func TestRenderK0sCloudConfig_WithDNSServers(t *testing.T) {
	data := TemplateData{
		Role:           "control-plane",
		SingleNode:     true,
		UserName:       "kairos",
		UserPassword:   "kairos",
		UserGroups:     []string{"admin"},
		HostnamePrefix: "metal-",
		DNSServers:     []string{"1.1.1.1", "8.8.8.8"},
	}

	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if !strings.Contains(result, "dns:\n        nameservers:\n          - 1.1.1.1\n          - 8.8.8.8") {
		t.Error("Missing DNS servers initramfs block")
	}
}

// TestRenderK0sCloudConfig_CapkNoManagementEndpoint asserts that when
// ManagementEndpoint is nil on the CAPK k0s control-plane template, the
// in-node kubeconfig-push block is omitted entirely. The pointer-nil check on
// .ManagementEndpoint is the single gate the renderer uses; this guards
// against accidentally re-introducing a flat-field-only check that would
// silently render an unauthenticated push attempt.
//
// Two gate sites in the k0s CAPK template close on ManagementEndpoint:
//
//  1. The KAIROS_MGMT_API / KAIROS_MGMT_TOKEN assignment lines in the
//     fetch_vmi_ip_from_management() helper (the rest of the helper body
//     always renders, so we assert on the ASSIGNMENT lines, not on the
//     identifier itself which is referenced in `[ -z "${KAIROS_MGMT_API}" ]`).
//  2. The push_kubeconfig() function and its call sites.
func TestRenderK0sCloudConfig_CapkNoManagementEndpoint(t *testing.T) {
	data := TemplateData{
		Role:               "control-plane",
		SingleNode:         true,
		UserName:           "kairos",
		IsKubeVirt:         true,
		ManagementEndpoint: nil,
	}
	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}
	// Assignment lines must be omitted (they only render under .ManagementEndpoint).
	if strings.Contains(result, "KAIROS_MGMT_API=") {
		t.Errorf("KAIROS_MGMT_API assignment must not appear when ManagementEndpoint is nil")
	}
	if strings.Contains(result, "KAIROS_MGMT_TOKEN=") {
		t.Errorf("KAIROS_MGMT_TOKEN assignment must not appear when ManagementEndpoint is nil")
	}
	// Entire push block must be omitted.
	if strings.Contains(result, "push_kubeconfig") {
		t.Errorf("push_kubeconfig must not appear when ManagementEndpoint is nil")
	}
}

// TestRenderK3sCloudConfig_CapkNoManagementEndpoint is the k3s twin of
// TestRenderK0sCloudConfig_CapkNoManagementEndpoint.
func TestRenderK3sCloudConfig_CapkNoManagementEndpoint(t *testing.T) {
	data := TemplateData{
		Role:               "control-plane",
		SingleNode:         true,
		UserName:           "kairos",
		IsKubeVirt:         true,
		ManagementEndpoint: nil,
	}
	result, err := RenderK3sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}
	if strings.Contains(result, "push_kubeconfig") {
		t.Errorf("push_kubeconfig must not appear when ManagementEndpoint is nil")
	}
	// k3s template doesn't have the KAIROS_MGMT_API SAN-detection block (only
	// the push block), so the push_kubeconfig check above is sufficient.
}

// TestRenderK0sCloudConfig_CapvKubeconfigPush asserts the KD-3b extension:
// when a CAPV control plane has a non-nil ManagementEndpoint, the CAPV k0s
// template renders the push_kubeconfig() block with the cluster-name label
// and the node-push provenance annotation in the curl payload. The
// ControlPlaneEndpointHost field drives the `server:` URL rewrite (CAPV has
// no LB Service, so the cluster's controlPlaneEndpoint host is the canonical
// reachable address from the management cluster).
func TestRenderK0sCloudConfig_CapvKubeconfigPush(t *testing.T) {
	data := TemplateData{
		Role:       "control-plane",
		SingleNode: true,
		UserName:   "kairos",
		IsKubeVirt: false, // CAPV path
		ManagementEndpoint: &ManagementEndpoint{
			Token:                     "test-token",
			KubeconfigSecretName:      "cluster-kubeconfig",
			KubeconfigSecretNamespace: "default",
			APIServer:                 "https://mgmt.example.com:6443",
			ClusterName:               "test-cluster",
			ControlPlaneEndpointHost:  "10.0.0.42",
		},
	}

	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if !strings.Contains(result, "push_kubeconfig()") {
		t.Error("Missing push_kubeconfig function for CAPV k0s control plane")
	}
	if !strings.Contains(result, "/var/lib/k0s/pki/admin.conf") {
		t.Error("Missing k0s kubeconfig path in push block")
	}
	// KD-3b race fix: post-bootstrap must wait for admin.conf to appear, not
	// silently abandon the push if k0s hasn't finished bootstrapping yet.
	if !strings.Contains(result, "_wait_budget=300") {
		t.Error("Missing 5min wait-loop for admin.conf in push block (KD-3b race)")
	}
	if !strings.Contains(result, "while [ ! -f \"${kubeconfig_file}\" ]") {
		t.Error("Missing wait-loop guard on kubeconfig_file existence")
	}
	if !strings.Contains(result, `\"cluster.x-k8s.io/cluster-name\":\"${cluster_name}\"`) {
		t.Error("Push block must stamp cluster-name label on the Secret payload")
	}
	if !strings.Contains(result, `\"controllers.cluster.x-k8s.io/kubeconfig-source\":\"node-push\"`) {
		t.Error("Push block must stamp node-push provenance annotation")
	}
	// ControlPlaneEndpointHost is shquote'd into a local shell var, then
	// referenced via ${cp_endpoint_host} in the sed substitution.
	if !strings.Contains(result, "local cp_endpoint_host='10.0.0.42'") {
		t.Error("Missing shquote'd cp_endpoint_host local var")
	}
	if !strings.Contains(result, "https://${cp_endpoint_host}:6443") {
		t.Error("Missing ${cp_endpoint_host} reference in sed URL rewrite")
	}
	// cluster_name shquote envelope must be present.
	if !strings.Contains(result, "local cluster_name='test-cluster'") {
		t.Error("Missing shquote'd cluster_name local var")
	}
}

// TestRenderK3sCloudConfig_CapvKubeconfigPush is the k3s twin of the CAPV
// push test above. The kubeconfig path differs (/etc/rancher/k3s/k3s.yaml)
// but otherwise the assertions are the same.
func TestRenderK3sCloudConfig_CapvKubeconfigPush(t *testing.T) {
	data := TemplateData{
		Role:       "control-plane",
		SingleNode: true,
		UserName:   "kairos",
		IsKubeVirt: false, // CAPV path
		ManagementEndpoint: &ManagementEndpoint{
			Token:                     "test-token",
			KubeconfigSecretName:      "cluster-kubeconfig",
			KubeconfigSecretNamespace: "default",
			APIServer:                 "https://mgmt.example.com:6443",
			ClusterName:               "test-cluster",
			ControlPlaneEndpointHost:  "10.0.0.42",
		},
	}

	result, err := RenderK3sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if !strings.Contains(result, "push_kubeconfig()") {
		t.Error("Missing push_kubeconfig function for CAPV k3s control plane")
	}
	if !strings.Contains(result, "/etc/rancher/k3s/k3s.yaml") {
		t.Error("Missing k3s kubeconfig path in push block")
	}
	// KD-3b race fix: post-bootstrap must wait for k3s.yaml to appear, not
	// silently abandon the push if k3s hasn't finished bootstrapping yet.
	if !strings.Contains(result, "_wait_budget=300") {
		t.Error("Missing 5min wait-loop for k3s.yaml in push block (KD-3b race)")
	}
	if !strings.Contains(result, "while [ ! -f \"${kubeconfig_file}\" ]") {
		t.Error("Missing wait-loop guard on kubeconfig_file existence")
	}
	if !strings.Contains(result, `\"cluster.x-k8s.io/cluster-name\":\"${cluster_name}\"`) {
		t.Error("Push block must stamp cluster-name label on the Secret payload")
	}
	if !strings.Contains(result, `\"controllers.cluster.x-k8s.io/kubeconfig-source\":\"node-push\"`) {
		t.Error("Push block must stamp node-push provenance annotation")
	}
	if !strings.Contains(result, "local cp_endpoint_host='10.0.0.42'") {
		t.Error("Missing shquote'd cp_endpoint_host local var")
	}
	if !strings.Contains(result, "https://${cp_endpoint_host}:6443") {
		t.Error("Missing ${cp_endpoint_host} reference in sed URL rewrite")
	}
}

// TestRenderK0sCloudConfig_CapvNoManagementEndpoint guards the gate: with a
// nil ManagementEndpoint the CAPV k0s template MUST NOT render the push
// block. This is the byte-identical preservation check the architect called
// out as the CRITICAL invariant for KD-3b commit 1.
func TestRenderK0sCloudConfig_CapvNoManagementEndpoint(t *testing.T) {
	data := TemplateData{
		Role:               "control-plane",
		SingleNode:         true,
		UserName:           "kairos",
		IsKubeVirt:         false,
		ManagementEndpoint: nil,
	}
	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}
	if strings.Contains(result, "push_kubeconfig") {
		t.Errorf("push_kubeconfig must not appear when ManagementEndpoint is nil on CAPV k0s template")
	}
	if strings.Contains(result, "controllers.cluster.x-k8s.io/kubeconfig-source") {
		t.Errorf("node-push annotation must not appear when ManagementEndpoint is nil")
	}
}

// TestRenderK3sCloudConfig_CapvNoManagementEndpoint is the k3s twin.
func TestRenderK3sCloudConfig_CapvNoManagementEndpoint(t *testing.T) {
	data := TemplateData{
		Role:               "control-plane",
		SingleNode:         true,
		UserName:           "kairos",
		IsKubeVirt:         false,
		ManagementEndpoint: nil,
	}
	result, err := RenderK3sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}
	if strings.Contains(result, "push_kubeconfig") {
		t.Errorf("push_kubeconfig must not appear when ManagementEndpoint is nil on CAPV k3s template")
	}
	if strings.Contains(result, "controllers.cluster.x-k8s.io/kubeconfig-source") {
		t.Errorf("node-push annotation must not appear when ManagementEndpoint is nil")
	}
}

// TestRenderCapkKubeconfigPush_CarriesClusterNameAndAnnotation asserts the
// CAPK templates also gained the cluster-name label and node-push annotation
// (so the controlplane controller's label-based watch predicate works
// uniformly across CAPK and CAPV).
func TestRenderCapkKubeconfigPush_CarriesClusterNameAndAnnotation(t *testing.T) {
	for _, dist := range []string{"k0s", "k3s"} {
		t.Run(dist, func(t *testing.T) {
			data := TemplateData{
				Role:       "control-plane",
				SingleNode: true,
				UserName:   "kairos",
				IsKubeVirt: true,
				ManagementEndpoint: &ManagementEndpoint{
					Token:                     "test-token",
					KubeconfigSecretName:      "cluster-kubeconfig",
					KubeconfigSecretNamespace: "default",
					APIServer:                 "https://1.2.3.4:6443",
					ClusterName:               "capk-cluster",
				},
				ControlPlaneLBEndpoint: "10.0.0.1",
			}
			var (
				result string
				err    error
			)
			if dist == "k0s" {
				result, err = RenderK0sCloudConfig(data)
			} else {
				result, err = RenderK3sCloudConfig(data)
			}
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.Contains(result, `\"cluster.x-k8s.io/cluster-name\":\"${cluster_name}\"`) {
				t.Errorf("CAPK %s push payload missing cluster-name label", dist)
			}
			if !strings.Contains(result, `\"controllers.cluster.x-k8s.io/kubeconfig-source\":\"node-push\"`) {
				t.Errorf("CAPK %s push payload missing node-push annotation", dist)
			}
			if !strings.Contains(result, "local cluster_name='capk-cluster'") {
				t.Errorf("CAPK %s push block missing shquote'd cluster_name var", dist)
			}
			// KD-3b race fix: wait-loop must be rendered in CAPK too — the race
			// exists for any distribution whose service starts faster than its
			// kubeconfig is written.
			if !strings.Contains(result, "_wait_budget=300") {
				t.Errorf("CAPK %s push block missing wait-loop for kubeconfig file", dist)
			}
		})
	}
}

func TestRenderK0sCloudConfig_WithoutInstallConfig(t *testing.T) {
	data := TemplateData{
		Role:           "control-plane",
		SingleNode:     true,
		UserName:       "kairos",
		UserPassword:   "kairos",
		UserGroups:     []string{"admin"},
		HostnamePrefix: "metal-",
		Install:        nil, // No install config
	}

	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	// Verify install block is NOT present when Install is nil
	if strings.Contains(result, "install:") {
		t.Error("Install block should not be present when Install is nil")
	}
}

// TestRenderK0sCloudConfig_ControlPlaneWithProviderID verifies the KD-3c
// kubelet-arg injection: when ProviderID is known at render time, the k0s
// args block must include `--kubelet-extra-args=--provider-id=<X>` so k0s
// passes that flag to kubelet at startup. Without this, kubelet registers
// the Node before the post-bootstrap kubectl patch can set ProviderID, and
// ProviderID being immutable in K8s once set means the patch silently
// no-ops. (PR-8 audit finding, ADR 0001 § E.)
func TestRenderK0sCloudConfig_ControlPlaneWithProviderID(t *testing.T) {
	data := TemplateData{
		Role:         "control-plane",
		SingleNode:   true,
		Hostname:     "kairos-control-plane-k0s-0",
		ProviderID:   "vsphere://422fa74a-5d60-3a4a-af24-1f07be515fcc",
		UserName:     "kairos",
		UserPassword: "kairos",
		UserGroups:   []string{"admin"},
	}
	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if !strings.Contains(result, "--kubelet-extra-args=--provider-id=vsphere://422fa74a-5d60-3a4a-af24-1f07be515fcc") {
		t.Error("k0s args MUST include --kubelet-extra-args=--provider-id=<X> when ProviderID is set so kubelet registers the Node with the correct providerID from the start (KD-3c)")
	}
	// The args block must be rendered (the surrounding `args:` key must exist) — a missing key would mean
	// the OR-gate in the template didn't pick up .ProviderID.
	if !strings.Contains(result, "  args:") {
		t.Error("k0s `args:` block missing entirely; the OR-gate for the args block must include .ProviderID")
	}
	// The post-bootstrap kubectl patch fallback stays — it's defense-in-depth
	// for the case where the kubelet flag didn't take (e.g., a future k0s version
	// that changes how --kubelet-extra-args is consumed). Assert it's still rendered.
	if !strings.Contains(result, "kubectl patch node $(hostname)") {
		t.Error("post-bootstrap kubectl patch fallback should still render for defense in depth")
	}
}

// TestRenderK0sCloudConfig_ControlPlaneWithoutProviderID is the negative
// case: with no ProviderID, the args block MUST NOT carry a
// --kubelet-extra-args entry (would emit a syntactically invalid k0s flag).
// The post-bootstrap patch block also adjusts — it logs "No providerID to
// set" instead of attempting a patch.
func TestRenderK0sCloudConfig_ControlPlaneWithoutProviderID(t *testing.T) {
	data := TemplateData{
		Role:       "control-plane",
		SingleNode: true,
		Hostname:   "kairos-control-plane-k0s-0",
		// ProviderID intentionally empty.
		UserName:     "kairos",
		UserPassword: "kairos",
		UserGroups:   []string{"admin"},
	}
	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if strings.Contains(result, "--kubelet-extra-args=--provider-id=") {
		t.Error("k0s args MUST NOT include --kubelet-extra-args=--provider-id= when ProviderID is empty (would emit a malformed flag)")
	}
	// SingleNode=true should still render --single, so the args block exists.
	if !strings.Contains(result, "--single") {
		t.Error("SingleNode=true must still render --single")
	}
}

// TestRenderK0sCloudConfig_WorkerWithProviderID verifies the same flag is
// injected into the k0s-worker args block. Worker scope matters less today
// (HA gated on KD-5), but the field exists and the rule is uniform.
func TestRenderK0sCloudConfig_WorkerWithProviderID(t *testing.T) {
	data := TemplateData{
		Role:         "worker",
		WorkerToken:  "fake-worker-token",
		Hostname:     "kairos-worker-k0s-0",
		ProviderID:   "kubevirt://kairos-worker-k0s-0",
		UserName:     "kairos",
		UserPassword: "kairos",
		UserGroups:   []string{"admin"},
	}
	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("Failed to render template: %v", err)
	}

	if !strings.Contains(result, "--kubelet-extra-args=--provider-id=kubevirt://kairos-worker-k0s-0") {
		t.Error("k0s-worker args MUST include --kubelet-extra-args=--provider-id=<X> when ProviderID is set")
	}
	// The existing --token-file arg must still render.
	if !strings.Contains(result, "--token-file /etc/k0s/token") {
		t.Error("worker --token-file arg lost; the addition of the providerID conditional must not break the existing arg")
	}
}

// TestRenderK0sCloudConfig_ProviderID_InjectionRejected pins the security
// rationale that lets us interpolate .ProviderID bare in a shell-context
// position (the args list value): the validator at internal/bootstrap/validate.go
// must reject any value containing characters outside the strict providerID
// regex. If a future refactor weakens the validator, this test catches it
// before reaching the renderer.
func TestRenderK0sCloudConfig_ProviderID_InjectionRejected(t *testing.T) {
	injectionAttempts := []string{
		`vsphere://X"; rm -rf /; echo "`,
		`vsphere://X` + "\n" + `bash -c "rm -rf /"`,
		`vsphere://X$(id)`,
		`vsphere://X` + "`id`",
		`vsphere://X--config=/etc/shadow`,
	}
	for _, p := range injectionAttempts {
		p := p
		t.Run("rejected:"+p[:min(len(p), 30)], func(t *testing.T) {
			if err := ValidateProviderID(p); err == nil {
				t.Errorf("ValidateProviderID(%q) returned nil; should have rejected (shell injection surface)", p)
			}
		})
	}
}

// metal3ControlPlaneData returns a TemplateData for a Metal3 control-plane node
// with a ManagementEndpoint set, used across the Metal3 test suite.
func metal3ControlPlaneData() TemplateData {
	return TemplateData{
		Role:       "control-plane",
		SingleNode: true,
		Hostname:   "bare-metal-cp-0",
		UserName:   "kairos",
		UserGroups: []string{"admin"},
		Metal3:     true,
		// ProviderID intentionally empty: the controller suppresses it for Metal3.
		ManagementEndpoint: &ManagementEndpoint{
			Token:                     "test-token",
			KubeconfigSecretName:      "cluster-kubeconfig",
			KubeconfigSecretNamespace: "default",
			APIServer:                 "https://mgmt.example.com:6443",
			ClusterName:               "bare-metal-cluster",
			ControlPlaneEndpointHost:  "192.168.10.10",
		},
	}
}

// extractWriteFile parses the rendered cloud-config and returns the content of
// the first write_files entry whose path ends with suffix (empty string if not
// found).
func extractWriteFile(t *testing.T, rendered, suffix string) string {
	t.Helper()
	var cc struct {
		WriteFiles []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &cc); err != nil {
		t.Fatalf("parse rendered YAML: %v", err)
	}
	for _, wf := range cc.WriteFiles {
		if strings.HasSuffix(wf.Path, suffix) {
			return wf.Content
		}
	}
	return ""
}

// TestRenderMetal3_ProviderIDScriptValidBash renders the Metal3 cloud-config
// for both distributions, extracts the config-drive providerID shell script
// (moved from the post-bootstrap script to a pre-start ExecStartPre after the
// CAPM3 v1.13 lab correction), and runs `bash -n` on it. The UUID validation
// guards (`case`/`[[ =~ ]]`) only execute at node boot, so a bash syntax
// regression there would otherwise stay invisible until lab provisioning. This
// locks the rendered script's syntax at unit-test time. We also bash -n the
// post-bootstrap script to ensure removing the Metal3 stanza left it valid.
func TestRenderMetal3_ProviderIDScriptValidBash(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available; skipping rendered-script syntax check")
	}
	cases := []struct {
		name        string
		render      func(TemplateData) (string, error)
		pidScript   string // suffix of the config-drive providerID script
		mustContain string // a marker that proves we extracted the right script
	}{
		// k0s: providerID is set by a post-bootstrap kubectl patch (ADR 0004 redesign),
		// so the Metal3 logic lives in the post-bootstrap script, not a pre-start unit.
		{"k0s", RenderK0sCloudConfig, "kairos-k0s-post-bootstrap.sh", "set_metal3_providerid"},
		{"k3s", RenderK3sCloudConfig, "kairos-k3s-metal3-providerid.sh", "provider-id=metal3://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.render(metal3ControlPlaneData())
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			// The config-drive providerID script must exist, be the Metal3
			// variant, and be syntactically valid bash.
			pid := extractWriteFile(t, out, tc.pidScript)
			if pid == "" {
				t.Fatalf("%s providerID script (%s) not found in rendered write_files", tc.name, tc.pidScript)
			}
			if !strings.Contains(pid, "metal3-providerid") {
				t.Fatalf("extracted script is not the Metal3 providerID variant (missing metal3-providerid marker)")
			}
			if !strings.Contains(pid, tc.mustContain) {
				t.Fatalf("%s providerID script missing %q", tc.name, tc.mustContain)
			}
			// The post-bootstrap script must no longer carry any Metal3 stanza
			// and must still be valid bash.
			pb := extractWriteFile(t, out, "post-bootstrap.sh")
			if pb == "" {
				t.Fatalf("%s post-bootstrap.sh not found in rendered write_files", tc.name)
			}
			if strings.Contains(pb, "set_metal3_uuid_label") {
				t.Errorf("%s post-bootstrap script must NOT contain the old set_metal3_uuid_label function (moved to pre-start)", tc.name)
			}
			for name, script := range map[string]string{tc.pidScript: pid, "post-bootstrap.sh": pb} {
				f := filepathJoinTemp(t, strings.ReplaceAll(name, "/", "_"))
				if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
					t.Fatalf("write temp script: %v", err)
				}
				if outBytes, err := exec.Command(bashPath, "-n", f).CombinedOutput(); err != nil {
					t.Fatalf("bash -n on rendered %s %s failed: %v\n%s", tc.name, name, err, outBytes)
				}
			}
		})
	}
}

// filepathJoinTemp returns a path under the test's temp dir.
func filepathJoinTemp(t *testing.T, name string) string {
	t.Helper()
	return t.TempDir() + string(os.PathSeparator) + name
}

// TestRenderMetal3_K0sControlPlane verifies the full Metal3 k0s render after
// the CAPM3 v1.13 lab correction:
// (a) the config-drive providerID pre-start mechanism is present and sets BOTH
//
//	provider-id=metal3://<uuid> and the metal3.io/uuid node-label, reading
//	meta_data.json and validating the UUID fail-closed;
//
// (b) NO vsphere://-style providerID and NO post-bootstrap kubectl patch
//
//	(those use the wrong, CAPV-shaped providerID for Metal3);
//
// (c) push_kubeconfig block still present; (d) valid YAML.
func TestRenderMetal3_K0sControlPlane(t *testing.T) {
	result, err := RenderK0sCloudConfig(metal3ControlPlaneData())
	if err != nil {
		t.Fatalf("RenderK0sCloudConfig with Metal3=true: %v", err)
	}

	// (a) The broken pre-start ExecStart-injection mechanism must be GONE. The k0s
	// Metal3 providerID is now set by a post-bootstrap kubectl patch (ADR 0004
	// redesign): the pre-start oneshot deadlocked k0scontroller in lab e2e (a
	// Before=k0scontroller unit that restarts k0scontroller is a systemd ordering
	// cycle), and the boot-stage enable ran too late (k0s starts before cloud-config).
	for _, gone := range []string{
		"kairos-k0s-metal3-providerid.service",
		"kairos-k0s-metal3-providerid.sh",
		"Before=k0scontroller.service",
		`--kubelet-extra-args=\"--provider-id=metal3://`,
	} {
		if strings.Contains(result, gone) {
			t.Errorf("Metal3 k0s: removed pre-start mechanism must NOT appear: %q", gone)
		}
	}
	if strings.Contains(result, "systemctl restart k0scontroller") {
		t.Error("Metal3 k0s: must NOT restart k0scontroller (the pre-start restart deadlocked); providerID is set via post-bootstrap patch")
	}

	// (b) The post-bootstrap patch mechanism must be present: the function, the
	// config-drive read (mounted ro,nodev,nosuid,noexec), the fail-closed UUID
	// validation, and a kubectl patch that sets providerID=metal3://<uuid> + the
	// metal3.io/uuid label via the local admin kubeconfig.
	if !strings.Contains(result, "set_metal3_providerid") {
		t.Error("Metal3 k0s: missing the set_metal3_providerid post-bootstrap function")
	}
	if !strings.Contains(result, "meta_data.json") {
		t.Error("Metal3 k0s: missing meta_data.json reference (config-drive read)")
	}
	if !strings.Contains(result, "ro,nodev,nosuid,noexec") {
		t.Error("Metal3 k0s: config-drive must be mounted ro,nodev,nosuid,noexec")
	}
	if !strings.Contains(result, `[0-9a-fA-F]`) {
		t.Error("Metal3 k0s: missing UUID regex validation")
	}
	if !strings.Contains(result, "case ") || !strings.Contains(result, "*[!0-9a-fA-F-]*") {
		t.Error("Metal3 k0s: missing the char-class case guard on the UUID")
	}
	if !strings.Contains(result, "fail-closed") {
		t.Error("Metal3 k0s: missing fail-closed handling in the providerID logic")
	}
	if !strings.Contains(result, "k0s kubectl patch node") {
		t.Error("Metal3 k0s: must patch the Node providerID via `k0s kubectl patch node`")
	}
	if !strings.Contains(result, `desired="metal3://`) {
		t.Error("Metal3 k0s: patch must target providerID=metal3://<uuid>")
	}
	if !strings.Contains(result, "metal3.io/uuid") {
		t.Error("Metal3 k0s: must set the metal3.io/uuid Node label")
	}

	// (c) No vsphere://-style providerID for Metal3.
	if strings.Contains(result, "vsphere://") || strings.Contains(result, "provider-id=vsphere") {
		t.Error("Metal3 k0s: vsphere:// providerID must NOT appear (wrong providerID for Metal3)")
	}

	// (d) push_kubeconfig block must still be present.
	if !strings.Contains(result, "push_kubeconfig") {
		t.Error("Metal3 k0s: push_kubeconfig block must still be rendered")
	}

	// (e) Rendered output must parse as valid YAML.
	var parsed any
	if err := yaml.Unmarshal([]byte(result), &parsed); err != nil {
		t.Errorf("Metal3 k0s render is not valid YAML: %v", err)
	}
}

// TestRenderMetal3_K3sControlPlane is the k3s twin of TestRenderMetal3_K0sControlPlane.
func TestRenderMetal3_K3sControlPlane(t *testing.T) {
	result, err := RenderK3sCloudConfig(metal3ControlPlaneData())
	if err != nil {
		t.Fatalf("RenderK3sCloudConfig with Metal3=true: %v", err)
	}

	// (a) Config-drive providerID pre-start mechanism present. k3s reuses the
	// z-provider-id.conf drop-in, but its ExecStartPre runs the Metal3 script.
	if !strings.Contains(result, "z-provider-id.conf") {
		t.Error("Metal3 k3s: z-provider-id.conf drop-in must be present (now drives the Metal3 ExecStartPre)")
	}
	if !strings.Contains(result, "kairos-k3s-metal3-providerid.sh") {
		t.Error("Metal3 k3s: missing kairos-k3s-metal3-providerid.sh script")
	}
	if !strings.Contains(result, "ExecStartPre=/bin/sh -c '/usr/local/bin/kairos-k3s-metal3-providerid.sh") {
		t.Error("Metal3 k3s: z-provider-id.conf must wire the Metal3 providerID script as ExecStartPre")
	}
	if !strings.Contains(result, "meta_data.json") {
		t.Error("Metal3 k3s: missing meta_data.json reference (config-drive read)")
	}
	// The script must write BOTH the kubelet provider-id (metal3://) and the
	// metal3.io/uuid node-label into config.yaml.d.
	if !strings.Contains(result, "provider-id=metal3://") {
		t.Error("Metal3 k3s: missing provider-id=metal3:// in the config.yaml.d write")
	}
	if !strings.Contains(result, "metal3.io/uuid=") {
		t.Error("Metal3 k3s: missing metal3.io/uuid= node-label in the config.yaml.d write")
	}
	if !strings.Contains(result, "90-provider-id.yaml") {
		t.Error("Metal3 k3s: missing 90-provider-id.yaml config.yaml.d target")
	}
	if !strings.Contains(result, `[0-9a-fA-F]`) {
		t.Error("Metal3 k3s: missing UUID regex validation in script")
	}
	if !strings.Contains(result, "case ") || !strings.Contains(result, "*[!0-9a-fA-F-]*") {
		t.Error("Metal3 k3s: missing the char-class case guard on the UUID")
	}
	if !strings.Contains(result, "fail-closed") {
		t.Error("Metal3 k3s: missing fail-closed comment/log in providerID script")
	}

	// (b) No vsphere://-style providerID and no post-bootstrap kubectl patch.
	if strings.Contains(result, "vsphere://") {
		t.Error("Metal3 k3s: vsphere:// must NOT appear (wrong providerID for Metal3)")
	}
	if strings.Contains(result, "provider-id=vsphere") {
		t.Error("Metal3 k3s: provider-id=vsphere must NOT appear")
	}
	if strings.Contains(result, `kubectl patch node`) {
		t.Error("Metal3 k3s: kubectl patch node (providerID) must NOT appear (set pre-start instead)")
	}
	if strings.Contains(result, "set_metal3_uuid_label") {
		t.Error("Metal3 k3s: old post-bootstrap set_metal3_uuid_label must NOT appear (moved to pre-start)")
	}
	// The CAPV VM self-discovery script must also be absent for Metal3.
	if strings.Contains(result, "kairos-k3s-discover-provider-id.sh") {
		t.Error("Metal3 k3s: kairos-k3s-discover-provider-id.sh must NOT appear for Metal3")
	}

	// (c) push_kubeconfig block must still be present.
	if !strings.Contains(result, "push_kubeconfig") {
		t.Error("Metal3 k3s: push_kubeconfig block must still be rendered")
	}

	// (d) Rendered output must parse as valid YAML.
	var parsed any
	if err := yaml.Unmarshal([]byte(result), &parsed); err != nil {
		t.Errorf("Metal3 k3s render is not valid YAML: %v", err)
	}
}

// TestRenderMetal3_SuppressionWithNonEmptyProviderID verifies that even when
// the controller accidentally passes a non-empty (CAPV-shaped) ProviderID
// alongside Metal3=true, the template guards (not .Metal3) prevent the
// RENDER-TIME providerID from being interpolated anywhere: no vsphere://-style
// kubelet arg, no kubectl patch. The only providerID that may appear is the
// metal3://<uuid> value computed at boot by the config-drive pre-start script.
// This is the belt-and-braces check for the controller's suppression logic.
func TestRenderMetal3_SuppressionWithNonEmptyProviderID(t *testing.T) {
	for _, dist := range []string{"k0s", "k3s"} {
		dist := dist
		t.Run(dist, func(t *testing.T) {
			data := metal3ControlPlaneData()
			// Deliberately set a CAPV-shaped ProviderID even though Metal3=true.
			// The template must ignore the render-time value entirely.
			data.ProviderID = "vsphere://422fa74a-5d60-3a4a-af24-1f07be515fcc"

			var (
				result string
				err    error
			)
			if dist == "k0s" {
				result, err = RenderK0sCloudConfig(data)
			} else {
				result, err = RenderK3sCloudConfig(data)
			}
			if err != nil {
				t.Fatalf("render with Metal3=true + ProviderID set (%s): %v", dist, err)
			}

			// The render-time providerID must not leak in anywhere.
			if strings.Contains(result, "vsphere://") {
				t.Errorf("%s: vsphere:// render-time providerID must NOT appear when Metal3=true", dist)
			}
			if strings.Contains(result, "422fa74a-5d60-3a4a-af24-1f07be515fcc") {
				t.Errorf("%s: the render-time providerID UUID must NOT appear when Metal3=true", dist)
			}
			// The render-time vsphere:// providerID path is suppressed for Metal3
			// (gated `(not .Metal3)`); only the boot-computed metal3:// mechanism
			// remains. The mechanism differs by distribution:
			if dist == "k3s" {
				// k3s sets providerID via a config.yaml.d drop-in — no kubectl patch.
				if strings.Contains(result, "kubectl patch node") {
					t.Errorf("%s: kubectl patch node must NOT appear when Metal3=true", dist)
				}
				if !strings.Contains(result, "provider-id=metal3://") {
					t.Errorf("%s: the metal3:// config-drive providerID mechanism must appear when Metal3=true", dist)
				}
			} else {
				// k0s sets providerID via a post-bootstrap `k0s kubectl patch node`
				// using the boot-read UUID (never the render-time vsphere:// value).
				if !strings.Contains(result, `desired="metal3://`) {
					t.Errorf("%s: the metal3:// config-drive providerID mechanism must appear when Metal3=true", dist)
				}
			}
		})
	}
}

// TestRenderMetal3_SizeGuard_Exceeded tests that renderTemplate returns the
// 60 KiB size-guard error when Metal3=true and the rendered output exceeds the
// config-drive budget.
func TestRenderMetal3_SizeGuard_Exceeded(t *testing.T) {
	// Build a data set with enough Manifests to push the output over 60 KiB.
	// Each manifest injects ~1 KiB of content; 70 manifests easily exceeds 60 KiB
	// for any of the CAPV templates (baseline ~8 KiB + Metal3 block ~3 KiB).
	data := metal3ControlPlaneData()
	for i := 0; i < 70; i++ {
		data.Manifests = append(data.Manifests, bootstrapv1beta2.Manifest{
			Name:    "big",
			File:    "manifest.yaml",
			Content: strings.Repeat("# padding line to inflate config-drive size budget\n", 20),
		})
	}

	// k0s: Metal3=true must error.
	_, err := RenderK0sCloudConfig(data)
	if err == nil {
		t.Error("k0s: expected size-guard error when Metal3=true and output > 60 KiB, got nil")
	} else if !strings.Contains(err.Error(), "60KiB safety budget") {
		t.Errorf("k0s: size-guard error message unexpected: %v", err)
	}

	// k3s: Metal3=true must error.
	_, err = RenderK3sCloudConfig(data)
	if err == nil {
		t.Error("k3s: expected size-guard error when Metal3=true and output > 60 KiB, got nil")
	} else if !strings.Contains(err.Error(), "60KiB safety budget") {
		t.Errorf("k3s: size-guard error message unexpected: %v", err)
	}
}

// TestRenderMetal3_SizeGuard_NotEnforcedForNonMetal3 tests that the same
// large Manifests set does NOT error when Metal3=false (CAPV path has no
// config-drive budget).
func TestRenderMetal3_SizeGuard_NotEnforcedForNonMetal3(t *testing.T) {
	data := metal3ControlPlaneData()
	data.Metal3 = false
	for i := 0; i < 70; i++ {
		data.Manifests = append(data.Manifests, bootstrapv1beta2.Manifest{
			Name:    "big",
			File:    "manifest.yaml",
			Content: strings.Repeat("# padding line to inflate config-drive size budget\n", 20),
		})
	}

	if _, err := RenderK0sCloudConfig(data); err != nil {
		t.Errorf("k0s: size-guard MUST NOT fire when Metal3=false, got: %v", err)
	}
	if _, err := RenderK3sCloudConfig(data); err != nil {
		t.Errorf("k3s: size-guard MUST NOT fire when Metal3=false, got: %v", err)
	}
}

// TestRenderMetal3_NonMetal3RendersUnchanged is the byte-identity guard:
// with Metal3=false, the (not .Metal3) guards must not alter what the templates
// were already rendering. We assert the existing CAPV invariants continue to
// hold — providerID injection present when ProviderID is set, push block
// present when ManagementEndpoint is set.
func TestRenderMetal3_NonMetal3RendersUnchanged(t *testing.T) {
	for _, dist := range []string{"k0s", "k3s"} {
		dist := dist
		t.Run(dist, func(t *testing.T) {
			data := TemplateData{
				Role:       "control-plane",
				SingleNode: true,
				Hostname:   "capv-cp-0",
				UserName:   "kairos",
				UserGroups: []string{"admin"},
				Metal3:     false,
				ProviderID: "vsphere://422fa74a-5d60-3a4a-af24-1f07be515fcc",
				ManagementEndpoint: &ManagementEndpoint{
					Token:                     "tok",
					KubeconfigSecretName:      "kc",
					KubeconfigSecretNamespace: "default",
					APIServer:                 "https://mgmt.example.com:6443",
					ClusterName:               "capv-cluster",
					ControlPlaneEndpointHost:  "10.0.0.5",
				},
			}
			var (
				result string
				err    error
			)
			if dist == "k0s" {
				result, err = RenderK0sCloudConfig(data)
			} else {
				result, err = RenderK3sCloudConfig(data)
			}
			if err != nil {
				t.Fatalf("render non-Metal3 CAPV (%s): %v", dist, err)
			}

			// Non-Metal3: providerID injection must still be present.
			if !strings.Contains(result, "provider-id=vsphere://422fa74a-5d60-3a4a-af24-1f07be515fcc") {
				t.Errorf("%s: non-Metal3 render lost provider-id injection", dist)
			}
			// Non-Metal3: push_kubeconfig must still be present.
			if !strings.Contains(result, "push_kubeconfig") {
				t.Errorf("%s: non-Metal3 render lost push_kubeconfig block", dist)
			}
			// Non-Metal3: Metal3 label stage must NOT appear.
			if strings.Contains(result, "set_metal3_uuid_label") {
				t.Errorf("%s: non-Metal3 render must NOT contain Metal3 label stage", dist)
			}
		})
	}
}

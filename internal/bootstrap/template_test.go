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
	"strings"
	"testing"

	bootstrapv1beta2 "github.com/kairos-io/kairos-capi/api/bootstrap/v1beta2"
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
	if !strings.Contains(result, "systemctl enable kairos-k3s-post-bootstrap.service") {
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
	if strings.Contains(result, "Push kubeconfig to management cluster without SSH") {
		t.Error("CAPV template should not include KubeVirt kubeconfig push")
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

	// Check for install.device
	if !strings.Contains(result, "device: \"auto\"") {
		t.Error("Missing install.device: \"auto\"")
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

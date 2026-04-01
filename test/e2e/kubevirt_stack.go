/*
Copyright 2026 The Kairos CAPI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR ANY KIND, either express or implied. See the
License for the specific language governing permissions and limitations
under the License.
*/

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// Versions and manifest URLs aligned with cmd/kubevirt-env (adjust together when bumping).
const (
	kubeVirtE2ECalicoVersion        = "v3.29.1"
	kubeVirtE2ECalicoManifestURL    = "https://raw.githubusercontent.com/projectcalico/calico/%s/manifests/calico.yaml"
	kubeVirtE2ELocalPathProvisioner = "https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.28/deploy/local-path-storage.yaml"
	kubeVirtE2ECDIOperatorURL       = "https://github.com/kubevirt/containerized-data-importer/releases/latest/download/cdi-operator.yaml"
	kubeVirtE2ECDICRURL             = "https://github.com/kubevirt/containerized-data-importer/releases/latest/download/cdi-cr.yaml"
	kubeVirtE2EKubeVirtVersion      = "v1.3.0"
	kubeVirtE2EKubeVirtOperatorURL  = "https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-operator.yaml"
	kubeVirtE2EKubeVirtCRURL        = "https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-cr.yaml"
	kubeVirtE2ECertManagerVersion   = "v1.16.2"
	kubeVirtE2ECertManagerURL       = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"
)

// EnsureDockerConfigJSON returns ~/.docker/config.json, creating the file with "{}" if missing (avoids kind mount issues).
func EnsureDockerConfigJSON() (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("user.Current: %w", err)
	}
	p := filepath.Join(usr.HomeDir, ".docker", "config.json")
	if _, err := os.Stat(p); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			return "", err
		}
	}
	return p, nil
}

// WriteKindClusterConfigKubeVirt writes a kind config with default CNI disabled (Calico applied separately)
// and mounts the Docker config into the node for registry auth (same pattern as kubevirt-env).
// Cluster name comes from kind create --name, not from this file.
func WriteKindClusterConfigKubeVirt(destPath, hostDockerConfig string) error {
	content := fmt.Sprintf(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
nodes:
- role: control-plane
  extraMounts:
  - containerPath: /var/lib/kubelet/config.json
    hostPath: %s
`, hostDockerConfig)
	return os.WriteFile(destPath, []byte(content), 0o644)
}

// KubectlApplyURL runs kubectl apply -f <url>.
func KubectlApplyURL(ctx context.Context, tools Tools, kubeconfig, url string) error {
	cmd := tools.KubectlWithKubeconfig(ctx, kubeconfig, "apply", "-f", url)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply -f %q: %w\n%s", url, err, out.String())
	}
	return nil
}

// KubectlWaitCondition runs kubectl wait with a timeout (e.g. args = deployment/foo -n bar --for=condition=Available).
func KubectlWait(ctx context.Context, tools Tools, kubeconfig, timeout string, args ...string) error {
	full := append([]string{"wait", "--timeout=" + timeout}, args...)
	cmd := tools.KubectlWithKubeconfig(ctx, kubeconfig, full...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl %s: %w\n%s", strings.Join(full, " "), err, out.String())
	}
	return nil
}

// KubectlPatchKubeVirtUseEmulation merges useEmulation: true (needed on hosts without /dev/kvm), unless KUBEVIRT_USE_EMULATION is "false".
func KubectlPatchKubeVirtUseEmulation(ctx context.Context, tools Tools, kubeconfig string) error {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("KUBEVIRT_USE_EMULATION")))
	if v == "false" || v == "0" || v == "no" {
		return nil
	}
	patch := `{"spec":{"configuration":{"developerConfiguration":{"useEmulation":true}}}}`
	cmd := tools.KubectlWithKubeconfig(ctx, kubeconfig,
		"patch", "kubevirts.kubevirt.io", "kubevirt", "-n", "kubevirt", "--type=merge", "-p", patch)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl patch kubevirt useEmulation: %w\n%s", err, out.String())
	}
	return nil
}

// KubectlMarkLocalPathDefaultStorageClass sets local-path as the default StorageClass if none is set.
func KubectlMarkLocalPathDefaultStorageClass(ctx context.Context, tools Tools, kubeconfig string) error {
	patch := `{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}`
	cmd := tools.KubectlWithKubeconfig(ctx, kubeconfig, "patch", "storageclass", "local-path", "--type=merge", "-p", patch)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl patch storageclass local-path: %w\n%s", err, out.String())
	}
	return nil
}

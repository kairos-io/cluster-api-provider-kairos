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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ClusterctlPinnedVersion returns the clusterctl release version from the binary catalog (e.g. v1.12.4).
func ClusterctlPinnedVersion() (string, error) {
	dep, ok := E2EBinaryCatalog["clusterctl"]
	if !ok {
		return "", fmt.Errorf("e2e catalog: no clusterctl entry")
	}
	arch := runtime.GOARCH
	art, ok := dep.Arches[arch]
	if !ok {
		return "", fmt.Errorf("e2e catalog: clusterctl missing GOARCH %q", arch)
	}
	return art.Version, nil
}

// KubectlAPIHealth hits the management API /version via kubectl.
func KubectlAPIHealth(ctx context.Context, tools Tools, kubeconfig string) error {
	cmd := tools.KubectlWithKubeconfig(ctx, kubeconfig, "get", "--raw", "/version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl get --raw /version: %w: %s", err, out.String())
	}
	return nil
}

// ClusterctlInitKubeadmCore installs Cluster API core with kubeadm bootstrap and control-plane providers (no infrastructure provider).
func ClusterctlInitKubeadmCore(ctx context.Context, tools Tools, kubeconfig, capiVersion string) error {
	args := []string{
		"init",
		"--kubeconfig", kubeconfig,
		"--core", "cluster-api:" + capiVersion,
		"--bootstrap", "kubeadm:" + capiVersion,
		"--control-plane", "kubeadm:" + capiVersion,
	}
	cmd := tools.Clusterctl(ctx, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clusterctl %v: %w\n%s", args, err, out.String())
	}
	return nil
}

// ClusterctlInitKubeVirtInfra adds the KubeVirt infrastructure (CAPK) via a second clusterctl init.
// Override the infra bundle with E2E_CLUSTERCTL_INFRA (default "kubevirt", or "kubevirt:vX.Y.Z").
func ClusterctlInitKubeVirtInfra(ctx context.Context, tools Tools, kubeconfig string) error {
	infra := strings.TrimSpace(os.Getenv("E2E_CLUSTERCTL_INFRA"))
	if infra == "" {
		infra = "kubevirt"
	}
	args := []string{"init", "--kubeconfig", kubeconfig, "--infrastructure", infra}
	cmd := tools.Clusterctl(ctx, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clusterctl %v: %w\n%s", args, err, out.String())
	}
	return nil
}

// ResolveKustomize returns ./bin/kustomize when present, otherwise kustomize from PATH.
func ResolveKustomize(repoRoot string) (string, error) {
	local := filepath.Join(repoRoot, "bin", "kustomize")
	if _, err := os.Stat(local); err == nil {
		return local, nil
	}
	path, err := exec.LookPath("kustomize")
	if err != nil {
		return "", fmt.Errorf("kustomize not found under %q or PATH (run make kustomize)", local)
	}
	return path, nil
}

// ApplyKairosCapiDevManifests runs kustomize build config/dev | kubectl apply -f - against kubeconfig.
func ApplyKairosCapiDevManifests(ctx context.Context, tools Tools, repoRoot, kubeconfig string) error {
	kust, err := ResolveKustomize(repoRoot)
	if err != nil {
		return err
	}
	kdir := filepath.Join(repoRoot, "config", "dev")
	build := exec.CommandContext(ctx, kust, "build", kdir)
	var manifest bytes.Buffer
	var berr bytes.Buffer
	build.Stdout = &manifest
	build.Stderr = &berr
	if err := build.Run(); err != nil {
		return fmt.Errorf("kustomize build %s: %w: %s", kdir, err, berr.String())
	}
	apply := tools.KubectlWithKubeconfig(ctx, kubeconfig, "apply", "-f", "-")
	apply.Stdin = &manifest
	var out bytes.Buffer
	apply.Stdout = &out
	apply.Stderr = &out
	if err := apply.Run(); err != nil {
		return fmt.Errorf("kubectl apply (config/dev): %w: %s", err, out.String())
	}
	return nil
}

// KubectlRolloutDeployment waits for a deployment rollout in namespace.
func KubectlRolloutDeployment(ctx context.Context, tools Tools, kubeconfig, namespace, deployment, timeout string) error {
	args := []string{
		"-n", namespace, "rollout", "status", "deployment/" + deployment, "--timeout=" + timeout,
	}
	cmd := tools.KubectlWithKubeconfig(ctx, kubeconfig, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl rollout status: %w: %s", err, out.String())
	}
	return nil
}

// KubectlRolloutDaemonSet waits for a daemonset rollout.
func KubectlRolloutDaemonSet(ctx context.Context, tools Tools, kubeconfig, namespace, name, timeout string) error {
	args := []string{
		"-n", namespace, "rollout", "status", "daemonset/" + name, "--timeout=" + timeout,
	}
	cmd := tools.KubectlWithKubeconfig(ctx, kubeconfig, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl rollout status daemonset: %w: %s", err, out.String())
	}
	return nil
}

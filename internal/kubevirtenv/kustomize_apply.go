package kubevirtenv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// DevControllerImageRef is the image tag used by config/dev kustomization (must be loaded into kind).
const DevControllerImageRef = "controller:latest"

// KairosCAPIReleaseImage is the default IMG for make docker-build in full setup.
const KairosCAPIReleaseImage = "ghcr.io/kairos-io/kairos-capi:latest"

// ResolveKustomize returns repoRoot/bin/kustomize when present, otherwise kustomize from PATH.
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

// ApplyDevKustomize runs kustomize build config/dev | kubectl apply -f -.
func (e *Environment) ApplyDevKustomize(ctx context.Context) error {
	if e.RepoRoot == "" {
		return fmt.Errorf("Environment.RepoRoot is required")
	}
	kust, err := ResolveKustomize(e.RepoRoot)
	if err != nil {
		return err
	}
	kdir := filepath.Join(e.RepoRoot, "config", "dev")
	build := exec.CommandContext(ctx, kust, "build", kdir)
	var manifest bytes.Buffer
	var berr bytes.Buffer
	build.Stdout = &manifest
	build.Stderr = &berr
	if err := build.Run(); err != nil {
		return fmt.Errorf("kustomize build %s: %w: %s", kdir, err, berr.String())
	}
	apply := e.KubectlWithKubeconfig(ctx, e.KubeconfigPath(), "apply", "-f", "-")
	apply.Stdin = &manifest
	var out bytes.Buffer
	apply.Stdout = &out
	apply.Stderr = &out
	if err := apply.Run(); err != nil {
		return fmt.Errorf("kubectl apply (config/dev): %w: %s", err, out.String())
	}
	return nil
}

// KubectlWithKubeconfig runs kubectl with KUBECONFIG set.
func (e *Environment) KubectlWithKubeconfig(ctx context.Context, kubeconfig string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, e.kubectlBin(), args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	return cmd
}

// KubectlApplyKustomizeDir runs kubectl apply -k on a path under RepoRoot (e.g. "config/namespace").
func (e *Environment) KubectlApplyKustomizeDir(ctx context.Context, dirUnderRepo string) error {
	if e.RepoRoot == "" {
		return fmt.Errorf("Environment.RepoRoot is required")
	}
	dir := filepath.Join(e.RepoRoot, filepath.FromSlash(dirUnderRepo))
	args := []string{"apply", "-k", dir, "--kubeconfig", e.KubeconfigPath(), "--context", e.KubectlContext()}
	cmd := exec.CommandContext(ctx, e.kubectlBin(), args...)
	stdout, stderr := e.execOut()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply -k %s: %w", dirUnderRepo, err)
	}
	return nil
}

// KubectlApplyKustomizeURL runs kubectl apply -k on a remote base (e.g. GitHub URL with ?ref= tag).
func (e *Environment) KubectlApplyKustomizeURL(ctx context.Context, url string, extraArgs ...string) error {
	args := append([]string{"apply", "-k", url, "--kubeconfig", e.KubeconfigPath(), "--context", e.KubectlContext()}, extraArgs...)
	cmd := exec.CommandContext(ctx, e.kubectlBin(), args...)
	stdout, stderr := e.execOut()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (url %s)", err, url)
	}
	return nil
}

// KubectlRolloutDeployment waits for a deployment rollout.
func (e *Environment) KubectlRolloutDeployment(ctx context.Context, kubeconfig, namespace, deployment, timeout string) error {
	args := []string{
		"-n", namespace, "rollout", "status", "deployment/" + deployment, "--timeout=" + timeout,
	}
	cmd := e.KubectlWithKubeconfig(ctx, kubeconfig, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl rollout status: %w: %s", err, out.String())
	}
	return nil
}

// KubectlAPIHealth hits the API /version via kubectl.
func (e *Environment) KubectlAPIHealth(ctx context.Context, kubeconfig string) error {
	cmd := e.KubectlWithKubeconfig(ctx, kubeconfig, "get", "--raw", "/version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl get --raw /version: %w: %s", err, out.String())
	}
	return nil
}

// DiscardWriter is an io.Writer that discards input (for tests).
var DiscardWriter io.Writer = io.Discard

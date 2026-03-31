package kubevirtenv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

// EnsureDockerConfigJSON returns ~/.docker/config.json, creating "{}" if missing (kind mount / registry auth).
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

// WriteKindClusterConfig writes a kind v1alpha4 config: no default CNI, docker config mount (cluster name from kind create --name).
func WriteKindClusterConfig(destPath, hostDockerConfig string) error {
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

func kubeconfigFileUsable(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Size() > 0
}

// kindSubprocessEnviron returns the process environment without KUBECONFIG so kind does not merge
// cluster kubeconfig into an unrelated file when using --kubeconfig.
func kindSubprocessEnviron() []string {
	env := os.Environ()
	var out []string
	const prefix = "KUBECONFIG="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (e *Environment) kindClusterListed() bool {
	kindCmd := exec.Command(e.kindBin(), "get", "clusters")
	kindCmd.Env = kindSubprocessEnviron()
	output, err := kindCmd.Output()
	if err != nil {
		return false
	}
	clusters := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range clusters {
		if strings.TrimSpace(line) == e.ClusterName {
			return true
		}
	}
	return false
}

func (e *Environment) absKubeconfigPath() (string, error) {
	workDirAbs, err := filepath.Abs(e.WorkDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(workDirAbs, "kubeconfig"), nil
}

// FetchKindKubeconfig writes `kind get kubeconfig` for this cluster to dest (parent dirs created).
func (e *Environment) FetchKindKubeconfig(ctx context.Context, dest string) error {
	getCfg := exec.CommandContext(ctx, e.kindBin(), "get", "kubeconfig", "--name", e.ClusterName)
	getCfg.Env = kindSubprocessEnviron()
	out, err := getCfg.CombinedOutput()
	if err != nil {
		s := strings.TrimSpace(string(out))
		return fmt.Errorf("kind get kubeconfig --name %q: %w%s", e.ClusterName, err, formatKindErrOutput(s))
	}
	body := string(out)
	if strings.TrimSpace(body) == "" || !strings.Contains(body, "apiVersion:") {
		return fmt.Errorf("kind get kubeconfig --name %q: empty or invalid kubeconfig output", e.ClusterName)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("kubeconfig dir: %w", err)
	}
	if err := os.WriteFile(dest, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}
	return nil
}

func formatKindErrOutput(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

// syncKubeconfigWithRetry runs FetchKindKubeconfig until it succeeds or ctx times out (kind may need a moment after create).
func (e *Environment) syncKubeconfigWithRetry(ctx context.Context, dest string) error {
	log := e.log()
	log.Infof("Syncing kubeconfig to %s (kind get kubeconfig, retrying until available)...", dest)
	retryCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var lastErr error
	err := wait.PollUntilContextCancel(retryCtx, 2*time.Second, true, func(pollCtx context.Context) (bool, error) {
		lastErr = e.FetchKindKubeconfig(pollCtx, dest)
		return lastErr == nil, nil
	})
	if err != nil {
		if lastErr != nil {
			return fmt.Errorf("sync kubeconfig: %w (last: %v)", err, lastErr)
		}
		return fmt.Errorf("sync kubeconfig: %w", err)
	}
	return nil
}

// waitForKubeAPIServer polls kubectl /readyz until success or timeout (KindCreateWait, default 15m).
func (e *Environment) waitForKubeAPIServer(ctx context.Context, kubeconfig string) error {
	log := e.log()
	waitStr := e.KindCreateWait
	if waitStr == "" {
		waitStr = "15m"
	}
	d, err := time.ParseDuration(waitStr)
	if err != nil {
		d = 15 * time.Minute
	}
	log.Infof("Waiting for Kubernetes API /readyz (timeout %s, context %s)...", waitStr, e.KubectlContext())
	waitCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return wait.PollUntilContextCancel(waitCtx, 2*time.Second, true, func(pollCtx context.Context) (bool, error) {
		args := []string{
			"get", "--raw", "/readyz",
			"--kubeconfig", kubeconfig,
			"--context", e.KubectlContext(),
			"--request-timeout=5s",
		}
		cmd := exec.CommandContext(pollCtx, e.kubectlBin(), args...)
		if err := cmd.Run(); err != nil {
			return false, nil
		}
		return true, nil
	})
}

// IsClusterReady reports whether the kind cluster exists and a non-empty kubeconfig exists under WorkDir.
func (e *Environment) IsClusterReady() bool {
	if !e.kindClusterListed() {
		return false
	}
	kc, err := e.absKubeconfigPath()
	if err != nil {
		return false
	}
	return kubeconfigFileUsable(kc)
}

// CreateTestCluster creates a kind cluster with Calico-ready networking and writes kubeconfig to WorkDir.
func (e *Environment) CreateTestCluster(ctx context.Context) error {
	log := e.log()
	workDirAbs, err := filepath.Abs(e.WorkDir)
	if err != nil {
		return fmt.Errorf("work directory: %w", err)
	}
	e.WorkDir = workDirAbs
	if err := os.MkdirAll(e.WorkDir, 0o755); err != nil {
		return fmt.Errorf("work directory: %w", err)
	}
	kubeconfigAbs := filepath.Join(e.WorkDir, "kubeconfig")

	listed := e.kindClusterListed()
	usable := kubeconfigFileUsable(kubeconfigAbs)

	if listed && usable {
		log.Infof("Cluster %q already exists and kubeconfig ready at %s ✓", e.ClusterName, kubeconfigAbs)
		return nil
	}

	if listed && !usable {
		log.Infof("Cluster %q exists but kubeconfig missing or empty at %s — writing kubeconfig from kind", e.ClusterName, kubeconfigAbs)
		if err := e.FetchKindKubeconfig(ctx, kubeconfigAbs); err != nil {
			return err
		}
		if err := e.kubectlClusterSanity(ctx, kubeconfigAbs); err != nil {
			return err
		}
		log.Step("Kubeconfig synced from kind ✓")
		log.Infof("Note: default CNI is disabled; install Calico before local-path and other pod workloads.")
		return nil
	}

	dockerConfigPath, err := EnsureDockerConfigJSON()
	if err != nil {
		return err
	}
	kindConfigPath := filepath.Join(e.WorkDir, "kind-config.yaml")
	if err := WriteKindClusterConfig(kindConfigPath, dockerConfigPath); err != nil {
		return fmt.Errorf("kind config: %w", err)
	}
	log.Infof("Kind config created with Docker config mount: %s", dockerConfigPath)

	// Use --wait 0s so kind returns as soon as its local steps finish; we sync kubeconfig immediately
	// via `kind get kubeconfig` and enforce readiness with waitForKubeAPIServer (KindCreateWait).
	apiWait := e.KindCreateWait
	if apiWait == "" {
		apiWait = "15m"
	}
	args := []string{"create", "cluster", "--name", e.ClusterName, "--config", kindConfigPath, "--kubeconfig", kubeconfigAbs, "--wait", "0s"}
	log.Infof("Creating kind cluster %q (kind --wait 0s; kubeconfig at %s will be synced next, then we wait for API up to %s).",
		e.ClusterName, kubeconfigAbs, apiWait)
	kindCmd := exec.CommandContext(ctx, e.kindBin(), args...)
	kindCmd.Env = kindSubprocessEnviron()
	stdout, stderr := e.execOut()
	kindCmd.Stdout = stdout
	kindCmd.Stderr = stderr
	if err := kindCmd.Run(); err != nil {
		return fmt.Errorf("kind create cluster: %w", err)
	}

	if err := e.syncKubeconfigWithRetry(ctx, kubeconfigAbs); err != nil {
		return fmt.Errorf("after kind create: %w", err)
	}
	log.Infof("Kubeconfig written to %s (you can use it while the next step waits for the API).", kubeconfigAbs)

	if err := e.waitForKubeAPIServer(ctx, kubeconfigAbs); err != nil {
		return fmt.Errorf("wait for Kubernetes API: %w", err)
	}

	if err := e.kubectlClusterSanity(ctx, kubeconfigAbs); err != nil {
		return err
	}
	log.Step("Kind cluster created ✓")
	log.Infof("Note: default CNI is disabled; install Calico before local-path and other pod workloads.")
	return nil
}

func (e *Environment) kubectlClusterSanity(ctx context.Context, kubeconfigPath string) error {
	log := e.log()
	stdout, stderr := e.execOut()
	log.Infof("Showing cluster info for context %s...", e.KubectlContext())
	kubectlCmd := exec.CommandContext(ctx, e.kubectlBin(), "cluster-info", "--context", e.KubectlContext(), "--kubeconfig", kubeconfigPath)
	kubectlCmd.Stdout = stdout
	kubectlCmd.Stderr = stderr
	if err := kubectlCmd.Run(); err != nil {
		return fmt.Errorf("kubectl cluster-info: %w", err)
	}
	return nil
}

// DeleteKindCluster deletes the kind cluster (best-effort logging on failure).
func (e *Environment) DeleteKindCluster(ctx context.Context) error {
	log := e.log()
	kindCmd := exec.CommandContext(ctx, e.kindBin(), "delete", "cluster", "--name", e.ClusterName)
	kindCmd.Env = kindSubprocessEnviron()
	stdout, stderr := e.execOut()
	kindCmd.Stdout = stdout
	kindCmd.Stderr = stderr
	if err := kindCmd.Run(); err != nil {
		log.Warnf("delete kind cluster: %v", err)
		return err
	}
	log.Infof("Kind cluster %q deleted ✓", e.ClusterName)
	return nil
}

// RemoveWorkDir removes WorkDir (cleanup helper).
func (e *Environment) RemoveWorkDir() error {
	log := e.log()
	if e.WorkDir == "" || e.WorkDir == "." || e.WorkDir == "/" {
		return fmt.Errorf("refusing to remove unsafe work dir %q", e.WorkDir)
	}
	if err := os.RemoveAll(e.WorkDir); err != nil {
		log.Warnf("remove work directory %s: %v", e.WorkDir, err)
		return err
	}
	log.Infof("Work directory %s removed ✓", e.WorkDir)
	return nil
}

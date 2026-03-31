package kubevirtenv

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	// DefaultCAPIVersion is a last-resort cluster-api version if the tool catalog cannot be read.
	DefaultCAPIVersion = "v1.12.4"
)

// Environment holds paths and options shared by install steps (kind cluster, kubeconfig, tool binaries, logging).
type Environment struct {
	ClusterName string
	WorkDir     string
	Logger      Logger

	// RepoRoot is the module root (go.mod) for make, kustomize, and docker build.
	RepoRoot string

	// DockerExe is the docker binary for image builds (empty = "docker").
	DockerExe string

	// Optional tool paths (empty = look up on PATH after EnsurePinnedCLIs, or bare name before).
	KindPath       string
	KubectlPath    string
	ClusterctlPath string
	VirtctlPath    string

	// KindCreateWait is passed to kind create as --wait (e.g. "15m"). Empty uses internal default.
	KindCreateWait string

	// ClusterctlExtraPath is prepended to PATH for clusterctl (e.g. repo ./bin for provider plugins).
	ClusterctlExtraPath string

	// CAPIVersion is used for clusterctl init core/bootstrap/control-plane (filled from catalog by EnsurePinnedCLIs if empty).
	CAPIVersion string

	// CAPKInfra is the --infrastructure argument (e.g. "kubevirt", "kubevirt:v1.2.3"). Empty defaults to kubevirt / CAPK_VERSION env in InstallCAPK.
	CAPKInfra string

	// Stdout/Stderr for kind, kubectl, clusterctl child processes (nil = os.Stdout / os.Stderr).
	Stdout io.Writer
	Stderr io.Writer
}

// KubeconfigPath returns the kubeconfig file path under WorkDir.
func (e *Environment) KubeconfigPath() string {
	return filepath.Join(e.WorkDir, "kubeconfig")
}

// KubectlContext returns the kind context name for this cluster.
func (e *Environment) KubectlContext() string {
	return fmt.Sprintf("kind-%s", e.ClusterName)
}

// BinDir is WorkDir/bin (pinned kubectl, kind, clusterctl, virtctl).
func (e *Environment) BinDir() string {
	return filepath.Join(e.WorkDir, "bin")
}

// EnsurePinnedCLIs downloads kubectl, kind, clusterctl, and virtctl into WorkDir/bin and sets *Path fields.
func (e *Environment) EnsurePinnedCLIs(ctx context.Context) error {
	if e.WorkDir == "" {
		return fmt.Errorf("kubevirtenv.Environment: WorkDir is required")
	}
	abs, err := filepath.Abs(e.WorkDir)
	if err != nil {
		return fmt.Errorf("workdir: %w", err)
	}
	e.WorkDir = abs
	if err := os.MkdirAll(e.WorkDir, 0o755); err != nil {
		return fmt.Errorf("workdir: %w", err)
	}
	bin := e.BinDir()
	if err := EnsureBinariesInDir(ctx, bin, nil, nil, e.log()); err != nil {
		return fmt.Errorf("ensure CLIs: %w", err)
	}
	e.KindPath = filepath.Join(bin, "kind")
	e.KubectlPath = filepath.Join(bin, "kubectl")
	e.ClusterctlPath = filepath.Join(bin, "clusterctl")
	e.VirtctlPath = filepath.Join(bin, "virtctl")
	if e.CAPIVersion == "" {
		v, err := ClusterctlPinnedVersion()
		if err != nil {
			return err
		}
		e.CAPIVersion = v
	}
	return nil
}

// RequireKubeconfig returns an error if the workdir kubeconfig is missing.
func (e *Environment) RequireKubeconfig() error {
	path := e.KubeconfigPath()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("kubeconfig not found at %s — create the kind cluster first", path)
		}
		return fmt.Errorf("kubeconfig %s: %w", path, err)
	}
	return nil
}

// HelmEnviron returns os.Environ with KUBECONFIG set to this environment's kubeconfig path.
func (e *Environment) HelmEnviron() []string {
	return append(os.Environ(), "KUBECONFIG="+e.KubeconfigPath())
}

// HelmCommand builds a helm subprocess using HelmEnviron (targets the same cluster as client-go).
func (e *Environment) HelmCommand(args ...string) *exec.Cmd {
	c := exec.Command("helm", args...)
	c.Env = e.HelmEnviron()
	return c
}

func (e *Environment) kindBin() string {
	if e.KindPath != "" {
		return e.KindPath
	}
	return "kind"
}

func (e *Environment) kubectlBin() string {
	if e.KubectlPath != "" {
		return e.KubectlPath
	}
	return "kubectl"
}

func (e *Environment) clusterctlBin() string {
	if e.ClusterctlPath != "" {
		return e.ClusterctlPath
	}
	return "clusterctl"
}

func (e *Environment) virtctlBin() string {
	if e.VirtctlPath != "" {
		return e.VirtctlPath
	}
	return "virtctl"
}

func (e *Environment) execOut() (io.Writer, io.Writer) {
	out, err := e.Stdout, e.Stderr
	if out == nil {
		out = os.Stdout
	}
	if err == nil {
		err = os.Stderr
	}
	return out, err
}

func (e *Environment) clusterctlEnv() []string {
	env := os.Environ()
	if e.ClusterctlExtraPath == "" {
		return env
	}
	p := os.Getenv("PATH")
	if p != "" {
		p = e.ClusterctlExtraPath + string(filepath.ListSeparator) + p
	} else {
		p = e.ClusterctlExtraPath
	}
	return append(env, "PATH="+p)
}

func (e *Environment) log() Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return StdLogger{}
}

// ValidateKindInstalled returns an error if kind is not available (empty KindPath uses PATH).
func ValidateKindInstalled(e *Environment) error {
	bin := e.kindBin()
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("required command %q not found in PATH", bin)
	}
	return nil
}

// ValidateClusterctlInstalled returns an error if clusterctl is not available.
func ValidateClusterctlInstalled(e *Environment) error {
	bin := e.clusterctlBin()
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("required command %q not found in PATH", bin)
	}
	return nil
}

func (e *Environment) capiVersionOrDefault() string {
	if e.CAPIVersion != "" {
		return e.CAPIVersion
	}
	if v, err := ClusterctlPinnedVersion(); err == nil {
		return v
	}
	return DefaultCAPIVersion
}

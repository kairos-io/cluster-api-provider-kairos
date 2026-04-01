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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// BinaryArchArtifact is everything needed to fetch one OS-architecture build of a tool.
// URLTemplate is formatted with fmt.Sprintf(template, version, goarch) — use %[1]s for Version, %[2]s for GOARCH.
type BinaryArchArtifact struct {
	Version     string
	URLTemplate string
	SHA256      string
}

// ResolvedURL expands URLTemplate for this artifact and the given GOARCH (must match the catalog map key).
func (a BinaryArchArtifact) ResolvedURL(goarch string) string {
	return fmt.Sprintf(a.URLTemplate, a.Version, goarch)
}

// Validate checks fields and that the resolved URL mentions version and goarch (catches template drift).
func (a BinaryArchArtifact) Validate(toolName, goarch string) error {
	if strings.TrimSpace(a.Version) == "" {
		return fmt.Errorf("%s %s: empty Version", toolName, goarch)
	}
	if strings.TrimSpace(a.URLTemplate) == "" {
		return fmt.Errorf("%s %s: empty URLTemplate", toolName, goarch)
	}
	if err := validateSHA256Hex(a.SHA256); err != nil {
		return fmt.Errorf("%s %s: %w", toolName, goarch, err)
	}
	u := a.ResolvedURL(goarch)
	if !strings.Contains(u, a.Version) {
		return fmt.Errorf("%s %s: resolved URL %q must contain version %q", toolName, goarch, u, a.Version)
	}
	if !strings.Contains(u, goarch) {
		return fmt.Errorf("%s %s: resolved URL %q must contain goarch %q", toolName, goarch, u, goarch)
	}
	return nil
}

// BinaryDependency describes one CLI (e.g. kubectl): filesystem name plus per-GOARCH artifacts.
type BinaryDependency struct {
	Name   string
	Arches map[string]BinaryArchArtifact
}

// Validate checks Name and every listed architecture.
func (d BinaryDependency) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("binary dependency: empty Name")
	}
	if len(d.Arches) == 0 {
		return fmt.Errorf("%s: no architectures defined", d.Name)
	}
	for goarch, art := range d.Arches {
		if err := art.Validate(d.Name, goarch); err != nil {
			return err
		}
	}
	return nil
}

// BinarySpec is the slim shape used by EnsureBinaries (resolved URL + checksum).
type BinarySpec struct {
	URL    string
	SHA256 string
}

// SpecForArch returns the download spec for this tool and GOARCH.
func (d BinaryDependency) SpecForArch(goarch string) (BinarySpec, error) {
	art, ok := d.Arches[goarch]
	if !ok {
		return BinarySpec{}, fmt.Errorf("%s: no artifact for GOARCH %q", d.Name, goarch)
	}
	return BinarySpec{
		URL:    art.ResolvedURL(goarch),
		SHA256: art.SHA256,
	}, nil
}

// E2EBinaryCatalog lists pinned e2e CLIs (versions / templates / checksums maintained here).
// Last synced with upstream "latest": kubectl stable.txt, kind + cluster-api GitHub releases API.
var E2EBinaryCatalog = map[string]BinaryDependency{
	"kubectl": {
		Name: "kubectl",
		Arches: map[string]BinaryArchArtifact{
			"amd64": {
				Version:     "v1.35.3",
				URLTemplate: "https://dl.k8s.io/release/%[1]s/bin/linux/%[2]s/kubectl",
				SHA256:      "fd31c7d7129260e608f6faf92d5984c3267ad0b5ead3bced2fe125686e286ad6",
			},
			"arm64": {
				Version:     "v1.35.3",
				URLTemplate: "https://dl.k8s.io/release/%[1]s/bin/linux/%[2]s/kubectl",
				SHA256:      "6f0cd088a82dde5d5807122056069e2fac4ed447cc518efc055547ae46525f14",
			},
		},
	},
	"kind": {
		Name: "kind",
		Arches: map[string]BinaryArchArtifact{
			"amd64": {
				Version:     "v0.31.0",
				URLTemplate: "https://github.com/kubernetes-sigs/kind/releases/download/%[1]s/kind-linux-%[2]s",
				SHA256:      "eb244cbafcc157dff60cf68693c14c9a75c4e6e6fedaf9cd71c58117cb93e3fa",
			},
			"arm64": {
				Version:     "v0.31.0",
				URLTemplate: "https://github.com/kubernetes-sigs/kind/releases/download/%[1]s/kind-linux-%[2]s",
				SHA256:      "8e1014e87c34901cc422a1445866835d1e666f2a61301c27e722bdeab5a1f7e4",
			},
		},
	},
	"clusterctl": {
		Name: "clusterctl",
		Arches: map[string]BinaryArchArtifact{
			"amd64": {
				Version:     "v1.12.4",
				URLTemplate: "https://github.com/kubernetes-sigs/cluster-api/releases/download/%[1]s/clusterctl-linux-%[2]s",
				SHA256:      "ec758f8355b8ee92a5a4463a43d374516d611dd41029fd5e5315eb6f167643e7",
			},
			"arm64": {
				Version:     "v1.12.4",
				URLTemplate: "https://github.com/kubernetes-sigs/cluster-api/releases/download/%[1]s/clusterctl-linux-%[2]s",
				SHA256:      "1260f75a3e936057b94a561c8b21ac6cba2ae75d9f1c2c25ea3ecc876017fbd6",
			},
		},
	},
}

// ValidateE2EBinaryCatalog checks catalog keys match BinaryDependency.Name and every arch entry.
func ValidateE2EBinaryCatalog() error {
	for key, dep := range E2EBinaryCatalog {
		if key != dep.Name {
			return fmt.Errorf("catalog key %q must equal BinaryDependency.Name %q", key, dep.Name)
		}
		if err := dep.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// DefaultBinaryDeps returns kubectl, kind, and clusterctl BinarySpecs for the current GOOS/GOARCH.
// Only linux/amd64 and linux/arm64 are supported.
func DefaultBinaryDeps() (map[string]BinarySpec, error) {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	if goos != "linux" {
		return nil, fmt.Errorf("e2e binaries: unsupported GOOS %q (need linux)", goos)
	}
	if goarch != "amd64" && goarch != "arm64" {
		return nil, fmt.Errorf("e2e binaries: unsupported GOARCH %q", goarch)
	}

	if err := ValidateE2EBinaryCatalog(); err != nil {
		return nil, fmt.Errorf("e2e binary catalog: %w", err)
	}

	out := make(map[string]BinarySpec, len(E2EBinaryCatalog))
	for key, dep := range E2EBinaryCatalog {
		spec, err := dep.SpecForArch(goarch)
		if err != nil {
			return nil, err
		}
		out[key] = spec
	}
	return out, nil
}

func validateSHA256Hex(s string) error {
	s = strings.TrimSpace(s)
	if len(s) != 64 {
		return fmt.Errorf("SHA256 must be 64 hex characters, got len %d", len(s))
	}
	for _, c := range strings.ToLower(s) {
		if c >= '0' && c <= '9' {
			continue
		}
		if c >= 'a' && c <= 'f' {
			continue
		}
		return fmt.Errorf("SHA256 must be hexadecimal")
	}
	return nil
}

// DefaultToolsDir returns E2E_TOOLS_DIR if set, otherwise <repo-root>/.e2e-bin.
func DefaultToolsDir() (string, error) {
	if d := os.Getenv("E2E_TOOLS_DIR"); d != "" {
		return filepath.Clean(d), nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	root, err := findRepoRoot(wd)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, ".e2e-bin"), nil
}

// RepoRoot returns the repository root (directory containing go.mod) starting from the process working directory.
func RepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	return findRepoRoot(wd)
}

func findRepoRoot(start string) (string, error) {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from %q", start)
		}
		dir = parent
	}
}

// EnsureBinaries downloads each named binary into dir when missing or checksum mismatch.
// Existing files with a matching SHA-256 are left unchanged.
func EnsureBinaries(dir string, deps map[string]BinarySpec, client *http.Client) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir tools dir: %w", err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	for name, spec := range deps {
		if err := ensureOne(dir, name, spec, client); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func ensureOne(dir, name string, spec BinarySpec, client *http.Client) error {
	dest := filepath.Join(dir, name)
	if ok, err := fileMatchesSHA256(dest, spec.SHA256); err != nil {
		return err
	} else if ok {
		return nil
	}

	req, err := http.NewRequest(http.MethodGet, spec.URL, nil)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}

	tmp, err := os.CreateTemp(dir, name+".download-*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpPath := tmp.Name()
	closeAndRemove := func() {
		tmp.Close()
		_ = os.Remove(tmpPath)
	}
	hash := sha256.New()
	w := io.MultiWriter(tmp, hash)
	if _, err := io.Copy(w, resp.Body); err != nil {
		closeAndRemove()
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", err)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	want := strings.ToLower(spec.SHA256)
	if got != want {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, want)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func fileMatchesSHA256(path, wantHex string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	got, err := sha256HashReader(f)
	if err != nil {
		return false, err
	}
	return got == strings.ToLower(wantHex), nil
}

func sha256HashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Tools shell-outs to binaries resolved under Dir (typically after EnsureBinaries).
type Tools struct {
	Dir string
}

func (t Tools) path(bin string) string {
	return filepath.Join(t.Dir, bin)
}

// Kubectl returns an exec.Cmd for kubectl with args (Dir left unset).
func (t Tools) Kubectl(ctx context.Context, args ...string) *exec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	return exec.CommandContext(ctx, t.path("kubectl"), args...)
}

// Kind returns an exec.Cmd for kind with args.
func (t Tools) Kind(ctx context.Context, args ...string) *exec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	return exec.CommandContext(ctx, t.path("kind"), args...)
}

// Clusterctl returns an exec.Cmd for clusterctl with args.
func (t Tools) Clusterctl(ctx context.Context, args ...string) *exec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	return exec.CommandContext(ctx, t.path("clusterctl"), args...)
}

// KubectlWithKubeconfig runs kubectl with KUBECONFIG set (other env inherits from the process).
func (t Tools) KubectlWithKubeconfig(ctx context.Context, kubeconfig string, args ...string) *exec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, t.path("kubectl"), args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	return cmd
}

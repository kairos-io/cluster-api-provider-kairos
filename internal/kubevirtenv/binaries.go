package kubevirtenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// BinaryArchArtifact describes one OS/architecture build of a CLI.
type BinaryArchArtifact struct {
	Version     string
	URLTemplate string
	SHA256      string
}

// ResolvedURL expands URLTemplate for this artifact and the given GOARCH.
func (a BinaryArchArtifact) ResolvedURL(goarch string) string {
	return fmt.Sprintf(a.URLTemplate, a.Version, goarch)
}

// Validate checks fields and that the resolved URL mentions version and goarch.
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

// BinaryDependency describes one CLI: filesystem name plus per-GOARCH artifacts.
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

// BinarySpec is the slim shape used by EnsureBinariesInDir (resolved URL + checksum).
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

// ToolBinaryCatalog lists pinned CLIs for kubevirt-env and e2e (versions maintained here).
var ToolBinaryCatalog = map[string]BinaryDependency{
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
	"virtctl": {
		Name: "virtctl",
		Arches: map[string]BinaryArchArtifact{
			"amd64": {
				Version:     KubeVirtVersion,
				URLTemplate: "https://github.com/kubevirt/kubevirt/releases/download/%[1]s/virtctl-%[1]s-linux-%[2]s",
				SHA256:      "c5bc1d0cea095645f3aca4fb86c8e9de27b949f7b06e08873472547596104ab7",
			},
			"arm64": {
				Version:     KubeVirtVersion,
				URLTemplate: "https://github.com/kubevirt/kubevirt/releases/download/%[1]s/virtctl-%[1]s-linux-%[2]s",
				SHA256:      "c209eca93501b193851b816b5be4de40d5ec850faefe4f158d81e5810bddee02",
			},
		},
	},
}

// ValidateToolBinaryCatalog checks catalog keys match BinaryDependency.Name and every arch entry.
func ValidateToolBinaryCatalog() error {
	for key, dep := range ToolBinaryCatalog {
		if key != dep.Name {
			return fmt.Errorf("catalog key %q must equal BinaryDependency.Name %q", key, dep.Name)
		}
		if err := dep.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// DefaultToolSpecs returns kubectl, kind, clusterctl, and virtctl BinarySpecs for the current GOOS/GOARCH.
func DefaultToolSpecs() (map[string]BinarySpec, error) {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	if goos != "linux" {
		return nil, fmt.Errorf("tool binaries: unsupported GOOS %q (need linux)", goos)
	}
	if goarch != "amd64" && goarch != "arm64" {
		return nil, fmt.Errorf("tool binaries: unsupported GOARCH %q", goarch)
	}
	if err := ValidateToolBinaryCatalog(); err != nil {
		return nil, fmt.Errorf("tool binary catalog: %w", err)
	}
	out := make(map[string]BinarySpec, len(ToolBinaryCatalog))
	for key, dep := range ToolBinaryCatalog {
		spec, err := dep.SpecForArch(goarch)
		if err != nil {
			return nil, err
		}
		out[key] = spec
	}
	return out, nil
}

// ClusterctlPinnedVersion returns the clusterctl release version from ToolBinaryCatalog.
func ClusterctlPinnedVersion() (string, error) {
	dep, ok := ToolBinaryCatalog["clusterctl"]
	if !ok {
		return "", fmt.Errorf("tool catalog: no clusterctl entry")
	}
	arch := runtime.GOARCH
	art, ok := dep.Arches[arch]
	if !ok {
		return "", fmt.Errorf("tool catalog: clusterctl missing GOARCH %q", arch)
	}
	return art.Version, nil
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

// FindRepoRoot walks up from start until go.mod is found.
func FindRepoRoot(start string) (string, error) {
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

// EnsureBinariesInDir downloads each named tool into dir when missing or checksum mismatch.
// If want is nil or empty, downloads kubectl, kind, clusterctl, and virtctl.
// If log is non-nil, emits progress (already present vs download/install per tool).
func EnsureBinariesInDir(ctx context.Context, dir string, want []string, client *http.Client, log Logger) error {
	_ = ctx
	all, err := DefaultToolSpecs()
	if err != nil {
		return err
	}
	var specs map[string]BinarySpec
	if len(want) == 0 {
		specs = all
	} else {
		specs = make(map[string]BinarySpec, len(want))
		for _, n := range want {
			s, ok := all[n]
			if !ok {
				return fmt.Errorf("unknown tool %q (want one of kubectl, kind, clusterctl, virtctl)", n)
			}
			specs[n] = s
		}
	}
	return ensureBinaries(dir, specs, client, log)
}

func ensureBinaries(dir string, deps map[string]BinarySpec, client *http.Client, log Logger) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir tools dir: %w", err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	names := make([]string, 0, len(deps))
	for name := range deps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ensureOneBinary(dir, name, deps[name], client, log); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func ensureOneBinary(dir, name string, spec BinarySpec, client *http.Client, log Logger) error {
	dest := filepath.Join(dir, name)
	if ok, err := fileMatchesSHA256(dest, spec.SHA256); err != nil {
		return err
	} else if ok {
		if log != nil {
			log.Infof("%s: already present in %s (checksum OK)", name, dir)
		}
		return nil
	}

	if log != nil {
		log.Infof("Installing %s: downloading pinned release (this may take a while)...", name)
	}

	req, err := http.NewRequest(http.MethodGet, spec.URL, nil)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}

	tmp, err := os.CreateTemp(dir, name+".download-*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpPath := tmp.Name()
	closeAndRemove := func() {
		_ = tmp.Close()
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
	if log != nil {
		log.Infof("%s: installed to %s", name, dest)
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
	defer func() { _ = f.Close() }()
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

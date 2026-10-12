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
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
	kubeadmv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/bootstrap"
)

const (
	kubeadmJoinScriptPath      = "/usr/local/bin/kairos-kubeadm-post-bootstrap.sh"
	kubeadmProviderIDPatchFile = "kubeletconfiguration0+merge.yaml"
)

// kubeadmFlagsAllowedWithConfig mirrors isAllowedFlag in kubeadm's
// cmd/kubeadm/app/apis/kubeadm/validation/validation.go: with --config, kubeadm
// refuses every other flag except these and any "skip-*" flag.
var kubeadmFlagsAllowedWithConfig = map[string]bool{
	"config": true, "ignore-preflight-errors": true, "dry-run": true,
	"kubeconfig": true, "node-name": true, "kubeconfig-dir": true, "upload-certs": true,
	"print-manifest": true, "allow-missing-template-keys": true, "output": true,
	"show-managed-fields": true, "print-join-command": true, "rootfs": true,
	"v": true, "log-file": true, "yes": true,
}

// TestKubeadmJoinCommand runs the rendered join script against a kubeadm stub and
// checks the command line it produces against kubeadm's own rules. With
// KUBEADM_BIN set to a real kubeadm binary, the stub hands the call to it with
// every join phase skipped, which exercises flag parsing and config loading
// without touching the host or the network.
func TestKubeadmJoinCommand(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cases := []struct {
		name         string
		providerID   string
		userPatches  string
		wantPatchDir string
	}{
		{"providerID known", "kubevirt://worker-0", "", defaultKubeadmPatchesDir},
		{"providerID unknown", "", "", defaultKubeadmPatchesDir},
		{"user patches directory", "kubevirt://worker-0", "/etc/kubernetes/user-patches", "/etc/kubernetes/user-patches"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			version := kubeadmTestVersion(t)
			cloudConfig := renderKubeadmWorker(t, version, tc.providerID, tc.userPatches)

			root := t.TempDir()
			script := materializeWriteFiles(t, cloudConfig, root)
			args, joinConfigPath := runJoinScript(t, root, script, version)

			g.Expect(args).NotTo(BeEmpty())
			g.Expect(args[0]).To(Equal("join"))
			for _, a := range args[1:] {
				if !strings.HasPrefix(a, "--") {
					continue
				}
				name := strings.SplitN(strings.TrimPrefix(a, "--"), "=", 2)[0]
				g.Expect(kubeadmFlagsAllowedWithConfig[name] || strings.HasPrefix(name, "skip-")).
					To(BeTrue(), "kubeadm refuses --%s together with --config", name)
			}

			raw, err := os.ReadFile(joinConfigPath)
			g.Expect(err).NotTo(HaveOccurred())
			var jc struct {
				Patches struct {
					Directory string `yaml:"directory"`
				} `yaml:"patches"`
			}
			g.Expect(yaml.Unmarshal(raw, &jc)).To(Succeed())
			g.Expect(jc.Patches.Directory).To(Equal(root + tc.wantPatchDir))
			info, err := os.Stat(jc.Patches.Directory)
			g.Expect(err).NotTo(HaveOccurred(), "kubeadm fails when the patches directory is missing")
			g.Expect(info.IsDir()).To(BeTrue())

			patch := filepath.Join(jc.Patches.Directory, kubeadmProviderIDPatchFile)
			if tc.providerID == "" {
				g.Expect(patch).NotTo(BeAnExistingFile())
			} else {
				content, err := os.ReadFile(patch)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(string(content)).To(ContainSubstring(tc.providerID))
			}
			g.Expect(filepath.Join(root, "/var/lib/kairos/kairos-kubeadm-join.completed")).To(BeAnExistingFile())

			if bin := os.Getenv("KUBEADM_BIN"); bin != "" {
				out, err := exec.Command(bin, "config", "validate", "--config", joinConfigPath).CombinedOutput()
				g.Expect(err).NotTo(HaveOccurred(), "kubeadm config validate: %s", out)
			}
		})
	}
}

// kubeadmTestVersion is the Machine version to render for: the version of
// KUBEADM_BIN when set, since the JoinConfiguration API version follows it.
func kubeadmTestVersion(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("KUBEADM_BIN")
	if bin == "" {
		return "v1.30.0"
	}
	out, err := exec.Command(bin, "version", "--output", "short").Output()
	if err != nil {
		t.Fatalf("%s version: %v", bin, err)
	}
	return strings.TrimSpace(string(out))
}

func renderKubeadmWorker(t *testing.T, version, providerID, userPatches string) string {
	t.Helper()
	src, req, _ := buildSource(t, defaultFixture(genTestCA(t, "cluster-ca")), nil)
	req.Machine.Spec.Version = version
	req.Config.Spec.Kubeadm = &bootstrapv1beta2.KubeadmConfig{
		JoinConfiguration: &kubeadmv1.JoinConfiguration{
			// A fixed socket, so a real kubeadm does not probe the host's CRIs and
			// fail with "found multiple CRI endpoints" on a machine that has more
			// than one.
			NodeRegistration: kubeadmv1.NodeRegistrationOptions{CRISocket: "unix:///run/containerd/containerd.sock"},
			Patches:          kubeadmv1.Patches{Directory: userPatches},
		},
	}
	mat, err := src.WorkerJoin(context.Background(), req)
	if err != nil {
		t.Fatalf("WorkerJoin: %v", err)
	}
	td := bootstrap.TemplateData{
		Role:       "worker",
		UserName:   "kairos",
		UserGroups: []string{"admin"},
		GitHubUser: "testuser",
		ProviderID: providerID,
	}
	bootstrapDistributions[bootstrapv1beta2.DistributionKubeadm].fillTemplateData(&td, req.Config, "", mat)
	out, err := bootstrap.Render(bootstrapv1beta2.DistributionKubeadm, td)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return out
}

// materializeWriteFiles writes every write_files entry under root and returns the
// join script's path. Absolute node paths in file contents are re-rooted too, so
// the script and the JoinConfiguration agree on where things live.
func materializeWriteFiles(t *testing.T, cloudConfig, root string) string {
	t.Helper()
	var cc struct {
		WriteFiles []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
	}
	if err := yaml.Unmarshal([]byte(cloudConfig), &cc); err != nil {
		t.Fatalf("parse cloud-config: %v", err)
	}
	reroot := regexp.MustCompile(`(^|[\s"'=:(])(/etc/kubernetes|/var/lib/kairos)`)
	script := ""
	for _, f := range cc.WriteFiles {
		dst := filepath.Join(root, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		content := reroot.ReplaceAllString(f.Content, "${1}"+root+"${2}")
		if err := os.WriteFile(dst, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
		if f.Path == kubeadmJoinScriptPath {
			script = dst
		}
	}
	if script == "" {
		t.Fatalf("rendered cloud-config has no %s", kubeadmJoinScriptPath)
	}
	return script
}

// runJoinScript runs the join script with a kubeadm stub first on PATH and returns
// the arguments the script passed to `kubeadm join` plus the --config path.
func runJoinScript(t *testing.T, root, script, version string) ([]string, string) {
	t.Helper()
	stubDir := t.TempDir()
	argsFile := filepath.Join(stubDir, "join-args")
	stub := `#!/bin/bash
set -euo pipefail
if [ "$1" = version ]; then echo "$WANT_KUBEADM_VERSION"; exit 0; fi
printf '%s\n' "$@" > "$KUBEADM_ARGS_FILE"
if [ -n "${KUBEADM_BIN:-}" ]; then
  exec "$KUBEADM_BIN" "$@" --skip-phases="$KUBEADM_JOIN_PHASES" --dry-run
fi
`
	if err := os.WriteFile(filepath.Join(stubDir, "kubeadm"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"WANT_KUBEADM_VERSION="+version,
		"KUBEADM_ARGS_FILE="+argsFile,
		"KUBEADM_JOIN_PHASES="+kubeadmJoinPhases(t),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("join script failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("kubeadm join was not invoked: %v", err)
	}
	args := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	configPath := ""
	for i, a := range args {
		if a == "--config" && i+1 < len(args) {
			configPath = args[i+1]
		} else if v, ok := strings.CutPrefix(a, "--config="); ok {
			configPath = v
		}
	}
	if !strings.HasPrefix(configPath, root) {
		t.Fatalf("kubeadm join got no --config under the test root: %v", args)
	}
	return args, configPath
}

// kubeadmJoinPhases lists the top-level `kubeadm join` phases of KUBEADM_BIN, so
// skipping them all keeps working when kubeadm adds a phase.
func kubeadmJoinPhases(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("KUBEADM_BIN")
	if bin == "" {
		return ""
	}
	out, err := exec.Command(bin, "join", "--help").Output()
	if err != nil {
		t.Fatalf("%s join --help: %v", bin, err)
	}
	var phases []string
	inList := false
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.Contains(line, "executes the following phases"):
			inList = true
		case inList && strings.HasPrefix(line, "```") && len(phases) > 0:
			inList = false
		case inList && line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "```"):
			phases = append(phases, strings.Fields(line)[0])
		}
	}
	if len(phases) == 0 {
		t.Fatalf("no phases found in `%s join --help`", bin)
	}
	return strings.Join(phases, ",")
}

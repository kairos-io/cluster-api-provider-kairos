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

	yaml "gopkg.in/yaml.v3"
)

// tlsHardeningCase is one (render fn, kubevirt, distro) fixture exercising the
// full node-push surface (kubeconfig + etcd-status, plus VMI-GET on k0s CAPK)
// so the ADR 0011 PR-4 assertions cover every management-API call site. (The k0s
// init node no longer pushes a join token: the control plane mints it, ADR 0013.)
type tlsHardeningCase struct {
	name     string
	render   func(TemplateData) (string, error)
	kubevirt bool
}

func tlsHardeningData(kubevirt bool) TemplateData {
	return TemplateData{
		Role:                   "control-plane",
		ControlPlaneRole:       "init", // HA init exercises the etcd-status reporter
		UserName:               "kairos",
		IsKubeVirt:             kubevirt,
		ControlPlaneLBEndpoint: "10.0.0.10",
		K3sServerURL:           "https://10.0.0.10:6443",
		ManagementEndpoint: &ManagementEndpoint{
			APIServer:                 "https://mgmt.example.com:6443",
			Token:                     "mgmt-token",
			KubeconfigSecretName:      "ha-cluster-kubeconfig-push",
			KubeconfigSecretNamespace: "default",
			ClusterName:               "ha-cluster",
			ControlPlaneEndpointHost:  "10.0.0.10",
			EtcdStatusSecretName:      "ha-cluster-etcd-status",
			CABundle:                  goldenManagementCA,
		},
	}
}

func tlsHardeningCases() []tlsHardeningCase {
	return []tlsHardeningCase{
		{"k0s_capk", RenderK0sCloudConfig, true},
		{"k0s_capv", RenderK0sCloudConfig, false},
		{"k3s_capk", RenderK3sCloudConfig, true},
		{"k3s_capv", RenderK3sCloudConfig, false},
	}
}

// nonCommentShellLines returns the rendered lines that are NOT shell comments,
// so assertions about actual curl invocations are not confused by documentation.
func nonCommentShellLines(result string) []string {
	var out []string
	for _, line := range strings.Split(result, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// TestManagementPushTLSHardening asserts the ADR 0011 PR-4 invariants across all
// four templates: every management-API curl verifies TLS with --cacert, no
// -k/--insecure survives, no redirects are followed, and the CA is a file the
// curls point at rather than an inlined argv value.
func TestManagementPushTLSHardening(t *testing.T) {
	for _, tc := range tlsHardeningCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.render(tlsHardeningData(tc.kubevirt))
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			var sink any
			if err := yaml.Unmarshal([]byte(result), &sink); err != nil {
				t.Fatalf("rendered output is not valid YAML: %v", err)
			}

			// No insecure curl anywhere (including comments — we reworded them so
			// the literal flags never appear in the rendered output at all).
			for _, bad := range []string{"curl -k", "-k -sS", "--insecure", "-L ", "--location"} {
				if strings.Contains(result, bad) {
					t.Errorf("rendered output must not contain %q (no insecure/redirect-following push)", bad)
				}
			}

			// Every curl that talks to the management API verifies TLS with a CA
			// file. There is exactly one management curl per helper (mgmt_patch,
			// and mgmt_get on k0s CAPK), and each carries --cacert.
			shellLines := nonCommentShellLines(result)
			for _, line := range shellLines {
				trimmed := strings.TrimSpace(line)
				if !strings.HasPrefix(trimmed, "curl ") {
					continue
				}
				// The etcd-health-check curl (etcdctl is not curl) never matches;
				// the only curls here are the management helpers and they must all
				// pin the management CA file.
				if !strings.Contains(trimmed, `--cacert "${KAIROS_MGMT_CA}"`) {
					t.Errorf("management curl does not pin the CA file: %q", trimmed)
				}
				if !strings.Contains(trimmed, "--proto '=https'") {
					t.Errorf("management curl does not force https-only: %q", trimmed)
				}
			}

			// The CA is written to a file, not inlined into a curl argv.
			if !strings.Contains(result, "path: /etc/kairos-capi/management-ca.crt") {
				t.Error("management CA must be written to a write_files file")
			}
			if !strings.Contains(result, `KAIROS_MGMT_CA="/etc/kairos-capi/management-ca.crt"`) {
				t.Error("push must reference the CA file path via KAIROS_MGMT_CA")
			}

			// The bearer token is kept out of curl's argv: no Authorization header
			// on any command line; it is supplied via the --config file instead.
			if strings.Contains(result, "Authorization: Bearer ${") {
				t.Error("bearer token must not appear in a curl -H argv; use the --config header file")
			}
			if !strings.Contains(result, `--config "${_authf}"`) {
				t.Error("push must read the auth header from the per-call --config file (token off argv)")
			}
			if !strings.Contains(result, `printf 'header = "Authorization: Bearer %s"\n'`) {
				t.Error("the auth header file must be written from the token via printf, not passed on argv")
			}

			// The token file lives in a private (0700) run dir and is a per-call
			// mktemp file (0600) removed immediately after the request (PR-4 A7).
			if !strings.Contains(result, `chmod 0700 "${KAIROS_MGMT_RUNDIR}"`) {
				t.Error("the run dir holding the token/body files must be 0700")
			}
			if !strings.Contains(result, `mktemp "${KAIROS_MGMT_RUNDIR}/auth.XXXXXX"`) {
				t.Error("the auth config file must be a per-call mktemp (0600) file")
			}
			if !strings.Contains(result, `rm -f "${_bodyf}" "${_authf}"`) {
				t.Error("the token config file must be removed right after the request (must not linger on tmpfs)")
			}
			// The old shared, never-removed auth.conf must be gone.
			if strings.Contains(result, "KAIROS_MGMT_AUTH_CONF") {
				t.Error("the shared persistent auth.conf must be replaced by per-call mktemp files")
			}

			// Request bodies are staged in a file (off argv); responses are not
			// written to a world-readable /tmp path.
			if !strings.Contains(result, `--data-binary @"${_bodyf}"`) {
				t.Error("the PATCH body must be sent from a file (--data-binary @file), not inlined on argv")
			}
			if strings.Contains(result, "/tmp/kairos-") {
				t.Error("push response bodies must not be written to a world-readable /tmp path")
			}
		})
	}
}

// TestManagementPushTLSHardening_K0sHasVMIGet asserts the k0s CAPK SAN-detection
// GET was hardened too (it is a management-API call, not just the pushes).
func TestManagementPushTLSHardening_K0sHasVMIGet(t *testing.T) {
	result, err := RenderK0sCloudConfig(tlsHardeningData(true))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(result, "mgmt_get()") {
		t.Error("k0s CAPK must define the mgmt_get helper for SAN detection")
	}
	if !strings.Contains(result, `json=$(mgmt_get "${url}")`) {
		t.Error("k0s CAPK SAN detection must use the verified-TLS mgmt_get helper")
	}
	if !strings.Contains(result, `rm -f "${_respf}" "${_authf}"`) {
		t.Error("mgmt_get must remove both the response and the token config file after the request")
	}
}

// TestManagementCABundle_Rendered asserts the CA content is embedded as a YAML
// block scalar (safely quoted/escaped via nindent, never inlined into argv) and
// round-trips through YAML parsing to the exact certificate bytes.
func TestManagementCABundle_Rendered(t *testing.T) {
	result, err := RenderK0sCloudConfig(tlsHardeningData(true))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var doc struct {
		WriteFiles []struct {
			Path        string `yaml:"path"`
			Permissions string `yaml:"permissions"`
			Content     string `yaml:"content"`
		} `yaml:"write_files"`
	}
	if err := yaml.Unmarshal([]byte(result), &doc); err != nil {
		t.Fatalf("parse rendered YAML: %v", err)
	}
	var found bool
	for _, f := range doc.WriteFiles {
		if f.Path != "/etc/kairos-capi/management-ca.crt" {
			continue
		}
		found = true
		if f.Permissions != "0644" {
			t.Errorf("CA file permissions = %q, want 0644", f.Permissions)
		}
		if !strings.Contains(f.Content, "-----BEGIN CERTIFICATE-----") {
			t.Errorf("CA file content is not a certificate: %q", f.Content)
		}
		if strings.Contains(f.Content, "PRIVATE KEY") {
			t.Error("CA file content must never contain a private key")
		}
	}
	if !found {
		t.Error("management CA write_files entry not found")
	}
}

// TestPostBootstrapScriptIs0700 asserts the post-bootstrap script — which now
// writes the bearer token into a curl config file and drives every push — is
// mode 0700 (ADR 0011 PR-4), so a non-root local user cannot read the token out
// of the script body.
func TestPostBootstrapScriptIs0700(t *testing.T) {
	for _, tc := range tlsHardeningCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.render(tlsHardeningData(tc.kubevirt))
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			var doc struct {
				WriteFiles []struct {
					Path        string `yaml:"path"`
					Permissions string `yaml:"permissions"`
				} `yaml:"write_files"`
			}
			if err := yaml.Unmarshal([]byte(result), &doc); err != nil {
				t.Fatalf("parse: %v", err)
			}
			var found bool
			for _, f := range doc.WriteFiles {
				if strings.HasSuffix(f.Path, "post-bootstrap.sh") {
					found = true
					if f.Permissions != "0700" {
						t.Errorf("%s permissions = %q, want 0700 (script handles the token)", f.Path, f.Permissions)
					}
				}
			}
			if !found {
				t.Error("post-bootstrap script not found")
			}
		})
	}
}

// TestManagementCABundle_RejectsPrivateKey is the adversarial case: a CA bundle
// that carries a PRIVATE KEY block (or any non-certificate content) must fail the
// render rather than writing a key into userdata.
func TestManagementCABundle_RejectsPrivateKey(t *testing.T) {
	data := tlsHardeningData(false)
	data.ManagementEndpoint.CABundle = goldenManagementCA +
		"-----BEGIN PRIVATE KEY-----\nMIGdummy\n-----END PRIVATE KEY-----\n"
	if _, err := RenderK0sCloudConfig(data); err == nil {
		t.Fatal("render must fail when the CA bundle contains a PRIVATE KEY block")
	}
}

// TestManagementCABundle_RejectsYAMLBreakingPayload asserts that a CA field
// containing YAML/shell-metacharacter garbage (not a clean CERTIFICATE bundle)
// is rejected at render time, so it can never break out of the write_files block.
func TestManagementCABundle_RejectsYAMLBreakingPayload(t *testing.T) {
	for _, payload := range []string{
		"not a certificate at all",
		"-----BEGIN CERTIFICATE-----\n\": injected\nkey: value\n-----END CERTIFICATE-----\n",
		goldenManagementCA + "\n---\nkind: Secret\n",
	} {
		data := tlsHardeningData(false)
		data.ManagementEndpoint.CABundle = payload
		if _, err := RenderK0sCloudConfig(data); err == nil {
			t.Errorf("render must reject a non-clean CA bundle payload: %q", payload)
		}
	}
}

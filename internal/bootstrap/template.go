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
	"bytes"
	"embed"
	"fmt"
	"text/template"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// TemplateData holds data for rendering the Kairos cloud-config template.
//
// Most string fields originate from KairosConfig spec (user-controlled). They
// MUST be emitted through the `quote` template func — never as raw `{{ .X }}`
// interpolation. See internal/bootstrap/funcs.go and the
// cloudconfig-rendering-safety skill for the rules.
//
// Files renders via `toYaml .Files | nindent 2` appended to the top-level
// `write_files:` list in each template. yaml.v3 selects safe block-scalar
// representation for Content automatically; per-field hand-assembly with
// `quote` is NOT used for Files (KD-Files design).
type TemplateData struct {
	Role         string
	SingleNode   bool
	Hostname     string
	UserName     string
	UserPassword string
	UserGroups   []string
	GitHubUser   string
	SSHPublicKey string
	WorkerToken  string
	Manifests    []bootstrapv1beta2.Manifest
	// Files are additional files written to the node via write_files:. Each
	// entry is rendered as a whole slice by toYaml — never assembled per-field.
	// Path/Permissions/Owner are validated by validateTemplateData and the
	// webhook; Content may contain newlines (yaml.v3 picks a block scalar form).
	Files          []bootstrapv1beta2.File
	HostnamePrefix string
	DNSServers     []string
	PodCIDR        string
	ServiceCIDR    string
	PrimaryIP      string
	MachineName    string
	ClusterNS      string
	IsKubeVirt     bool
	// Metal3 selects the CAPM3 bare-metal render path: emit the metal3.io/uuid
	// node-label shell stage (read from the Ironic config-drive at boot) and
	// SUPPRESS our providerID arg + kubectl-patch block, because CAPM3 owns
	// Node.spec.providerID. Metal3 rides the generic (non-KubeVirt) CAPV
	// template. (ADR 0004, OQ-1 RESOLVED.)
	Metal3                         bool
	Install                        *InstallConfig
	ProviderID                     string // ProviderID for the Node (e.g., "vsphere://<vm-uuid>"). Validated against providerIDPattern at render time.
	K3sServerURL                   string
	K3sToken                       string
	ControlPlaneLBServiceName      string
	ControlPlaneLBServiceNamespace string
	ControlPlaneLBEndpoint         string
	// ManagementEndpoint, if non-nil, enables the in-node kubeconfig-push path
	// (CAPK today; other infra providers under KD-3b). The renderer treats the
	// pointer as the single gate for emitting the push block — when nil, no
	// management-cluster contact is rendered. Resolved by the controller from a
	// ManagementEndpointResolver; see internal/controllers/bootstrap/CLAUDE.md.
	ManagementEndpoint *ManagementEndpoint
}

// ManagementEndpoint bundles the values the rendered cloud-config needs
// to push the workload kubeconfig back to the management cluster without SSH:
// the management API URL, an authenticated bearer token, the (namespace, name)
// of the kubeconfig Secret to write, plus identity metadata stamped onto that
// Secret. All shell-context fields are rendered into shell command positions
// via the shquote template func — any new field added here that lands in a
// shell context MUST be routed through shquote per the rules in
// internal/bootstrap/CLAUDE.md § "Shell contexts".
//
// ClusterName is stamped into the Secret as the `cluster.x-k8s.io/cluster-name`
// label so the controller's Secret-watch predicate (KD-15-compliant under
// KD-3b) can match by label rather than name suffix.
//
// ControlPlaneEndpointHost is used by non-CAPK infrastructure paths
// (CAPV today; CAPM3/Tinkerbell when they land) to rewrite the kubeconfig
// `server:` URL so the management cluster reaches the API server via the
// canonical control-plane endpoint instead of `127.0.0.1`. For CAPK,
// ControlPlaneLBEndpoint covers this — ControlPlaneEndpointHost is only read
// by the CAPV templates.
type ManagementEndpoint struct {
	APIServer                 string
	Token                     string
	KubeconfigSecretName      string
	KubeconfigSecretNamespace string
	ClusterName               string
	ControlPlaneEndpointHost  string
}

// InstallConfig holds installation configuration for the template
type InstallConfig struct {
	Auto   bool
	Device string
	Reboot bool
}

// RenderK0sCloudConfig renders the k0s Kairos cloud-config template.
func RenderK0sCloudConfig(data TemplateData) (string, error) {
	templatePath := "templates/k0s_kairos_cloud_config_capv.yaml.tmpl"
	if data.IsKubeVirt {
		templatePath = "templates/k0s_kairos_cloud_config_capk.yaml.tmpl"
	}
	return renderTemplate("k0s_kairos_cloud_config", templatePath, data)
}

// RenderK3sCloudConfig renders the k3s Kairos cloud-config template.
func RenderK3sCloudConfig(data TemplateData) (string, error) {
	templatePath := "templates/k3s_kairos_cloud_config_capv.yaml.tmpl"
	if data.IsKubeVirt {
		templatePath = "templates/k3s_kairos_cloud_config_capk.yaml.tmpl"
	}
	return renderTemplate("k3s_kairos_cloud_config", templatePath, data)
}

// renderTemplate is the shared entry point for both distribution renderers.
// It validates TemplateData, loads the template from the embedded FS, attaches
// the shared FuncMap, executes, and returns the rendered cloud-config.
//
// Centralizing this prevents the FuncMap from drifting between the k0s and
// k3s paths — a real risk in the previous two-function layout, since adding
// `quote` to one and forgetting the other would silently leave half the
// renders unsafe.
func renderTemplate(name, templatePath string, data TemplateData) (string, error) {
	if err := validateTemplateData(&data); err != nil {
		return "", fmt.Errorf("invalid template data: %w", err)
	}
	tmplContent, err := templateFS.ReadFile(templatePath)
	if err != nil {
		return "", fmt.Errorf("failed to read template %s: %w", templatePath, err)
	}
	tmpl, err := template.New(name).Funcs(newFuncMap()).Parse(string(tmplContent))
	if err != nil {
		return "", fmt.Errorf("failed to parse template %s: %w", templatePath, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template %s: %w", templatePath, err)
	}
	rendered := buf.String()
	// Metal3 config-drive size guard: Ironic delivers bootstrap data via
	// config-drive, which without Swift is capped at ~64 KiB. We enforce a
	// conservative 60 KiB budget to leave headroom for other config-drive
	// partitions. Only enforced for Metal3; CAPK/CAPV/CAPD have no config-drive.
	// (ADR 0004, RISK-2.)
	const metal3ConfigDriveSafetyBudget = 60 * 1024
	if data.Metal3 && len(rendered) > metal3ConfigDriveSafetyBudget {
		return "", fmt.Errorf("rendered cloud-config is %d bytes; exceeds the 60KiB safety budget for the Ironic config-drive (~64KiB hard cap without Swift) — reduce manifests/files", len(rendered))
	}
	return rendered, nil
}

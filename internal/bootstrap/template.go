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
	// ControlPlaneRole is the init/join/single discriminator for an HA control
	// plane (ADR 0005, Phase 2). It mirrors KairosConfig.Spec.ControlPlaneRole.
	//
	// The zero value ("") MUST behave exactly as "single" — the existing
	// single-node render path — so configs that predate the field (and every
	// render until the Phase-3 controller assigns roles) keep their current
	// output byte-for-byte. Use IsSingleControlPlane / IsInitControlPlane /
	// IsJoinControlPlane rather than comparing the raw string; those helpers
	// encode the empty-equals-single rule in one place.
	//
	// Only consulted on the control-plane path. On worker renders this field is
	// ignored entirely (CPR-INV-1/2 are the controller's enforcement that a
	// worker config never carries a control-plane role; the renderer simply does
	// not branch on ControlPlaneRole when Role != "control-plane").
	ControlPlaneRole string
	// JoinToken is the cluster join token for an HA "join" node (k0s
	// controller-join token / shared k3s server token). It is written to a
	// 0600 token file via a write_files YAML block scalar — NOT a shell
	// context — so its protection is literal-block-scalar embedding plus the
	// rejectControlChars check in validate.go (no newline/CR/NUL), NOT shquote.
	// Do not relocate it into a runcmd/ExecStart line without adding shquote.
	//
	// PHASE-3 SEAM: where this value comes from (controller-generated k3s token,
	// or k0s token retrieved over the node-push channel post-init, stored in a
	// management-cluster Secret and resolved via *SecretRef) is owned by Phase 3
	// (ADR 0005 §3, TOKEN-INV). Phase 2 only renders it given the input; the
	// controller does NOT populate it yet, so it is empty in practice until
	// Phase 3. A join render with an empty JoinToken still produces valid YAML
	// (the join node simply cannot authenticate until Phase 3 wires the token).
	JoinToken string
	// VIP, when non-nil AND ControlPlaneRole is init or join, drives the kube-vip
	// static-pod manifest rendered into the distribution's manifests directory
	// (ADR 0005, OQ-1). It is the mechanism that MAKES the control-plane endpoint
	// floatable; it is NOT the join target. The join server URL is the stable
	// ControlPlaneEndpointHost (from KD-12 / the InfraCluster), never the VIP.
	//
	// VIP is rendered ONLY for the CAPV/CAPM3 (generic, non-KubeVirt) templates.
	// CAPK uses its built-in LoadBalancer Service as the endpoint and MUST NOT
	// render kube-vip (OQ-5); the CAPK templates ignore this field. When VIP is
	// nil, no kube-vip is rendered (external-LB HA is a valid topology).
	VIP *VIPConfig
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
	// JoinTokenSecretName, when non-empty AND the render is a k0s HA init node
	// (IsInitControlPlane), enables the controller-join-token push block: the
	// init node runs `k0s token create --role=controller` and PUSHes the token
	// into this Secret over the same node-push channel as the kubeconfig (ADR
	// 0005 Phase 3). The Secret lives in KubeconfigSecretNamespace (the cluster
	// namespace) and is pre-created empty + owner-ref'd by the KCP controller.
	// The token is base64-enveloped into data.token; it is NEVER interpolated
	// through text/template. Only meaningful for k0s init; k3s uses a
	// controller-generated token and never runs this block.
	JoinTokenSecretName string
}

// InstallConfig holds installation configuration for the template
type InstallConfig struct {
	Auto   bool
	Device string
	Reboot bool
}

// VIPConfig is the normalized, render-ready view of
// KairosControlPlane.Spec.HA.VIP (kube-vip). The controller marshals the CRD
// type into this flat struct; the renderer stays CAPI-type-unaware
// (internal/bootstrap/CLAUDE.md §4).
//
// Security (ADR 0005 §C, VIP-INV-1/2/3):
//   - Address/Interface/Mode are operator-influenced. Every shell-context use
//     is routed through shquote (VIP-INV-1).
//   - The kube-vip static-pod manifest is produced via marshaled YAML, never
//     string concatenation (VIP-INV-2) — see kubeVIPManifest in funcs.go.
//   - Address (net.ParseIP || DNS-1123) and Interface (interface-name regex)
//     are re-validated at render time, not just at admission (VIP-INV-3) —
//     see validateVIP in validate.go.
type VIPConfig struct {
	// Address is the virtual IP or hostname the control-plane endpoint floats
	// on. Validated as an IPv4/IPv6 address or RFC-1123 hostname at render time.
	Address string
	// Interface is the Linux NIC name kube-vip advertises the VIP on (ARP/BGP).
	// Validated against the interface-name regex at render time.
	Interface string
	// Mode is "ARP" (L2, default) or "BGP" (L3). Empty is treated as ARP.
	Mode string
}

// IsControlPlane reports whether this render is for a control-plane node. The
// ControlPlaneRole field is only meaningful (and only consulted) when this is
// true; worker renders ignore ControlPlaneRole entirely.
func (d TemplateData) IsControlPlane() bool {
	return d.Role == "control-plane"
}

// IsSingleControlPlane reports whether the control plane should render in
// single-node mode. The zero value ("") is treated as "single" so configs that
// predate ControlPlaneRole keep their exact current output (ADR 0005, CPR-INV-2).
// Only true on the control-plane path.
func (d TemplateData) IsSingleControlPlane() bool {
	return d.IsControlPlane() && (d.ControlPlaneRole == "" || d.ControlPlaneRole == "single")
}

// IsInitControlPlane reports whether this is the HA first (init) control-plane
// node. Only true on the control-plane path.
func (d TemplateData) IsInitControlPlane() bool {
	return d.IsControlPlane() && d.ControlPlaneRole == "init"
}

// IsJoinControlPlane reports whether this is an HA joining control-plane node.
// Only true on the control-plane path.
func (d TemplateData) IsJoinControlPlane() bool {
	return d.IsControlPlane() && d.ControlPlaneRole == "join"
}

// IsHAControlPlane reports whether this is an HA control-plane node (init or
// join). Single-node (and the empty zero value) is NOT HA.
func (d TemplateData) IsHAControlPlane() bool {
	return d.IsInitControlPlane() || d.IsJoinControlPlane()
}

// RenderKubeVIP reports whether the kube-vip static-pod manifest should be
// emitted for this render. True only for an HA control-plane node (init/join)
// with a non-nil VIP block on a NON-KubeVirt (CAPV/CAPM3) template. CAPK uses
// its built-in LoadBalancer Service and never renders kube-vip (ADR 0005, OQ-5).
func (d TemplateData) RenderKubeVIP() bool {
	return d.IsHAControlPlane() && d.VIP != nil && !d.IsKubeVirt
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

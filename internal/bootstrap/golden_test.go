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
	"flag"
	"os"
	"path/filepath"
	"testing"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	yaml "gopkg.in/yaml.v3"
)

// updateGolden regenerates the golden files instead of asserting against them.
// Run `go test ./internal/bootstrap/... -run TestGolden -update` to refresh.
//
// The single/"" goldens are the byte-for-byte snapshot of the pre-Phase-2
// render. They are the proof that the Phase-2 role branches do not perturb the
// single-node output: any unintended drift in those files fails CI and must be
// explained in review. The init/join goldens are the new HA shapes; they are
// human-reviewed on first generation and then frozen.
var updateGolden = flag.Bool("update", false, "regenerate golden files under testdata/golden")

// goldenCase is one (distribution, infra, role) render fixture.
type goldenCase struct {
	name   string                             // golden file basename (without .yaml)
	render func(TemplateData) (string, error) // RenderK0sCloudConfig / RenderK3sCloudConfig
	data   TemplateData
}

// goldenVIP is the representative, well-formed VIP block used by the HA golden
// fixtures. Address is an IP (ARP-mode realistic), interface a normal NIC name.
func goldenVIP() *VIPConfig {
	return &VIPConfig{
		Address:   "192.168.1.240",
		Interface: "eth0",
		Mode:      "ARP",
	}
}

// goldenCases enumerates every (distro × infra × role) snapshot we freeze.
//
// single/"" cases use the exact field set the controller produces today (and
// that the existing substring tests exercise), so the goldens are an honest
// snapshot of current behaviour. The "" cases assert ControlPlaneRole zero-value
// renders identically to "single": the data is byte-identical to the single
// case except the explicit role, and we assert equality of the two outputs in
// TestGolden_EmptyRoleEqualsSingle.
func goldenCases() []goldenCase {
	base := func(role bootstrapv1beta2.ControlPlaneRole, single bool, kubevirt, metal3 bool) TemplateData {
		d := TemplateData{
			Role:             "control-plane",
			SingleNode:       single,
			ControlPlaneRole: string(role),
			Hostname:         "kairos-cp-0",
			UserName:         "kairos",
			UserGroups:       []string{"admin"},
			GitHubUser:       "testuser",
			IsKubeVirt:       kubevirt,
			Metal3:           metal3,
		}
		return d
	}
	withVIP := func(d TemplateData) TemplateData {
		d.VIP = goldenVIP()
		return d
	}
	withEndpoint := func(d TemplateData, host string) TemplateData {
		d.ManagementEndpoint = &ManagementEndpoint{
			APIServer:                 "https://mgmt.example.com:6443",
			Token:                     "mgmt-token",
			KubeconfigSecretName:      "cluster-kubeconfig",
			KubeconfigSecretNamespace: "default",
			ClusterName:               "ha-cluster",
			ControlPlaneEndpointHost:  host,
		}
		return d
	}

	return []goldenCase{
		// --- single (current behaviour snapshot) ---
		{"k0s_capv_single", RenderK0sCloudConfig, base(bootstrapv1beta2.ControlPlaneRoleSingle, true, false, false)},
		{"k3s_capv_single", RenderK3sCloudConfig, base(bootstrapv1beta2.ControlPlaneRoleSingle, true, false, false)},
		{"k0s_capk_single", RenderK0sCloudConfig, base(bootstrapv1beta2.ControlPlaneRoleSingle, true, true, false)},
		{"k3s_capk_single", RenderK3sCloudConfig, base(bootstrapv1beta2.ControlPlaneRoleSingle, true, true, false)},

		// --- init (HA first node, CAPV: kube-vip + etcd-health reporter rendered) ---
		{"k0s_capv_init", RenderK0sCloudConfig, withEtcdStatusSecretName(withJoinTokenSecretName(withEndpoint(withVIP(base(bootstrapv1beta2.ControlPlaneRoleInit, false, false, false)), "192.168.1.240"), "ha-cluster-control-plane-join-token"), "ha-cluster-etcd-status")},
		{"k3s_capv_init", RenderK3sCloudConfig, withEtcdStatusSecretName(withEndpoint(withVIP(base(bootstrapv1beta2.ControlPlaneRoleInit, false, false, false)), "192.168.1.240"), "ha-cluster-etcd-status")},

		// --- join (HA subsequent node, CAPV: kube-vip + etcd-health reporter rendered) ---
		{"k0s_capv_join", RenderK0sCloudConfig, withEtcdStatusSecretName(withJoinToken(withEndpoint(withVIP(base(bootstrapv1beta2.ControlPlaneRoleJoin, false, false, false)), "192.168.1.240")), "ha-cluster-etcd-status")},
		{"k3s_capv_join", RenderK3sCloudConfig, withEtcdStatusSecretName(withJoinToken(withEndpoint(withVIP(base(bootstrapv1beta2.ControlPlaneRoleJoin, false, false, false)), "192.168.1.240")), "ha-cluster-etcd-status")},

		// --- CAPK HA (NO kube-vip; OQ-5): init/join still branch, LB Service is the endpoint ---
		{"k0s_capk_init", RenderK0sCloudConfig, withJoinTokenSecretName(capkHA(base(bootstrapv1beta2.ControlPlaneRoleInit, false, true, false)), "ha-cluster-control-plane-join-token")},
		{"k3s_capk_init", RenderK3sCloudConfig, capkHA(base(bootstrapv1beta2.ControlPlaneRoleInit, false, true, false))},
		{"k0s_capk_join", RenderK0sCloudConfig, withJoinToken(capkHA(base(bootstrapv1beta2.ControlPlaneRoleJoin, false, true, false)))},
		{"k3s_capk_join", RenderK3sCloudConfig, withJoinToken(capkHA(base(bootstrapv1beta2.ControlPlaneRoleJoin, false, true, false)))},
	}
}

func withJoinToken(d TemplateData) TemplateData {
	d.JoinToken = "K10aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa::server:bbbbbbbbbbbbbbbbbbbbbbbb"
	return d
}

// withJoinTokenSecretName stamps the k0s HA init-node push-block gate
// (ManagementEndpoint.JoinTokenSecretName). Only the k0s init render mints
// `k0s token create` and pushes the token; this makes the golden snapshot
// exercise that highest-blast-radius block. Requires ManagementEndpoint set.
func withJoinTokenSecretName(d TemplateData, name string) TemplateData {
	d.ManagementEndpoint.JoinTokenSecretName = name
	return d
}

// withEtcdStatusSecretName stamps the HA etcd-health reporter gate
// (ManagementEndpoint.EtcdStatusSecretName, ADR 0005 §E.1). Set for every HA
// control-plane node (init AND join, both distros) on the CAPV path so the
// golden snapshots exercise the node-push reporter block. Requires
// ManagementEndpoint set.
func withEtcdStatusSecretName(d TemplateData, name string) TemplateData {
	d.ManagementEndpoint.EtcdStatusSecretName = name
	return d
}

// capkHA sets the CAPK LoadBalancer endpoint that drives the CAPK HA join URL
// and tls-san. CAPK never renders kube-vip (OQ-5), so VIP is intentionally nil.
func capkHA(d TemplateData) TemplateData {
	d.ControlPlaneLBEndpoint = "10.96.0.10"
	d.K3sServerURL = "https://10.96.0.10:6443"
	d.ManagementEndpoint = &ManagementEndpoint{
		APIServer:                 "https://mgmt.example.com:6443",
		Token:                     "mgmt-token",
		KubeconfigSecretName:      "cluster-kubeconfig",
		KubeconfigSecretNamespace: "default",
		ClusterName:               "ha-cluster",
	}
	return d
}

func goldenPath(name string) string {
	return filepath.Join("testdata", "golden", name+".yaml")
}

// TestGolden renders every fixture and compares to its frozen golden file.
// With -update it (re)writes the golden files instead.
func TestGolden(t *testing.T) {
	for _, tc := range goldenCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.render(tc.data)
			if err != nil {
				t.Fatalf("render %s: %v", tc.name, err)
			}
			// Every rendered cloud-config MUST be valid YAML.
			var sink any
			if err := yaml.Unmarshal([]byte(got), &sink); err != nil {
				t.Fatalf("rendered %s is not valid YAML: %v", tc.name, err)
			}
			path := goldenPath(tc.name)
			if *updateGolden {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden %s: %v", path, err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s (run with -update to create): %v", path, err)
			}
			if got != string(want) {
				t.Errorf("rendered %s does not match golden %s.\n"+
					"If this change is intended, re-run with -update and review the diff.\n"+
					"--- got ---\n%s\n--- want ---\n%s",
					tc.name, path, got, string(want))
			}
		})
	}
}

// TestGolden_EmptyRoleEqualsSingle is the load-bearing backward-compat proof:
// ControlPlaneRole=="" MUST render byte-identically to ControlPlaneRole=="single"
// for the same inputs, across both distributions and both infra paths.
func TestGolden_EmptyRoleEqualsSingle(t *testing.T) {
	cases := []struct {
		name   string
		render func(TemplateData) (string, error)
		single TemplateData
	}{
		{"k0s_capv", RenderK0sCloudConfig, singleEqualityData(false)},
		{"k3s_capv", RenderK3sCloudConfig, singleEqualityData(false)},
		{"k0s_capk", RenderK0sCloudConfig, singleEqualityData(true)},
		{"k3s_capk", RenderK3sCloudConfig, singleEqualityData(true)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			single := tc.single
			single.ControlPlaneRole = "single"
			empty := tc.single
			empty.ControlPlaneRole = ""

			gotSingle, err := tc.render(single)
			if err != nil {
				t.Fatalf("render single: %v", err)
			}
			gotEmpty, err := tc.render(empty)
			if err != nil {
				t.Fatalf("render empty-role: %v", err)
			}
			if gotSingle != gotEmpty {
				t.Errorf("ControlPlaneRole=\"\" did not render identically to \"single\" for %s; "+
					"the zero value MUST behave exactly as single (ADR 0005 / CPR-INV-2)", tc.name)
			}
		})
	}
}

func singleEqualityData(kubevirt bool) TemplateData {
	return TemplateData{
		Role:       "control-plane",
		SingleNode: true,
		Hostname:   "kairos-cp-0",
		UserName:   "kairos",
		UserGroups: []string{"admin"},
		GitHubUser: "testuser",
		IsKubeVirt: kubevirt,
	}
}

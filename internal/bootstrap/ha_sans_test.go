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

// The control-plane endpoint (the kube-vip VIP on CAPV/CAPM3, the LoadBalancer
// Service on CAPK) is not pinned to the HA init node. When the member that holds
// it is replaced the VIP moves to a survivor, and every TLS-verifying client of
// the endpoint, the management cluster's KCP controller included, then lands on
// THAT node's apiserver. So every HA control-plane member must carry the
// endpoint in its apiserver serving-cert SANs, not just the init node.
//
// Reproduced live (2026-10-16, v0.1.4, CAPV, 3-replica k0s, spec.ha.vip
// 192.0.2.95): the k0s init node alone had the endpoint in api.sans; after the
// init node was deleted and the VIP moved to a joiner, KCP failed with
//
//	failed to reconcile etcd member leave for ...-cp-0: ... Get
//	"https://192.0.2.95:6443/api": tls: failed to verify certificate: x509:
//	certificate is valid for 127.0.0.1, ::1, 192.0.2.55, ..., not 192.0.2.95
//
// and the rollout stalled. The k0s CAPK template and both k3s templates already
// covered every member; the generic (CAPV/CAPM3/fleet/Beskar7) k0s template did
// not.

// sansTestEndpoint is deliberately different from the VIP address haCPData puts
// in VIPConfig (192.168.1.240), so these tests prove which field the SAN is
// sourced from rather than passing because the two happen to be equal.
const sansTestEndpoint = "192.0.2.95"

// k0sConfigSpec parses the rendered cloud-config, pulls out the content of the
// /etc/k0s/k0s.yaml write_files entry and returns its parsed document. Both
// documents must be valid YAML; ok is false when the file is not written at all.
func k0sConfigSpec(t *testing.T, out string) (doc map[string]any, ok bool) {
	t.Helper()
	content := extractWriteFile(t, out, "/etc/k0s/k0s.yaml")
	if content == "" {
		return nil, false
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("embedded /etc/k0s/k0s.yaml is not valid YAML: %v\n---\n%s", err, content)
	}
	return doc, true
}

// mapAt walks nested string-keyed maps and returns the value at path, or nil
// when any step is missing or not a mapping.
func mapAt(m map[string]any, path ...string) any {
	var cur any = m
	for _, k := range path {
		next, isMap := cur.(map[string]any)
		if !isMap {
			return nil
		}
		cur = next[k]
	}
	return cur
}

// stringList converts a decoded YAML sequence to []string, failing the test if
// it is not a sequence of strings.
func stringList(t *testing.T, v any, what string) []string {
	t.Helper()
	seq, isSeq := v.([]any)
	if !isSeq {
		t.Fatalf("%s is %T (%v); want a YAML sequence", what, v, v)
	}
	out := make([]string, 0, len(seq))
	for _, e := range seq {
		s, isStr := e.(string)
		if !isStr {
			t.Fatalf("%s has a non-string element %T (%v)", what, e, e)
		}
		out = append(out, s)
	}
	return out
}

// TestHA_K0sCapvEndpointSANOnEveryMember is the regression guard for the failure
// above. The generic k0s template used to gate api.sans on the init role only,
// so a joiner's /etc/k0s/k0s.yaml carried api.address but no api.sans.
func TestHA_K0sCapvEndpointSANOnEveryMember(t *testing.T) {
	cases := []struct {
		name  string
		role  string
		cidrs bool
	}{
		{"init", "init", false},
		{"join", "join", false},
		{"init with cluster CIDRs", "init", true},
		{"join with cluster CIDRs", "join", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := haCPData(tc.role, false)
			d.ManagementEndpoint.ControlPlaneEndpointHost = sansTestEndpoint
			if tc.cidrs {
				d.PodCIDR = "10.244.0.0/16"
				d.ServiceCIDR = "10.96.0.0/12"
			}
			out, err := RenderK0sCloudConfig(d)
			if err != nil {
				t.Fatalf("render: %v", err)
			}

			// The whole cloud-config parses (extractWriteFile fails the test if
			// not), and so does the embedded k0s.yaml.
			doc, ok := k0sConfigSpec(t, out)
			if !ok {
				t.Fatal("HA control-plane render must write /etc/k0s/k0s.yaml")
			}
			if got := mapAt(doc, "kind"); got != "ClusterConfig" {
				t.Errorf("k0s.yaml kind = %v; want ClusterConfig", got)
			}

			sans := stringList(t, mapAt(doc, "spec", "api", "sans"), "spec.api.sans")
			if len(sans) != 1 || sans[0] != sansTestEndpoint {
				t.Errorf("spec.api.sans = %v; want exactly [%s]: every HA member must carry the "+
					"endpoint, because the VIP can fail over to any of them", sans, sansTestEndpoint)
			}

			// The SAN sits next to the per-node advertise and etcd peer addresses
			// it must not disturb, and the optional CIDR block stays well-formed.
			if got := mapAt(doc, "spec", "api", "address"); got != "__API_ADDR__" {
				t.Errorf("spec.api.address = %v; want the __API_ADDR__ sentinel", got)
			}
			if got := mapAt(doc, "spec", "storage", "etcd", "peerAddress"); got != "__ETCD_PEER_ADDR__" {
				t.Errorf("spec.storage.etcd.peerAddress = %v; want the __ETCD_PEER_ADDR__ sentinel", got)
			}
			if tc.cidrs {
				if got := mapAt(doc, "spec", "network", "podCIDR"); got != "10.244.0.0/16" {
					t.Errorf("spec.network.podCIDR = %v; want 10.244.0.0/16", got)
				}
				if got := mapAt(doc, "spec", "network", "serviceCIDR"); got != "10.96.0.0/12" {
					t.Errorf("spec.network.serviceCIDR = %v; want 10.96.0.0/12", got)
				}
			}
		})
	}
}

// TestHA_K0sCapvNoEndpointNoSANs pins the other side of the gate: with no
// control-plane endpoint known there is nothing to put in api.sans, for either
// HA role. The SAN is never invented from the VIP.
func TestHA_K0sCapvNoEndpointNoSANs(t *testing.T) {
	for _, role := range []string{"init", "join"} {
		for _, tc := range []struct {
			name  string
			tweak func(*TemplateData)
		}{
			{"empty endpoint host", func(d *TemplateData) { d.ManagementEndpoint.ControlPlaneEndpointHost = "" }},
			{"nil management endpoint", func(d *TemplateData) { d.ManagementEndpoint = nil }},
		} {
			t.Run(role+"/"+tc.name, func(t *testing.T) {
				d := haCPData(role, false)
				tc.tweak(&d)
				out, err := RenderK0sCloudConfig(d)
				if err != nil {
					t.Fatalf("render: %v", err)
				}
				if strings.Contains(out, "sans:") {
					t.Error("api.sans rendered with no control-plane endpoint; the VIP must not be a fallback")
				}
				if doc, ok := k0sConfigSpec(t, out); ok {
					if got := mapAt(doc, "spec", "api", "sans"); got != nil {
						t.Errorf("spec.api.sans = %v with no control-plane endpoint; want it absent", got)
					}
				}
			})
		}
	}
}

// TestHA_K0sCapvSingleAndWorkerHaveNoAPISANs keeps the non-HA paths as they were:
// a single control plane and a worker render no api section at all (a single
// node has no joiners and no VIP to fail over), even when the same endpoint
// inputs an HA member would use are present. The frozen k0s_capv_single golden
// covers the single path byte-for-byte; this pins the case the golden fixture
// omits, a ManagementEndpoint with an endpoint host set.
func TestHA_K0sCapvSingleAndWorkerHaveNoAPISANs(t *testing.T) {
	withEndpoint := func(d TemplateData) TemplateData {
		d.ManagementEndpoint.ControlPlaneEndpointHost = sansTestEndpoint
		d.PodCIDR = "10.244.0.0/16"
		d.ServiceCIDR = "10.96.0.0/12"
		return d
	}

	single := withEndpoint(haCPData("single", false))
	single.SingleNode = true

	standalone := single
	standalone.K0sSingleNode = true

	// ControlPlaneRole "" is the zero value every pre-HA caller produces.
	unset := single
	unset.ControlPlaneRole = ""

	// A worker ignores whatever control-plane role it was handed.
	worker := withEndpoint(haCPData("join", false))
	worker.Role = "worker"
	worker.WorkerToken = "k0s-worker-join-token"

	for _, tc := range []struct {
		name string
		data TemplateData
	}{
		{"single", single},
		{"single standalone", standalone},
		{"unset role", unset},
		{"worker", worker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := RenderK0sCloudConfig(tc.data)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if strings.Contains(out, "sans:") {
				t.Error("non-HA render must not carry api.sans")
			}
			if strings.Contains(out, "__API_ADDR__\n") {
				t.Error("non-HA render must not carry the api.address sentinel")
			}
			doc, ok := k0sConfigSpec(t, out)
			if tc.data.Role == "worker" {
				if ok {
					t.Error("a worker must not write /etc/k0s/k0s.yaml")
				}
				return
			}
			if !ok {
				t.Fatal("single control plane with cluster CIDRs must still write /etc/k0s/k0s.yaml")
			}
			if got := mapAt(doc, "spec", "api"); got != nil {
				t.Errorf("spec.api = %v on a non-HA control plane; want it absent", got)
			}
			if got := mapAt(doc, "spec", "network", "podCIDR"); got != "10.244.0.0/16" {
				t.Errorf("spec.network.podCIDR = %v; the CIDR-only path must be preserved", got)
			}
		})
	}
}

// TestHA_EndpointSANOnEveryMemberAllTemplates confirms the property across the
// other three control-plane templates, so the generic k0s template cannot drift
// away from them again. For every (distribution x infrastructure) pair, both the
// init node and a joiner must advertise the stable endpoint in the apiserver
// serving cert.
func TestHA_EndpointSANOnEveryMemberAllTemplates(t *testing.T) {
	const lbEndpoint = "10.96.0.10" // haCPData's ControlPlaneLBEndpoint on CAPK

	for _, role := range []string{"init", "join"} {
		t.Run("k0s/capk/"+role, func(t *testing.T) {
			out, err := RenderK0sCloudConfig(haCPData(role, true))
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			doc, ok := k0sConfigSpec(t, out)
			if !ok {
				t.Fatal("k0s CAPK HA render must write /etc/k0s/k0s.yaml")
			}
			sans := stringList(t, mapAt(doc, "spec", "api", "sans"), "spec.api.sans")
			if len(sans) != 1 || sans[0] != lbEndpoint {
				t.Errorf("spec.api.sans = %v; want [%s]", sans, lbEndpoint)
			}
		})

		for _, kubevirt := range []bool{false, true} {
			infra, endpoint := "capv", sansTestEndpoint
			if kubevirt {
				infra, endpoint = "capk", lbEndpoint
			}
			t.Run("k3s/"+infra+"/"+role, func(t *testing.T) {
				d := haCPData(role, kubevirt)
				if !kubevirt {
					d.ManagementEndpoint.ControlPlaneEndpointHost = endpoint
				}
				out, err := RenderK3sCloudConfig(d)
				if err != nil {
					t.Fatalf("render: %v", err)
				}

				var cc map[string]any
				if err := yaml.Unmarshal([]byte(out), &cc); err != nil {
					t.Fatalf("rendered cloud-config is not valid YAML: %v", err)
				}
				args := stringList(t, mapAt(cc, "k3s", "args"), "k3s.args")
				want := "--tls-san=" + endpoint
				if !strSliceHas(args, want) {
					t.Errorf("k3s.args = %v; want it to contain %q", args, want)
				}

				// The config.yaml.d drop-in is the belt-and-braces copy k3s reads
				// on every start, independent of the provider-kairos args plumbing.
				var dropIn map[string]any
				content := extractWriteFile(t, out, "/etc/rancher/k3s/config.yaml.d/91-tls-san.yaml")
				if err := yaml.Unmarshal([]byte(content), &dropIn); err != nil {
					t.Fatalf("91-tls-san.yaml is not valid YAML: %v", err)
				}
				sans := stringList(t, mapAt(dropIn, "tls-san"), "91-tls-san.yaml tls-san")
				if !strSliceHas(sans, endpoint) {
					t.Errorf("91-tls-san.yaml tls-san = %v; want it to contain %q", sans, endpoint)
				}
			})
		}
	}
}

package bootstrap

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestRenderK3sCapvFleetProviderID covers the Kairos fleet (AuroraBoot) providerID
// self-discovery path. A fleet node is claimed from a group after its bootstrap data
// is generated, so the providerID is not known at render time; the node runs
// kairos-fleet-discover-provider-id.sh (before k3s starts) to derive
// kairos-fleet://<node-id> from the phone-home agent's persisted credentials.
//
// Assertions, for both control-plane and worker roles:
//
//	(a) the fleet discovery script is rendered and wired as a k3s ExecStartPre;
//	(b) it reads the persisted credentials, validates the node-id as a UUID, and
//	    writes the kairos-fleet:// kubelet drop-in;
//	(c) the vSphere DMI / hostname discovery does NOT leak into a fleet render;
//	(d) output is valid YAML.
func TestRenderK3sCapvFleetProviderID(t *testing.T) {
	for _, role := range []string{"control-plane", "worker"} {
		t.Run(role, func(t *testing.T) {
			data := TemplateData{
				Role:         role,
				SingleNode:   role == "control-plane",
				Hostname:     "fleet-node-0",
				UserName:     "kairos",
				IsFleet:      true,
				ProviderID:   "", // fleet never has a render-time providerID
				K3sToken:     "test-token",
				K3sServerURL: "https://198.51.100.10:6443",
			}
			result, err := RenderK3sCloudConfig(data)
			if err != nil {
				t.Fatalf("render: %v", err)
			}

			// (a) discovery script present and wired as an ExecStartPre.
			if !strings.Contains(result, "/usr/local/bin/kairos-fleet-discover-provider-id.sh") {
				t.Error("missing fleet discovery script kairos-fleet-discover-provider-id.sh")
			}
			if !strings.Contains(result, "ExecStartPre=/bin/sh -c '/usr/local/bin/kairos-fleet-discover-provider-id.sh || true'") {
				t.Error("fleet discovery script is not wired as a k3s ExecStartPre")
			}

			// (b) reads creds, validates UUID, writes the kairos-fleet:// drop-in.
			if !strings.Contains(result, "/usr/local/.kairos/phonehome-credentials.yaml") {
				t.Error("fleet discovery must read the persisted phone-home credentials")
			}
			if !strings.Contains(result, `'^[0-9a-fA-F]{8}-([0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12}$'`) {
				t.Error("fleet discovery must validate node_id as a UUID (injection guard) before use")
			}
			if !strings.Contains(result, "provider-id=kairos-fleet://") {
				t.Error("fleet discovery must write a kairos-fleet:// kubelet drop-in")
			}
			if !strings.Contains(result, "/etc/rancher/k3s/config.yaml.d") || !strings.Contains(result, "90-provider-id.yaml") {
				t.Error("fleet discovery must write the k3s provider-id drop-in under config.yaml.d")
			}

			// (c) no vSphere/DMI or hostname discovery in a fleet render.
			if strings.Contains(result, "product_uuid") {
				t.Error("vSphere DMI discovery (product_uuid) leaked into a fleet render")
			}
			if strings.Contains(result, "kairos-k3s-discover-provider-id.sh") {
				t.Error("vSphere self-discovery script leaked into a fleet render")
			}
			if strings.Contains(result, "vsphere://") {
				t.Error("vsphere:// providerID leaked into a fleet render")
			}

			// (d) valid YAML.
			var parsed any
			if err := yaml.Unmarshal([]byte(result), &parsed); err != nil {
				t.Errorf("render is not valid YAML: %v", err)
			}
		})
	}
}

// TestRenderK3sCapvFleet_NonFleetUnchanged is the regression guard: a non-fleet CAPV
// control-plane with no render-time providerID must still use the vSphere DMI
// self-discovery and must NOT emit the fleet script.
func TestRenderK3sCapvFleet_NonFleetUnchanged(t *testing.T) {
	data := TemplateData{
		Role:       "control-plane",
		SingleNode: true,
		Hostname:   "capv-cp-0",
		UserName:   "kairos",
		IsFleet:    false,
		ProviderID: "",
	}
	result, err := RenderK3sCloudConfig(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(result, "kairos-fleet-discover-provider-id.sh") {
		t.Error("non-fleet render must NOT emit the fleet discovery script")
	}
	if !strings.Contains(result, "product_uuid") {
		t.Error("non-fleet CAPV control-plane must still use vSphere DMI self-discovery")
	}
}

// TestRenderK0sFleet_FailsLoudly guards against the silent k0s+fleet dead-end: the k0s
// templates have no fleet branch (fleet is k3s-only for now), so a k0s fleet render
// must error rather than fall through to vSphere DMI discovery.
func TestRenderK0sFleet_FailsLoudly(t *testing.T) {
	_, err := RenderK0sCloudConfig(TemplateData{
		Role: "control-plane", SingleNode: true, Hostname: "n0", UserName: "kairos", IsFleet: true,
	})
	if err == nil {
		t.Fatal("expected k0s + fleet render to fail loudly, got nil error")
	}
	if !strings.Contains(err.Error(), "fleet") || !strings.Contains(err.Error(), "k0s") {
		t.Errorf("error should name k0s + fleet, got: %v", err)
	}
}

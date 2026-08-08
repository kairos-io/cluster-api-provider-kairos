package bootstrap

import (
	"os"
	"os/exec"
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

// TestRenderK0sCapvFleetProviderID covers the Kairos fleet (AuroraBoot) providerID
// self-discovery path for k0s. Unlike k3s (which uses one config.yaml.d drop-in for
// both roles), k0s splits by role because it has no config.yaml.d-style pre-start
// kubelet drop-in and workers have no admin.conf:
//
//   - control-plane: derives kairos-fleet://<node-id> from the persisted credentials
//     and patches the Node post-bootstrap via `k0s kubectl` (admin.conf is CP-only);
//   - worker: writes a KubeletConfiguration providerID drop-in the kubelet reads via
//     a STATIC --config-dir arg, before the kubelet registers the Node.
//
// Both derive the node-id from /usr/local/.kairos/phonehome-credentials.yaml, validate
// it as a UUID, and must never leak the vSphere DMI self-discovery.
func TestRenderK0sCapvFleetProviderID(t *testing.T) {
	for _, role := range []string{"control-plane", "worker"} {
		t.Run(role, func(t *testing.T) {
			data := TemplateData{
				Role:        role,
				SingleNode:  role == "control-plane",
				Hostname:    "fleet-node-0",
				UserName:    "kairos",
				IsFleet:     true,
				ProviderID:  "", // fleet never has a render-time providerID
				WorkerToken: "test-token",
			}
			result, err := RenderK0sCloudConfig(data)
			if err != nil {
				t.Fatalf("render: %v", err)
			}

			// (a+b) node-id derived from the persisted credentials, validated as a
			// UUID, and written as a kairos-fleet:// providerID — for both roles.
			if !strings.Contains(result, "/usr/local/.kairos/phonehome-credentials.yaml") {
				t.Error("fleet discovery must read the persisted phone-home credentials")
			}
			if !strings.Contains(result, `'^[0-9a-fA-F]{8}-([0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12}$'`) {
				t.Error("fleet discovery must validate node_id as a UUID (injection guard) before use")
			}
			if !strings.Contains(result, "kairos-fleet://") {
				t.Error("fleet discovery must produce a kairos-fleet:// providerID")
			}

			// (c) no vSphere/DMI self-discovery in a fleet render, for either role.
			if strings.Contains(result, "product_uuid") {
				t.Error("vSphere DMI discovery (product_uuid) leaked into a fleet render")
			}
			if strings.Contains(result, "vsphere://") {
				t.Error("vsphere:// providerID leaked into a fleet render")
			}

			// Role-specific mechanism assertions.
			switch role {
			case "control-plane":
				// CP patches its own Node via the local admin kubeconfig.
				if !strings.Contains(result, "export KUBECONFIG=/var/lib/k0s/pki/admin.conf") {
					t.Error("fleet control-plane must use the local admin kubeconfig for the patch")
				}
				if !strings.Contains(result, "k0s kubectl patch node") {
					t.Error("fleet control-plane must patch the Node providerID via k0s kubectl")
				}
				// The worker-only pre-start mechanism must NOT appear on the CP.
				if strings.Contains(result, "kairos-k0s-fleet-provider-id.sh") {
					t.Error("fleet control-plane must not emit the worker pre-start discovery script")
				}
			case "worker":
				// Worker sets providerID pre-start via a KubeletConfiguration drop-in
				// read through a STATIC --config-dir arg.
				if !strings.Contains(result, "/usr/local/bin/kairos-k0s-fleet-provider-id.sh") {
					t.Error("fleet worker must emit the pre-start discovery script")
				}
				if !strings.Contains(result, "ExecStartPre=/bin/sh -c '/usr/local/bin/kairos-k0s-fleet-provider-id.sh || true'") {
					t.Error("fleet worker discovery script must be wired as a k0sworker ExecStartPre")
				}
				if !strings.Contains(result, "--kubelet-extra-args=--config-dir=/etc/kubernetes/kubelet.conf.d") {
					t.Error("fleet worker must point the kubelet at the config-dir drop-in")
				}
				if !strings.Contains(result, "10-provider-id.conf") {
					t.Error("fleet worker must write the KubeletConfiguration providerID drop-in")
				}
				if !strings.Contains(result, "kind: KubeletConfiguration") {
					t.Error("fleet worker drop-in must be a KubeletConfiguration")
				}
				// Never pin an empty --provider-id= (would never match kairos-fleet://).
				if strings.Contains(result, "--kubelet-extra-args=--provider-id=") {
					t.Error("fleet worker must not emit a --provider-id kubelet arg (empty or otherwise)")
				}
				// The CP-only kubectl-patch mechanism must NOT appear on the worker.
				if strings.Contains(result, "k0s kubectl patch node") {
					t.Error("fleet worker must not run the control-plane kubectl-patch path")
				}
			}

			// (d) valid YAML.
			var parsed any
			if err := yaml.Unmarshal([]byte(result), &parsed); err != nil {
				t.Errorf("render is not valid YAML: %v", err)
			}
		})
	}
}

// TestRenderK0sCapvFleet_BashSyntax runs `bash -n` on the k0s fleet shell that only
// executes at node boot: the control-plane's inline discovery+patch (inside the
// post-bootstrap script) and the worker's kairos-k0s-fleet-provider-id.sh. The
// in-script UUID validation (`grep -qE`) never runs at unit-test time, so a syntax
// regression there would otherwise stay invisible until lab provisioning.
func TestRenderK0sCapvFleet_BashSyntax(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available; skipping rendered-script syntax check")
	}
	cases := []struct {
		name        string
		role        string
		script      string // write_files path suffix of the script to syntax-check
		mustContain string // marker proving we extracted the fleet variant
	}{
		{"control-plane", "control-plane", "kairos-k0s-post-bootstrap.sh", "fleet: discovered providerID"},
		{"worker", "worker", "kairos-k0s-fleet-provider-id.sh", "phonehome-credentials.yaml"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			out, err := RenderK0sCloudConfig(TemplateData{
				Role:        tc.role,
				SingleNode:  tc.role == "control-plane",
				Hostname:    "fleet-node-0",
				UserName:    "kairos",
				IsFleet:     true,
				WorkerToken: "test-token",
			})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			script := extractWriteFile(t, out, tc.script)
			if script == "" {
				t.Fatalf("%s script (%s) not found in rendered write_files", tc.name, tc.script)
			}
			if !strings.Contains(script, tc.mustContain) {
				t.Fatalf("extracted %s does not contain %q; may have extracted the wrong file", tc.script, tc.mustContain)
			}
			f := filepathJoinTemp(t, "kairos-k0s-fleet-"+tc.name+".sh")
			if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
				t.Fatalf("write temp script: %v", err)
			}
			if o, err := exec.Command(bashPath, "-n", f).CombinedOutput(); err != nil {
				t.Fatalf("bash -n on rendered %s failed: %v\n%s", tc.script, err, o)
			}
		})
	}
}

// TestRenderK0sCapvFleet_NonFleetUnchanged is the regression guard: a non-fleet CAPV
// k0s control-plane with no render-time providerID must still use the vSphere DMI
// self-discovery and must NOT emit any fleet discovery.
func TestRenderK0sCapvFleet_NonFleetUnchanged(t *testing.T) {
	data := TemplateData{
		Role:       "control-plane",
		SingleNode: true,
		Hostname:   "capv-cp-0",
		UserName:   "kairos",
		IsFleet:    false,
		ProviderID: "",
	}
	result, err := RenderK0sCloudConfig(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(result, "kairos-k0s-fleet-provider-id.sh") {
		t.Error("non-fleet render must NOT emit the fleet discovery script")
	}
	if strings.Contains(result, "kairos-fleet://") {
		t.Error("non-fleet render must NOT emit a kairos-fleet:// providerID")
	}
	if !strings.Contains(result, "product_uuid") {
		t.Error("non-fleet CAPV control-plane must still use vSphere DMI self-discovery")
	}
}

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

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

func dispatchCPData(kubevirt bool) TemplateData {
	d := TemplateData{
		Role:       "control-plane",
		SingleNode: true,
		Hostname:   "kairos-cp-0",
		UserName:   "kairos",
		UserGroups: []string{"admin"},
		GitHubUser: "testuser",
		ProviderID: "vsphere://vm-uuid",
		IsKubeVirt: kubevirt,
	}
	if kubevirt {
		d.ProviderID = "kubevirt://kairos-cp-0"
		d.ControlPlaneLBEndpoint = "10.96.0.10"
	}
	return d
}

// dispatchKubeadmWorkerData is the worker fixture for the kubeadm template, which
// is worker-only (a kubeadm control-plane render is refused). The JoinConfiguration
// and providerID patch are stand-in YAML; the controller produces the real values
// via the version-aware marshaller.
func dispatchKubeadmWorkerData(kubevirt bool) TemplateData {
	d := TemplateData{
		Role:       "worker",
		Hostname:   "kairos-worker-0",
		UserName:   "kairos",
		UserGroups: []string{"admin"},
		GitHubUser: "testuser",
		ProviderID: "vsphere://vm-uuid",
		IsKubeVirt: kubevirt,
		Kubeadm: &KubeadmTemplateData{
			JoinConfiguration: "apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\nnodeRegistration:\n  name: kairos-worker-0\n",
			ProviderIDPatch:   "providerID: \"vsphere://vm-uuid\"\n",
			KubernetesVersion: "v1.30.0",
		},
	}
	if kubevirt {
		d.ProviderID = "kubevirt://kairos-worker-0"
		d.Kubeadm.ProviderIDPatch = "providerID: \"kubevirt://kairos-worker-0\"\n"
	}
	return d
}

// dispatchDataFor returns the render fixture appropriate to the distribution:
// worker data for kubeadm (worker-only), control-plane data for k0s/k3s.
func dispatchDataFor(dist string, kubevirt bool) TemplateData {
	if dist == bootstrapv1beta2.DistributionKubeadm {
		return dispatchKubeadmWorkerData(kubevirt)
	}
	return dispatchCPData(kubevirt)
}

// TestRender_UnknownDistribution pins the unsupported-distribution error.
func TestRender_UnknownDistribution(t *testing.T) {
	_, err := Render("foo", dispatchCPData(false))
	if err == nil {
		t.Fatal("Render(foo) returned nil error; expected unsupported-distribution error")
	}
	if got, want := err.Error(), "unsupported distribution: foo"; got != want {
		t.Errorf("Render(foo) error = %q; want %q", got, want)
	}
}

// TestRender_KubeadmControlPlaneRefused pins the render-time refusal of a kubeadm
// control-plane config (ADR 0010 P1 item 5): the P1 seam resolves worker join
// material only and has no control-plane hook.
func TestRender_KubeadmControlPlaneRefused(t *testing.T) {
	data := dispatchCPData(false) // Role: control-plane
	_, err := Render(bootstrapv1beta2.DistributionKubeadm, data)
	if err == nil {
		t.Fatal("Render(kubeadm, control-plane) returned nil error; expected a refusal")
	}
	if !strings.Contains(err.Error(), "not supported for control-plane") {
		t.Errorf("Render(kubeadm, control-plane) error = %q; want a control-plane refusal", err.Error())
	}
}

// TestRender_KubeadmWorker proves a kubeadm worker renders and carries the join
// configuration, the providerID patch, the marker, and the SSH-enable stage.
func TestRender_KubeadmWorker(t *testing.T) {
	out, err := Render(bootstrapv1beta2.DistributionKubeadm, dispatchKubeadmWorkerData(false))
	if err != nil {
		t.Fatalf("Render(kubeadm, worker): %v", err)
	}
	for _, want := range []string{
		"kind: JoinConfiguration",
		"providerID: \"vsphere://vm-uuid\"",
		"kairos-kubeadm-post-bootstrap.service",
		"systemctl enable --now sshd",
		"kubeadm join",
		// Boot-mode / idempotence gate (ADR 0010 "Node side", KD-59).
		"ConditionPathExists=|/run/cos/active_mode",
		"ConditionPathExists=|/run/cos/passive_mode",
		"ConditionPathExists=!/run/cos/in_ram_mode",
		"ConditionPathIsMountPoint=/etc/kubernetes",
		"ConditionPathIsMountPoint=/var/lib/kubelet",
		// Persistent attempted/completion markers + credential-only cleanup.
		"MARKER_DIR=/var/lib/kairos",
		"kairos-kubeadm-join.attempted",
		"kairos-kubeadm-join.completed",
		"rm -f /etc/kubernetes/bootstrap-kubelet.conf",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("kubeadm worker render missing %q", want)
		}
	}
}

// TestRender_MatchesWrappers proves the permanent wrappers and Render produce
// byte-identical output for both infra paths (OQ-E).
func TestRender_MatchesWrappers(t *testing.T) {
	cases := []struct {
		dist    string
		wrapper func(TemplateData) (string, error)
	}{
		{bootstrapv1beta2.DistributionK0s, RenderK0sCloudConfig},
		{bootstrapv1beta2.DistributionK3s, RenderK3sCloudConfig},
	}
	for _, tc := range cases {
		tc := tc
		for _, kubevirt := range []bool{false, true} {
			t.Run(tc.dist, func(t *testing.T) {
				data := dispatchCPData(kubevirt)
				viaRender, err := Render(tc.dist, data)
				if err != nil {
					t.Fatalf("Render(%s): %v", tc.dist, err)
				}
				viaWrapper, err := tc.wrapper(data)
				if err != nil {
					t.Fatalf("wrapper(%s): %v", tc.dist, err)
				}
				if viaRender != viaWrapper {
					t.Errorf("Render(%s) and its wrapper diverge (kubevirt=%v)", tc.dist, kubevirt)
				}
			})
		}
	}
}

// TestProviderIDMarker pins the per-distribution markers and the unknown case.
func TestProviderIDMarker(t *testing.T) {
	cases := []struct {
		dist   string
		marker string
		ok     bool
	}{
		{bootstrapv1beta2.DistributionK0s, "kairos-k0s-post-bootstrap.service", true},
		{bootstrapv1beta2.DistributionK3s, "kairos-k3s-post-bootstrap.service", true},
		{bootstrapv1beta2.DistributionKubeadm, "kairos-kubeadm-post-bootstrap.service", true},
		{"foo", "", false},
	}
	for _, tc := range cases {
		marker, ok := ProviderIDMarker(tc.dist)
		if marker != tc.marker || ok != tc.ok {
			t.Errorf("ProviderIDMarker(%q) = (%q,%v); want (%q,%v)", tc.dist, marker, ok, tc.marker, tc.ok)
		}
	}
}

// TestDistributionTemplatesTableAgrees asserts the renderer table has exactly the
// api-supported distributions, each with a working Render and an ok
// ProviderIDMarker — no missing rows, no extra keys.
func TestDistributionTemplatesTableAgrees(t *testing.T) {
	supported := bootstrapv1beta2.SupportedDistributions()
	if len(distributionTemplates) != len(supported) {
		t.Errorf("distributionTemplates has %d keys; SupportedDistributions() has %d", len(distributionTemplates), len(supported))
	}
	for _, d := range supported {
		if _, ok := distributionTemplates[d]; !ok {
			t.Errorf("distributionTemplates missing a row for supported distribution %q", d)
			continue
		}
		if _, err := Render(d, dispatchDataFor(d, false)); err != nil {
			t.Errorf("Render(%q) failed: %v", d, err)
		}
		if _, ok := ProviderIDMarker(d); !ok {
			t.Errorf("ProviderIDMarker(%q) returned ok=false", d)
		}
	}
	for k := range distributionTemplates {
		if !bootstrapv1beta2.IsSupportedDistribution(k) {
			t.Errorf("distributionTemplates has extra key %q not in SupportedDistributions()", k)
		}
	}
}

// TestRenderContract (S2) asserts every distribution and infra variant, rendered
// with a providerID, contains its post-bootstrap marker and an SSH-enable stage —
// the substrings the controller's regeneration heuristic depends on (KD-9).
func TestRenderContract(t *testing.T) {
	for _, dist := range bootstrapv1beta2.SupportedDistributions() {
		for _, kubevirt := range []bool{false, true} {
			dist, kubevirt := dist, kubevirt
			t.Run(dist, func(t *testing.T) {
				data := dispatchDataFor(dist, kubevirt)
				out, err := Render(dist, data)
				if err != nil {
					t.Fatalf("Render(%s, kubevirt=%v): %v", dist, kubevirt, err)
				}
				marker, _ := ProviderIDMarker(dist)
				if !strings.Contains(out, marker) {
					t.Errorf("Render(%s, kubevirt=%v) missing providerID marker %q", dist, kubevirt, marker)
				}
				if !strings.Contains(out, "systemctl enable --now sshd") &&
					!strings.Contains(out, "systemctl enable --now ssh") {
					t.Errorf("Render(%s, kubevirt=%v) missing SSH-enable stage", dist, kubevirt)
				}
			})
		}
	}
}

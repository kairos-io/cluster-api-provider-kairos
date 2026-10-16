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
)

// kubeVIPPodSpec is the slice of the kube-vip DaemonSet pod spec the
// local-election tests assert on.
type kubeVIPPodSpec struct {
	ServiceAccountName string `yaml:"serviceAccountName"`
	HostNetwork        bool   `yaml:"hostNetwork"`
	Containers         []struct {
		Env []struct {
			Name  string `yaml:"name"`
			Value string `yaml:"value"`
		} `yaml:"env"`
		VolumeMounts []map[string]any `yaml:"volumeMounts"`
	} `yaml:"containers"`
	Volumes []map[string]any `yaml:"volumes"`
}

// TestKubeVIP_ElectsThroughLocalAPIServer pins ADR 0014 PR-A on every rendered
// kube-vip manifest: the container reaches its own Node's apiserver at
// 127.0.0.1:6443 through the in-cluster ServiceAccount, and nothing mounts
// /etc/kubernetes/admin.conf (a regular file at that path would switch kube-vip
// to its admin.conf branch, which ignores the two variables).
//
// It covers both distributions, both HA roles, and the Metal3 flavour, and
// checks that the manifest is still the full four-document set with the
// ServiceAccount and the lease RBAC from the earlier kube-vip fixes.
func TestKubeVIP_ElectsThroughLocalAPIServer(t *testing.T) {
	cases := []struct {
		name   string
		render func(TemplateData) (string, error)
		role   string
		metal3 bool
	}{
		{"k0s_init", RenderK0sCloudConfig, "init", false},
		{"k0s_join", RenderK0sCloudConfig, "join", false},
		{"k0s_metal3_init", RenderK0sCloudConfig, "init", true},
		{"k3s_init", RenderK3sCloudConfig, "init", false},
		{"k3s_join", RenderK3sCloudConfig, "join", false},
		{"k3s_metal3_join", RenderK3sCloudConfig, "join", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := haCPData(tc.role, false)
			d.Metal3 = tc.metal3
			out, err := tc.render(d)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			manifest := extractWriteFile(t, out, "kube-vip.yaml")
			if manifest == "" {
				t.Fatalf("no kube-vip manifest rendered")
			}

			// The whole multi-document set parses and keeps its shape.
			docs := decodeKubeVIPDocs(t, manifest)
			for _, kind := range []string{"ServiceAccount", "ClusterRole", "ClusterRoleBinding", "DaemonSet"} {
				if _, ok := docs[kind]; !ok {
					t.Errorf("kube-vip manifest missing %s document", kind)
				}
			}

			// No reference to the host kubeconfig survives anywhere in the set.
			if strings.Contains(manifest, "admin.conf") {
				t.Errorf("kube-vip manifest still references admin.conf:\n%s", manifest)
			}

			var ds struct {
				Spec struct {
					Template struct {
						Spec kubeVIPPodSpec `yaml:"spec"`
					} `yaml:"template"`
				} `yaml:"spec"`
			}
			remarshalInto(t, docs["DaemonSet"], &ds)
			ps := ds.Spec.Template.Spec

			if len(ps.Containers) != 1 {
				t.Fatalf("DaemonSet has %d containers, want 1", len(ps.Containers))
			}
			if len(ps.Volumes) != 0 {
				t.Errorf("DaemonSet declares volumes %v; none expected", ps.Volumes)
			}
			if len(ps.Containers[0].VolumeMounts) != 0 {
				t.Errorf("kube-vip container declares volumeMounts %v; none expected", ps.Containers[0].VolumeMounts)
			}
			if !ps.HostNetwork {
				t.Error("DaemonSet must stay hostNetwork: 127.0.0.1 has to be the Node's loopback")
			}
			if ps.ServiceAccountName != "kube-vip" {
				t.Errorf("serviceAccountName=%q, want kube-vip (the in-cluster identity that holds the lease RBAC)", ps.ServiceAccountName)
			}

			// Each variable appears exactly once, with the loopback value.
			count := map[string]int{}
			env := map[string]string{}
			for _, e := range ps.Containers[0].Env {
				count[e.Name]++
				env[e.Name] = e.Value
			}
			for name, want := range map[string]string{
				"KUBERNETES_SERVICE_HOST": "127.0.0.1",
				"KUBERNETES_SERVICE_PORT": "6443",
			} {
				if count[name] != 1 {
					t.Errorf("env %s appears %d times, want exactly 1", name, count[name])
				}
				if env[name] != want {
					t.Errorf("env %s=%q, want %q", name, env[name], want)
				}
			}

			// The earlier kube-vip fixes stay intact: the ClusterRole still grants
			// the lease verbs leader election needs, and the binding still targets
			// the kube-vip ServiceAccount.
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
					strSliceHas(r.Verbs, "get") && strSliceHas(r.Verbs, "update") && strSliceHas(r.Verbs, "create") {
					leaseRule = true
				}
			}
			if !leaseRule {
				t.Error("kube-vip ClusterRole lost the coordination.k8s.io/leases get+update+create rule")
			}
			var crb struct {
				Subjects []struct {
					Kind      string `yaml:"kind"`
					Name      string `yaml:"name"`
					Namespace string `yaml:"namespace"`
				} `yaml:"subjects"`
			}
			remarshalInto(t, docs["ClusterRoleBinding"], &crb)
			if len(crb.Subjects) != 1 || crb.Subjects[0].Kind != "ServiceAccount" ||
				crb.Subjects[0].Name != "kube-vip" || crb.Subjects[0].Namespace != "kube-system" {
				t.Errorf("kube-vip ClusterRoleBinding subjects=%+v, want ServiceAccount kube-system/kube-vip", crb.Subjects)
			}
		})
	}
}

// TestKubeVIP_LocalElectionIsStatic proves the two apiserver variables are fixed
// strings: an operator-supplied VIP block cannot change them, in either mode.
func TestKubeVIP_LocalElectionIsStatic(t *testing.T) {
	for _, mode := range []string{"ARP", "BGP", ""} {
		mode := mode
		t.Run("mode="+mode, func(t *testing.T) {
			out, err := kubeVIPManifest(&VIPConfig{Address: "10.9.8.7", Interface: "ens192", Mode: mode})
			if err != nil {
				t.Fatalf("kubeVIPManifest: %v", err)
			}
			env := kubeVIPEnv(t, out)
			if env["KUBERNETES_SERVICE_HOST"] != "127.0.0.1" || env["KUBERNETES_SERVICE_PORT"] != "6443" {
				t.Errorf("apiserver env = %q:%q, want 127.0.0.1:6443",
					env["KUBERNETES_SERVICE_HOST"], env["KUBERNETES_SERVICE_PORT"])
			}
			if strings.Contains(out, "volumes") || strings.Contains(out, "hostPath") || strings.Contains(out, "admin.conf") {
				t.Errorf("manifest still carries a volume or admin.conf reference:\n%s", out)
			}
		})
	}
}

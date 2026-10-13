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
	"testing"

	yaml "gopkg.in/yaml.v3"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// stagedResolvConf is where the DNS step writes, before a rename installs it
// as /etc/resolv.conf. A rename replaces a symlink instead of writing through it.
const stagedResolvConf = "/etc/.kairos-capi-resolv.conf"

// TestRender_DNSResolversBeforeRuncmd pins that spec.dnsServers is applied in
// boot.before. yip turns runcmd into the first step of the boot stage, ahead
// of every stages.boot step, so a DNS step in boot is written only after
// runcmd has started the join or post-bootstrap unit.
func TestRender_DNSResolversBeforeRuncmd(t *testing.T) {
	withDNS := func(d TemplateData) TemplateData {
		d.DNSServers = []string{"192.0.2.53"}
		return d
	}
	worker := func(d TemplateData) TemplateData {
		d.Role = "worker"
		return d
	}
	cases := []struct {
		name string
		dist string
		data TemplateData
	}{
		{"kubeadm capv", bootstrapv1beta2.DistributionKubeadm, dispatchKubeadmWorkerData(false)},
		{"kubeadm capk", bootstrapv1beta2.DistributionKubeadm, dispatchKubeadmWorkerData(true)},
		{"k3s capv control-plane", bootstrapv1beta2.DistributionK3s, dispatchCPData(false)},
		{"k3s capk control-plane", bootstrapv1beta2.DistributionK3s, dispatchCPData(true)},
		{"k3s capv worker", bootstrapv1beta2.DistributionK3s, worker(dispatchCPData(false))},
		{"k3s capk worker", bootstrapv1beta2.DistributionK3s, worker(dispatchCPData(true))},
		{"k0s capv control-plane", bootstrapv1beta2.DistributionK0s, dispatchCPData(false)},
		{"k0s capk control-plane", bootstrapv1beta2.DistributionK0s, dispatchCPData(true)},
		{"k0s capv worker", bootstrapv1beta2.DistributionK0s, worker(dispatchCPData(false))},
		{"k0s capk worker", bootstrapv1beta2.DistributionK0s, worker(dispatchCPData(true))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Render(tc.dist, withDNS(tc.data))
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			var cc struct {
				Stages map[string][]map[string]any `yaml:"stages"`
			}
			if err := yaml.Unmarshal([]byte(stripCloudConfigHeader(out)), &cc); err != nil {
				t.Fatalf("parse cloud-config: %v", err)
			}
			for _, step := range cc.Stages["boot"] {
				if _, ok := step["dns"]; ok {
					t.Errorf("boot stage step %q sets DNS; it runs after runcmd", step["name"])
				}
			}
			steps := cc.Stages["boot.before"]
			dnsAt := -1
			for i, step := range steps {
				if dns, ok := step["dns"].(map[string]any); ok {
					dnsAt = i
					if ns, _ := dns["nameservers"].([]any); len(ns) != 1 || ns[0] != "192.0.2.53" {
						t.Errorf("boot.before DNS nameservers = %v, want [192.0.2.53]", dns["nameservers"])
					}
					if dns["path"] != stagedResolvConf {
						t.Errorf("DNS step writes %v, want the staged %s", dns["path"], stagedResolvConf)
					}
				}
			}
			if dnsAt < 0 {
				t.Fatal("no DNS step in boot.before")
			}
			wantMove := "mv -f " + stagedResolvConf + " /etc/resolv.conf"
			moved := false
			for _, step := range steps[dnsAt+1:] {
				cmds, _ := step["commands"].([]any)
				for _, c := range cmds {
					if c == wantMove {
						moved = true
					}
				}
			}
			if !moved {
				t.Errorf("no %q step after the DNS step in boot.before", wantMove)
			}
		})
	}
}

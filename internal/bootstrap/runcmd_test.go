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
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// execLine matches any Exec*= command line of a unit (ExecStart, ExecStartPre,
// ExecStartPost, ExecReload, ...).
var execLine = regexp.MustCompile(`^\s*Exec[A-Za-z]*=(.*)$`)

// systemctlStart matches a systemctl call that starts a unit.
var systemctlStart = regexp.MustCompile(`\bsystemctl\b.*(\s--now\b|\s(re)?start\s)`)

// TestRender_PostBootstrapStartDoesNotBlock pins that runcmd, and any unit that
// starts another unit, only queue a oneshot unit from the same cloud-config.
// The post-bootstrap units are oneshots that can retry for minutes, and
// systemctl without --no-block waits for a oneshot to finish, which holds the
// cloud-config stage for that long, so users and SSH keys are not applied
// until it ends.
func TestRender_PostBootstrapStartDoesNotBlock(t *testing.T) {
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
			out, err := Render(tc.dist, tc.data)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			var cc struct {
				RunCmd     []string `yaml:"runcmd"`
				WriteFiles []struct {
					Path    string `yaml:"path"`
					Content string `yaml:"content"`
				} `yaml:"write_files"`
			}
			if err := yaml.Unmarshal([]byte(stripCloudConfigHeader(out)), &cc); err != nil {
				t.Fatalf("parse cloud-config: %v", err)
			}
			oneshot := map[string]bool{}
			commands := append([]string(nil), cc.RunCmd...)
			for _, f := range cc.WriteFiles {
				if !strings.HasSuffix(f.Path, ".service") {
					continue
				}
				if strings.Contains(f.Content, "Type=oneshot") {
					oneshot[path.Base(f.Path)] = true
				}
				for _, line := range strings.Split(f.Content, "\n") {
					if m := execLine.FindStringSubmatch(line); m != nil {
						commands = append(commands, m[1])
					}
				}
			}
			started := 0
			for _, c := range commands {
				if !systemctlStart.MatchString(c) || !startsOneOf(c, oneshot) {
					continue
				}
				started++
				if !strings.Contains(c, "--no-block") {
					t.Errorf("%q waits for a oneshot unit to finish; add --no-block", c)
				}
			}
			if started == 0 {
				t.Fatal("no systemctl start of a oneshot unit found")
			}
		})
	}
}

func startsOneOf(command string, units map[string]bool) bool {
	for _, f := range strings.Fields(command) {
		if units[f] {
			return true
		}
	}
	return false
}

// TestRender_K0sLBSANsWaitsForPostBootstrap pins that the CAPK k0s lb-sans
// update cannot start while post-bootstrap runs. kairos-k0s-lb-sans.sh removes
// the apiserver cert and restarts k0scontroller, which must not overlap
// k0scontroller's HA etcd join, the apply-args join branch or
// post-bootstrap's own edits of k0s.yaml. The service is ordered after
// post-bootstrap, and its path unit is started by the systemctl call that
// queues post-bootstrap, after it in the argument list: an ordering only
// binds jobs that are queued, and systemctl starts its arguments in order, so
// a path listed first, or started on its own, could fire before the
// post-bootstrap job exists.
func TestRender_K0sLBSANsWaitsForPostBootstrap(t *testing.T) {
	out, err := Render(bootstrapv1beta2.DistributionK0s, dispatchCPData(true))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cc struct {
		RunCmd     []string `yaml:"runcmd"`
		WriteFiles []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
	}
	if err := yaml.Unmarshal([]byte(stripCloudConfigHeader(out)), &cc); err != nil {
		t.Fatalf("parse cloud-config: %v", err)
	}
	units := map[string]string{}
	for _, f := range cc.WriteFiles {
		units[path.Base(f.Path)] = f.Content
	}
	lbSANs, ok := units["kairos-k0s-lb-sans.service"]
	if !ok {
		t.Fatal("no kairos-k0s-lb-sans.service in the CAPK control-plane render")
	}
	ordered := false
	for _, line := range strings.Split(lbSANs, "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "After="); ok &&
			slices.Contains(strings.Fields(after), "kairos-k0s-post-bootstrap.service") {
			ordered = true
		}
	}
	if !ordered {
		t.Error("kairos-k0s-lb-sans.service is not ordered after kairos-k0s-post-bootstrap.service")
	}
	for _, c := range cc.RunCmd {
		if strings.Contains(c, "kairos-k0s-lb-sans.path") && systemctlStart.MatchString(c) {
			t.Errorf("runcmd starts the lb-sans path on its own: %q", c)
		}
	}
	together := false
	for _, line := range strings.Split(units["kairos-k0s-post-bootstrap-enable.service"], "\n") {
		m := execLine.FindStringSubmatch(line)
		if m == nil || !systemctlStart.MatchString(m[1]) {
			continue
		}
		fields := strings.Fields(m[1])
		pb := slices.Index(fields, "kairos-k0s-post-bootstrap.service")
		lb := slices.Index(fields, "kairos-k0s-lb-sans.path")
		if pb >= 0 && lb >= 0 {
			together = true
			if lb < pb {
				t.Errorf("the helper starts kairos-k0s-lb-sans.path before kairos-k0s-post-bootstrap.service: %q", m[1])
			}
		}
	}
	if !together {
		t.Error("the helper does not start kairos-k0s-lb-sans.path in the systemctl call that queues kairos-k0s-post-bootstrap.service")
	}
}

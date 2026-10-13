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
	cases := []struct {
		name string
		dist string
		data TemplateData
	}{
		{"kubeadm capv", bootstrapv1beta2.DistributionKubeadm, dispatchKubeadmWorkerData(false)},
		{"kubeadm capk", bootstrapv1beta2.DistributionKubeadm, dispatchKubeadmWorkerData(true)},
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

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
	"regexp"
	"strings"
	"testing"
)

// providerIDDropInWrite matches a shell redirect into a providerID drop-in
// that k3s or the kubelet reads, or into a temporary name every writer shares.
var providerIDDropInWrite = regexp.MustCompile(`>\s*"?\S*/(90-provider-id\.yaml|10-provider-id\.conf)(\.tmp)?"?'?\s*$`)

// TestTemplates_ProviderIDDropInWrittenAtomically pins that the discovery
// scripts write the providerID drop-ins to a per-process temporary file and
// rename it into place. The boot stage and the unit's ExecStartPre run the
// same script concurrently, and a reader must never see the file truncated
// mid-write. An ExecStartPre that writes a static providerID itself is exempt:
// the other writer of that drop-in, write_files, lands before runcmd starts k3s,
// and the ExecStartPre finishes before k3s reads it.
func TestTemplates_ProviderIDDropInWrittenAtomically(t *testing.T) {
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := templateFS.ReadFile("templates/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "ExecStartPre=") {
				continue
			}
			if providerIDDropInWrite.MatchString(line) {
				t.Errorf("%s:%d writes the drop-in in place or through a shared name; write a .tmp.$$ file and mv it: %s", e.Name(), i+1, strings.TrimSpace(line))
			}
		}
	}
}

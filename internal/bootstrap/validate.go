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
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// providerIDPattern restricts ProviderID to the shape every CAPI infrastructure
// provider uses: `<scheme>://<opaque-path>`. The scheme is a Kubernetes-style
// identifier (letter then letter/digit/`.`/`+`/`-`); the path allows only
// alphanumerics, dot, dash, underscore, slash, colon — no shell metacharacters,
// no quotes, no whitespace, no semicolons.
//
// ProviderID is NOT a free-form user-provided string. It is propagated by the
// infrastructure provider (CAPV/CAPK/CAPD/...) onto the CAPI Machine spec, and
// our bootstrap renderer interpolates it into a systemd `ExecStartPre=` shell
// command:
//
//	kubectl patch node $(hostname) -p '{"spec":{"providerID":"<here>"}}'
//
// Any character that escapes the shell context here becomes remote code
// execution as root on first boot. We validate at render time as a defensive
// gate even though the infra provider is the originator.
var providerIDPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*://[a-zA-Z0-9._/:\-]+$`)

// ValidateProviderID returns nil if s is empty or matches providerIDPattern.
// Empty is allowed because not every render path sets ProviderID (workers and
// CAPV control planes during early reconcile both render with ProviderID="").
//
// On failure, callers should treat the error as a render-time validation
// failure and surface it as a status condition on the KairosConfig rather
// than retrying — a malformed ProviderID never becomes well-formed by retry.
func ValidateProviderID(s string) error {
	if s == "" {
		return nil
	}
	if !providerIDPattern.MatchString(s) {
		return fmt.Errorf("providerID %q does not match required pattern %q", s, providerIDPattern.String())
	}
	return nil
}

// validateTemplateData runs all render-time invariants on TemplateData. It is
// called from RenderK0sCloudConfig and RenderK3sCloudConfig before Execute, so
// bad inputs fail loudly with a clear message instead of producing broken
// userdata that a node executes silently at boot.
//
// The single common rule across every string field below: no newlines, no
// carriage returns, no NUL. Those characters would either (a) inject extra
// keys into the rendered YAML, (b) break the YAML block-scalar / list-item
// embedding of the value, or (c) escape the shell context for fields that
// also appear inside file-content block scalars or systemd ExecStartPre
// directives.
func validateTemplateData(d *TemplateData) error {
	var errs []error
	if err := ValidateProviderID(d.ProviderID); err != nil {
		errs = append(errs, err)
	}
	// Generic "no control characters" check for fields that are interpolated
	// into either YAML scalars OR block-scalar/shell contexts. The list below
	// is the union of every string field on TemplateData (excluding ProviderID
	// which has its own strict pattern check above).
	stringFields := []struct{ name, value string }{
		{"hostname", d.Hostname},
		{"hostnamePrefix", d.HostnamePrefix},
		{"userName", d.UserName},
		{"userPassword", d.UserPassword},
		{"gitHubUser", d.GitHubUser},
		{"sshPublicKey", d.SSHPublicKey},
		{"workerToken", d.WorkerToken},
		{"k3sServerURL", d.K3sServerURL},
		{"k3sToken", d.K3sToken},
		{"podCIDR", d.PodCIDR},
		{"serviceCIDR", d.ServiceCIDR},
		{"primaryIP", d.PrimaryIP},
		{"machineName", d.MachineName},
		{"clusterNS", d.ClusterNS},
		{"controlPlaneLBServiceName", d.ControlPlaneLBServiceName},
		{"controlPlaneLBServiceNamespace", d.ControlPlaneLBServiceNamespace},
		{"controlPlaneLBEndpoint", d.ControlPlaneLBEndpoint},
	}
	for _, f := range stringFields {
		if err := rejectControlChars(f.name, f.value); err != nil {
			errs = append(errs, err)
		}
	}
	// ManagementEndpoint is an optional nested struct; only when set do we
	// check its fields. The nested-field names below mirror the JSON-style
	// dotted form so the error message points the user back to the resolver
	// output that produced it.
	if d.ManagementEndpoint != nil {
		nested := []struct{ name, value string }{
			{"managementEndpoint.apiServer", d.ManagementEndpoint.APIServer},
			{"managementEndpoint.token", d.ManagementEndpoint.Token},
			{"managementEndpoint.kubeconfigSecretName", d.ManagementEndpoint.KubeconfigSecretName},
			{"managementEndpoint.kubeconfigSecretNamespace", d.ManagementEndpoint.KubeconfigSecretNamespace},
		}
		for _, f := range nested {
			if err := rejectControlChars(f.name, f.value); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for i, g := range d.UserGroups {
		if err := rejectControlChars(fmt.Sprintf("userGroups[%d]", i), g); err != nil {
			errs = append(errs, err)
		}
	}
	for i, dns := range d.DNSServers {
		if err := rejectControlChars(fmt.Sprintf("dnsServers[%d]", i), dns); err != nil {
			errs = append(errs, err)
		}
	}
	// Files: validate path/permissions/owner for control characters and absolute path
	// requirement. Content is deliberately NOT checked for control characters —
	// file content legitimately contains newlines. yaml.v3 picks a safe block-scalar
	// form for multi-line Content, so there is no injection surface in the YAML output.
	for i, f := range d.Files {
		fIdx := fmt.Sprintf("files[%d]", i)
		if err := rejectControlChars(fIdx+".path", f.Path); err != nil {
			errs = append(errs, err)
		} else if !strings.HasPrefix(f.Path, "/") {
			errs = append(errs, fmt.Errorf("%s.path: must be absolute (starts with '/'): %q", fIdx, f.Path))
		} else {
			// Belt-and-braces ".." traversal check behind the webhook.
			for _, seg := range strings.Split(f.Path, "/") {
				if seg == ".." {
					errs = append(errs, fmt.Errorf("%s.path: must not contain '..' path segments: %q", fIdx, f.Path))
					break
				}
			}
		}
		if f.Permissions != "" {
			if err := rejectControlChars(fIdx+".permissions", f.Permissions); err != nil {
				errs = append(errs, err)
			} else if !filePermissionsPattern.MatchString(f.Permissions) {
				errs = append(errs, fmt.Errorf("%s.permissions: must be an octal string (e.g. \"0644\"): %q", fIdx, f.Permissions))
			}
		}
		if f.Owner != "" {
			if err := rejectControlChars(fIdx+".owner", f.Owner); err != nil {
				errs = append(errs, err)
			} else if !fileOwnerPattern.MatchString(f.Owner) {
				errs = append(errs, fmt.Errorf("%s.owner: must be user or user:group with POSIX username chars (e.g. \"root:root\"): %q", fIdx, f.Owner))
			}
		}
	}
	return errors.Join(errs...)
}

// filePermissionsPattern matches octal file-permission strings.
// Mirrors the kubebuilder marker on File.Permissions.
var filePermissionsPattern = regexp.MustCompile(`^0?[0-7]{3,4}$`)

// fileOwnerPattern matches POSIX user or user:group strings.
// Mirrors the kubebuilder marker on File.Owner.
var fileOwnerPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*(:[a-z_][a-z0-9_-]*)?$`)

// rejectControlChars returns an error if s contains any character that would
// break the YAML or shell embed of s downstream: `\n`, `\r`, or NUL. Every
// other byte is allowed — character-class validation is the webhook layer's
// job, not the renderer's.
func rejectControlChars(field, s string) error {
	for i, r := range s {
		switch r {
		case '\n', '\r', 0x00:
			return fmt.Errorf("%s: contains forbidden control character (byte 0x%02x at offset %d)", field, r, i)
		}
	}
	return nil
}

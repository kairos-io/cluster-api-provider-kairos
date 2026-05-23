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
)

// TestValidate_ManagementEndpointControlChars asserts that a control character
// in any of the four ManagementEndpoint nested fields is rejected at render
// time with an error message that names the offending nested field. These are
// the gate-controlled fields the CAPK push block injects into shell scripts;
// a stray `\n`/`\r`/NUL would break the YAML block scalar or escape the
// shell-string-literal context inside `local foo='...'`.
//
// The renderer's reject is the secondary control — the primary control is the
// shquote envelope on each field at the template site (see CLAUDE.md
// § "Shell contexts"). This test only verifies the secondary control.
func TestValidate_ManagementEndpointControlChars(t *testing.T) {
	const payload = "evil\nshell"

	cases := []struct {
		name        string
		ep          *ManagementEndpoint
		wantInError string
	}{
		{
			name: "APIServer",
			ep: &ManagementEndpoint{
				APIServer:                 "https://1.2.3.4:6443" + payload,
				Token:                     "ok",
				KubeconfigSecretName:      "ok",
				KubeconfigSecretNamespace: "ok",
			},
			wantInError: "managementEndpoint.apiServer",
		},
		{
			name: "Token",
			ep: &ManagementEndpoint{
				APIServer:                 "https://1.2.3.4:6443",
				Token:                     "tok" + payload,
				KubeconfigSecretName:      "ok",
				KubeconfigSecretNamespace: "ok",
			},
			wantInError: "managementEndpoint.token",
		},
		{
			name: "KubeconfigSecretName",
			ep: &ManagementEndpoint{
				APIServer:                 "https://1.2.3.4:6443",
				Token:                     "ok",
				KubeconfigSecretName:      "sec" + payload,
				KubeconfigSecretNamespace: "ok",
			},
			wantInError: "managementEndpoint.kubeconfigSecretName",
		},
		{
			name: "KubeconfigSecretNamespace",
			ep: &ManagementEndpoint{
				APIServer:                 "https://1.2.3.4:6443",
				Token:                     "ok",
				KubeconfigSecretName:      "ok",
				KubeconfigSecretNamespace: "ns" + payload,
			},
			wantInError: "managementEndpoint.kubeconfigSecretNamespace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := TemplateData{
				Role:               "control-plane",
				SingleNode:         true,
				UserName:           "kairos",
				IsKubeVirt:         true,
				ManagementEndpoint: tc.ep,
			}
			_, err := RenderK0sCloudConfig(data)
			if err == nil {
				t.Fatalf("expected render error for control char in %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantInError) {
				t.Errorf("error %q does not mention expected field name %q", err.Error(), tc.wantInError)
			}
		})
	}
}

// TestValidate_NilManagementEndpointSkipsNestedCheck asserts that a nil
// ManagementEndpoint pointer does NOT trigger the nested-field control-char
// check. The CAPV path and worker path never populate ManagementEndpoint;
// asserting that the nested-field walk is gated by the nil check guards
// against an accidental dereference (which would surface as a panic, not a
// validation error).
func TestValidate_NilManagementEndpointSkipsNestedCheck(t *testing.T) {
	data := TemplateData{
		Role:               "control-plane",
		SingleNode:         true,
		UserName:           "kairos",
		IsKubeVirt:         false,
		ManagementEndpoint: nil,
	}
	if _, err := RenderK0sCloudConfig(data); err != nil {
		t.Fatalf("render with nil ManagementEndpoint should not error: %v", err)
	}
}

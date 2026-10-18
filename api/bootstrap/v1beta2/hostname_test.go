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

package v1beta2

import "testing"

func TestRenderedHostname(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    *KairosConfigSpec
		machine string
		want    string
	}{
		{"spec.hostname wins", &KairosConfigSpec{Hostname: "cp-host"}, "kcp-1", "cp-host"},
		{"else the Machine name", &KairosConfigSpec{}, "kcp-1", "kcp-1"},
		{"the prefix is not a hostname", &KairosConfigSpec{HostnamePrefix: "metal-"}, "kcp-1", "kcp-1"},
		{"no spec", nil, "kcp-1", "kcp-1"},
		{"no Machine", &KairosConfigSpec{}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderedHostname(tc.spec, tc.machine); got != tc.want {
				t.Fatalf("RenderedHostname() = %q, want %q", got, tc.want)
			}
		})
	}
}

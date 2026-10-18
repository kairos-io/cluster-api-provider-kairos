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

import (
	"context"
	"strings"
	"testing"
)

func TestNodeHostnameErrors(t *testing.T) {
	for _, h := range []string{"cp-0", "kcp-abcde", "a", strings.Repeat("a", 63)} {
		if errs := NodeHostnameErrors(h); errs != nil {
			t.Errorf("%q: unexpected errors %v", h, errs)
		}
	}
	for _, h := range []string{
		"", "CP-0", "cp_0", "cp.example.com", "-cp", "cp-", strings.Repeat("a", 64),
		"metal-{{ trunc 4 .MachineID }}", "{{x}}",
	} {
		if ValidNodeHostname(h) {
			t.Errorf("%q must be rejected", h)
		}
	}
}

// TestKairosConfig_Webhook_Hostname: spec.hostname must be a lowercase
// DNS-1123 label with no template markup on create, and on an update that
// changes it. An update that leaves an older, now-invalid hostname alone is
// accepted, so existing objects keep working.
func TestKairosConfig_Webhook_Hostname(t *testing.T) {
	v := &kairosConfigValidator{}
	ctx := context.Background()
	with := func(h string) *KairosConfig {
		kc := newValidKairosConfig()
		kc.Spec.Hostname = h
		return kc
	}

	for _, h := range []string{"", "cp-host"} {
		if _, err := v.ValidateCreate(ctx, with(h)); err != nil {
			t.Errorf("create with hostname %q: %v", h, err)
		}
	}
	for _, h := range []string{"CP-Host", "cp.example.com", "metal-{{ trunc 4 .MachineID }}"} {
		_, err := v.ValidateCreate(ctx, with(h))
		if err == nil || !strings.Contains(err.Error(), "spec.hostname") {
			t.Errorf("create with hostname %q: want a spec.hostname error, got %v", h, err)
		}
	}

	legacy := with("Legacy.Host")
	updated := legacy.DeepCopy()
	updated.Labels = map[string]string{"touched": "yes"}
	if _, err := v.ValidateUpdate(ctx, legacy, updated); err != nil {
		t.Errorf("an update that keeps an old hostname must pass: %v", err)
	}
	changed := legacy.DeepCopy()
	changed.Spec.Hostname = "Other.Host"
	if _, err := v.ValidateUpdate(ctx, legacy, changed); err == nil || !strings.Contains(err.Error(), "spec.hostname") {
		t.Errorf("an update that changes the hostname to an invalid one must fail, got %v", err)
	}
	fixed := legacy.DeepCopy()
	fixed.Spec.Hostname = "fixed-host"
	if _, err := v.ValidateUpdate(ctx, legacy, fixed); err != nil {
		t.Errorf("an update to a valid hostname must pass: %v", err)
	}
}

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

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
	"testing"

	. "github.com/onsi/gomega"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// TestBootstrapDistributionsTableAgrees asserts the controller distribution table
// has exactly the api-supported distributions, each with a non-nil
// fillTemplateData and builtinJoin — no missing rows, no extra keys.
func TestBootstrapDistributionsTableAgrees(t *testing.T) {
	g := NewWithT(t)
	supported := bootstrapv1beta2.SupportedDistributions()
	g.Expect(bootstrapDistributions).To(HaveLen(len(supported)))
	for _, d := range supported {
		row, ok := bootstrapDistributions[d]
		g.Expect(ok).To(BeTrue(), "controller table missing a row for %q", d)
		g.Expect(row.fillTemplateData).NotTo(BeNil(), "row %q has nil fillTemplateData", d)
		g.Expect(row.builtinJoin).NotTo(BeNil(), "row %q has nil builtinJoin", d)
	}
	for k := range bootstrapDistributions {
		g.Expect(bootstrapv1beta2.IsSupportedDistribution(k)).To(BeTrue(), "controller table has extra key %q", k)
	}
}

// TestRenderCarriesProviderID pins the regeneration predicate, including the
// unknown-distribution fallback to the default marker (the pre-seam else-branch).
func TestRenderCarriesProviderID(t *testing.T) {
	const pid = "vsphere://x"
	k0sMarker := "kairos-k0s-post-bootstrap.service"
	k3sMarker := "kairos-k3s-post-bootstrap.service"

	cases := []struct {
		name       string
		dist       string
		cloud      string
		wantID     bool
		wantMarker bool
	}{
		{"k0s both present", "k0s", pid + " " + k0sMarker, true, true},
		{"k0s marker missing", "k0s", pid, true, false},
		{"k0s id missing", "k0s", k0sMarker, false, true},
		{"empty uses k0s marker", "", pid + " " + k0sMarker, true, true},
		{"k3s both present", "k3s", pid + " " + k3sMarker, true, true},
		{"k3s with k0s marker is not a match", "k3s", pid + " " + k0sMarker, true, false},
		{"unknown falls back to default (k0s) marker", "rke2", pid + " " + k0sMarker, true, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			hasID, hasMarker := renderCarriesProviderID(tc.dist, tc.cloud, pid)
			g.Expect(hasID).To(Equal(tc.wantID))
			g.Expect(hasMarker).To(Equal(tc.wantMarker))
		})
	}
}

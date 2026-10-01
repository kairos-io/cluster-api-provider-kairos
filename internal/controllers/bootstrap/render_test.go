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
// fillTemplateData — no missing rows, no extra keys. builtinJoin is non-nil for
// every distribution EXCEPT kubeadm, whose worker JoinMaterialSource needs
// dependencies only main.go has and is injected via JoinSources (ADR 0010 P1 N2).
func TestBootstrapDistributionsTableAgrees(t *testing.T) {
	g := NewWithT(t)
	supported := bootstrapv1beta2.SupportedDistributions()
	g.Expect(bootstrapDistributions).To(HaveLen(len(supported)))
	for _, d := range supported {
		row, ok := bootstrapDistributions[d]
		g.Expect(ok).To(BeTrue(), "controller table missing a row for %q", d)
		g.Expect(row.fillTemplateData).NotTo(BeNil(), "row %q has nil fillTemplateData", d)
		if d == bootstrapv1beta2.DistributionKubeadm {
			g.Expect(row.builtinJoin).To(BeNil(), "kubeadm must have a nil builtinJoin (injected via JoinSources)")
		} else {
			g.Expect(row.builtinJoin).NotTo(BeNil(), "row %q has nil builtinJoin", d)
		}
	}
	for k := range bootstrapDistributions {
		g.Expect(bootstrapv1beta2.IsSupportedDistribution(k)).To(BeTrue(), "controller table has extra key %q", k)
	}
}

// TestJoinSourceFor_KubeadmHardFails pins N2: a kubeadm worker with no injected
// JoinSources entry hard-fails instead of silently falling back to the k0s
// built-in join source.
func TestJoinSourceFor_KubeadmHardFails(t *testing.T) {
	g := NewWithT(t)
	r := &KairosConfigReconciler{} // no JoinSources wired
	src, err := r.joinSourceFor(bootstrapv1beta2.DistributionKubeadm)
	g.Expect(err).To(HaveOccurred())
	g.Expect(src).To(BeNil())
	g.Expect(err.Error()).To(ContainSubstring("no worker join source configured for distribution \"kubeadm\""))

	// k0s/k3s still resolve to their built-ins with no JoinSources.
	for _, d := range []string{bootstrapv1beta2.DistributionK0s, bootstrapv1beta2.DistributionK3s} {
		s, err := r.joinSourceFor(d)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(s).NotTo(BeNil())
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

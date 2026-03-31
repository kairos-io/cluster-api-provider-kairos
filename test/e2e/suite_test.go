/*
Copyright 2026 The Kairos CAPI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OF ANY KIND, either express or implied. See the
License for the specific language governing permissions and limitations
under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e suite in short mode")
	}
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cluster API Provider Kairos E2E Suite")
}

// SuiteTools is kubectl/kind/clusterctl from the tools dir after BeforeSuite runs.
var SuiteTools Tools

// BeforeSuite only runs when Ginkgo has at least one selected spec (no specs ⇒ skipped entirely).
var _ = BeforeSuite(func(ctx context.Context) {
	By("preparing e2e CLI binaries — kubectl, kind, clusterctl (first run downloads can take several minutes)")
	By("resolving e2e tools install directory (E2E_TOOLS_DIR or <repo>/.e2e-bin)")
	dir, err := DefaultToolsDir()
	Expect(err).NotTo(HaveOccurred())

	deps, err := DefaultBinaryDeps()
	Expect(err).NotTo(HaveOccurred())

	for _, name := range []string{"kubectl", "kind", "clusterctl"} {
		spec, ok := deps[name]
		Expect(ok).To(BeTrue(), "catalog should define %s", name)
		By(fmt.Sprintf("ensuring %q — skip download if file exists and SHA256 matches", name))
		Expect(EnsureBinaries(dir, map[string]BinarySpec{name: spec}, nil)).To(Succeed())
	}

	By("kubectl, kind, and clusterctl are on disk")
	SuiteTools = Tools{Dir: dir}
}, NodeTimeout(15*time.Minute))

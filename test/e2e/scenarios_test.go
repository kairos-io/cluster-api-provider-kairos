/*
Copyright 2026 The Kairos CAPI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR ANY KIND, either express or implied.
See the License for the specific language governing permissions and limitations
under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kairos-io/kairos-capi/internal/kubevirtenv"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	kairosProviderNamespace     = "kairos-capi-system"
	kairosControllerDeployName  = "kairos-capi-controller-manager"
	kairosManagerRolloutTimeout = "8m"
)

// ginkgoKubevirtLogger routes kubevirtenv output to Ginkgo (By for steps, GinkgoWriter for detail).
type ginkgoKubevirtLogger struct{}

func (ginkgoKubevirtLogger) Step(msg string) { By(msg) }

func (ginkgoKubevirtLogger) Infof(format string, args ...any) {
	_, _ = fmt.Fprintf(GinkgoWriter, format+"\n", args...)
}

func (ginkgoKubevirtLogger) Warnf(format string, args ...any) {
	_, _ = fmt.Fprintf(GinkgoWriter, "[warn] "+format+"\n", args...)
}

func (ginkgoKubevirtLogger) WriteString(s string) {
	_, _ = fmt.Fprint(GinkgoWriter, s)
}

// mgmtCluster is set in BeforeAll for specs in this Ordered block.
var mgmtCluster *PreparedKindCluster

var _ = Describe("Cluster API Provider Kairos", Ordered, func() {
	var (
		repoRoot    string
		workDir     string
		clusterName string
		kubeconfig  string
		dockerExe   string
		stackEnv    *kubevirtenv.Environment
	)

	BeforeAll(func(ctx context.Context) {
		By("provisioning management kind cluster (shared kubevirtenv.RunDevManagementStack — CLIs in workDir/bin)")
		var err error

		By("resolving repository root (directory containing go.mod)")
		repoRoot, err = RepoRoot()
		Expect(err).NotTo(HaveOccurred())

		workDir = GinkgoT().TempDir()
		clusterName = fmt.Sprintf("kcapk-%d", GinkgoRandomSeed())
		kubeconfig = filepath.Join(workDir, "kubeconfig")
		dockerExe = DockerExeFromEnvOrInput("")

		kindWait := os.Getenv("E2E_KIND_CREATE_WAIT")
		if kindWait == "" {
			kindWait = "15m"
		}

		stackEnv = &kubevirtenv.Environment{
			ClusterName:         clusterName,
			WorkDir:             workDir,
			RepoRoot:            repoRoot,
			DockerExe:           dockerExe,
			Logger:              ginkgoKubevirtLogger{},
			ClusterctlExtraPath: filepath.Join(repoRoot, "bin"),
			KindCreateWait:      kindWait,
			Stdout:              GinkgoWriter,
			Stderr:              GinkgoWriter,
		}

		Expect(os.MkdirAll(workDir, 0o755)).To(Succeed())
		Expect(kubevirtenv.RunDevManagementStack(ctx, stackEnv)).To(Succeed())

		_, err = os.Stat(kubeconfig)
		Expect(err).NotTo(HaveOccurred())
		mgmtCluster = &PreparedKindCluster{
			ClusterName: clusterName,
			Kubeconfig:  kubeconfig,
			ImageRef:    DevControllerImageRef,
		}

		By("kubectl sanity check against management API")
		Expect(stackEnv.KubectlAPIHealth(ctx, stackEnv.KubeconfigPath())).To(Succeed())
	}, NodeTimeout(120*time.Minute))

	AfterAll(func(ctx context.Context) {
		if stackEnv == nil || clusterName == "" {
			return
		}
		By(fmt.Sprintf("deleting kind cluster %q", clusterName))
		teardown := &kubevirtenv.Environment{
			ClusterName: clusterName,
			WorkDir:     workDir,
			Logger:      ginkgoKubevirtLogger{},
			KindPath:    stackEnv.KindPath,
			Stdout:      GinkgoWriter,
			Stderr:      GinkgoWriter,
		}
		_ = teardown.DeleteKindCluster(ctx)
	})

	It("deploys the Kairos provider on the management cluster and creates a single-node workload cluster on KubeVirt", func(ctx context.Context) {
		Expect(mgmtCluster).NotTo(BeNil())

		By(fmt.Sprintf("kubectl rollout status deployment/%s -n %s", kairosControllerDeployName, kairosProviderNamespace))
		Expect(stackEnv.KubectlRolloutDeployment(ctx, mgmtCluster.Kubeconfig, kairosProviderNamespace, kairosControllerDeployName, kairosManagerRolloutTimeout)).To(Succeed())

		By("creating a single-node workload cluster on KubeVirt")
		Skip("KubeVirt workload cluster not implemented yet (needs DataVolume/Kairos image pipeline like kubevirt-env full setup)")
	})
})

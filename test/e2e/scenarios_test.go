/*
Copyright 2026 The Kairos CAPI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR ANY KIND, either express or implied. See the
License for the specific language governing permissions and limitations
under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	kairosProviderNamespace     = "kairos-capi-system"
	kairosControllerDeployName  = "kairos-capi-controller-manager"
	kairosManagerRolloutTimeout = "8m"
)

// mgmtCluster is set in BeforeAll for specs in this Ordered block.
var mgmtCluster *PreparedKindCluster

var _ = Describe("Cluster API Provider Kairos", Ordered, func() {
	var (
		repoRoot       string
		workDir        string
		clusterName    string
		kubeconfig     string
		dockerExe      string
		kindConfigPath string
	)

	BeforeAll(func(ctx context.Context) {
		By("provisioning management kind cluster with Calico + CDI + KubeVirt, Cluster API (kubeadm) + CAPK, cert-manager, then kairos-capi from config/dev")
		var err error

		By("resolving repository root (directory containing go.mod)")
		repoRoot, err = RepoRoot()
		Expect(err).NotTo(HaveOccurred())

		workDir = GinkgoT().TempDir()
		clusterName = fmt.Sprintf("kcapk-%d", GinkgoRandomSeed())
		kubeconfig = filepath.Join(workDir, "kubeconfig")
		dockerExe = DockerExeFromEnvOrInput("")
		kindConfigPath = filepath.Join(workDir, "kind-config.yaml")

		By("creating per-run work directory for kubeconfig and kind config")
		Expect(os.MkdirAll(workDir, 0o755)).To(Succeed())

		By("ensuring ~/.docker/config.json exists and writing kind cluster config (disable default CNI; mount docker config)")
		dockerCfg, err := EnsureDockerConfigJSON()
		Expect(err).NotTo(HaveOccurred())
		Expect(WriteKindClusterConfigKubeVirt(kindConfigPath, dockerCfg)).To(Succeed())

		By(fmt.Sprintf("kind create cluster — name=%q with kubevirt-style config (Calico installed next)", clusterName))
		Expect(KindCreateClusterWithConfig(ctx, SuiteTools, clusterName, kubeconfig, kindConfigPath)).To(Succeed())
		By("kind control plane is ready")

		By("installing local-path provisioner and default StorageClass")
		Expect(KubectlApplyURL(ctx, SuiteTools, kubeconfig, kubeVirtE2ELocalPathProvisioner)).To(Succeed())
		Expect(KubectlRolloutDeployment(ctx, SuiteTools, kubeconfig, "local-path-storage", "local-path-provisioner", "5m")).To(Succeed())
		Expect(KubectlMarkLocalPathDefaultStorageClass(ctx, SuiteTools, kubeconfig)).To(Succeed())

		calicoURL := fmt.Sprintf(kubeVirtE2ECalicoManifestURL, kubeVirtE2ECalicoVersion)
		By("installing Calico CNI")
		Expect(KubectlApplyURL(ctx, SuiteTools, kubeconfig, calicoURL)).To(Succeed())
		Expect(KubectlRolloutDeployment(ctx, SuiteTools, kubeconfig, "kube-system", "calico-kube-controllers", "5m")).To(Succeed())
		Expect(KubectlRolloutDaemonSet(ctx, SuiteTools, kubeconfig, "kube-system", "calico-node", "5m")).To(Succeed())

		By("installing CDI")
		Expect(KubectlApplyURL(ctx, SuiteTools, kubeconfig, kubeVirtE2ECDIOperatorURL)).To(Succeed())
		Expect(KubectlApplyURL(ctx, SuiteTools, kubeconfig, kubeVirtE2ECDICRURL)).To(Succeed())
		Expect(KubectlRolloutDeployment(ctx, SuiteTools, kubeconfig, "cdi", "cdi-operator", "5m")).To(Succeed())

		By(fmt.Sprintf("installing KubeVirt %s", kubeVirtE2EKubeVirtVersion))
		Expect(KubectlApplyURL(ctx, SuiteTools, kubeconfig, fmt.Sprintf(kubeVirtE2EKubeVirtOperatorURL, kubeVirtE2EKubeVirtVersion))).To(Succeed())
		Expect(KubectlApplyURL(ctx, SuiteTools, kubeconfig, fmt.Sprintf(kubeVirtE2EKubeVirtCRURL, kubeVirtE2EKubeVirtVersion))).To(Succeed())
		Expect(KubectlPatchKubeVirtUseEmulation(ctx, SuiteTools, kubeconfig)).To(Succeed())
		Expect(KubectlRolloutDeployment(ctx, SuiteTools, kubeconfig, "kubevirt", "virt-operator", "10m")).To(Succeed())
		Expect(KubectlWait(ctx, SuiteTools, kubeconfig, "10m", "--for=condition=Available", "kubevirts.kubevirt.io/kubevirt", "-n", "kubevirt")).To(Succeed())

		_, err = os.Stat(kubeconfig)
		Expect(err).NotTo(HaveOccurred())
		mgmtCluster = &PreparedKindCluster{
			ClusterName: clusterName,
			Kubeconfig:  kubeconfig,
			ImageRef:    DevControllerImageRef,
		}

		By("kubectl sanity check against management API")
		Expect(KubectlAPIHealth(ctx, SuiteTools, mgmtCluster.Kubeconfig)).To(Succeed())

		capiVer, err := ClusterctlPinnedVersion()
		Expect(err).NotTo(HaveOccurred())
		By(fmt.Sprintf("clusterctl init — core + kubeadm @ %s (no infrastructure yet)", capiVer))
		Expect(ClusterctlInitKubeadmCore(ctx, SuiteTools, mgmtCluster.Kubeconfig, capiVer)).To(Succeed())
		Expect(KubectlRolloutDeployment(ctx, SuiteTools, kubeconfig, "capi-system", "capi-controller-manager", "10m")).To(Succeed())

		By("clusterctl init — KubeVirt / CAPK infrastructure provider")
		Expect(ClusterctlInitKubeVirtInfra(ctx, SuiteTools, mgmtCluster.Kubeconfig)).To(Succeed())
		Expect(KubectlRolloutDeployment(ctx, SuiteTools, kubeconfig, "capk-system", "capk-controller-manager", "10m")).To(Succeed())

		By(fmt.Sprintf("docker build -t %q and kind load (kairos-capi controller)", DevControllerImageRef))
		Expect(DockerBuildController(ctx, dockerExe, repoRoot, DevControllerImageRef)).To(Succeed())
		Expect(KindLoadDockerImage(ctx, SuiteTools, clusterName, DevControllerImageRef)).To(Succeed())

		certURL := fmt.Sprintf(kubeVirtE2ECertManagerURL, kubeVirtE2ECertManagerVersion)
		By("installing cert-manager (webhooks for kairos-capi)")
		Expect(KubectlApplyURL(ctx, SuiteTools, kubeconfig, certURL)).To(Succeed())
		Expect(KubectlRolloutDeployment(ctx, SuiteTools, kubeconfig, "cert-manager", "cert-manager", "5m")).To(Succeed())
		Expect(KubectlRolloutDeployment(ctx, SuiteTools, kubeconfig, "cert-manager", "cert-manager-webhook", "5m")).To(Succeed())
		Expect(KubectlRolloutDeployment(ctx, SuiteTools, kubeconfig, "cert-manager", "cert-manager-cainjector", "5m")).To(Succeed())

		By("kustomize build config/dev | kubectl apply -f")
		Expect(ApplyKairosCapiDevManifests(ctx, SuiteTools, repoRoot, mgmtCluster.Kubeconfig)).To(Succeed())
		By("kairos-capi dev stack applied (osbuilder and Kairos image uploads are not in this flow yet — see kubevirt-env setup for parity)")
	}, NodeTimeout(120*time.Minute))

	AfterAll(func(ctx context.Context) {
		if clusterName == "" {
			return
		}
		By(fmt.Sprintf("deleting kind cluster %q", clusterName))
		_ = KindDeleteCluster(ctx, SuiteTools, clusterName)
	})

	It("deploys the Kairos provider on the management cluster and creates a single-node workload cluster on KubeVirt", func(ctx context.Context) {
		Expect(mgmtCluster).NotTo(BeNil())

		By(fmt.Sprintf("kubectl rollout status deployment/%s -n %s", kairosControllerDeployName, kairosProviderNamespace))
		Expect(KubectlRolloutDeployment(ctx, SuiteTools, mgmtCluster.Kubeconfig, kairosProviderNamespace, kairosControllerDeployName, kairosManagerRolloutTimeout)).To(Succeed())

		By("creating a single-node workload cluster on KubeVirt")
		Skip("KubeVirt workload cluster not implemented yet (needs DataVolume/Kairos image pipeline like kubevirt-env)")
	})
})

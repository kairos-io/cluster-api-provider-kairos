package kubevirtenv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IsCAPIInstalled reports whether capi-controller-manager is available.
func (e *Environment) IsCAPIInstalled() bool {
	clientset, err := e.Clientset()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deployment, err := clientset.AppsV1().Deployments("capi-system").Get(ctx, "capi-controller-manager", metav1.GetOptions{})
	if err != nil {
		return false
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// IsCAPKInstalled reports whether capk-controller-manager is available.
func (e *Environment) IsCAPKInstalled() bool {
	clientset, err := e.Clientset()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deployment, err := clientset.AppsV1().Deployments("capk-system").Get(ctx, "capk-controller-manager", metav1.GetOptions{})
	if err != nil {
		return false
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// InstallCAPIKubeadmCore runs clusterctl init for core + kubeadm bootstrap + kubeadm control-plane at CAPIVersion.
func (e *Environment) InstallCAPIKubeadmCore(ctx context.Context) error {
	log := e.log()
	if e.IsCAPIInstalled() {
		log.Step("Cluster API (CAPI) is already installed ✓")
		return nil
	}
	ver := e.capiVersionOrDefault()
	log.Infof("Installing Cluster API %s (kubeadm bootstrap + control-plane)...", ver)
	args := []string{
		"init",
		"--kubeconfig", e.KubeconfigPath(),
		"--core", "cluster-api:" + ver,
		"--bootstrap", "kubeadm:" + ver,
		"--control-plane", "kubeadm:" + ver,
	}
	cmd := exec.CommandContext(ctx, e.clusterctlBin(), args...)
	cmd.Env = e.clusterctlEnv()
	stdout, stderr := e.execOut()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clusterctl init: %w", err)
	}
	log.Step("Waiting for CAPI components to be ready...")
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	clientset, err := e.Clientset()
	if err != nil {
		return err
	}
	if err := WaitForDeployment(waitCtx, clientset, "capi-system", "capi-controller-manager"); err != nil {
		log.Warnf("CAPI core controller may not be fully ready: %v", err)
	}
	log.Step("Cluster API (CAPI) installed ✓")
	return nil
}

// capkInfraArg resolves the --infrastructure value for clusterctl.
func (e *Environment) capkInfraArg() string {
	if e.CAPKInfra != "" {
		return e.CAPKInfra
	}
	if v := strings.TrimSpace(os.Getenv("E2E_CLUSTERCTL_INFRA")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("CAPK_VERSION")); v != "" {
		return "kubevirt:" + v
	}
	return "kubevirt"
}

// InstallCAPK runs clusterctl init --infrastructure for KubeVirt (CAPK).
func (e *Environment) InstallCAPK(ctx context.Context) error {
	log := e.log()
	if e.IsCAPKInstalled() {
		log.Step("CAPK is already installed ✓")
		return nil
	}
	infra := e.capkInfraArg()
	log.Infof("Installing CAPK (infrastructure %q)...", infra)
	args := []string{"init", "--kubeconfig", e.KubeconfigPath(), "--infrastructure", infra}
	cmd := exec.CommandContext(ctx, e.clusterctlBin(), args...)
	cmd.Env = e.clusterctlEnv()
	stdout, stderr := e.execOut()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clusterctl init --infrastructure: %w", err)
	}
	log.Step("Waiting for CAPK infrastructure controller...")
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	clientset, err := e.Clientset()
	if err != nil {
		return err
	}
	if err := WaitForDeployment(waitCtx, clientset, "capk-system", "capk-controller-manager"); err != nil {
		log.Warnf("CAPK controller may not be fully ready: %v", err)
		log.Infof("Check: kubectl get pods -n capk-system")
	} else {
		log.Infof("✓ CAPK infrastructure controller is ready")
	}
	log.Step("CAPK installed ✓")
	return nil
}

// UninstallCAPI runs clusterctl delete --all (removes CAPI and related providers).
func (e *Environment) UninstallCAPI(ctx context.Context) error {
	log := e.log()
	if !e.IsCAPIInstalled() {
		log.Infof("Cluster API (CAPI) is not installed")
		return nil
	}
	log.Step("Uninstalling Cluster API...")
	cmd := exec.CommandContext(ctx, e.clusterctlBin(), "delete", "--all", "--kubeconfig", e.KubeconfigPath())
	cmd.Env = e.clusterctlEnv()
	stdout, stderr := e.execOut()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clusterctl delete --all: %w", err)
	}
	log.Step("Cluster API (CAPI) uninstalled ✓")
	return nil
}

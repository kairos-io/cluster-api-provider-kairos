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

// clusterctlInitArgs builds the argument list for a single `clusterctl init`.
//
// Both CAPI and CAPK are requested in one invocation on purpose. clusterctl
// bootstraps cert-manager itself on every init, and before installing it probes
// for an existing install with one non-retried attempt that creates and then
// deletes a cert-manager-test namespace. Two inits run back to back make the
// second probe race the first probe's namespace teardown; when it loses,
// clusterctl reinstalls cert-manager over the running one and blocks for its
// 10 minute timeout, which is long enough to kill the e2e job. One init has no
// second probe. See kairos-io/kairos#5501.
//
// needCAPI and needCAPK let a developer re-run against a cluster that already
// carries one of the two: whatever is already there is left out of the command.
func clusterctlInitArgs(kubeconfig, capiVersion, infra string, needCAPI, needCAPK bool) []string {
	args := []string{"init", "--kubeconfig", kubeconfig}
	if needCAPI {
		args = append(args,
			"--core", "cluster-api:"+capiVersion,
			"--bootstrap", "kubeadm:"+capiVersion,
			"--control-plane", "kubeadm:"+capiVersion,
		)
	}
	if needCAPK {
		args = append(args, "--infrastructure", infra)
	}
	return args
}

// InstallCAPIAndCAPK runs one `clusterctl init` for Cluster API core, the kubeadm
// bootstrap and control-plane providers, and the KubeVirt infrastructure provider.
// Whichever of the two is already installed is skipped; if both are, it is a no-op.
func (e *Environment) InstallCAPIAndCAPK(ctx context.Context) error {
	log := e.log()
	needCAPI := !e.IsCAPIInstalled()
	needCAPK := !e.IsCAPKInstalled()
	if !needCAPI {
		log.Step("Cluster API (CAPI) is already installed ✓")
	}
	if !needCAPK {
		log.Step("CAPK is already installed ✓")
	}
	if !needCAPI && !needCAPK {
		return nil
	}
	if needCAPI && e.CAPIVersion == "" {
		return fmt.Errorf("Environment.CAPIVersion is empty (call EnsurePinnedCLIs first)")
	}
	infra := e.capkInfraArg()
	switch {
	case needCAPI && needCAPK:
		log.Infof("Installing Cluster API %s (kubeadm bootstrap + control-plane) and CAPK (infrastructure %q)...", e.CAPIVersion, infra)
	case needCAPI:
		log.Infof("Installing Cluster API %s (kubeadm bootstrap + control-plane)...", e.CAPIVersion)
	default:
		log.Infof("Installing CAPK (infrastructure %q)...", infra)
	}

	args := clusterctlInitArgs(e.KubeconfigPath(), e.CAPIVersion, infra, needCAPI, needCAPK)
	cmd := exec.CommandContext(ctx, e.clusterctlBin(), args...)
	cmd.Env = e.clusterctlEnv()
	stdout, stderr := e.execOut()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clusterctl init: %w", err)
	}

	log.Step("Waiting for CAPI and CAPK components to be ready...")
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	clientset, err := e.Clientset()
	if err != nil {
		return err
	}
	if needCAPI {
		if err := WaitForDeployment(waitCtx, clientset, "capi-system", "capi-controller-manager"); err != nil {
			log.Warnf("CAPI core controller may not be fully ready: %v", err)
		}
	}
	if needCAPK {
		if err := WaitForDeployment(waitCtx, clientset, "capk-system", "capk-controller-manager"); err != nil {
			log.Warnf("CAPK controller may not be fully ready: %v", err)
			log.Infof("Check: kubectl get pods -n capk-system")
		} else {
			log.Infof("✓ CAPK infrastructure controller is ready")
		}
	}
	log.Step("Cluster API (CAPI) and CAPK installed ✓")
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

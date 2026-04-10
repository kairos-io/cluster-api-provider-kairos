package kubevirtenv

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1api "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	kairosOperatorNamespace       = "operator-system"
	kairosOperatorDeploymentName  = "operator-kairos-operator"
	kairosOperatorNginxDeployment = "nginx"
)

func deploymentAvailable(d *appsv1.Deployment) bool {
	if d == nil {
		return false
	}
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// IsKairosOperatorStackInstalled reports whether the Kairos operator, OSArtifact CRD, and nginx upload service are ready.
func (e *Environment) IsKairosOperatorStackInstalled() bool {
	clientset, err := e.Clientset()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	op, err := clientset.AppsV1().Deployments(kairosOperatorNamespace).Get(ctx, kairosOperatorDeploymentName, metav1.GetOptions{})
	if err != nil || !deploymentAvailable(op) {
		return false
	}
	ngx, err := clientset.AppsV1().Deployments("default").Get(ctx, kairosOperatorNginxDeployment, metav1.GetOptions{})
	if err != nil || !deploymentAvailable(ngx) {
		return false
	}
	cfg, err := e.RESTConfig()
	if err != nil {
		return false
	}
	ext, err := apiextensionsv1.NewForConfig(cfg)
	if err != nil {
		return false
	}
	_, err = ext.CustomResourceDefinitions().Get(ctx, "osartifacts.build.kairos.io", metav1.GetOptions{})
	return err == nil
}

// InstallKairosOperator installs the Kairos operator and nginx artifact sink using upstream kustomize bases
// (see https://kairos.io/docs/operator-docs/installation/).
func (e *Environment) InstallKairosOperator(ctx context.Context) error {
	log := e.log()
	if err := e.RequireKubeconfig(); err != nil {
		return err
	}
	if e.IsKairosOperatorStackInstalled() {
		log.Step("Kairos operator stack is already installed ✓")
		return nil
	}

	defaultURL := KairosOperatorKustomizeDefaultURL()
	log.Step("Installing Kairos operator (CRDs, RBAC, controller)...")
	log.Infof("kubectl apply -k %s", defaultURL)
	if err := e.KubectlApplyKustomizeURL(ctx, defaultURL); err != nil {
		return fmt.Errorf("kairos operator: %w", err)
	}

	crdCtx, crdCancel := context.WithTimeout(ctx, 120*time.Second)
	defer crdCancel()
	log.Infof("Waiting for OSArtifact CRD to be established...")
	if err := waitForCRDEstablished(crdCtx, e, "osartifacts.build.kairos.io"); err != nil {
		return fmt.Errorf("OSArtifact CRD: %w", err)
	}

	clientset, err := e.Clientset()
	if err != nil {
		return err
	}
	opCtx, opCancel := context.WithTimeout(ctx, 300*time.Second)
	defer opCancel()
	log.Infof("Waiting for deployment %s/%s...", kairosOperatorNamespace, kairosOperatorDeploymentName)
	if err := WaitForDeployment(opCtx, clientset, kairosOperatorNamespace, kairosOperatorDeploymentName); err != nil {
		log.Warnf("Kairos operator deployment: %v", err)
	}

	nginxURL := KairosOperatorKustomizeNginxURL()
	log.Step("Installing Kairos operator nginx (artifact upload / download)...")
	log.Infof("kubectl apply -k %s -n default", nginxURL)
	if err := e.KubectlApplyKustomizeURL(ctx, nginxURL, "-n", "default"); err != nil {
		return fmt.Errorf("kairos operator nginx: %w", err)
	}

	ngxCtx, ngxCancel := context.WithTimeout(ctx, 300*time.Second)
	defer ngxCancel()
	log.Infof("Waiting for deployment default/%s...", kairosOperatorNginxDeployment)
	if err := WaitForDeployment(ngxCtx, clientset, "default", kairosOperatorNginxDeployment); err != nil {
		log.Warnf("nginx deployment: %v", err)
	}

	log.Step("Kairos operator stack installed ✓")
	return nil
}

func waitForCRDEstablished(ctx context.Context, e *Environment, crdName string) error {
	cfg, err := e.RESTConfig()
	if err != nil {
		return err
	}
	apiextensionsClient, err := apiextensionsv1.NewForConfig(cfg)
	if err != nil {
		return err
	}
	return wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		crd, err := apiextensionsClient.CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		for _, condition := range crd.Status.Conditions {
			if condition.Type == apiextensionsv1api.Established && condition.Status == apiextensionsv1api.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}

// UninstallKairosOperator removes nginx and the operator kustomize bases (best-effort; delete OSArtifacts first if needed).
func (e *Environment) UninstallKairosOperator(ctx context.Context) error {
	log := e.log()
	if err := e.RequireKubeconfig(); err != nil {
		return err
	}
	if !e.IsKairosOperatorStackInstalled() {
		log.Infof("Kairos operator stack does not appear to be fully installed")
	}
	log.Step("Removing Kairos operator nginx...")
	_ = e.kubectlDeleteKustomizeURL(ctx, KairosOperatorKustomizeNginxURL(), "-n", "default")
	log.Step("Removing Kairos operator...")
	_ = e.kubectlDeleteKustomizeURL(ctx, KairosOperatorKustomizeDefaultURL())
	log.Step("Kairos operator uninstall attempted ✓")
	return nil
}

func (e *Environment) kubectlDeleteKustomizeURL(ctx context.Context, url string, extraArgs ...string) error {
	args := append([]string{
		"delete", "-k", url,
		"--ignore-not-found=true", "--wait=false",
		"--kubeconfig", e.KubeconfigPath(), "--context", e.KubectlContext(),
	}, extraArgs...)
	cmd := exec.CommandContext(ctx, e.kubectlBin(), args...)
	stdout, stderr := e.execOut()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

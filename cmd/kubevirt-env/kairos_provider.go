package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"

	"github.com/spf13/cobra"
)

const (
	kairosCapiImg = "ghcr.io/kairos-io/cluster-api-provider-kairos:latest"
)

func newInstallKairosProviderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kairos-provider",
		Short: "Install Kairos CAPI Provider",
		Long:  "Install Kairos CAPI Provider on the kind cluster (requires cert-manager)",
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// Validate cert-manager is installed
			if !isCertManagerInstalled() {
				return fmt.Errorf("cert-manager is not installed. Please install it first with: kubevirt-env install cert-manager")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return installKairosProvider()
		},
	}

	return cmd
}

func newUninstallKairosProviderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kairos-provider",
		Short: "Uninstall Kairos CAPI Provider",
		Long:  "Uninstall Kairos CAPI Provider from the cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			return uninstallKairosProvider()
		},
	}

	return cmd
}

func newReinstallKairosProviderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kairos-provider",
		Short: "Reinstall Kairos CAPI Provider",
		Long:  "Uninstall and reinstall Kairos CAPI Provider",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := uninstallKairosProvider(); err != nil {
				return fmt.Errorf("failed to uninstall Kairos CAPI Provider: %w", err)
			}
			return installKairosProvider()
		},
	}
	return cmd
}

func isKairosProviderInstalled() bool {
	clientset, err := getKubeClient()
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check if kairos-capi-controller-manager deployment exists and is available
	deployment, err := clientset.AppsV1().Deployments("kairos-capi-system").Get(ctx, "kairos-capi-controller-manager", metav1.GetOptions{})
	if err != nil {
		return false
	}

	// Check if deployment is available
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

func installKairosProvider() error {
	// Check if Kairos Provider is already installed
	if isKairosProviderInstalled() {
		fmt.Println("Kairos CAPI Provider is already installed ✓")
		return nil
	}

	// Build and load Docker image
	if err := buildAndLoadKairosProviderImage(); err != nil {
		return fmt.Errorf("failed to build and load image: %w", err)
	}

	// Apply kustomize configs
	if err := applyKairosProviderConfigs(); err != nil {
		return fmt.Errorf("failed to apply configs: %w", err)
	}

	fmt.Println("Kairos CAPI Provider installed ✓")
	return nil
}

func buildAndLoadKairosProviderImage() error {
	fmt.Println("Building Kairos CAPI Provider image...")

	// Build Docker image using Makefile
	makeCmd := exec.Command("make", "-f", "Makefile", "docker-build", fmt.Sprintf("IMG=%s", kairosCapiImg))
	makeCmd.Dir = "."
	makeCmd.Stdout = os.Stdout
	makeCmd.Stderr = os.Stderr
	if err := makeCmd.Run(); err != nil {
		return fmt.Errorf("failed to build Docker image: %w", err)
	}

	// Load image into Kind cluster
	clusterName := getClusterName()
	fmt.Println("Loading image into Kind cluster...")
	kindCmd := exec.Command("kind", "load", "docker-image", kairosCapiImg, "--name", clusterName)
	kindCmd.Stdout = os.Stdout
	kindCmd.Stderr = os.Stderr
	if err := kindCmd.Run(); err != nil {
		return fmt.Errorf("failed to load image into Kind: %w", err)
	}

	return nil
}

func applyKairosProviderConfigs() error {
	fmt.Println("Installing Kairos CAPI Provider...")
	kubeconfigPath := getKubeconfigPath()
	kubectlContext := getKubectlContext()

	// Apply namespace
	if err := applyKustomize(kubeconfigPath, kubectlContext, "config/namespace"); err != nil {
		return fmt.Errorf("failed to apply namespace: %w", err)
	}

	// Apply CRDs
	if err := applyKustomize(kubeconfigPath, kubectlContext, "config/crd"); err != nil {
		return fmt.Errorf("failed to apply CRDs: %w", err)
	}

	// Apply RBAC
	if err := applyKustomize(kubeconfigPath, kubectlContext, "config/rbac"); err != nil {
		return fmt.Errorf("failed to apply RBAC: %w", err)
	}

	// Apply cert-manager config
	if err := applyKustomize(kubeconfigPath, kubectlContext, "config/certmanager"); err != nil {
		return fmt.Errorf("failed to apply cert-manager config: %w", err)
	}

	// Wait for webhook certificate
	if err := waitForWebhookCertificate(kubeconfigPath, kubectlContext); err != nil {
		fmt.Printf("Warning: Webhook certificate may not be ready: %v\n", err)
	}

	// Apply webhook
	if err := applyKustomize(kubeconfigPath, kubectlContext, "config/webhook"); err != nil {
		return fmt.Errorf("failed to apply webhook: %w", err)
	}

	// Wait for CA bundle injection
	if err := waitForCABundleInjection(kubeconfigPath, kubectlContext); err != nil {
		fmt.Printf("Warning: CA bundle may not be injected: %v\n", err)
	}

	// Apply manager
	if err := applyKustomize(kubeconfigPath, kubectlContext, "config/manager"); err != nil {
		return fmt.Errorf("failed to apply manager: %w", err)
	}

	// Wait for deployment
	fmt.Println("Waiting for Kairos CAPI Provider to be ready...")
	clientset, err := getKubeClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	if err := waitForDeployment(ctx, clientset, "kairos-capi-system", "kairos-capi-controller-manager"); err != nil {
		fmt.Printf("Warning: Kairos CAPI Provider may not be fully ready: %v\n", err)
	}

	return nil
}

func applyKustomize(kubeconfigPath, kubectlContext, path string) error {
	kubectlCmd := exec.Command("kubectl", "apply", "-k", path, "--kubeconfig", kubeconfigPath, "--context", kubectlContext)
	kubectlCmd.Stdout = os.Stdout
	kubectlCmd.Stderr = os.Stderr
	return kubectlCmd.Run()
}

func waitForWebhookCertificate(kubeconfigPath, kubectlContext string) error {
	fmt.Println("Waiting for webhook certificate to be created...")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	config, err := getKubeConfig()
	if err != nil {
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}

	return wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		// Check if secret exists
		clientset, err := getKubeClient()
		if err != nil {
			return false, nil
		}

		_, err = clientset.CoreV1().Secrets("kairos-capi-system").Get(ctx, "kairos-capi-webhook-server-cert", metav1.GetOptions{})
		if err != nil {
			fmt.Print(".")
			return false, nil
		}

		// Check certificate status
		certGVR := schema.GroupVersionResource{
			Group:    "cert-manager.io",
			Version:  "v1",
			Resource: "certificates",
		}

		cert, err := dynamicClient.Resource(certGVR).Namespace("kairos-capi-system").Get(ctx, "kairos-capi-webhook-server-cert", metav1.GetOptions{})
		if err != nil {
			fmt.Print(".")
			return false, nil
		}

		conditions, found, err := unstructured.NestedSlice(cert.Object, "status", "conditions")
		if found && err == nil {
			for _, cond := range conditions {
				if condMap, ok := cond.(map[string]interface{}); ok {
					if condType, _ := condMap["type"].(string); condType == "Ready" {
						if status, _ := condMap["status"].(string); status == "True" {
							fmt.Println("\n✓ Webhook certificate is ready")
							return true, nil
						}
					}
				}
			}
		}

		fmt.Print(".")
		return false, nil
	})
}

func waitForCABundleInjection(kubeconfigPath, kubectlContext string) error {
	fmt.Println("Waiting for cert-manager CA injector to inject CA bundle into webhook...")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	config, err := getKubeConfig()
	if err != nil {
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}

	return wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		mwhGVR := schema.GroupVersionResource{
			Group:    "admissionregistration.k8s.io",
			Version:  "v1",
			Resource: "mutatingwebhookconfigurations",
		}

		mwh, err := dynamicClient.Resource(mwhGVR).Get(ctx, "mutating-webhook-configuration", metav1.GetOptions{})
		if err != nil {
			fmt.Print(".")
			return false, nil
		}

		webhooks, found, err := unstructured.NestedSlice(mwh.Object, "webhooks")
		if found && err == nil && len(webhooks) > 0 {
			if webhook, ok := webhooks[0].(map[string]interface{}); ok {
				if clientConfig, ok := webhook["clientConfig"].(map[string]interface{}); ok {
					if caBundle, ok := clientConfig["caBundle"].(string); ok && caBundle != "" && caBundle != "null" {
						fmt.Println("\n✓ CA bundle injected into webhook")
						return true, nil
					}
				}
			}
		}

		fmt.Print(".")
		return false, nil
	})
}

func uninstallKairosProvider() error {
	// Check if Kairos Provider is installed
	if !isKairosProviderInstalled() {
		fmt.Println("Kairos CAPI Provider is not installed")
		return nil
	}

	fmt.Println("Uninstalling Kairos CAPI Provider...")
	kubeconfigPath := getKubeconfigPath()
	kubectlContext := getKubectlContext()

	// Delete in reverse order
	configs := []string{"config/manager", "config/webhook", "config/certmanager", "config/rbac", "config/crd", "config/namespace"}
	for _, config := range configs {
		kubectlCmd := exec.Command("kubectl", "delete", "-k", config, "--kubeconfig", kubeconfigPath, "--context", kubectlContext, "--ignore-not-found=true")
		kubectlCmd.Stdout = os.Stdout
		kubectlCmd.Stderr = os.Stderr
		kubectlCmd.Run() // Ignore errors
	}

	fmt.Println("Kairos CAPI Provider uninstalled ✓")
	return nil
}

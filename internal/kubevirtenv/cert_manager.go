package kubevirtenv

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
)

// IsCertManagerInstalled reports whether cert-manager core, webhook, and cainjector are available.
func (e *Environment) IsCertManagerInstalled() bool {
	clientset, err := e.Clientset()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deployment, err := clientset.AppsV1().Deployments("cert-manager").Get(ctx, "cert-manager", metav1.GetOptions{})
	if err != nil {
		return false
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
			webhook, err := clientset.AppsV1().Deployments("cert-manager").Get(ctx, "cert-manager-webhook", metav1.GetOptions{})
			if err != nil {
				return false
			}
			cainjector, err := clientset.AppsV1().Deployments("cert-manager").Get(ctx, "cert-manager-cainjector", metav1.GetOptions{})
			if err != nil {
				return false
			}
			webhookReady := false
			for _, cond := range webhook.Status.Conditions {
				if cond.Type == appsv1.DeploymentAvailable && cond.Status == corev1.ConditionTrue {
					webhookReady = true
					break
				}
			}
			cainjectorReady := false
			for _, cond := range cainjector.Status.Conditions {
				if cond.Type == appsv1.DeploymentAvailable && cond.Status == corev1.ConditionTrue {
					cainjectorReady = true
					break
				}
			}
			return webhookReady && cainjectorReady
		}
	}
	return false
}

// InstallCertManager installs cert-manager from the pinned release manifest.
func (e *Environment) InstallCertManager(ctx context.Context) error {
	log := e.log()
	if e.IsCertManagerInstalled() {
		log.Step("cert-manager is already installed ✓")
		return nil
	}
	clientset, err := e.Clientset()
	if err != nil {
		return err
	}
	config, err := e.RESTConfig()
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}
	log.Infof("Installing cert-manager %s...", CertManagerVersion)
	u := fmt.Sprintf(CertManagerURL, CertManagerVersion)
	if err := e.ApplyManifestFromURL(dynamicClient, config, u); err != nil {
		log.Warnf("cert-manager apply: %v (may already be installed)", err)
	}
	log.Step("Waiting for cert-manager to be ready...")
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	if err := WaitForDeployment(waitCtx, clientset, "cert-manager", "cert-manager"); err != nil {
		log.Warnf("cert-manager: %v", err)
	}
	if err := WaitForDeployment(waitCtx, clientset, "cert-manager", "cert-manager-webhook"); err != nil {
		log.Warnf("cert-manager-webhook: %v", err)
	}
	if err := WaitForDeployment(waitCtx, clientset, "cert-manager", "cert-manager-cainjector"); err != nil {
		log.Warnf("cert-manager-cainjector: %v", err)
	}
	log.Step("cert-manager installed ✓")
	return nil
}

// UninstallCertManager removes cert-manager (best-effort).
func (e *Environment) UninstallCertManager(ctx context.Context) error {
	log := e.log()
	if !e.IsCertManagerInstalled() {
		log.Infof("cert-manager is not installed")
		return nil
	}
	config, err := e.RESTConfig()
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}
	u := fmt.Sprintf(CertManagerURL, CertManagerVersion)
	log.Step("Uninstalling cert-manager...")
	if err := e.DeleteResourcesFromManifestURL(dynamicClient, config, u); err != nil {
		return fmt.Errorf("delete cert-manager manifest: %w", err)
	}
	clientset, err := e.Clientset()
	if err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := WaitForNamespaceDeleted(waitCtx, clientset, "cert-manager"); err != nil {
		log.Warnf("cert-manager namespace may still be terminating: %v", err)
	} else {
		log.Infof("cert-manager namespace deleted ✓")
	}
	log.Step("cert-manager uninstalled ✓")
	return nil
}

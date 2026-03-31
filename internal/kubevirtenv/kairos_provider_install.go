package kubevirtenv

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

const kairosCAPIControllerDeployment = "kairos-capi-controller-manager"

// IsKairosCAPIProviderInstalled reports whether the kairos-capi controller deployment is available.
func (e *Environment) IsKairosCAPIProviderInstalled() bool {
	clientset, err := e.Clientset()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deployment, err := clientset.AppsV1().Deployments("kairos-capi-system").Get(ctx, kairosCAPIControllerDeployment, metav1.GetOptions{})
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

// InstallKairosCAPIProviderRelease builds the release image (make docker-build), loads it into kind, and applies layered kustomize (not config/dev).
func (e *Environment) InstallKairosCAPIProviderRelease(ctx context.Context) error {
	log := e.log()
	if err := e.RequireKubeconfig(); err != nil {
		return err
	}
	if !e.IsCertManagerInstalled() {
		return fmt.Errorf("cert-manager is not installed (install it before the Kairos provider)")
	}
	if e.IsKairosCAPIProviderInstalled() {
		log.Step("Kairos CAPI provider is already installed ✓")
		return nil
	}
	img := KairosCAPIReleaseImage
	log.Infof("Building Kairos CAPI image %q (make docker-build)...", img)
	if err := e.MakeDockerBuildKairosCAPI(ctx, img); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	log.Infof("Loading image into kind cluster %q...", e.ClusterName)
	if err := e.KindLoadDockerImage(ctx, img); err != nil {
		return fmt.Errorf("kind load: %w", err)
	}
	preWebhook := []string{"config/namespace", "config/crd", "config/rbac", "config/certmanager"}
	for i, rel := range preWebhook {
		log.Infof("Applying %s (%d/%d)...", rel, i+1, len(preWebhook))
		if err := e.KubectlApplyKustomizeDir(ctx, rel); err != nil {
			return fmt.Errorf("apply %s: %w", rel, err)
		}
	}
	if err := e.waitForKairosWebhookCertificate(ctx); err != nil {
		log.Warnf("webhook certificate: %v", err)
	}
	log.Infof("Applying config/webhook...")
	if err := e.KubectlApplyKustomizeDir(ctx, "config/webhook"); err != nil {
		return fmt.Errorf("apply config/webhook: %w", err)
	}
	if err := e.waitForKairosCABundleInjection(ctx); err != nil {
		log.Warnf("CA bundle injection: %v", err)
	}
	log.Infof("Applying config/manager...")
	if err := e.KubectlApplyKustomizeDir(ctx, "config/manager"); err != nil {
		return fmt.Errorf("apply config/manager: %w", err)
	}
	log.Step("Waiting for Kairos CAPI provider deployment...")
	clientset, err := e.Clientset()
	if err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	if err := WaitForDeployment(waitCtx, clientset, "kairos-capi-system", kairosCAPIControllerDeployment); err != nil {
		log.Warnf("deployment may not be ready: %v", err)
	}
	log.Step("Kairos CAPI provider installed ✓")
	return nil
}

func (e *Environment) waitForKairosWebhookCertificate(ctx context.Context) error {
	log := e.log()
	log.Infof("Waiting for webhook certificate...")
	waitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	cfg, err := e.RESTConfig()
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	return wait.PollUntilContextCancel(waitCtx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		clientset, err := e.Clientset()
		if err != nil {
			return false, nil
		}
		_, err = clientset.CoreV1().Secrets("kairos-capi-system").Get(ctx, "kairos-capi-webhook-server-cert", metav1.GetOptions{})
		if err != nil {
			log.WriteString(".")
			return false, nil
		}
		certGVR := schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
		cert, err := dynamicClient.Resource(certGVR).Namespace("kairos-capi-system").Get(ctx, "kairos-capi-webhook-server-cert", metav1.GetOptions{})
		if err != nil {
			log.WriteString(".")
			return false, nil
		}
		conditions, found, err := unstructured.NestedSlice(cert.Object, "status", "conditions")
		if found && err == nil {
			for _, cond := range conditions {
				if condMap, ok := cond.(map[string]interface{}); ok {
					if condType, _ := condMap["type"].(string); condType == "Ready" {
						if status, _ := condMap["status"].(string); status == "True" {
							log.Infof("✓ Webhook certificate ready")
							return true, nil
						}
					}
				}
			}
		}
		log.WriteString(".")
		return false, nil
	})
}

func (e *Environment) waitForKairosCABundleInjection(ctx context.Context) error {
	log := e.log()
	log.Infof("Waiting for CA bundle injection into webhook...")
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cfg, err := e.RESTConfig()
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	mwhGVR := schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "mutatingwebhookconfigurations"}
	return wait.PollUntilContextCancel(waitCtx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		mwh, err := dynamicClient.Resource(mwhGVR).Get(ctx, "mutating-webhook-configuration", metav1.GetOptions{})
		if err != nil {
			log.WriteString(".")
			return false, nil
		}
		webhooks, found, err := unstructured.NestedSlice(mwh.Object, "webhooks")
		if found && err == nil && len(webhooks) > 0 {
			if webhook, ok := webhooks[0].(map[string]interface{}); ok {
				if clientConfig, ok := webhook["clientConfig"].(map[string]interface{}); ok {
					if caBundle, ok := clientConfig["caBundle"].(string); ok && caBundle != "" && caBundle != "null" {
						log.Infof("✓ CA bundle injected")
						return true, nil
					}
				}
			}
		}
		log.WriteString(".")
		return false, nil
	})
}

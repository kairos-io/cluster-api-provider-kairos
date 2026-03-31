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

// IsCDIInstalled reports whether cdi-operator deployment is available.
func (e *Environment) IsCDIInstalled() bool {
	clientset, err := e.Clientset()
	if err != nil {
		return false
	}
	ctx := context.Background()
	deployment, err := clientset.AppsV1().Deployments("cdi").Get(ctx, "cdi-operator", metav1.GetOptions{})
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

// InstallCDI installs CDI operator and CR, then waits for readiness.
func (e *Environment) InstallCDI(ctx context.Context) error {
	log := e.log()
	if e.IsCDIInstalled() {
		log.Step("CDI is already installed ✓")
		return nil
	}
	clientset, err := e.Clientset()
	if err != nil {
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if ns, gerr := clientset.CoreV1().Namespaces().Get(checkCtx, "cdi", metav1.GetOptions{}); gerr == nil {
		if ns.Status.Phase == corev1.NamespaceTerminating {
			log.Infof("CDI namespace is terminating, waiting for deletion...")
			if err := WaitForNamespaceDeleted(checkCtx, clientset, "cdi"); err != nil {
				return fmt.Errorf("wait for CDI namespace deletion: %w", err)
			}
			log.Infof("CDI namespace deleted ✓")
		}
	}
	log.Step("Installing CDI (Containerized Data Importer)...")
	config, err := e.RESTConfig()
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}
	if err := e.ApplyManifestFromURL(dynamicClient, config, CDIOperatorURL); err != nil {
		return fmt.Errorf("apply CDI operator: %w", err)
	}
	if err := e.ApplyManifestFromURL(dynamicClient, config, CDICRURL); err != nil {
		return fmt.Errorf("apply CDI CR: %w", err)
	}
	log.Step("Waiting for CDI to be ready...")
	waitCtx, waitCancel := context.WithTimeout(ctx, 300*time.Second)
	defer waitCancel()
	if err := WaitForDeployment(waitCtx, clientset, "cdi", "cdi-operator"); err != nil {
		return fmt.Errorf("wait for CDI operator: %w", err)
	}
	log.Infof("Waiting for CDI CR...")
	if err := waitForCdiCRReady(waitCtx, log, dynamicClient); err != nil {
		return fmt.Errorf("wait for CDI CR: %w", err)
	}
	log.Step("CDI installed ✓")
	return nil
}

func waitForCdiCRReady(ctx context.Context, log Logger, dynamicClient dynamic.Interface) error {
	cdiGVR := schema.GroupVersionResource{
		Group:    "cdi.kubevirt.io",
		Version:  "v1beta1",
		Resource: "cdis",
	}
	log.Infof("Checking CDI status...")
	conditionCtx, conditionCancel := context.WithTimeout(ctx, 10*time.Second)
	defer conditionCancel()
	conditionMet := false
	_ = wait.PollUntilContextCancel(conditionCtx, 1*time.Second, true, func(checkCtx context.Context) (bool, error) {
		cdi, err := dynamicClient.Resource(cdiGVR).Get(checkCtx, "cdi", metav1.GetOptions{})
		if err != nil {
			cdi, err = dynamicClient.Resource(cdiGVR).Namespace("cdi").Get(checkCtx, "cdi", metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
		}
		conditions, found, err := unstructured.NestedSlice(cdi.Object, "status", "conditions")
		if found && err == nil {
			for _, cond := range conditions {
				if condMap, ok := cond.(map[string]interface{}); ok {
					if condType, ok := condMap["type"].(string); ok && condType == "Available" {
						if condStatus, ok := condMap["status"].(string); ok && condStatus == "True" {
							log.Infof("✓ CDI is ready (Available condition met)")
							conditionMet = true
							return true, nil
						}
					}
				}
			}
		}
		return false, nil
	})
	if conditionMet {
		return nil
	}
	log.Infof("Waiting for CDI phase to be Deployed...")
	return wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(checkCtx context.Context) (bool, error) {
		cdi, err := dynamicClient.Resource(cdiGVR).Get(checkCtx, "cdi", metav1.GetOptions{})
		if err != nil {
			cdi, err = dynamicClient.Resource(cdiGVR).Namespace("cdi").Get(checkCtx, "cdi", metav1.GetOptions{})
			if err != nil {
				log.WriteString(".")
				return false, nil
			}
		}
		phase, found, err := unstructured.NestedString(cdi.Object, "status", "phase")
		if !found || err != nil {
			log.WriteString(".")
			return false, nil
		}
		if phase == "Deployed" {
			log.Infof("✓ CDI is ready (phase: %s)", phase)
			return true, nil
		}
		log.WriteString(".")
		return false, nil
	})
}

// UninstallCDI removes CDI.
func (e *Environment) UninstallCDI(ctx context.Context) error {
	log := e.log()
	log.Step("Uninstalling CDI...")
	config, err := e.RESTConfig()
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}
	if err := e.DeleteResourcesFromManifestURL(dynamicClient, config, CDICRURL); err != nil {
		return fmt.Errorf("delete CDI CR: %w", err)
	}
	if err := e.DeleteResourcesFromManifestURL(dynamicClient, config, CDIOperatorURL); err != nil {
		return fmt.Errorf("delete CDI operator: %w", err)
	}
	clientset, err := e.Clientset()
	if err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	if err := WaitForNamespaceDeleted(waitCtx, clientset, "cdi"); err != nil {
		return fmt.Errorf("wait for CDI namespace: %w", err)
	}
	log.Step("CDI uninstalled ✓")
	return nil
}

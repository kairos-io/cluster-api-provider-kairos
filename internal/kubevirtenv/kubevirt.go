package kubevirtenv

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

// IsKubeVirtInstalled reports whether the KubeVirt CR is Available or Deployed.
func (e *Environment) IsKubeVirtInstalled() bool {
	config, err := e.RESTConfig()
	if err != nil {
		return false
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	kubevirt, err := getKubeVirtCR(ctx, dynamicClient)
	if err != nil {
		return false
	}
	conditions, found, err := unstructured.NestedSlice(kubevirt.Object, "status", "conditions")
	if found && err == nil {
		for _, cond := range conditions {
			if condMap, ok := cond.(map[string]interface{}); ok {
				if condType, _ := condMap["type"].(string); condType == "Available" {
					if status, _ := condMap["status"].(string); status == "True" {
						return true
					}
				}
			}
		}
	}
	phase, found, err := unstructured.NestedString(kubevirt.Object, "status", "phase")
	return found && err == nil && phase == "Deployed"
}

// InstallKubeVirt installs KubeVirt operator and CR, optionally enabling emulation for hosts without KVM.
func (e *Environment) InstallKubeVirt(ctx context.Context) error {
	log := e.log()
	if e.IsKubeVirtInstalled() {
		log.Step("KubeVirt is already installed ✓")
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
	log.Infof("Installing KubeVirt %s...", KubeVirtVersion)
	operatorURL := fmt.Sprintf(KubeVirtOperatorURL, KubeVirtVersion)
	if err := e.ApplyManifestFromURL(dynamicClient, config, operatorURL); err != nil {
		return fmt.Errorf("apply KubeVirt operator: %w", err)
	}
	crURL := fmt.Sprintf(KubeVirtCRURL, KubeVirtVersion)
	if err := e.ApplyManifestFromURL(dynamicClient, config, crURL); err != nil {
		return fmt.Errorf("apply KubeVirt CR: %w", err)
	}
	if shouldUseEmulation() {
		patchCtx, patchCancel := context.WithTimeout(ctx, 30*time.Second)
		defer patchCancel()
		if err := ensureKubeVirtEmulation(patchCtx, dynamicClient); err != nil {
			log.Warnf("enable KubeVirt emulation: %v", err)
		} else {
			log.Infof("✓ KubeVirt emulation enabled")
		}
	}
	log.Step("Waiting for KubeVirt to be ready...")
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	log.Infof("Waiting for virt-operator deployment...")
	if err := WaitForDeployment(waitCtx, clientset, "kubevirt", "virt-operator"); err != nil {
		log.Warnf("virt-operator may not be fully ready: %v", err)
	}
	log.Infof("Waiting for KubeVirt CR...")
	if err := waitForKubeVirtCR(waitCtx, log, dynamicClient); err != nil {
		log.Warnf("KubeVirt CR may not be fully ready: %v", err)
		if kubevirt, gerr := getKubeVirtCR(waitCtx, dynamicClient); gerr == nil && kubevirt != nil {
			phase, _, _ := unstructured.NestedString(kubevirt.Object, "status", "phase")
			log.Infof("KubeVirt phase: %s", phase)
		}
	}
	log.Step("KubeVirt installed ✓")
	return nil
}

func waitForKubeVirtCR(ctx context.Context, log Logger, dynamicClient dynamic.Interface) error {
	return wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		kubevirt, err := getKubeVirtCR(ctx, dynamicClient)
		if err != nil {
			return false, nil
		}
		conditions, found, err := unstructured.NestedSlice(kubevirt.Object, "status", "conditions")
		if found && err == nil {
			for _, cond := range conditions {
				if condMap, ok := cond.(map[string]interface{}); ok {
					if condType, _ := condMap["type"].(string); condType == "Available" {
						if status, _ := condMap["status"].(string); status == "True" {
							log.Infof("✓ KubeVirt is ready (Available condition met)")
							return true, nil
						}
					}
				}
			}
		}
		phase, found, err := unstructured.NestedString(kubevirt.Object, "status", "phase")
		if found && err == nil && phase == "Deployed" {
			log.Infof("✓ KubeVirt is ready (phase: %s)", phase)
			return true, nil
		}
		log.WriteString(".")
		return false, nil
	})
}

func getKubeVirtCR(ctx context.Context, dynamicClient dynamic.Interface) (*unstructured.Unstructured, error) {
	gvr := schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "kubevirts"}
	return dynamicClient.Resource(gvr).Namespace("kubevirt").Get(ctx, "kubevirt", metav1.GetOptions{})
}

func shouldUseEmulation() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("KUBEVIRT_USE_EMULATION")))
	if value == "" {
		return true
	}
	return value != "false" && value != "0" && value != "no"
}

func ensureKubeVirtEmulation(ctx context.Context, dynamicClient dynamic.Interface) error {
	kubevirt, err := getKubeVirtCR(ctx, dynamicClient)
	if err != nil {
		return err
	}
	if current, found, _ := unstructured.NestedBool(kubevirt.Object, "spec", "configuration", "developerConfiguration", "useEmulation"); found && current {
		return nil
	}
	if err := unstructured.SetNestedField(kubevirt.Object, true, "spec", "configuration", "developerConfiguration", "useEmulation"); err != nil {
		return fmt.Errorf("set useEmulation: %w", err)
	}
	gvr := schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "kubevirts"}
	_, err = dynamicClient.Resource(gvr).Namespace("kubevirt").Update(ctx, kubevirt, metav1.UpdateOptions{})
	return err
}

// UninstallKubeVirt removes KubeVirt.
func (e *Environment) UninstallKubeVirt(ctx context.Context) error {
	log := e.log()
	if !e.IsKubeVirtInstalled() {
		log.Infof("KubeVirt is not installed")
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
	log.Step("Uninstalling KubeVirt...")
	crURL := fmt.Sprintf(KubeVirtCRURL, KubeVirtVersion)
	if err := e.DeleteResourcesFromManifestURL(dynamicClient, config, crURL); err != nil {
		log.Warnf("delete KubeVirt CR: %v", err)
	}
	operatorURL := fmt.Sprintf(KubeVirtOperatorURL, KubeVirtVersion)
	if err := e.DeleteResourcesFromManifestURL(dynamicClient, config, operatorURL); err != nil {
		return fmt.Errorf("delete KubeVirt operator: %w", err)
	}
	log.Step("KubeVirt uninstalled ✓")
	return nil
}

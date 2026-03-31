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

// IsCalicoInstalled reports whether Calico node and kube-controllers are ready.
func (e *Environment) IsCalicoInstalled() bool {
	clientset, err := e.Clientset()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ds, err := clientset.AppsV1().DaemonSets("kube-system").Get(ctx, "calico-node", metav1.GetOptions{})
	if err != nil {
		return false
	}
	deployment, err := clientset.AppsV1().Deployments("kube-system").Get(ctx, "calico-kube-controllers", metav1.GetOptions{})
	if err != nil {
		return false
	}
	dsReady := ds.Status.NumberReady == ds.Status.DesiredNumberScheduled && ds.Status.DesiredNumberScheduled > 0
	deploymentReady := false
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
			deploymentReady = true
			break
		}
	}
	return dsReady && deploymentReady
}

// InstallCalico installs Calico CNI from the pinned manifest.
func (e *Environment) InstallCalico(ctx context.Context) error {
	log := e.log()
	if e.IsCalicoInstalled() {
		log.Step("Calico CNI is already installed ✓")
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
	calicoURL := fmt.Sprintf(CalicoManifestURL, CalicoVersion)
	log.Infof("Installing Calico CNI %s...", CalicoVersion)
	if err := e.ApplyManifestFromURL(dynamicClient, config, calicoURL); err != nil {
		return fmt.Errorf("apply Calico manifest: %w", err)
	}
	log.Step("Waiting for Calico to be ready...")
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	log.Infof("Waiting for Calico kube-controllers deployment...")
	if err := WaitForDeployment(waitCtx, clientset, "kube-system", "calico-kube-controllers"); err != nil {
		log.Warnf("Calico kube-controllers may not be fully ready: %v", err)
	}
	log.Infof("Waiting for Calico node daemonset...")
	if err := WaitForDaemonset(waitCtx, log, clientset, "kube-system", "calico-node"); err != nil {
		log.Warnf("Calico node daemonset may not be fully ready: %v", err)
		if ds, gerr := clientset.AppsV1().DaemonSets("kube-system").Get(waitCtx, "calico-node", metav1.GetOptions{}); gerr == nil {
			log.Infof("Daemonset status: %d/%d pods ready", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
		}
	}
	log.Step("Calico CNI installed ✓")
	return nil
}

// UninstallCalico removes Calico (best-effort).
func (e *Environment) UninstallCalico(ctx context.Context) error {
	log := e.log()
	if !e.IsCalicoInstalled() {
		log.Infof("Calico CNI is not installed")
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
	calicoURL := fmt.Sprintf(CalicoManifestURL, CalicoVersion)
	log.Step("Uninstalling Calico CNI...")
	if err := e.DeleteResourcesFromManifestURL(dynamicClient, config, calicoURL); err != nil {
		return fmt.Errorf("delete Calico manifest: %w", err)
	}
	time.Sleep(5 * time.Second)
	if e.IsCalicoInstalled() {
		log.Warnf("Some Calico resources may still be present")
	} else {
		log.Step("Calico CNI uninstalled ✓")
	}
	return nil
}

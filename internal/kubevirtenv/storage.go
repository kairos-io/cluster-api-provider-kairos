package kubevirtenv

import (
	"context"
	"fmt"
	"time"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// IsLocalPathInstalled reports whether the local-path StorageClass exists.
func (e *Environment) IsLocalPathInstalled() bool {
	clientset, err := e.Clientset()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = clientset.StorageV1().StorageClasses().Get(ctx, LocalPathClassName, metav1.GetOptions{})
	return err == nil
}

// InstallLocalPath installs the local-path provisioner and ensures a default StorageClass.
func (e *Environment) InstallLocalPath(ctx context.Context) error {
	log := e.log()
	if e.IsLocalPathInstalled() {
		log.Infof("local-path provisioner is already installed ✓")
		return nil
	}
	log.Step("Installing local-path provisioner...")
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
	if err := e.ApplyManifestFromURL(dynamicClient, config, LocalPathManifestURL); err != nil {
		return fmt.Errorf("apply local-path manifest: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := WaitForDeployment(waitCtx, clientset, LocalPathNamespace, "local-path-provisioner"); err != nil {
		log.Warnf("local-path provisioner may not be fully ready: %v", err)
	}
	if err := ensureDefaultStorageClass(waitCtx, clientset); err != nil {
		log.Warnf("default StorageClass: %v", err)
	} else {
		log.Infof("✓ Default StorageClass confirmed")
	}
	log.Step("local-path provisioner installed ✓")
	return nil
}

func ensureDefaultStorageClass(ctx context.Context, clientset kubernetes.Interface) error {
	classes, err := clientset.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list StorageClasses: %w", err)
	}
	for _, sc := range classes.Items {
		if isDefaultStorageClass(&sc) {
			return nil
		}
	}
	localPath, err := clientset.StorageV1().StorageClasses().Get(ctx, LocalPathClassName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("local-path StorageClass: %w", err)
	}
	patch := localPath.DeepCopy()
	if patch.Annotations == nil {
		patch.Annotations = map[string]string{}
	}
	patch.Annotations["storageclass.kubernetes.io/is-default-class"] = "true"
	patch.Annotations["storageclass.beta.kubernetes.io/is-default-class"] = "true"
	_, err = clientset.StorageV1().StorageClasses().Update(ctx, patch, metav1.UpdateOptions{})
	return err
}

func isDefaultStorageClass(sc *storagev1.StorageClass) bool {
	if sc.Annotations == nil {
		return false
	}
	if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
		return true
	}
	return sc.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true"
}

// UninstallLocalPath removes local-path (best-effort).
func (e *Environment) UninstallLocalPath(ctx context.Context) error {
	log := e.log()
	log.Step("Uninstalling local-path provisioner...")
	config, err := e.RESTConfig()
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	if err := e.DeleteResourcesFromManifestURL(dynamicClient, config, LocalPathManifestURL); err != nil {
		return fmt.Errorf("delete local-path: %w", err)
	}
	clientset, err := e.Clientset()
	if err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := WaitForNamespaceDeleted(waitCtx, clientset, LocalPathNamespace); err != nil {
		log.Warnf("local-path namespace may still be terminating: %v", err)
	}
	log.Step("local-path provisioner uninstalled ✓")
	return nil
}

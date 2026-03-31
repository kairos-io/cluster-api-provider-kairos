package kubevirtenv

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// WaitForDeployment polls until the deployment has Available=True.
func WaitForDeployment(ctx context.Context, clientset kubernetes.Interface, namespace, name string) error {
	return wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		deployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		for _, condition := range deployment.Status.Conditions {
			if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}

// WaitForDaemonset polls until the daemonset has all desired pods ready (progress dots go to log).
func WaitForDaemonset(ctx context.Context, log Logger, clientset kubernetes.Interface, namespace, name string) error {
	return wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		ds, err := clientset.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		if ds.Status.NumberReady == ds.Status.DesiredNumberScheduled && ds.Status.DesiredNumberScheduled > 0 {
			log.Infof("✓ %s daemonset is ready (%d/%d pods)", name, ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
			return true, nil
		}
		log.WriteString(".")
		return false, nil
	})
}

// WaitForNamespaceDeleted polls until the namespace is gone.
func WaitForNamespaceDeleted(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	return wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		if err != nil {
			return true, nil
		}
		return false, nil
	})
}

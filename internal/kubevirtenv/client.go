package kubevirtenv

import (
	"fmt"
	"os"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// RESTConfig builds a rest.Config from the environment kubeconfig and context.
func (e *Environment) RESTConfig() (*rest.Config, error) {
	kubeconfigPath := e.KubeconfigPath()
	if _, err := os.Stat(kubeconfigPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("kubeconfig not found at %s (cluster may not be created yet)", kubeconfigPath)
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("build kubeconfig: %w", err)
	}
	ctxName := e.KubectlContext()
	if ctxName != "" {
		loading := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath}
		overrides := &clientcmd.ConfigOverrides{CurrentContext: ctxName}
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, overrides).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig with context %q: %w", ctxName, err)
		}
	}
	return config, nil
}

// Clientset returns a kubernetes clientset for the management cluster.
func (e *Environment) Clientset() (kubernetes.Interface, error) {
	cfg, err := e.RESTConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// DynamicClient returns a dynamic client for the management cluster.
func (e *Environment) DynamicClient() (dynamic.Interface, error) {
	cfg, err := e.RESTConfig()
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(cfg)
}

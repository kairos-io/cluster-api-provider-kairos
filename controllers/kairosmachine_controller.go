/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/cluster-api/api/v1alpha3"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1alpha3 "github.com/kairos-io/cluster-api-provider-kairos/api/v1alpha3"
	"github.com/pkg/errors"
)

// KairosMachineReconciler reconciles a KairosMachine object
type KairosMachineReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	clientSet *kubernetes.Clientset
}

// GetOwnerCluster returns the Cluster object owning the current resource.
func GetOwnerMachine(ctx context.Context, c client.Client, obj metav1.ObjectMeta) (*v1alpha3.Machine, error) {
	for _, ref := range obj.OwnerReferences {
		if ref.Kind != "Machine" {
			continue
		}
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		if gv.Group == v1alpha3.GroupVersion.Group {
			return GetMachineByName(ctx, c, obj.Namespace, ref.Name)
		}
	}
	return nil, nil
}

// GetClusterByName finds and return a Cluster object using the specified params.
func GetMachineByName(ctx context.Context, c client.Client, namespace, name string) (*v1alpha3.Machine, error) {
	cluster := &v1alpha3.Machine{}
	key := client.ObjectKey{
		Namespace: namespace,
		Name:      name,
	}

	if err := c.Get(ctx, key, cluster); err != nil {
		return nil, errors.Wrapf(err, "failed to get Machine/%s", name)
	}

	return cluster, nil
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kairosmachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kairosmachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kairosmachines/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the KairosMachine object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.2/pkg/reconcile
func (r *KairosMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var machine infrastructurev1alpha3.KairosMachine
	if err := r.Get(ctx, req.NamespacedName, &machine); err != nil {
		// 	import apierrors "k8s.io/apimachinery/pkg/api/errors"
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	capiMachine, err := GetOwnerMachine(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if capiMachine == nil {
		logger.Info("Waiting for Cluster Controller to set OwnerRef on KairosMachine")
		return ctrl.Result{}, nil
	}

	// Check if the Controltoken has an associated secret that will be used when calling the node remotely
	secret, err := r.clientSet.CoreV1().Secrets(req.Namespace).Get(context.Background(), machine.Name, v1.GetOptions{})
	if err != nil || secret == nil {
		_, err := r.clientSet.CoreV1().Secrets(machine.Namespace).Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("control-%s", machine.Name),
				Namespace: machine.Namespace,
				Labels:    map[string]string{},
			},
			StringData: map[string]string{"network_token": machine.Spec.ControlToken},
		}, v1.CreateOptions{})
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "re-enqueue %s", machine.Name)
		}
	}

	// TODO(user): your logic here

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KairosMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return err
	}
	r.clientSet = clientset

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1alpha3.KairosMachine{}).
		Complete(r)
}

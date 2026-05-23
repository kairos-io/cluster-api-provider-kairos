/*
Copyright 2024 The Kairos CAPI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing
permissions and limitations under the License.
*/

package bootstrap

import (
	"context"
	"fmt"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// kubeVirtTokenResolver implements ManagementEndpointResolver against the CAPK
// kubeconfig-push pattern: a per-cluster ServiceAccount whose Role can write
// the cluster's kubeconfig Secret and read the controlling VMI for
// SAN-detection. The resolver mints a fresh 24h TokenRequest on every call —
// see TODO(KD-33b) on Resolve below.
//
// Behavior must remain byte-identical to the pre-refactor
// KairosConfigReconciler.ensureKubeconfigPushConfig method this struct
// replaces. PR-6 is a pure move; the SA/Role/RoleBinding shape, the
// 24h-token-on-every-render policy, the cluster.x-k8s.io/cluster-name labels,
// and the (nil,nil) "disabled" signal when ManagementAPIServer is empty are
// all preserved.
type kubeVirtTokenResolver struct {
	// Client is the controller-runtime client used to upsert the
	// ServiceAccount, Role and RoleBinding. Must be the management-cluster
	// client (not a workload-cluster client).
	Client client.Client

	// SubResource returns a SubResourceClient for the named subresource
	// ("token"). The indirection lets tests inject a fake that returns a
	// canned TokenRequest without spinning up envtest; production passes
	// (client.Client).SubResource directly.
	SubResource func(string) client.SubResourceClient

	// Scheme is needed by controllerutil.SetControllerReference on the SA /
	// Role / RoleBinding. Must include core/v1 and rbac/v1.
	Scheme *runtime.Scheme

	// ManagementAPIServer is the URL nodes will dial back to. Production
	// wiring pulls this from mgr.GetConfig().Host at SetupWithManager time.
	// Empty string is the disabled-resolver signal.
	ManagementAPIServer string
}

// Resolve performs the same idempotent SA/Role/RoleBinding upsert as the
// removed KairosConfigReconciler.ensureKubeconfigPushConfig and mints a fresh
// 24h serviceaccount token via the TokenRequest subresource API.
//
// Returns (nil, nil) when ManagementAPIServer is empty — the same "REST
// config not available" disabled signal the legacy method used. Callers
// (currently generateK0sCloudConfig / generateK3sCloudConfig) must treat that
// as "render without the push block", not as an error.
//
// TODO(KD-33b): cache tokens; refresh when <30m remaining. Each Reconcile
// that renders today re-mints, which costs one TokenRequest API call per
// reconcile per CAPK control-plane Machine. Acceptable for alpha-2; revisit
// once we have lab data on Reconcile frequency under steady-state. Caching
// belongs here in the resolver, not on the reconciler.
func (r *kubeVirtTokenResolver) Resolve(ctx context.Context, kc *bootstrapv1beta2.KairosConfig, cluster *clusterv1.Cluster) (*ManagementEndpoint, error) {
	log := ctrl.LoggerFrom(ctx)
	if r.ManagementAPIServer == "" {
		log.Info("Skipping kubeconfig push config; REST config not available")
		return nil, nil
	}

	secretName := fmt.Sprintf("%s-kubeconfig", cluster.Name)
	saName := kubeconfigWriterName(cluster.Name)

	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: cluster.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, serviceAccount, func() error {
		if serviceAccount.Labels == nil {
			serviceAccount.Labels = map[string]string{}
		}
		serviceAccount.Labels[clusterv1.ClusterNameLabel] = cluster.Name
		return controllerutil.SetControllerReference(kc, serviceAccount, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to ensure kubeconfig writer serviceaccount: %w", err)
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: cluster.Namespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				ResourceNames: []string{secretName},
				Verbs:         []string{"get", "create", "update", "patch"},
			},
			{
				APIGroups: []string{"kubevirt.io"},
				Resources: []string{"virtualmachineinstances"},
				Verbs:     []string{"get"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"services"},
				Verbs:     []string{"get"},
			},
		}
		if role.Labels == nil {
			role.Labels = map[string]string{}
		}
		role.Labels[clusterv1.ClusterNameLabel] = cluster.Name
		return controllerutil.SetControllerReference(kc, role, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to ensure kubeconfig writer role: %w", err)
	}

	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: cluster.Namespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, roleBinding, func() error {
		roleBinding.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     role.Name,
		}
		roleBinding.Subjects = []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceAccount.Name,
				Namespace: serviceAccount.Namespace,
			},
		}
		if roleBinding.Labels == nil {
			roleBinding.Labels = map[string]string{}
		}
		roleBinding.Labels[clusterv1.ClusterNameLabel] = cluster.Name
		return controllerutil.SetControllerReference(kc, roleBinding, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to ensure kubeconfig writer rolebinding: %w", err)
	}

	expirationSeconds := int64(24 * 60 * 60)
	tokenRequest := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{"https://kubernetes.default.svc"},
			ExpirationSeconds: &expirationSeconds,
		},
	}
	if err := r.SubResource("token").Create(ctx, serviceAccount, tokenRequest); err != nil {
		return nil, fmt.Errorf("failed to create serviceaccount token: %w", err)
	}
	if tokenRequest.Status.Token == "" {
		return nil, fmt.Errorf("serviceaccount token request returned empty token")
	}

	return &ManagementEndpoint{
		Token:                     tokenRequest.Status.Token,
		APIServer:                 r.ManagementAPIServer,
		KubeconfigSecretName:      secretName,
		KubeconfigSecretNamespace: cluster.Namespace,
	}, nil
}

// Compile-time guard that kubeVirtTokenResolver satisfies the interface.
var _ ManagementEndpointResolver = (*kubeVirtTokenResolver)(nil)

// NewKubeVirtTokenResolver constructs the production resolver wiring the
// controller-runtime client's SubResource method directly. main.go calls this
// at manager startup; tests in this package construct the struct literally
// with a fake SubResource client.
func NewKubeVirtTokenResolver(c client.Client, scheme *runtime.Scheme, mgmtAPIServer string) ManagementEndpointResolver {
	return &kubeVirtTokenResolver{
		Client:              c,
		SubResource:         c.SubResource,
		Scheme:              scheme,
		ManagementAPIServer: mgmtAPIServer,
	}
}

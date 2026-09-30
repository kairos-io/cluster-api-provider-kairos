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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

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
	// Role / RoleBinding. Must include core/v1, rbac/v1, and
	// cluster.x-k8s.io/v1beta1 (the shared objects are owned by the Cluster —
	// see the ownership note in Resolve).
	Scheme *runtime.Scheme

	// ManagementAPIServer is the URL nodes will dial back to. Production
	// wiring pulls this from mgr.GetConfig().Host at SetupWithManager time.
	// Empty string is the disabled-resolver signal.
	ManagementAPIServer string

	// ManagementCABundle is the canonical PEM CA bundle nodes verify the
	// management API server's TLS certificate against (ADR 0011 PR-4). It is
	// resolved once at manager startup (ResolveManagementCA) and stamped into
	// every ManagementEndpoint the resolver returns. When ManagementAPIServer is
	// set (push enabled) but this is empty, Resolve fails with
	// ErrManagementCAUnavailable rather than emitting an unverifiable push — the
	// manager's startup check normally prevents this, so a hit here is a wiring
	// bug, not an operator error.
	ManagementCABundle []byte
}

// Resolve performs the same idempotent SA/Role/RoleBinding upsert as the
// removed KairosConfigReconciler.ensureKubeconfigPushConfig and mints a fresh
// 24h serviceaccount token via the TokenRequest subresource API.
//
// Returns (nil, nil) when ManagementAPIServer is empty — the same "REST
// config not available" disabled signal the legacy method used. Callers
// (renderCloudConfig) must treat that as "render without the push block", not as
// an error.
//
// TODO(KD-33b): cache tokens; refresh when <30m remaining. Each Reconcile
// that renders today re-mints, which costs one TokenRequest API call per
// reconcile per CAPK control-plane Machine. Acceptable for alpha-2; revisit
// once we have lab data on Reconcile frequency under steady-state. Caching
// belongs here in the resolver, not on the reconciler.
func (r *kubeVirtTokenResolver) Resolve(ctx context.Context, kc *bootstrapv1beta2.KairosConfig, machine *clusterv1.Machine, cluster *clusterv1.Cluster) (*ManagementEndpoint, error) {
	log := ctrl.LoggerFrom(ctx)
	if r.ManagementAPIServer == "" {
		log.Info("Skipping kubeconfig push config; REST config not available")
		return nil, nil
	}

	// PR-4 (ADR 0011): the node-push channel is TLS-verified against the
	// management API CA (curl --cacert; no -k fallback). If push is enabled but
	// no CA was resolved, fail closed rather than render a push the node cannot
	// verify. The startup check in main.go normally catches this first.
	if len(r.ManagementCABundle) == 0 {
		return nil, ErrManagementCAUnavailable
	}

	// PR-3 resolver gate (ADR 0011): the node PATCHes ONLY the controller-owned
	// staging Secret <cluster>-kubeconfig-push. Do not hand out a push token until
	// that Secret exists AND is controller-owned by the same KairosControlPlane
	// that controls this Machine. The bootstrap manager reads Machines but not
	// KCPs, so the authoritative check compares the staging Secret's controller
	// owner UID with the Machine's controller owner UID (both are the KCP). A
	// forged/absent staging owner, or a missing staging Secret, is not-ready.
	stagingName := bootstrapv1beta2.KubeconfigPushSecretName(cluster.Name)
	if err := r.requireOwnedStaging(ctx, machine, cluster, stagingName); err != nil {
		return nil, err
	}

	// The node's push target is the STAGING Secret, never the contract Secret.
	secretName := stagingName
	saName := kubeconfigWriterName(cluster.Name)

	// OWNERSHIP (ADR 0005 § D.2): the SA/Role/RoleBinding are a single
	// per-CLUSTER object set (named by cluster.Name) shared by every
	// control-plane Machine's KairosConfig. They are owned by the *Cluster*,
	// not by the per-Machine KairosConfig `kc`. An object gets exactly one
	// controller owner, so owning by kc made the first config (the init node)
	// the controller owner and every joiner's Resolve fail with
	// "already owned by another controller" — the multi-node bring-up blocker
	// found in the Phase-4 lab. Owning by the Cluster is idempotent across all
	// KairosConfigs (they set the same owner) and GCs the RBAC when the Cluster
	// is deleted. NOTE (migration): a cluster provisioned before this change
	// has these objects controller-owned by the init KairosConfig; delete the
	// three `kairos-kubeconfig-writer-<cluster>` objects once so they are
	// recreated Cluster-owned. Pre-1.0; not otherwise handled.
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
		return controllerutil.SetControllerReference(cluster, serviceAccount, r.Scheme)
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
		// PR-3 (ADR 0011, KD-60/A1): the node has NO `secrets: create` and NO
		// `get`/`update`. It may only PATCH two controller-pre-created, named
		// Secrets. Removing `create` closes KD-60 (a node could otherwise create
		// `<other>-kubeconfig`/`-ca`/`-etcd`/... for a foreign cluster that an
		// upstream KCP or CAPI core would then adopt/read, the A3 attack). Every
		// node-push in the rendered cloud-config is now a strategic-merge PATCH:
		//
		//   secrets:patch (staging kubeconfig) — node-push kubeconfig PATCH of the
		//                                        `value` key of the controller-owned
		//                                        staging Secret <cluster>-kubeconfig-push.
		//                                        The node no longer touches the
		//                                        contract Secret <cluster>-kubeconfig.
		//   secrets:patch (etcd-status)        — ADR 0005 §E.1: HA control-plane
		//                                        nodes PATCH their own member key into
		//                                        the per-cluster etcd-status Secret.
		//   virtualmachineinstances:get        — ONLY the k0s CAPK control-plane
		//                                        SAN-detection block
		//                                        (fetch_vmi_ip_from_management in
		//                                        k0s_kairos_cloud_config_capk.yaml.tmpl,
		//                                        gated `and .IsKubeVirt (eq .Role "control-plane")`).
		//                                        Granted conditionally below.
		//   services:get                       — REMOVED. No template path reads
		//                                        Services with the node SA token; it
		//                                        was a dead grant.
		//   secrets (join-token)               — REMOVED (ADR 0013). The k0s init
		//                                        node used to PATCH its `k0s token
		//                                        create` output into the join-token
		//                                        Secret. The KairosControlPlane now
		//                                        mints that token and is the Secret's
		//                                        only writer, so no node is granted
		//                                        it.
		//
		// Both Secrets are pre-created by the controller (staging: KCP-owned;
		// etcd-status: Cluster-owned), so the node never needs `create`; and PATCH
		// does not require `get` (strategic-merge-patch by name is authorized as
		// the `patch` verb alone). Named Secrets only; per-cluster Role+RoleBinding
		// scope — a node only ever holds its own cluster's token. Rewriting the
		// rules on every Resolve narrows Roles created by older releases too.
		etcdStatusSecretName := bootstrapv1beta2.EtcdStatusSecretName(cluster.Name)
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				ResourceNames: []string{secretName, etcdStatusSecretName},
				Verbs:         []string{"patch"},
			},
		}
		// Grant virtualmachineinstances:get only for the exact config shape
		// that consumes it. This resolver is the CAPK (KubeVirt) resolver, so
		// IsKubeVirt is implied; the SAN-detection block additionally requires
		// the control-plane role and the k0s distribution (the k3s CAPK
		// template has no VMI query). Distribution is webhook-defaulted to
		// "k0s" when empty, so this comparison is reliable. k3s control-plane
		// SAs and all worker SAs no longer receive this grant.
		if kc.Spec.Role == "control-plane" && (kc.Spec.Distribution == "k0s" || kc.Spec.Distribution == "") {
			role.Rules = append(role.Rules, rbacv1.PolicyRule{
				APIGroups: []string{"kubevirt.io"},
				Resources: []string{"virtualmachineinstances"},
				Verbs:     []string{"get"},
			})
		}
		if role.Labels == nil {
			role.Labels = map[string]string{}
		}
		role.Labels[clusterv1.ClusterNameLabel] = cluster.Name
		return controllerutil.SetControllerReference(cluster, role, r.Scheme)
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
		return controllerutil.SetControllerReference(cluster, roleBinding, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to ensure kubeconfig writer rolebinding: %w", err)
	}

	expirationSeconds := int64(24 * 60 * 60)
	tokenRequest := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			// Include both common audience variants so the token validates
			// against kube-apiservers configured with either the short DNS
			// name (--api-audiences=https://kubernetes.default.svc) or the
			// FQDN (--api-audiences=https://kubernetes.default.svc.cluster.local,
			// which is the kubeadm/kind/most-managed-K8s default). Tokens
			// minted with a single audience that doesn't match the
			// api-server's accepted set fail with HTTP 401 at push time —
			// surfaced by the KD-3b PR-7 lab regression on the CI kind
			// cluster (which uses the FQDN), where the production lab
			// cluster happens to accept the short form. KD-50.
			Audiences: []string{
				"https://kubernetes.default.svc",
				"https://kubernetes.default.svc.cluster.local",
			},
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
		CABundle:                  r.ManagementCABundle,
	}, nil
}

// requireOwnedStaging enforces the PR-3 resolver gate (ADR 0011): the per-cluster
// staging kubeconfig Secret must exist AND be controller-owned by the same
// KairosControlPlane that controls this Machine. It returns
// ErrKubeconfigPushTargetNotReady otherwise, which the reconciler maps to a timed
// requeue with WaitingForKubeconfigPushTargetReason so no push token is handed out
// before the control plane has pre-created and owns the staging Secret. The check
// compares the staging Secret's controller-owner UID with the Machine's
// controller-owner UID (both the KCP) — the bootstrap manager reads Machines but
// not KCPs, so it cannot resolve the KCP directly, and owner UIDs are not
// node-forgeable.
func (r *kubeVirtTokenResolver) requireOwnedStaging(ctx context.Context, machine *clusterv1.Machine, cluster *clusterv1.Cluster, stagingName string) error {
	if machine == nil {
		return ErrKubeconfigPushTargetNotReady
	}
	machineOwner := metav1.GetControllerOfNoCopy(machine)
	if machineOwner == nil {
		return ErrKubeconfigPushTargetNotReady
	}
	staging := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: stagingName, Namespace: cluster.Namespace}, staging); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrKubeconfigPushTargetNotReady
		}
		return fmt.Errorf("get staging kubeconfig secret %s/%s: %w", cluster.Namespace, stagingName, err)
	}
	stagingOwner := metav1.GetControllerOfNoCopy(staging)
	if stagingOwner == nil || stagingOwner.UID != machineOwner.UID {
		return ErrKubeconfigPushTargetNotReady
	}
	return nil
}

// Compile-time guard that kubeVirtTokenResolver satisfies the interface.
var _ ManagementEndpointResolver = (*kubeVirtTokenResolver)(nil)

// NewKubeVirtTokenResolver constructs the production resolver wiring the
// controller-runtime client's SubResource method directly. main.go calls this
// at manager startup; tests in this package construct the struct literally
// with a fake SubResource client.
//
// mgmtCABundle is the canonical PEM CA bundle nodes verify the management API
// server against on the node-push channel (ADR 0011 PR-4), resolved by
// ResolveManagementCA. It must be non-empty whenever mgmtAPIServer is set.
func NewKubeVirtTokenResolver(c client.Client, scheme *runtime.Scheme, mgmtAPIServer string, mgmtCABundle []byte) ManagementEndpointResolver {
	return &kubeVirtTokenResolver{
		Client:              c,
		SubResource:         c.SubResource,
		Scheme:              scheme,
		ManagementAPIServer: mgmtAPIServer,
		ManagementCABundle:  mgmtCABundle,
	}
}

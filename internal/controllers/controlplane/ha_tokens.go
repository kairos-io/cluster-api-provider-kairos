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

package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// joinTokenSecretDataKey is the data key under which the HA control-plane join
// token is stored (shared with the bootstrap package via the API constant).
const joinTokenSecretDataKey = bootstrapv1beta2.ControlPlaneJoinTokenSecretDataKey

// joinTokenSecretName returns the name of the per-cluster HA join-token Secret.
func joinTokenSecretName(clusterName string) string {
	return bootstrapv1beta2.ControlPlaneJoinTokenSecretName(clusterName)
}

// ensureJoinTokenSecret guarantees the per-cluster HA join-token Secret exists,
// owner-ref'd to the KairosControlPlane and labeled with the cluster-name label
// (TOKEN-INV: owner-ref'd, label-filtered watch; KD-15). It is idempotent.
//
// Distribution behavior (ADR 0005 Phase 3 §3):
//   - k3s: the controller generates a strong random shared server token ONCE
//     (crypto/rand). If the Secret already has a non-empty token, it is left
//     untouched (never regenerated — regenerating would orphan the running
//     etcd cluster's token).
//   - k0s: the Secret is created EMPTY. The KCP mints the controller-join token
//     into it when it admits a joiner (ensureK0sControllerJoinToken, ADR 0013)
//     and is its only writer; nodes have no write access to it.
//
// The token value is NEVER logged (root rule § "No secrets in logs").
func (r *KairosControlPlaneReconciler) ensureJoinTokenSecret(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster) error {
	distribution := kcp.Spec.Distribution
	if distribution == "" {
		distribution = "k0s"
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      joinTokenSecretName(cluster.Name),
			Namespace: cluster.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels[clusterv1.ClusterNameLabel] = cluster.Name
		secret.Labels[controlPlaneJoinTokenSecretTypeLabel] = controlPlaneJoinTokenSecretTypeValue

		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		// k3s: generate the shared server token exactly once. Never overwrite an
		// existing non-empty token. k0s: leave the data alone; the KCP's join
		// path writes the token and its ID annotation (ADR 0013).
		if distribution == "k3s" {
			if len(secret.Data[joinTokenSecretDataKey]) == 0 {
				tok, genErr := generateJoinToken()
				if genErr != nil {
					return genErr
				}
				secret.Data[joinTokenSecretDataKey] = []byte(tok)
			}
		}

		// Owner-ref to the KCP so the Secret cascades on KCP delete (TOKEN-INV).
		return controllerutil.SetControllerReference(kcp, secret, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensure join-token secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	// Log the Secret identity only — never its contents.
	log.V(4).Info("Ensured HA join-token secret",
		"secret", joinTokenSecretName(cluster.Name), "namespace", cluster.Namespace, "distribution", distribution)
	return nil
}

// generateJoinToken returns a strong random join token suitable for use as a k3s
// shared server token: 32 bytes of crypto/rand entropy, base64url-encoded
// (no padding), so it is shell- and YAML-safe (alphanumerics, '-' and '_'
// only). Stdlib only (root rule § "No new external dependency").
func generateJoinToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate join token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// applyControlPlaneHASpec stamps the HA fields onto a control-plane
// KairosConfig spec for the given role (ADR 0005 Phase 3):
//   - ControlPlaneRole = role; SingleNode = (role == single) for back-compat.
//   - For init/join: point the distribution-appropriate join-token *SecretRef at
//     the per-cluster join-token Secret (TOKEN-INV: *SecretRef only, never
//     inline) and copy the VIP block down so the renderer can emit kube-vip.
//
// Single-node clusters get no token ref and no VIP.
func (r *KairosControlPlaneReconciler) applyControlPlaneHASpec(spec *bootstrapv1beta2.KairosConfigSpec, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, role bootstrapv1beta2.ControlPlaneRole) {
	spec.ControlPlaneRole = role
	spec.SingleNode = role == bootstrapv1beta2.ControlPlaneRoleSingle

	if role == bootstrapv1beta2.ControlPlaneRoleSingle {
		return
	}

	tokenRef := &bootstrapv1beta2.WorkerTokenSecretReference{
		Name:      joinTokenSecretName(cluster.Name),
		Namespace: cluster.Namespace,
		Key:       joinTokenSecretDataKey,
	}
	distribution := spec.Distribution
	if distribution == "" {
		distribution = "k0s"
	}
	switch distribution {
	case "k3s":
		// k3s HA reuses the K3sTokenSecretRef plumbing for the shared server token.
		spec.K3sTokenSecretRef = tokenRef
	default: // k0s
		spec.ControlPlaneJoinTokenSecretRef = tokenRef
	}

	// Copy the VIP block down so the bootstrap renderer can emit kube-vip on
	// init/join nodes (RenderKubeVIP gate handles the CAPK/non-KubeVirt rules).
	if kcp.Spec.HA != nil && kcp.Spec.HA.VIP != nil {
		spec.ControlPlaneVIP = &bootstrapv1beta2.ControlPlaneVIP{
			Address:   kcp.Spec.HA.VIP.Address,
			Interface: kcp.Spec.HA.VIP.Interface,
			Mode:      string(kcp.Spec.HA.VIP.Mode),
		}
	}
}

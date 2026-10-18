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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/k0sjointoken"
)

// resolveWorkerToken walks the shared worker/legacy precedence tail:
// WorkerTokenSecretRef > WorkerToken > TokenSecretRef > Token. It is the
// common suffix of both the k0s and k3s worker chains. Reads through the given
// client.Reader so a JoinMaterialSource can carry either the management client
// or (in P1) a workload client.
func resolveWorkerToken(ctx context.Context, c client.Reader, kc *bootstrapv1beta2.KairosConfig, cluster *clusterv1.Cluster) (string, error) {
	switch {
	case kc.Spec.WorkerTokenSecretRef != nil:
		return tokenFromWorkerRef(ctx, c, kc.Namespace, kc.Spec.WorkerTokenSecretRef, "worker token")
	case kc.Spec.WorkerToken != "":
		return kc.Spec.WorkerToken, nil
	case kc.Spec.TokenSecretRef != nil:
		return tokenFromLegacyRef(ctx, c, cluster.Namespace, kc.Spec.TokenSecretRef)
	case kc.Spec.Token != "":
		return kc.Spec.Token, nil
	}
	return "", nil
}

// resolveK3sWorkerToken walks the k3s worker chain: K3sTokenSecretRef > K3sToken,
// then the shared worker/legacy tail. It is the resolution the k3s join source
// (join.go) uses.
func resolveK3sWorkerToken(ctx context.Context, c client.Reader, kc *bootstrapv1beta2.KairosConfig, cluster *clusterv1.Cluster) (string, error) {
	if kc.Spec.K3sTokenSecretRef != nil {
		return tokenFromWorkerRef(ctx, c, kc.Namespace, kc.Spec.K3sTokenSecretRef, "k3s token")
	}
	if kc.Spec.K3sToken != "" {
		return kc.Spec.K3sToken, nil
	}
	return resolveWorkerToken(ctx, c, kc, cluster)
}

// tokenFromWorkerRef reads a WorkerTokenSecretReference-shaped ref. The Secret
// must be in the KairosConfig namespace (see secretRefKey); the data key defaults
// to "token". A 404 surfaces as errTokenNotReady (requeue); a present-but-keyless
// Secret is a hard error. label is a human-readable noun for error messages
// ("worker token" / "k3s token") and is the only thing logged — never the
// resolved value.
func tokenFromWorkerRef(ctx context.Context, c client.Reader, ownerNamespace string, ref *bootstrapv1beta2.WorkerTokenSecretReference, label string) (string, error) {
	secretKey, err := secretRefKey(ownerNamespace, ref.Namespace, ref.Name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", errTokenNotReady
		}
		return "", fmt.Errorf("failed to get %s secret %s/%s: %w", label, secretKey.Namespace, secretKey.Name, err)
	}
	key := ref.Key
	if key == "" {
		key = "token"
	}
	tokenData, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("%s secret %s/%s does not contain key '%s'", label, secretKey.Namespace, secretKey.Name, key)
	}
	return string(tokenData), nil
}

// errCrossNamespaceSecretRef reports a Secret reference that names a namespace
// other than the KairosConfig's own.
var errCrossNamespaceSecretRef = errors.New("cross-namespace Secret references are not allowed")

// secretRefKey resolves a Secret reference in ownerNamespace, the namespace of
// the KairosConfig that holds it. An empty reference namespace means that one;
// any other namespace is refused.
//
// The controller reads Secrets cluster-wide and copies what these references
// point at into the bootstrap Secret beside the KairosConfig, so honouring a
// foreign namespace would copy another namespace's Secret into data the
// KairosConfig's author can read. The KairosConfig webhook rejects such a
// reference at admission; this refuses it again here, because a webhook is not
// always in the path (not yet installed, failurePolicy, direct etcd writes).
func secretRefKey(ownerNamespace, refNamespace, name string) (types.NamespacedName, error) {
	if refNamespace != "" && refNamespace != ownerNamespace {
		return types.NamespacedName{}, fmt.Errorf("%w: Secret %s/%s is outside namespace %s",
			errCrossNamespaceSecretRef, refNamespace, name, ownerNamespace)
	}
	return types.NamespacedName{Namespace: ownerNamespace, Name: name}, nil
}

// tokenFromLegacyRef reads the legacy TokenSecretRef (a bare
// corev1.ObjectReference). It is resolved in the cluster namespace and accepts
// either a "token" or "value" data key, preserving the pre-refactor behavior.
func tokenFromLegacyRef(ctx context.Context, c client.Reader, namespace string, ref *corev1.ObjectReference) (string, error) {
	secretKey := types.NamespacedName{
		Namespace: namespace,
		Name:      ref.Name,
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", errTokenNotReady
		}
		return "", fmt.Errorf("failed to get token secret: %w", err)
	}
	if tokenData, ok := secret.Data["token"]; ok {
		return string(tokenData), nil
	}
	if tokenData, ok := secret.Data["value"]; ok {
		return string(tokenData), nil
	}
	return "", fmt.Errorf("token secret does not contain 'token' or 'value' key")
}

// resolveControlPlaneJoinToken resolves the HA control-plane join token from the
// distribution-appropriate *SecretRef (TOKEN-INV: *SecretRef only, never
// inline). k3s uses K3sTokenSecretRef (the controller-generated shared server
// token); k0s uses ControlPlaneJoinTokenSecretRef (the controller-join token the
// KairosControlPlane mints, ADR 0013). A missing Secret returns errTokenNotReady
// so the caller requeues until the token lands.
//
// A k0s join KairosConfig names the token the KCP issued for it in the
// K0sControllerJoinTokenIDAnnotation. It renders only once the Secret carries
// the same ID, and requeues before that. The KCP writes the Secret just before
// it creates the KairosConfig, and this controller can see the new
// KairosConfig before its cache has the new Secret; without the check that
// window would render the previous token. A KairosConfig without the
// annotation was created by an older KCP and renders whatever the Secret holds,
// as before.
//
// The control-plane join stays outside the JoinMaterialSource seam: it is
// coupled to the kubeconfig publish rework (M2). applyControlPlaneRenderData calls this
// directly.
func (r *KairosConfigReconciler) resolveControlPlaneJoinToken(ctx context.Context, kc *bootstrapv1beta2.KairosConfig, _ *clusterv1.Cluster) (string, error) {
	distribution := bootstrapv1beta2.EffectiveDistribution(kc.Spec.Distribution)
	switch distribution {
	case "k3s":
		if kc.Spec.K3sTokenSecretRef == nil {
			return "", errTokenNotReady
		}
		return tokenFromWorkerRef(ctx, r.Client, kc.Namespace, kc.Spec.K3sTokenSecretRef, "k3s control-plane join token")
	case "k0s":
		if kc.Spec.ControlPlaneJoinTokenSecretRef == nil {
			return "", errTokenNotReady
		}
		if wantID := kc.Annotations[bootstrapv1beta2.K0sControllerJoinTokenIDAnnotation]; wantID != "" {
			return k0sIssuedControlPlaneJoinToken(ctx, r.Client, kc.Namespace, kc.Spec.ControlPlaneJoinTokenSecretRef, wantID)
		}
		return tokenFromWorkerRef(ctx, r.Client, kc.Namespace, kc.Spec.ControlPlaneJoinTokenSecretRef, "k0s control-plane join token")
	default:
		return "", fmt.Errorf("unsupported distribution for control-plane join token: %s", distribution)
	}
}

// k0sIssuedControlPlaneJoinToken reads the k0s controller-join token from the
// join-token Secret, but only when it is the token the KCP issued for this
// KairosConfig (ADR 0013): the Secret's ID annotation is wantID, and so is the
// ID of the bootstrap token inside the encoded join token. Any other state, a
// different or missing ID, an empty value or one that does not decode, is
// errTokenNotReady: the KCP is the Secret's only writer and the right value is
// on its way. The token value is never logged or put in an error; the IDs are
// public.
func k0sIssuedControlPlaneJoinToken(ctx context.Context, c client.Reader, ownerNamespace string, ref *bootstrapv1beta2.WorkerTokenSecretReference, wantID string) (string, error) {
	const label = "k0s control-plane join token"
	secretKey, err := secretRefKey(ownerNamespace, ref.Namespace, ref.Name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", errTokenNotReady
		}
		return "", fmt.Errorf("failed to get %s secret %s/%s: %w", label, secretKey.Namespace, secretKey.Name, err)
	}
	if got := secret.Annotations[bootstrapv1beta2.K0sControllerJoinTokenIDAnnotation]; got != wantID {
		return "", fmt.Errorf("%w: %s secret %s/%s holds token ID %q, this config needs %q",
			errTokenNotReady, label, secretKey.Namespace, secretKey.Name, got, wantID)
	}
	key := ref.Key
	if key == "" {
		key = "token"
	}
	token := secret.Data[key]
	if len(token) == 0 {
		return "", fmt.Errorf("%w: %s secret %s/%s has no token under key %q",
			errTokenNotReady, label, secretKey.Namespace, secretKey.Name, key)
	}
	// The annotation and the value are written in one patch, but check the value
	// itself too, the same way the KCP does before it reuses a stored token.
	decoded, err := k0sjointoken.Decode(string(token))
	if err != nil {
		return "", fmt.Errorf("%w: %s secret %s/%s does not hold a k0s join token under key %q",
			errTokenNotReady, label, secretKey.Namespace, secretKey.Name, key)
	}
	if got := decoded.Token.ID(); got != wantID {
		return "", fmt.Errorf("%w: %s secret %s/%s holds a join token with token ID %q, this config needs %q",
			errTokenNotReady, label, secretKey.Namespace, secretKey.Name, got, wantID)
	}
	return string(token), nil
}

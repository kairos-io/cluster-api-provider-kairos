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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// tokenKind selects which precedence chain resolveToken walks. The two chains
// are NOT identical: the k3s worker path prefers the k3s-specific ref/inline
// fields before falling through to the generic worker/legacy fields, whereas
// the k0s worker path starts at the generic worker fields. resolveToken
// preserves each chain's historical precedence verbatim (this is a pure
// refactor of the open-coded blocks that used to live in generateK0sCloudConfig
// / generateK3sCloudConfig — controllers/CLAUDE.md § "resolveToken").
type tokenKind int

const (
	// tokenKindK0sWorker resolves a k0s worker join token.
	// Precedence: WorkerTokenSecretRef > WorkerToken > TokenSecretRef > Token.
	tokenKindK0sWorker tokenKind = iota

	// tokenKindK3sWorker resolves a k3s worker/server join token.
	// Precedence: K3sTokenSecretRef > K3sToken > WorkerTokenSecretRef >
	// WorkerToken > TokenSecretRef > Token.
	tokenKindK3sWorker

	// tokenKindControlPlaneJoin resolves the control-plane join token used by an
	// HA join node (ADR 0005 Phase 3). It is ONLY sourced from a *SecretRef —
	// never an inline spec field (TOKEN-INV / api CLAUDE.md § "No new
	// inline-secret fields"):
	//   - k3s: K3sTokenSecretRef (the controller-generated shared server token).
	//   - k0s: ControlPlaneJoinTokenSecretRef (the controller-join token the
	//     init node minted via `k0s token create` and pushed back over the
	//     node-push channel).
	// A missing referenced Secret returns errTokenNotReady so the caller
	// requeues until the token Secret exists, rather than failing hard.
	//
	// The CP-join resolution arm itself is wired in a later commit (it needs the
	// additive ControlPlaneJoinTokenSecretRef API field); this kind is declared
	// here so resolveToken's switch is complete from the start.
	tokenKindControlPlaneJoin
)

// resolveToken resolves a join token for the given kind from the KairosConfig's
// inline fields and/or referenced Secrets, in the kind's documented precedence
// order. It centralizes the previously-duplicated token-resolution blocks and
// adds the Phase-3 control-plane-join path.
//
// Secret lookups that 404 surface as errTokenNotReady (a requeue signal), not a
// hard error: the token Secret may simply not exist yet (the k0s init node has
// not pushed it, or the controller has not generated the k3s token). All other
// Get failures, and a present-but-keyless Secret, are returned as hard errors.
//
// The returned string is NEVER logged by this function (root rule § "No secrets
// in logs"). Callers must not log it either.
func (r *KairosConfigReconciler) resolveToken(ctx context.Context, kind tokenKind, kc *bootstrapv1beta2.KairosConfig, cluster *clusterv1.Cluster) (string, error) {
	switch kind {
	case tokenKindControlPlaneJoin:
		return r.resolveControlPlaneJoinToken(ctx, kc, cluster)
	case tokenKindK3sWorker:
		if kc.Spec.K3sTokenSecretRef != nil {
			tok, err := r.tokenFromWorkerRef(ctx, kc.Namespace, kc.Spec.K3sTokenSecretRef, "k3s token")
			if err != nil {
				return "", err
			}
			return tok, nil
		}
		if kc.Spec.K3sToken != "" {
			return kc.Spec.K3sToken, nil
		}
		return r.resolveWorkerToken(ctx, kc, cluster)
	case tokenKindK0sWorker:
		return r.resolveWorkerToken(ctx, kc, cluster)
	default:
		return "", fmt.Errorf("unknown token kind %d", kind)
	}
}

// resolveWorkerToken walks the shared worker/legacy precedence tail:
// WorkerTokenSecretRef > WorkerToken > TokenSecretRef > Token. It is the
// common suffix of both the k0s and k3s worker chains.
func (r *KairosConfigReconciler) resolveWorkerToken(ctx context.Context, kc *bootstrapv1beta2.KairosConfig, cluster *clusterv1.Cluster) (string, error) {
	switch {
	case kc.Spec.WorkerTokenSecretRef != nil:
		return r.tokenFromWorkerRef(ctx, kc.Namespace, kc.Spec.WorkerTokenSecretRef, "worker token")
	case kc.Spec.WorkerToken != "":
		return kc.Spec.WorkerToken, nil
	case kc.Spec.TokenSecretRef != nil:
		return r.tokenFromLegacyRef(ctx, cluster.Namespace, kc.Spec.TokenSecretRef)
	case kc.Spec.Token != "":
		return kc.Spec.Token, nil
	}
	return "", nil
}

// tokenFromWorkerRef reads a WorkerTokenSecretReference-shaped ref. The Secret's
// namespace defaults to the KairosConfig namespace; the data key defaults to
// "token". A 404 surfaces as errTokenNotReady (requeue); a present-but-keyless
// Secret is a hard error. label is a human-readable noun for error messages
// ("worker token" / "k3s token") and is the only thing logged — never the
// resolved value.
func (r *KairosConfigReconciler) tokenFromWorkerRef(ctx context.Context, defaultNamespace string, ref *bootstrapv1beta2.WorkerTokenSecretReference, label string) (string, error) {
	secretKey := types.NamespacedName{
		Namespace: defaultNamespace,
		Name:      ref.Name,
	}
	if ref.Namespace != "" {
		secretKey.Namespace = ref.Namespace
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, secretKey, secret); err != nil {
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

// tokenFromLegacyRef reads the legacy TokenSecretRef (a bare
// corev1.ObjectReference). It is resolved in the cluster namespace and accepts
// either a "token" or "value" data key, preserving the pre-refactor behavior.
func (r *KairosConfigReconciler) tokenFromLegacyRef(ctx context.Context, namespace string, ref *corev1.ObjectReference) (string, error) {
	secretKey := types.NamespacedName{
		Namespace: namespace,
		Name:      ref.Name,
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, secretKey, secret); err != nil {
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
// token); k0s uses ControlPlaneJoinTokenSecretRef (the init-node-minted
// controller-join token pushed back over the node-push channel). A missing
// Secret returns errTokenNotReady so the caller requeues until the token lands.
//
// The k0s arm (ControlPlaneJoinTokenSecretRef) is wired in the API/controller
// commit that adds the field; in this refactor commit only the k3s arm is live.
func (r *KairosConfigReconciler) resolveControlPlaneJoinToken(ctx context.Context, kc *bootstrapv1beta2.KairosConfig, _ *clusterv1.Cluster) (string, error) {
	distribution := kc.Spec.Distribution
	if distribution == "" {
		distribution = "k0s"
	}
	switch distribution {
	case "k3s":
		if kc.Spec.K3sTokenSecretRef == nil {
			return "", errTokenNotReady
		}
		return r.tokenFromWorkerRef(ctx, kc.Namespace, kc.Spec.K3sTokenSecretRef, "k3s control-plane join token")
	case "k0s":
		if kc.Spec.ControlPlaneJoinTokenSecretRef == nil {
			return "", errTokenNotReady
		}
		return r.tokenFromWorkerRef(ctx, kc.Namespace, kc.Spec.ControlPlaneJoinTokenSecretRef, "k0s control-plane join token")
	default:
		return "", fmt.Errorf("unsupported distribution for control-plane join token: %s", distribution)
	}
}

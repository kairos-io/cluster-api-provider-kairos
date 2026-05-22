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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// deleteBootstrapSecret removes the bootstrap Secret named in
// kc.Status.DataSecretName. Callers use the returned `gone` flag to decide
// whether to requeue or proceed with finalizer removal.
//
// Semantics:
//
//   - If kc.Status.DataSecretName is nil or empty, return gone=true. There
//     is nothing to delete and the finalizer can be removed immediately.
//   - Get the Secret. If IsNotFound: gone=true (already removed or never
//     created). On Get error other than IsNotFound: gone=false, err.
//   - Issue Delete. On IsNotFound during the Delete call: gone=true (raced
//     with another reaper). On other errors: gone=false, err.
//   - On a successful Delete: return gone=false because Kubernetes Delete is
//     asynchronous. The follow-up reconcile's Get will return IsNotFound
//     and we will report gone=true. Requeuing once is intentional -- we do
//     NOT trust garbage collection to be deterministic in envtest or under
//     load. (KD-4, maintainer-confirmed plan.)
//
// The bootstrap Secret is owned (Controller=true) by the KairosConfig via
// controllerutil.SetControllerReference, so Kubernetes garbage collection
// would eventually reap it -- but waiting for that race-prone path was the
// failure mode that motivated KD-4. Explicit Delete makes the contract
// deterministic.
func deleteBootstrapSecret(ctx context.Context, c client.Client, kc *bootstrapv1beta2.KairosConfig) (gone bool, err error) {
	if kc.Status.DataSecretName == nil || *kc.Status.DataSecretName == "" {
		return true, nil
	}
	secretKey := types.NamespacedName{
		Name:      *kc.Status.DataSecretName,
		Namespace: kc.Namespace,
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if err := c.Delete(ctx, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	// Delete is asynchronous -- the apiserver may still return the object
	// from a Get until the GC sweep completes. Request a requeue and let
	// the next pass observe IsNotFound.
	return false, nil
}

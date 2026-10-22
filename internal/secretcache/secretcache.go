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

// Package secretcache holds the manager settings that keep Secret bodies out
// of the controller-runtime cache. main.go applies them to the options the
// provider starts with, and the envtest guard applies the same function, so
// the test exercises the settings the running provider actually uses.
//
// The manager reads Secrets on two paths: the bootstrap-data Secrets the
// KairosConfig, KairosControlPlane and SSH-fallback controllers write and read
// back, and the <cluster>-kubeconfig of workload clusters. controller-runtime
// starts an informer on the first cached read of a type, and that informer
// lists and watches the whole scope the manager is configured for, not the one
// object asked for. On a management cluster watching all namespaces that would
// make the process hold, for its whole lifetime, admin credentials for every
// workload cluster present, Kairos or not, plus Kamaji's CA key material.
//
// A label selector does not narrow this: CAPI labels the kubeconfig Secrets
// with cluster.x-k8s.io/cluster-name, which is the same label the watch
// predicates select on, so a selector-scoped cache would still hold them.
//
// Two settings, both needed:
//
//   - Client.Cache.DisableFor: the manager's client reads Secrets straight
//     from the API server (kairos-io/kairos#5465). This costs a GET per read on
//     paths that run per reconcile; they are all namespaced GETs by name.
//   - Cache.ByObject transform: the three Secret watches
//     (kairosconfig_controller.go, kairoscontrolplane_controller.go) are
//     registered with builder.OnlyMetadata, because Watches builds its informer
//     on mgr.GetCache() directly and DisableFor does not reach it. Their
//     informers hold PartialObjectMetadata, and StripPayload removes the two
//     ObjectMeta fields that can still carry a Secret body
//     (kairos-io/kairos#5499).
//
// The role still needs list and watch on secrets for those metadata
// informers: without watch the reflector falls back to re-LISTing every Secret
// in scope.
package secretcache

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LastAppliedConfigAnnotation is kubectl's record of the object as applied. On
// a Secret it is the full object, data included, so it defeats a metadata-only
// watch unless it is stripped before the object reaches the cache.
const LastAppliedConfigAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// Apply installs both Secret settings on opts. Call it after opts.Cache has
// otherwise been set: it writes into opts.Cache.ByObject, and a later
// assignment of the whole opts.Cache would drop it. ByObject.Namespaces is
// left nil so the Secret entry inherits opts.Cache.DefaultNamespaces.
func Apply(opts *ctrl.Options) {
	if opts.Client.Cache == nil {
		opts.Client.Cache = &client.CacheOptions{}
	}
	opts.Client.Cache.DisableFor = append(opts.Client.Cache.DisableFor, &corev1.Secret{})

	if opts.Cache.ByObject == nil {
		opts.Cache.ByObject = map[client.Object]cache.ByObject{}
	}
	// Keyed by corev1.Secret: the cache resolves ByObject entries by GVK, and a
	// PartialObjectMetadata for v1/Secret resolves to the same GVK, so this
	// reaches the metadata informers.
	opts.Cache.ByObject[&corev1.Secret{}] = cache.ByObject{Transform: StripPayload}
}

// StripPayload removes everything that can carry Secret data into the cache
// through ObjectMeta:
//
//   - managedFields, which is pure bookkeeping for anything this provider does;
//   - the kubectl.kubernetes.io/last-applied-configuration annotation, which is
//     the full serialised object, data included, for any Secret created with
//     `kubectl apply`.
//
// It is a cache transform, so it runs on the cache's own copy and never on an
// object handed back to a caller by a live (uncached) read.
func StripPayload(in any) (any, error) {
	obj, err := meta.Accessor(in)
	if err != nil {
		// Not an API object (a cache.DeletedFinalStateUnknown tombstone, say).
		// Nothing to strip, and dropping it would lose the delete event.
		return in, nil
	}
	// Nil-check before setting, per kubernetes/kubernetes#124337.
	if obj.GetManagedFields() != nil {
		obj.SetManagedFields(nil)
	}
	if annotations := obj.GetAnnotations(); annotations != nil {
		if _, ok := annotations[LastAppliedConfigAnnotation]; ok {
			delete(annotations, LastAppliedConfigAnnotation)
			obj.SetAnnotations(annotations)
		}
	}
	return in, nil
}

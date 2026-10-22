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

package envtest

import (
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kairos-io/cluster-api-provider-kairos/internal/secretcache"
)

// withProductionSecretCache applies the Secret cache settings main.go applies
// (secretcache.Apply) to a test manager's options, so the controllers under
// test read Secrets the way they do in production: live, not from the cache.
//
// Every envtest manager that runs the real controllers must use it. The
// controllers' Secret watches are metadata-only, so their events come from a
// metadata informer. Without these settings a Secret read through the manager
// client is served by a separate, typed informer, which can lag the metadata
// informer: a reconcile woken by a Secret event can then read the Secret as it
// was before that event. A KairosConfig parked on that stale read waits for a
// wake-up that already happened, and a test waiting for it times out. With the
// production settings the read goes to the API server and always sees the
// change that triggered it.
func withProductionSecretCache(opts manager.Options) manager.Options {
	secretcache.Apply(&opts)
	return opts
}

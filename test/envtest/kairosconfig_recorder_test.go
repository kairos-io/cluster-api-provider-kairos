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
	"context"
	"sync"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// kairosConfigRecorder records every KairosConfig created in one namespace,
// and which of them were deleted, through a watch.
//
// No infrastructure provider runs in envtest, so the KCP's control-plane
// create fails at the infrastructure Machine, and since ADR 0015 PR-4 the
// KairosConfig made by that attempt is deleted again at once, and the next
// attempt uses a new random name. The HA tests used to Get the KairosConfig
// left behind under <kcp>-0 or <kcp>-1; they now read what the KCP created
// from this record instead.
type kairosConfigRecorder struct {
	mu      sync.Mutex
	created []*bootstrapv1beta2.KairosConfig
	deleted map[string]bool
}

// recordKairosConfigs starts recording in namespace ns until ctx ends. Start
// it before anything can create a KairosConfig there.
func recordKairosConfigs(t *testing.T, ctx context.Context, cfg *rest.Config, ns string) *kairosConfigRecorder {
	t.Helper()
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())
	wc, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())
	w, err := wc.Watch(ctx, &bootstrapv1beta2.KairosConfigList{}, client.InNamespace(ns))
	g.Expect(err).NotTo(HaveOccurred())

	rec := &kairosConfigRecorder{deleted: map[string]bool{}}
	go func() {
		defer w.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.ResultChan():
				if !ok {
					return
				}
				kc, isKC := ev.Object.(*bootstrapv1beta2.KairosConfig)
				if !isKC {
					continue
				}
				rec.mu.Lock()
				switch ev.Type {
				case watch.Added:
					rec.created = append(rec.created, kc.DeepCopy())
				case watch.Deleted:
					rec.deleted[kc.Name] = true
				}
				rec.mu.Unlock()
			}
		}
	}()
	return rec
}

// withRole returns a copy of every recorded KairosConfig of the given role,
// oldest first.
func (r *kairosConfigRecorder) withRole(role bootstrapv1beta2.ControlPlaneRole) []*bootstrapv1beta2.KairosConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*bootstrapv1beta2.KairosConfig
	for _, kc := range r.created {
		if kc.Spec.ControlPlaneRole == role {
			out = append(out, kc.DeepCopy())
		}
	}
	return out
}

// wasDeleted reports whether the KairosConfig of that name was deleted.
func (r *kairosConfigRecorder) wasDeleted(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deleted[name]
}

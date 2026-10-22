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

package secretcache

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestStripPayload_RemovesBodyCarryingMetadata is the guard for the
// cache transform installed on v1/Secret. The Secret watches are metadata-only,
// so the cache holds PartialObjectMetadata and never the Secret body — but
// ObjectMeta can smuggle the body back in two ways, and both must be gone
// before the object is committed.
func TestStripPayload_RemovesBodyCarryingMetadata(t *testing.T) {
	in := &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workload-kubeconfig",
			Namespace: "default",
			Labels:    map[string]string{"cluster.x-k8s.io/cluster-name": "workload"},
			Annotations: map[string]string{
				// kubectl's copy of the applied object: the whole Secret,
				// base64 data included.
				LastAppliedConfigAnnotation: `{"apiVersion":"v1","kind":"Secret","data":{"value":"c3VwZXItc2VjcmV0"}}`,
				"keep-me":                   "yes",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}},
		},
	}

	out, err := StripPayload(in)
	if err != nil {
		t.Fatalf("StripPayload returned an error: %v", err)
	}
	got, ok := out.(*metav1.PartialObjectMetadata)
	if !ok {
		t.Fatalf("transform changed the object type: got %T", out)
	}
	if got.ManagedFields != nil {
		t.Errorf("managedFields survived the transform: %v", got.ManagedFields)
	}
	if v, present := got.Annotations[LastAppliedConfigAnnotation]; present {
		t.Errorf("last-applied-configuration survived the transform, carrying the Secret body: %q", v)
	}
	// The transform must strip, not sanitise: everything else is load-bearing
	// for the mappers and predicates that read this object.
	if got.Annotations["keep-me"] != "yes" {
		t.Errorf("unrelated annotation was dropped: %v", got.Annotations)
	}
	if got.Labels["cluster.x-k8s.io/cluster-name"] != "workload" {
		t.Errorf("labels were dropped, which would break every Secret mapper: %v", got.Labels)
	}
	if got.Name != "workload-kubeconfig" || got.Namespace != "default" {
		t.Errorf("name/namespace were altered: %s/%s", got.Namespace, got.Name)
	}
}

// TestStripPayload_PassesThroughNonObjects asserts the transform
// never drops a value it cannot introspect. A cache transform that returns an
// error on a DeletedFinalStateUnknown tombstone loses the delete event, so the
// controller would keep a Secret alive in cache forever.
func TestStripPayload_PassesThroughNonObjects(t *testing.T) {
	in := "not an api object"
	out, err := StripPayload(in)
	if err != nil {
		t.Fatalf("StripPayload returned an error: %v", err)
	}
	if out != in {
		t.Errorf("transform did not pass the value through: got %v", out)
	}
}

// TestStripPayload_NoAnnotations asserts the nil-annotation path
// does not panic and does not materialise an empty map.
func TestStripPayload_NoAnnotations(t *testing.T) {
	in := &metav1.PartialObjectMetadata{
		ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "default"},
	}
	out, err := StripPayload(in)
	if err != nil {
		t.Fatalf("StripPayload returned an error: %v", err)
	}
	if got := out.(*metav1.PartialObjectMetadata); got.Annotations != nil {
		t.Errorf("transform created an annotations map where there was none: %v", got.Annotations)
	}
}

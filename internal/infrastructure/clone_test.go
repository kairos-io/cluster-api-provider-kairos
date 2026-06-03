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

package infrastructure

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func makeMetal3MachineTemplate(version string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: version,
		Kind:    "Metal3MachineTemplate",
	})
	obj.SetName("tmpl")
	obj.SetNamespace("default")
	_ = unstructured.SetNestedMap(obj.Object, map[string]interface{}{
		"template": map[string]interface{}{
			"spec": map[string]interface{}{
				"image": map[string]interface{}{
					"url":      "https://example.com/kairos.qcow2",
					"checksum": "sha256:abc123",
				},
			},
		},
	}, "spec")
	return obj
}

func TestCloneMetal3MachineTemplate(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	labels := map[string]string{"cluster.x-k8s.io/cluster-name": "test-cluster"}
	annotations := map[string]string{"test-key": "test-value"}

	tests := []struct {
		name            string
		templateVersion string
		wantVersion     string
	}{
		{
			name:            "v1beta2 template preserves version",
			templateVersion: "v1beta2",
			wantVersion:     "v1beta2",
		},
		{
			name:            "v1beta1 template preserves version",
			templateVersion: "v1beta1",
			wantVersion:     "v1beta1",
		},
		{
			name:            "versionless template defaults to v1beta2",
			templateVersion: "",
			wantVersion:     "v1beta2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			template := makeMetal3MachineTemplate(tc.templateVersion)

			// Build a fake client; cloneMetal3MachineTemplate does not call the
			// API server (it only reads the in-memory template object), but the
			// signature accepts a client for consistency with the other clone funcs.
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

			got, err := cloneMetal3MachineTemplate(ctx, fakeClient, scheme, template, "test-machine", "default", labels, annotations)
			if err != nil {
				t.Fatalf("cloneMetal3MachineTemplate() error = %v", err)
			}

			u, ok := got.(*unstructured.Unstructured)
			if !ok {
				t.Fatalf("expected *unstructured.Unstructured, got %T", got)
			}

			// GVK checks
			gvk := u.GroupVersionKind()
			if gvk.Group != "infrastructure.cluster.x-k8s.io" {
				t.Errorf("Group = %q, want %q", gvk.Group, "infrastructure.cluster.x-k8s.io")
			}
			if gvk.Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", gvk.Version, tc.wantVersion)
			}
			if gvk.Kind != "Metal3Machine" {
				t.Errorf("Kind = %q, want %q", gvk.Kind, "Metal3Machine")
			}

			// Metadata checks
			if u.GetName() != "test-machine" {
				t.Errorf("Name = %q, want %q", u.GetName(), "test-machine")
			}
			if u.GetNamespace() != "default" {
				t.Errorf("Namespace = %q, want %q", u.GetNamespace(), "default")
			}
			if u.GetLabels()["cluster.x-k8s.io/cluster-name"] != "test-cluster" {
				t.Errorf("Labels missing cluster name label")
			}
			if u.GetAnnotations()["test-key"] != "test-value" {
				t.Errorf("Annotations missing test-key")
			}

			// Spec was copied from spec.template.spec
			imageURL, _, _ := unstructured.NestedString(u.Object, "spec", "image", "url")
			if imageURL != "https://example.com/kairos.qcow2" {
				t.Errorf("spec.image.url = %q, want %q", imageURL, "https://example.com/kairos.qcow2")
			}
		})
	}
}

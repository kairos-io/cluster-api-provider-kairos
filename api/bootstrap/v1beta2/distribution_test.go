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

package v1beta2

import (
	"os"
	"reflect"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// TestDistributionTableMatchesCRDEnum keeps the api-layer distribution table in
// lock-step with the declarative source of truth: the kubebuilder enum and
// default markers on KairosConfigSpec.Distribution, as rendered into the CRD. If
// P1 adds kubeadm, the marker edit (make manifests) and the table change must
// land together or this fails.
func TestDistributionTableMatchesCRDEnum(t *testing.T) {
	const crdPath = "../../../config/crd/bases/bootstrap.cluster.x-k8s.io_kairosconfigs.yaml"
	data, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read CRD %s: %v", crdPath, err)
	}

	var crd map[string]any
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("unmarshal CRD: %v", err)
	}

	dist := crdDistributionSchema(t, crd)

	// enum must equal SupportedDistributions() exactly (order included: the enum
	// order is the admission-message order).
	rawEnum, ok := dist["enum"].([]any)
	if !ok {
		t.Fatalf("CRD distribution has no enum list; got %T", dist["enum"])
	}
	enum := make([]string, 0, len(rawEnum))
	for _, e := range rawEnum {
		enum = append(enum, e.(string))
	}
	if want := SupportedDistributions(); !reflect.DeepEqual(enum, want) {
		t.Errorf("CRD distribution enum %v does not match SupportedDistributions() %v", enum, want)
	}

	// default must equal DefaultDistribution.
	def, _ := dist["default"].(string)
	if def != DefaultDistribution {
		t.Errorf("CRD distribution default %q does not match DefaultDistribution %q", def, DefaultDistribution)
	}
}

// crdDistributionSchema navigates to
// spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.distribution.
func crdDistributionSchema(t *testing.T, crd map[string]any) map[string]any {
	t.Helper()
	child := func(m map[string]any, k string) map[string]any {
		v, ok := m[k].(map[string]any)
		if !ok {
			t.Fatalf("CRD path element %q missing or not a map (got %T)", k, m[k])
		}
		return v
	}

	spec := child(crd, "spec")
	versions, ok := spec["versions"].([]any)
	if !ok || len(versions) == 0 {
		t.Fatalf("CRD spec.versions missing or empty")
	}
	v0, ok := versions[0].(map[string]any)
	if !ok {
		t.Fatalf("CRD spec.versions[0] is not a map")
	}
	schema := child(v0, "schema")
	openAPI := child(schema, "openAPIV3Schema")
	topProps := child(openAPI, "properties")
	specSchema := child(topProps, "spec")
	specProps := child(specSchema, "properties")
	return child(specProps, "distribution")
}

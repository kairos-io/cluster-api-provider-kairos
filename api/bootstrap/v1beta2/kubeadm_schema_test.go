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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// kubeadmSchemaDigest is a SHA-256 over the canonical JSON of the served
// spec.kubeadm sub-schema in the KairosConfig CRD. ADR 0010 OQ-5 requires a
// schema-diff test because spec.kubeadm embeds CAPI kubeadm v1beta2 types, so a
// CAPI bump silently changes OUR served CRD (and can invisibly break a webhook
// refusal that points at a field CAPI renamed or dropped).
//
// When this digest changes you MUST:
//  1. diff the CAPI kubeadm JoinConfiguration schema between the old and new pin;
//  2. confirm every validateKubeadmConfig refusal still points at a field that
//     exists (inline token, unsafeSkipCAVerification, discovery.file);
//  3. re-check the CRD size against the client-side-apply limit;
//  4. update this constant to the value the test prints.
//
// Do NOT update it blindly — that defeats the guard.
const kubeadmSchemaDigest = "11bf64a1dcaefa7b3f8e9542b6f46e3d38a578bf5415a5c3ed9001c2de225adf"

// TestKubeadmSchemaDiff fails when the served spec.kubeadm schema drifts from the
// recorded digest (ADR 0010 OQ-5).
func TestKubeadmSchemaDiff(t *testing.T) {
	const crdPath = "../../../config/crd/bases/bootstrap.cluster.x-k8s.io_kairosconfigs.yaml"
	data, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read CRD %s: %v", crdPath, err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("unmarshal CRD: %v", err)
	}

	specProps := crdSpecProperties(t, crd)
	kubeadm, ok := specProps["kubeadm"].(map[string]any)
	if !ok {
		t.Fatalf("CRD spec.properties.kubeadm missing or not a map (got %T); the kubeadm embedding is not served", specProps["kubeadm"])
	}

	// json.Marshal sorts map keys, so this is a stable canonical form.
	canonical, err := json.Marshal(kubeadm)
	if err != nil {
		t.Fatalf("marshal kubeadm schema: %v", err)
	}
	sum := sha256.Sum256(canonical)
	got := hex.EncodeToString(sum[:])
	if got != kubeadmSchemaDigest {
		t.Errorf("served spec.kubeadm schema digest changed:\n  recorded: %s\n  current:  %s\n"+
			"This means the embedded CAPI kubeadm schema drifted. Review the CAPI kubeadm "+
			"JoinConfiguration diff, confirm every validateKubeadmConfig refusal still points "+
			"at a live field, re-check the CRD size, then update kubeadmSchemaDigest.", kubeadmSchemaDigest, got)
	}
}

// crdSpecProperties navigates to
// spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.
func crdSpecProperties(t *testing.T, crd map[string]any) map[string]any {
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
	return child(specSchema, "properties")
}

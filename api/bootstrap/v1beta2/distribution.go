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
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

// Distributions a KairosConfig can install (the spec.distribution enum values),
// and what "" means. TestDistributionTableMatchesCRDEnum keeps these equal to the
// kubebuilder enum and default markers on KairosConfigSpec.Distribution.
//
// This is the single, api-layer source of truth for the accepted distribution
// names. It carries no controller or renderer concerns (api/ must not import
// internal/), so the per-layer tables in internal/bootstrap and
// internal/controllers/bootstrap register against these constants and
// consistency tests keep them in agreement.
const (
	DistributionK0s     = "k0s"
	DistributionK3s     = "k3s"
	DefaultDistribution = DistributionK0s
)

// supportedDistributions is the accepted set, in admission-message order. It is
// the order rendered into "spec.distribution must be one of [...]".
var supportedDistributions = []string{DistributionK0s, DistributionK3s}

// EffectiveDistribution returns d, or DefaultDistribution when d is empty. It
// does not validate: an unknown non-empty name is returned unchanged.
func EffectiveDistribution(d string) string {
	if d == "" {
		return DefaultDistribution
	}
	return d
}

// SupportedDistributions returns a copy of the accepted names, in
// admission-message order.
func SupportedDistributions() []string {
	out := make([]string, len(supportedDistributions))
	copy(out, supportedDistributions)
	return out
}

// IsSupportedDistribution reports whether d is an accepted name. "" is not a
// name; it means the default, so it reports false.
func IsSupportedDistribution(d string) bool {
	for _, s := range supportedDistributions {
		if s == d {
			return true
		}
	}
	return false
}

// distributionRules is the per-distribution part of KairosConfig admission. Each
// rule owns its own gates: the k0s and k3s rules return nil unless
// Role == "worker", matching the pre-seam worker-token switch. They run after the
// name check and before the credential check, preserving the aggregate order.
type distributionRules struct {
	name     string
	validate func(kc *KairosConfig) field.ErrorList
}

// bootstrapDistributions is the api-layer registration table, keyed by the
// distribution constants. The order is not significant for lookup; the
// admission-message order is supportedDistributions.
var bootstrapDistributions = []distributionRules{
	{DistributionK0s, validateK0sConfig}, // the default and the pre-seam default: arm
	{DistributionK3s, validateK3sConfig}, // the pre-seam case "k3s": arm
}

// rulesForDistribution returns the rules for the effective distribution, falling
// back to the DefaultDistribution's rules for an unknown name — exactly what the
// pre-seam `default:` arm did (OQ-C). An unknown name can only be observed
// through a direct call, since the CRD enum is checked before the webhook runs.
func rulesForDistribution(effective string) distributionRules {
	var fallback distributionRules
	for _, d := range bootstrapDistributions {
		if d.name == effective {
			return d
		}
		if d.name == DefaultDistribution {
			fallback = d
		}
	}
	return fallback
}

// validateDistribution replaces the inline distribution name-check and
// worker-token switch in the webhook's validate(). It emits the byte-identical
// enum message, then runs the effective distribution's rules; the aggregate
// order (name error, then token error) is preserved.
func (r *KairosConfig) validateDistribution() field.ErrorList {
	var errs field.ErrorList
	path := field.NewPath("spec", "distribution")

	// Name check — byte-identical to the previous inline message.
	if r.Spec.Distribution != "" && !IsSupportedDistribution(r.Spec.Distribution) {
		errs = append(errs, field.Invalid(
			path,
			r.Spec.Distribution,
			fmt.Sprintf("%s must be one of [%s]", path.String(), strings.Join(SupportedDistributions(), ", ")),
		))
	}

	// Per-distribution rules of the effective distribution (unknown -> default).
	errs = append(errs, rulesForDistribution(EffectiveDistribution(r.Spec.Distribution)).validate(r)...)
	return errs
}

// validateK0sConfig is the k0s worker-token rule (the pre-seam default: arm).
func validateK0sConfig(kc *KairosConfig) field.ErrorList {
	if kc.Spec.Role != "worker" {
		return nil
	}
	hasToken := kc.Spec.WorkerToken != ""
	hasTokenRef := kc.Spec.WorkerTokenSecretRef != nil && kc.Spec.WorkerTokenSecretRef.Name != ""
	if !hasToken && !hasTokenRef {
		return field.ErrorList{field.Required(
			field.NewPath("spec", "workerToken"),
			"worker KairosConfig requires either spec.workerToken or spec.workerTokenSecretRef to be set",
		)}
	}
	return nil
}

// validateK3sConfig is the k3s worker-token rule (the pre-seam case "k3s": arm).
func validateK3sConfig(kc *KairosConfig) field.ErrorList {
	if kc.Spec.Role != "worker" {
		return nil
	}
	hasK3sToken := kc.Spec.K3sToken != ""
	hasK3sTokenRef := kc.Spec.K3sTokenSecretRef != nil && kc.Spec.K3sTokenSecretRef.Name != ""
	hasWorkerToken := kc.Spec.WorkerToken != ""
	hasWorkerTokenRef := kc.Spec.WorkerTokenSecretRef != nil && kc.Spec.WorkerTokenSecretRef.Name != ""
	if !hasK3sToken && !hasK3sTokenRef && !hasWorkerToken && !hasWorkerTokenRef {
		return field.ErrorList{field.Required(
			field.NewPath("spec", "k3sToken"),
			"k3s worker requires spec.k3sToken, spec.k3sTokenSecretRef, spec.workerToken, or spec.workerTokenSecretRef to be set",
		)}
	}
	return nil
}

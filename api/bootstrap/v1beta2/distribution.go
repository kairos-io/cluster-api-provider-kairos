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
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
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
	DistributionKubeadm = "kubeadm"
	DefaultDistribution = DistributionK0s
)

// supportedDistributions is the accepted set, in admission-message order. It is
// the order rendered into "spec.distribution must be one of [...]".
var supportedDistributions = []string{DistributionK0s, DistributionK3s, DistributionKubeadm}

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
	{DistributionK0s, validateK0sConfig},         // the default and the pre-seam default: arm
	{DistributionK3s, validateK3sConfig},         // the pre-seam case "k3s": arm
	{DistributionKubeadm, validateKubeadmConfig}, // ADR 0010 P1: hosted worker join
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

// validateKubeadmConfig is the kubeadm admission rule (ADR 0010 P1). Unlike k0s
// and k3s, a kubeadm worker needs no token in the spec: the controller mints a
// short-lived bootstrap token against the workload cluster after an owner-verified
// trust check, so this rule is purely a set of refusals. It runs for every role
// (not gated on "worker") because the control-plane refusal is one of them.
//
// Refusals (ADR 0010 P1 API section):
//   - role control-plane: a kubeadm KairosControlPlane is P2; P1 is worker-only.
//   - an inline bootstrap token on the join discovery: tokens never appear in the
//     spec (the controller mints and refreshes them).
//   - unsafeSkipCAVerification: it disables the CA pin, defeating the trust chain.
//   - discovery.file: the file-discovery path is out of P1 scope.
//   - any "{{" anywhere in the kubeadm block: Jinja placeholders are not rendered
//     on Kairos, and the JoinConfiguration is marshalled into a file on the node,
//     so an unexpanded "{{ ... }}" would land verbatim in node-side config.
//   - a nodeRegistration.name that is not a DNS-1123 subdomain (it becomes the
//     Node object name and the kubelet client-cert CommonName).
func validateKubeadmConfig(kc *KairosConfig) field.ErrorList {
	var errs field.ErrorList

	if kc.Spec.Role == "control-plane" {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "role"),
			kc.Spec.Role,
			"kubeadm is supported only for worker nodes in this release; a kubeadm "+
				"KairosControlPlane is deferred to a later phase (ADR 0010 P2). Set "+
				"spec.role to worker, or use the k0s/k3s distribution for a control plane.",
		))
	}

	ka := kc.Spec.Kubeadm
	if ka == nil {
		return errs
	}
	kaPath := field.NewPath("spec", "kubeadm")

	// No "{{" anywhere in the kubeadm block. Marshalling the whole block catches
	// every user-settable string that is rendered into the on-node file, including
	// fields not called out individually below.
	if raw, err := json.Marshal(ka); err == nil {
		if strings.Contains(string(raw), "{{") {
			errs = append(errs, field.Invalid(
				kaPath, "<redacted>",
				"spec.kubeadm must not contain '{{': Jinja-style placeholders are not "+
					"expanded on Kairos and would be written verbatim into the node's "+
					"kubeadm configuration",
			))
		}
	}

	jc := ka.JoinConfiguration
	if jc == nil {
		return errs
	}
	jcPath := kaPath.Child("joinConfiguration")

	// Inline bootstrap token on discovery — tokens never live in the spec.
	if jc.Discovery.BootstrapToken.Token != "" {
		errs = append(errs, field.Forbidden(
			jcPath.Child("discovery", "bootstrapToken", "token"),
			"an inline bootstrap token is not allowed: the controller mints and refreshes "+
				"a short-lived token against the workload cluster",
		))
	}

	// unsafeSkipCAVerification defeats the CA pin.
	if jc.Discovery.BootstrapToken.UnsafeSkipCAVerification != nil && *jc.Discovery.BootstrapToken.UnsafeSkipCAVerification {
		errs = append(errs, field.Forbidden(
			jcPath.Child("discovery", "bootstrapToken", "unsafeSkipCAVerification"),
			"unsafeSkipCAVerification is not allowed: it disables the cluster-CA pin that the join relies on",
		))
	}

	// discovery.file is out of P1 scope.
	if jc.Discovery.File.KubeConfigPath != "" {
		errs = append(errs, field.Forbidden(
			jcPath.Child("discovery", "file"),
			"file-based discovery is not supported in this release; the controller "+
				"configures bootstrap-token discovery",
		))
	}

	// nodeRegistration.name, when set, must be a DNS-1123 subdomain.
	if name := jc.NodeRegistration.Name; name != "" {
		if msgs := validation.IsDNS1123Subdomain(name); len(msgs) > 0 {
			errs = append(errs, field.Invalid(
				jcPath.Child("nodeRegistration", "name"), name,
				"nodeRegistration.name must be a valid DNS-1123 subdomain (it becomes the Node name)",
			))
		}
	}

	return errs
}

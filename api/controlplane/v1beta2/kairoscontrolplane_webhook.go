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
	"regexp"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// sshFallbackUserRegex matches the same pattern the kubebuilder marker on
// SSHFallback.User enforces. Kept here as the webhook's defensive validator
// because CRD-level pattern validation is not a substitute for admission
// validation (older API servers, defaulting paths, custom storage versions).
var sshFallbackUserRegex = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// log is for logging in this package.
var kairoscontrolplaneLog = logf.Log.WithName("kairoscontrolplane-resource")

// SetupWebhookWithManager sets up the webhook with the Manager.
func (r *KairosControlPlane) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-controlplane-cluster-x-k8s-io-v1beta2-kairoscontrolplane,mutating=true,failurePolicy=fail,sideEffects=None,groups=controlplane.cluster.x-k8s.io,resources=kairoscontrolplanes,verbs=create;update,versions=v1beta2,name=mkairoscontrolplane.kb.io,admissionReviewVersions=v1

var _ webhook.Defaulter = &KairosControlPlane{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *KairosControlPlane) Default() {
	kairoscontrolplaneLog.Info("default", "name", r.Name)

	// Set default replicas to 1 if not specified
	if r.Spec.Replicas == nil {
		replicas := int32(1)
		r.Spec.Replicas = &replicas
	}

	// Set default distribution
	if r.Spec.Distribution == "" {
		r.Spec.Distribution = "k0s"
	}

	defaultSSHFallback(r.Spec.SSHFallback)
}

// defaultSSHFallback applies runtime defaults to a non-nil SSHFallback
// block. Static kubebuilder defaults (Enabled=false, User=kairos, Port=22,
// ActivateAfter=15m) are applied declaratively at CRD level; this helper
// is the second-line defaulter for older API servers and for paths that
// bypass the declarative defaulting (storage version upgrades, conversion
// webhooks).
//
// Defaults are only applied when the field is the Go zero value AND the
// SSHFallback block itself is present. If the operator does not set
// Spec.SSHFallback at all, this helper is a no-op — nil stays nil, which
// the runtime treats identically to Enabled=false.
func defaultSSHFallback(s *SSHFallback) {
	if s == nil {
		return
	}
	if s.User == "" {
		s.User = "kairos"
	}
	if s.Port == 0 {
		s.Port = 22
	}
	if s.ActivateAfter == nil {
		s.ActivateAfter = &metav1.Duration{Duration: 15 * time.Minute}
	}
}

//+kubebuilder:webhook:path=/validate-controlplane-cluster-x-k8s-io-v1beta2-kairoscontrolplane,mutating=false,failurePolicy=fail,sideEffects=None,groups=controlplane.cluster.x-k8s.io,resources=kairoscontrolplanes,verbs=create;update,versions=v1beta2,name=vkairoscontrolplane.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &KairosControlPlane{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *KairosControlPlane) ValidateCreate() (admission.Warnings, error) {
	kairoscontrolplaneLog.Info("validate create", "name", r.Name)
	return nil, r.validate()
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *KairosControlPlane) ValidateUpdate(old runtime.Object) (admission.Warnings, error) {
	kairoscontrolplaneLog.Info("validate update", "name", r.Name)
	return nil, r.validate()
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *KairosControlPlane) ValidateDelete() (admission.Warnings, error) {
	kairoscontrolplaneLog.Info("validate delete", "name", r.Name)
	return nil, nil
}

// validate performs validation on the KairosControlPlane spec.
//
// The replicas bounds check is duplicated declaratively via
// kubebuilder:validation:Minimum=1 / Maximum=1 markers on the type. The webhook
// remains as a second line of defense and to surface a clearer message when
// users hit the upper bound — the CRD-level message ("spec.replicas in body
// should be less than or equal to 1") tells them WHAT is wrong but not WHY.
func (r *KairosControlPlane) validate() error {
	var allErrs field.ErrorList

	// Validate replicas. Lower bound: must be >= 1 (control plane with zero
	// machines is nonsensical). Upper bound: must be <= 1 in this release —
	// see the field doc-comment and foundational-review item KD-5 for context.
	if r.Spec.Replicas != nil {
		switch {
		case *r.Spec.Replicas < 1:
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("spec", "replicas"),
				*r.Spec.Replicas,
				"spec.replicas must be greater than or equal to 1",
			))
		case *r.Spec.Replicas > 1:
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("spec", "replicas"),
				*r.Spec.Replicas,
				"spec.replicas > 1 is not supported in this release: the current "+
					"control-plane implementation would produce N independent "+
					"single-node clusters instead of an HA cluster. HA support "+
					"(both classic and P2P/decentralized) is planned for a "+
					"future release. Use spec.replicas: 1 for now.",
			))
		}
	}

	// Validate distribution
	if r.Spec.Distribution != "" && r.Spec.Distribution != "k0s" && r.Spec.Distribution != "k3s" {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "distribution"),
			r.Spec.Distribution,
			"spec.distribution must be one of [k0s, k3s]",
		))
	}

	allErrs = append(allErrs, validateSSHFallback(r.Spec.SSHFallback, r.Namespace, field.NewPath("spec", "sshFallback"))...)

	if len(allErrs) > 0 {
		return errors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "KairosControlPlane"},
			r.Name,
			allErrs,
		)
	}

	return nil
}

// validateSSHFallback enforces the cross-field invariants of the opt-in
// SSH fallback block. Designed to be called from any webhook that admits
// a KairosControlPlaneSpec (currently only the KCP webhook itself; the
// KairosControlPlaneTemplate webhook will adopt this helper when it
// lands — templates don't go through reconciliation but admitting them
// with invalid SSHFallback configs would still produce broken KCPs when
// instantiated).
//
// Rules:
//
//  1. If Enabled=true, KnownHostsSecretRef MUST be non-nil with a
//     non-empty Name. Without it the controller cannot verify the
//     workload node's host key, and TOFU is explicitly NOT a path.
//  2. If Enabled=true, IdentitySecretRef MUST be non-nil with a
//     non-empty Name. The controller has no other way to authenticate
//     to the workload node.
//  3. ActivateAfter must be strictly greater than KubeconfigReadyTimeout.
//     The fallback is meant to fire AFTER operators see the Info →
//     Warning escalation on KubeconfigReadyCondition, not before. The
//     strict invariant lets us catch misconfiguration at admission
//     rather than at runtime.
//  4. Cross-namespace Secret references are rejected in this release.
//     A future PR will introduce an allow-list mechanism; until then
//     the only safe default is same-namespace.
//
// Enabled=false (with or without other fields populated) is always
// valid — operators can stage a configured-but-disabled block as a
// known-good template they will flip on later.
func validateSSHFallback(s *SSHFallback, ownerNamespace string, base *field.Path) field.ErrorList {
	var errs field.ErrorList
	if s == nil || !s.Enabled {
		return errs
	}

	if s.KnownHostsSecretRef == nil || s.KnownHostsSecretRef.Name == "" {
		errs = append(errs, field.Required(
			base.Child("knownHostsSecretRef"),
			"knownHostsSecretRef is required when sshFallback.enabled is true; host-key verification cannot be bypassed",
		))
	} else if s.KnownHostsSecretRef.Namespace != "" && s.KnownHostsSecretRef.Namespace != ownerNamespace {
		errs = append(errs, field.Forbidden(
			base.Child("knownHostsSecretRef", "namespace"),
			"cross-namespace Secret references are not allowed in this release",
		))
	}

	if s.IdentitySecretRef == nil || s.IdentitySecretRef.Name == "" {
		errs = append(errs, field.Required(
			base.Child("identitySecretRef"),
			"identitySecretRef is required when sshFallback.enabled is true; the controller has no other way to authenticate to the workload node",
		))
	} else if s.IdentitySecretRef.Namespace != "" && s.IdentitySecretRef.Namespace != ownerNamespace {
		errs = append(errs, field.Forbidden(
			base.Child("identitySecretRef", "namespace"),
			"cross-namespace Secret references are not allowed in this release",
		))
	}

	if s.ActivateAfter != nil && s.ActivateAfter.Duration <= KubeconfigReadyTimeout {
		errs = append(errs, field.Invalid(
			base.Child("activateAfter"),
			s.ActivateAfter.Duration.String(),
			"activateAfter must be strictly greater than the controller's KubeconfigReadyTimeout ("+KubeconfigReadyTimeout.String()+"); the fallback is meant to fire AFTER the Info → Warning escalation on KubeconfigReadyCondition, not before",
		))
	}

	if s.User != "" && !sshFallbackUserRegex.MatchString(s.User) {
		errs = append(errs, field.Invalid(
			base.Child("user"),
			s.User,
			"user must match the POSIX-portable pattern ^[a-z_][a-z0-9_-]{0,31}$",
		))
	}

	return errs
}

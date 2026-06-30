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
	"net"
	"regexp"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
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

// vipInterfaceRe matches valid Linux network interface names: 1–15 characters,
// starting with a letter, followed by letters, digits, dots, underscores, or
// hyphens. Mirrors the kubebuilder marker on KubeVIPConfig.Interface; the
// webhook adds a second line of defense for older API servers.
var vipInterfaceRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9._-]{0,14}$`)

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
	return r.validateWithWarnings()
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *KairosControlPlane) ValidateUpdate(old runtime.Object) (admission.Warnings, error) {
	kairoscontrolplaneLog.Info("validate update", "name", r.Name)
	return r.validateWithWarnings()
}

// validateWithWarnings runs validate() and also collects non-blocking warnings.
func (r *KairosControlPlane) validateWithWarnings() (admission.Warnings, error) {
	var warnings admission.Warnings

	// Warn (but do not reject) when a VIP block is set on a single-node
	// control plane. The VIP configuration will be silently ignored by the
	// controller until replicas is increased to 3 or 5, so surfacing this as a
	// warning lets operators catch configuration drift early.
	if r.Spec.HA != nil && r.Spec.HA.VIP != nil &&
		r.Spec.Replicas != nil && *r.Spec.Replicas == 1 {
		warnings = append(warnings,
			"spec.ha.vip is set but spec.replicas is 1: kube-vip is not "+
				"rendered for single-node control planes. Remove spec.ha.vip "+
				"or set spec.replicas to 3 or 5.",
		)
	}

	return warnings, r.validate()
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *KairosControlPlane) ValidateDelete() (admission.Warnings, error) {
	kairoscontrolplaneLog.Info("validate delete", "name", r.Name)
	return nil, nil
}

// validate performs validation on the KairosControlPlane spec.
//
// The replica bounds (Minimum=1, Maximum=5) are also enforced declaratively
// via kubebuilder markers on the type. The webhook is a second line of defense
// and provides the etcd-quorum explanation that the CRD-level message omits.
func (r *KairosControlPlane) validate() error {
	var allErrs field.ErrorList

	// Validate replicas. Three rejection arms:
	//   1. below minimum — a control plane with zero machines is nonsensical.
	//   2. above maximum — beyond 5 etcd members the quorum cost outweighs
	//      the additional fault tolerance for a control plane.
	//   3. even count — provides the same etcd fault tolerance as the
	//      next-lower odd count while increasing the quorum requirement;
	//      always replace an even count with the next-higher odd count.
	if r.Spec.Replicas != nil {
		n := *r.Spec.Replicas
		replicasPath := field.NewPath("spec", "replicas")
		switch {
		case n < 1:
			allErrs = append(allErrs, field.Invalid(
				replicasPath, n,
				"spec.replicas must be at least 1",
			))
		case n > 5:
			allErrs = append(allErrs, field.Invalid(
				replicasPath, n,
				"spec.replicas must be 1, 3, or 5; values above 5 are not "+
					"supported. etcd fault tolerance is (N-1)/2 failures; beyond "+
					"5 members the quorum cost outweighs the additional fault "+
					"tolerance for a control plane.",
			))
		case n%2 == 0:
			allErrs = append(allErrs, field.Invalid(
				replicasPath, n,
				"spec.replicas must be an odd number (1, 3, or 5). Even replica "+
					"counts give the same etcd fault tolerance as the next-lower "+
					"odd count (e.g. 4 tolerates 1 failure, the same as 3) while "+
					"increasing the quorum requirement. Use 3 instead of 2 or 4, "+
					"and 5 instead of 6.",
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

	allErrs = append(allErrs, validateHA(r.Spec.HA, field.NewPath("spec", "ha"))...)
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

// validateHA validates the optional HA configuration block. When ha is nil
// (single-node or unset HA), it is a no-op. Shape validation of the VIP
// address and interface name runs unconditionally when VIP is non-nil;
// the controller enforces operational constraints (e.g., CAPK exception)
// at runtime.
func validateHA(ha *HAConfig, base *field.Path) field.ErrorList {
	var errs field.ErrorList
	if ha == nil || ha.VIP == nil {
		return errs
	}
	v := ha.VIP
	addrPath := base.Child("vip", "address")

	// Address: try net.ParseIP first (authoritative for IPv4 + IPv6); fall
	// back to IsDNS1123Subdomain for hostnames. net.ParseIP is the canonical
	// IP parser so it handles all valid IPv4 and IPv6 forms including
	// compressed IPv6. IsDNS1123Subdomain covers hostname forms like
	// "cp.example.com" or bare single-label names like "cp-vip".
	if net.ParseIP(v.Address) == nil {
		if msgs := validation.IsDNS1123Subdomain(v.Address); len(msgs) > 0 {
			errs = append(errs, field.Invalid(
				addrPath, v.Address,
				"vip.address must be a valid IPv4 address, IPv6 address, or "+
					"RFC-1123 hostname (e.g. \"192.168.1.10\", \"2001:db8::1\", "+
					"or \"cp.example.com\")",
			))
		}
	}

	// Interface: mirrors the kubebuilder marker; belt-and-suspenders for
	// older API servers that may skip CRD-level pattern validation.
	if !vipInterfaceRe.MatchString(v.Interface) {
		errs = append(errs, field.Invalid(
			base.Child("vip", "interface"), v.Interface,
			"vip.interface must be a valid Linux network interface name: "+
				"1–15 characters, starting with a letter, followed by "+
				"letters, digits, '.', '_', or '-' (e.g. \"eth0\", \"ens3\", \"bond0\")",
		))
	}

	return errs
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

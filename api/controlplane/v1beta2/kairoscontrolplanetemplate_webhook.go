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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// log is for logging in this package.
var kairoscontrolplanetemplateLog = logf.Log.WithName("kairoscontrolplanetemplate-resource")

// SetupWebhookWithManager sets up the webhook with the Manager.
func (r *KairosControlPlaneTemplate) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-controlplane-cluster-x-k8s-io-v1beta2-kairoscontrolplanetemplate,mutating=true,failurePolicy=fail,sideEffects=None,groups=controlplane.cluster.x-k8s.io,resources=kairoscontrolplanetemplates,verbs=create;update,versions=v1beta2,name=mkairoscontrolplanetemplate.kb.io,admissionReviewVersions=v1

var _ webhook.Defaulter = &KairosControlPlaneTemplate{}

// Default implements webhook.Defaulter. Applies the same field defaults
// the KCP webhook applies, but on the nested template spec
// (r.Spec.Template.Spec). Operators creating a template should see the
// same defaulted shape they would see on a KCP after admission, so
// `kubectl get kairoscontrolplanetemplate -o yaml` reflects the values
// CAPI's MachineDeployment / template stamping will produce.
func (r *KairosControlPlaneTemplate) Default() {
	kairoscontrolplanetemplateLog.Info("default", "name", r.Name)

	s := &r.Spec.Template.Spec

	// Replicas default — mirrors KCP.Default().
	if s.Replicas == nil {
		replicas := int32(1)
		s.Replicas = &replicas
	}

	// Distribution default — mirrors KCP.Default().
	if s.Distribution == "" {
		s.Distribution = "k0s"
	}

	// SSHFallback default — shared helper with KCP, defined in
	// kairoscontrolplane_webhook.go. The helper is a no-op on nil
	// blocks, so leaving SSHFallback unset on a template is idiomatic
	// and admission-clean.
	defaultSSHFallback(s.SSHFallback)
}

//+kubebuilder:webhook:path=/validate-controlplane-cluster-x-k8s-io-v1beta2-kairoscontrolplanetemplate,mutating=false,failurePolicy=fail,sideEffects=None,groups=controlplane.cluster.x-k8s.io,resources=kairoscontrolplanetemplates,verbs=create;update,versions=v1beta2,name=vkairoscontrolplanetemplate.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &KairosControlPlaneTemplate{}

// ValidateCreate implements webhook.Validator.
func (r *KairosControlPlaneTemplate) ValidateCreate() (admission.Warnings, error) {
	kairoscontrolplanetemplateLog.Info("validate create", "name", r.Name)
	return nil, r.validate()
}

// ValidateUpdate implements webhook.Validator.
func (r *KairosControlPlaneTemplate) ValidateUpdate(_ runtime.Object) (admission.Warnings, error) {
	kairoscontrolplanetemplateLog.Info("validate update", "name", r.Name)
	return nil, r.validate()
}

// ValidateDelete implements webhook.Validator.
func (r *KairosControlPlaneTemplate) ValidateDelete() (admission.Warnings, error) {
	kairoscontrolplanetemplateLog.Info("validate delete", "name", r.Name)
	return nil, nil
}

// validate mirrors the KairosControlPlane webhook's validate() against
// the nested template spec. The two validation functions share the same
// per-field rules and the SSHFallback helper; the field paths in error
// messages differ (`spec.template.spec.*` vs. `spec.*`) so operators
// reading admission errors can tell which resource type the rejection
// came from.
//
// Per CAPI convention, anything invalid in the controlled KCP type
// MUST be invalid in the template too — otherwise an operator can
// stage a known-invalid template and only discover the problem at KCP
// creation time, which defeats the purpose of templating.
func (r *KairosControlPlaneTemplate) validate() error {
	var allErrs field.ErrorList
	s := &r.Spec.Template.Spec
	base := field.NewPath("spec", "template", "spec")

	// Replicas: same bounds as KCP (min 1, max 1 in this release; see
	// KD-5 for the HA roadmap).
	if s.Replicas != nil {
		switch {
		case *s.Replicas < 1:
			allErrs = append(allErrs, field.Invalid(
				base.Child("replicas"),
				*s.Replicas,
				"spec.template.spec.replicas must be greater than or equal to 1",
			))
		case *s.Replicas > 1:
			allErrs = append(allErrs, field.Invalid(
				base.Child("replicas"),
				*s.Replicas,
				"spec.template.spec.replicas > 1 is not supported in this release: the current "+
					"control-plane implementation would produce N independent "+
					"single-node clusters instead of an HA cluster. HA support "+
					"(both classic and P2P/decentralized) is planned for a "+
					"future release. Use spec.template.spec.replicas: 1 for now.",
			))
		}
	}

	// Distribution: same enum as KCP.
	if s.Distribution != "" && s.Distribution != "k0s" && s.Distribution != "k3s" {
		allErrs = append(allErrs, field.Invalid(
			base.Child("distribution"),
			s.Distribution,
			"spec.template.spec.distribution must be one of [k0s, k3s]",
		))
	}

	// SSHFallback: shared helper with KCP. The helper takes the owner
	// namespace because cross-namespace Secret refs are rejected and
	// the template's namespace is the same one a stamped KCP would
	// live in.
	allErrs = append(allErrs, validateSSHFallback(s.SSHFallback, r.Namespace, base.Child("sshFallback"))...)

	if len(allErrs) > 0 {
		return errors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "KairosControlPlaneTemplate"},
			r.Name,
			allErrs,
		)
	}

	return nil
}

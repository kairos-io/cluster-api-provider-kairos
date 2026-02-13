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

// validate performs validation on the KairosControlPlane spec
func (r *KairosControlPlane) validate() error {
	var allErrs field.ErrorList

	// Validate replicas
	if r.Spec.Replicas != nil && *r.Spec.Replicas < 1 {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "replicas"),
			*r.Spec.Replicas,
			"spec.replicas must be greater than or equal to 1",
		))
	}

	// Validate distribution
	if r.Spec.Distribution != "" && r.Spec.Distribution != "k0s" && r.Spec.Distribution != "k3s" {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "distribution"),
			r.Spec.Distribution,
			"spec.distribution must be one of [k0s, k3s]",
		))
	}

	if len(allErrs) > 0 {
		return errors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "KairosControlPlane"},
			r.Name,
			allErrs,
		)
	}

	return nil
}

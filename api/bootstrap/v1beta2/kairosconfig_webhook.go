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
	"strings"

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
var kairosconfigLog = logf.Log.WithName("kairosconfig-resource")

// SetupWebhookWithManager sets up the webhook with the Manager.
func (r *KairosConfig) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-bootstrap-cluster-x-k8s-io-v1beta2-kairosconfig,mutating=true,failurePolicy=fail,sideEffects=None,groups=bootstrap.cluster.x-k8s.io,resources=kairosconfigs,verbs=create;update,versions=v1beta2,name=mkairosconfig.kb.io,admissionReviewVersions=v1

var _ webhook.Defaulter = &KairosConfig{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *KairosConfig) Default() {
	kairosconfigLog.Info("default", "name", r.Name)

	// Set defaults for user configuration. UserPassword is no longer defaulted
	// here (KD-3a, v0.1.0-alpha.2): the previous "kairos" default put a known
	// credential on every node and the validating webhook now requires the
	// user to set at least one explicit credential (userPassword,
	// userPasswordSecretRef, sshPublicKey, or gitHubUser).
	if r.Spec.UserName == "" {
		r.Spec.UserName = "kairos"
	}
	if len(r.Spec.UserGroups) == 0 {
		r.Spec.UserGroups = []string{"admin"}
	}

	// Set default distribution
	if r.Spec.Distribution == "" {
		r.Spec.Distribution = "k0s"
	}

	// Set default role
	if r.Spec.Role == "" {
		r.Spec.Role = "worker"
	}
}

//+kubebuilder:webhook:path=/validate-bootstrap-cluster-x-k8s-io-v1beta2-kairosconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=bootstrap.cluster.x-k8s.io,resources=kairosconfigs,verbs=create;update,versions=v1beta2,name=vkairosconfig.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &KairosConfig{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *KairosConfig) ValidateCreate() (admission.Warnings, error) {
	kairosconfigLog.Info("validate create", "name", r.Name)
	return nil, r.validate()
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *KairosConfig) ValidateUpdate(old runtime.Object) (admission.Warnings, error) {
	kairosconfigLog.Info("validate update", "name", r.Name)
	return nil, r.validate()
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *KairosConfig) ValidateDelete() (admission.Warnings, error) {
	kairosconfigLog.Info("validate delete", "name", r.Name)
	return nil, nil
}

// validate performs validation on the KairosConfig spec
func (r *KairosConfig) validate() error {
	var allErrs field.ErrorList

	// Validate role
	if r.Spec.Role != "control-plane" && r.Spec.Role != "worker" {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "role"),
			r.Spec.Role,
			"spec.role must be one of [control-plane, worker]",
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

	// Validate worker token requirement
	if r.Spec.Role == "worker" {
		switch r.Spec.Distribution {
		case "k3s":
			hasK3sToken := r.Spec.K3sToken != ""
			hasK3sTokenRef := r.Spec.K3sTokenSecretRef != nil && r.Spec.K3sTokenSecretRef.Name != ""
			hasWorkerToken := r.Spec.WorkerToken != ""
			hasWorkerTokenRef := r.Spec.WorkerTokenSecretRef != nil && r.Spec.WorkerTokenSecretRef.Name != ""
			if !hasK3sToken && !hasK3sTokenRef && !hasWorkerToken && !hasWorkerTokenRef {
				allErrs = append(allErrs, field.Required(
					field.NewPath("spec", "k3sToken"),
					"k3s worker requires spec.k3sToken, spec.k3sTokenSecretRef, spec.workerToken, or spec.workerTokenSecretRef to be set",
				))
			}
		default:
			hasToken := r.Spec.WorkerToken != ""
			hasTokenRef := r.Spec.WorkerTokenSecretRef != nil && r.Spec.WorkerTokenSecretRef.Name != ""
			if !hasToken && !hasTokenRef {
				allErrs = append(allErrs, field.Required(
					field.NewPath("spec", "workerToken"),
					"worker KairosConfig requires either spec.workerToken or spec.workerTokenSecretRef to be set",
				))
			}
		}
	}

	// KD-3a: require at least one explicit credential. Previously UserPassword
	// silently defaulted to "kairos", which combined with PasswordAuthentication
	// yes in the rendered cloud-config gave every node well-known SSH access.
	// At least one of these must now be set:
	//   - userPassword            (inline; discouraged)
	//   - userPasswordSecretRef   (Secret reference; recommended for passwords)
	//   - sshPublicKey            (raw SSH public key; recommended)
	//   - gitHubUser              (fetches SSH keys from GitHub at first boot)
	hasInlinePassword := r.Spec.UserPassword != ""
	hasPasswordRef := r.Spec.UserPasswordSecretRef != nil && r.Spec.UserPasswordSecretRef.Name != ""
	hasSSHKey := r.Spec.SSHPublicKey != ""
	hasGitHubUser := r.Spec.GitHubUser != ""
	if !hasInlinePassword && !hasPasswordRef && !hasSSHKey && !hasGitHubUser {
		allErrs = append(allErrs, field.Required(
			field.NewPath("spec"),
			"at least one of spec.userPassword, spec.userPasswordSecretRef, "+
				"spec.sshPublicKey, or spec.gitHubUser must be set so the "+
				"node has a working authentication mechanism. The previous "+
				"behavior of defaulting userPassword to 'kairos' was removed "+
				"in v0.1.0-alpha.2; see release notes for details.",
		))
	}

	// Validate spec.files entries.
	for i, f := range r.Spec.Files {
		fPath := field.NewPath("spec", "files").Index(i)
		// Path must be absolute (kubebuilder marker covers the ^/ prefix; here we
		// also catch ".." traversal that no declarative pattern can express).
		if !strings.HasPrefix(f.Path, "/") {
			allErrs = append(allErrs, field.Invalid(
				fPath.Child("path"),
				f.Path,
				"path must be absolute (must begin with '/')",
			))
		} else {
			// Check the ORIGINAL path for ".." segments — not the cleaned form,
			// since filepath.Clean resolves ".." and would hide the traversal.
			for _, seg := range strings.Split(f.Path, "/") {
				if seg == ".." {
					allErrs = append(allErrs, field.Invalid(
						fPath.Child("path"),
						f.Path,
						"path must not contain '..' path traversal segments",
					))
					break
				}
			}
		}
		// Permissions: optional, must be octal if set.
		if f.Permissions != "" && !webhookOctalPermissionsRe.MatchString(f.Permissions) {
			allErrs = append(allErrs, field.Invalid(
				fPath.Child("permissions"),
				f.Permissions,
				"permissions must be an octal string (e.g. \"0644\" or \"644\")",
			))
		}
		// Owner: optional, must match POSIX username[:[group]] if set.
		if f.Owner != "" && !webhookOwnerRe.MatchString(f.Owner) {
			allErrs = append(allErrs, field.Invalid(
				fPath.Child("owner"),
				f.Owner,
				"owner must be in user or user:group format using POSIX username characters (e.g. \"root\" or \"root:root\")",
			))
		}
	}

	if len(allErrs) > 0 {
		return errors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "KairosConfig"},
			r.Name,
			allErrs,
		)
	}

	return nil
}

// webhookOctalPermissionsRe matches octal file-permission strings such as
// "644", "0644", "1755". It mirrors the kubebuilder marker on File.Permissions.
var webhookOctalPermissionsRe = regexp.MustCompile(`^0?[0-7]{3,4}$`)

// webhookOwnerRe matches user or user:group strings with POSIX username
// characters. It mirrors the kubebuilder marker on File.Owner.
var webhookOwnerRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]*(:[a-z_][a-z0-9_-]*)?$`)

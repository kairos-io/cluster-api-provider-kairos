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

package controlplane

// The control-plane hostname rule, checked on the KairosConfigTemplate a
// KairosControlPlane references (ADR 0015 PR-3).
//
// The KairosConfig webhook rejects a control-plane config whose spec.hostname
// is not a lowercase DNS-1123 label without "{{". The KCP stamps its configs
// from the referenced KairosConfigTemplate and forces role control-plane, so a
// template with such a hostname would only fail at scale-up, when the KCP
// creates a config from it. Neither the KCP nor the KCP template carries a
// hostname of its own, and the KairosConfigTemplate has no webhook (and
// cannot tell from its own role whether a KCP will use it), so the KCP checks
// the template it references: it reports the problem on every reconcile, as
// soon as the template is referenced, and refuses to create a control-plane
// Machine from it.

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// invalidControlPlaneHostnameReason is the Warning event reason for a
// referenced KairosConfigTemplate whose spec.hostname breaks the
// control-plane hostname rule.
const invalidControlPlaneHostnameReason = "InvalidControlPlaneHostname"

// controlPlaneTemplateHostnameProblem returns why the KairosConfigTemplate kcp
// references cannot be used for control-plane nodes, or "" when it can (or
// there is no template, or it does not exist yet). err is a read failure.
func (r *KairosControlPlaneReconciler) controlPlaneTemplateHostnameProblem(ctx context.Context, kcp *controlplanev1beta2.KairosControlPlane) (string, error) {
	if kcp.Spec.KairosConfigTemplate.Name == "" {
		return "", nil
	}
	tmpl := &bootstrapv1beta2.KairosConfigTemplate{}
	key := types.NamespacedName{Namespace: kcp.Namespace, Name: kcp.Spec.KairosConfigTemplate.Name}
	if err := r.Get(ctx, key, tmpl); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("read KairosConfigTemplate %s: %w", key, err)
	}
	h := tmpl.Spec.Template.Spec.Hostname
	if h == "" {
		return "", nil
	}
	errs := bootstrapv1beta2.NodeHostnameErrors(h)
	if len(errs) == 0 {
		return "", nil
	}
	return fmt.Sprintf("KairosConfigTemplate %s sets spec.template.spec.hostname to a value control-plane nodes cannot use (%s): "+
		"it must be a lowercase DNS-1123 label with no '{{'. No control-plane Machine is created from it until it is fixed",
		key.Name, strings.Join(errs, "; ")), nil
}

// reportControlPlaneTemplateHostname emits a Warning event while the
// referenced template breaks the hostname rule. It never blocks anything
// itself; createControlPlaneMachine refuses.
func (r *KairosControlPlaneReconciler) reportControlPlaneTemplateHostname(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane) {
	problem, err := r.controlPlaneTemplateHostnameProblem(ctx, kcp)
	if err != nil || problem == "" {
		return
	}
	log.Info("The referenced KairosConfigTemplate cannot be used for control-plane nodes", "reason", problem)
	if r.Recorder != nil {
		r.Recorder.Event(kcp, corev1.EventTypeWarning, invalidControlPlaneHostnameReason, problem)
	}
}

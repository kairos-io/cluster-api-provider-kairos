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

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// machineKairosConfig returns the KairosConfig that m's bootstrap configRef
// names, read through the manager's cache. It returns nil and no error when m
// names no KairosConfig, or when that KairosConfig does not exist: right after
// a create the cache may not show it yet, and once CAPI deletes the Machine it
// deletes the config too. Any other read error is returned, and the caller
// must not guess.
func (r *KairosControlPlaneReconciler) machineKairosConfig(ctx context.Context, m *clusterv1.Machine) (*bootstrapv1beta2.KairosConfig, error) {
	ref := m.Spec.Bootstrap.ConfigRef
	if ref.Name == "" || ref.Kind != "KairosConfig" || ref.APIGroup != bootstrapv1beta2.GroupVersion.Group {
		return nil, nil
	}
	kc := &bootstrapv1beta2.KairosConfig{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: ref.Name}, kc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return kc, nil
}

// machineDistribution returns the distribution m's node was rendered with:
// spec.distribution of its KairosConfig, with the empty value read as k0s, as
// the bootstrap renderer reads it (ADR 0016). createControlPlaneMachine stamps
// the value on every control-plane KairosConfig, and the node renders it once,
// so it records what the node runs, whatever KairosControlPlane.spec.distribution
// says now. found is false when m has no readable KairosConfig
// (machineKairosConfig); the caller then decides without it.
func (r *KairosControlPlaneReconciler) machineDistribution(ctx context.Context, m *clusterv1.Machine) (distribution string, found bool, err error) {
	kc, err := r.machineKairosConfig(ctx, m)
	if err != nil {
		return "", false, fmt.Errorf("read KairosConfig %s/%s of Machine %s for its distribution: %w",
			m.Namespace, m.Spec.Bootstrap.ConfigRef.Name, m.Name, err)
	}
	if kc == nil {
		return "", false, nil
	}
	return bootstrapv1beta2.EffectiveDistribution(kc.Spec.Distribution), true, nil
}

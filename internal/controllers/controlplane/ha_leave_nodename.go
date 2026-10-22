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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// leaveNodeName returns the name a control-plane Machine's member goes by in
// the workload cluster: the key of its etcd-leave request, of its etcd-status
// report, and the name of its k0s EtcdMember.
//
// With a NodeRef it is the Node name. Without one it is the hostname the
// bootstrap renderer gave the node (renderedNodeName). k0s creates the
// EtcdMember, and the node writes its reports, under that hostname long
// before the Machine gets a NodeRef: etcd membership comes first (ADR 0015).
func (r *KairosControlPlaneReconciler) leaveNodeName(ctx context.Context, m *clusterv1.Machine) (string, error) {
	if m.Status.NodeRef.IsDefined() {
		return m.Status.NodeRef.Name, nil
	}
	return r.renderedNodeName(ctx, m)
}

// renderedNodeName returns the hostname the bootstrap renderer gives m's node
// (bootstrapv1beta2.RenderedHostname), lowercased the way k0s and the kubelet
// lowercase it into the EtcdMember and Node names. A Machine whose
// KairosConfig is gone, or that has none, renders its own name. Any other
// read error is returned: the caller must not guess a name.
func (r *KairosControlPlaneReconciler) renderedNodeName(ctx context.Context, m *clusterv1.Machine) (string, error) {
	h, err := r.renderedHostname(ctx, m)
	return strings.ToLower(h), err
}

// renderedHostname is the hostname the renderer gives m's node, as written
// (not lowercased): spec.hostname of its KairosConfig, else the Machine name.
func (r *KairosControlPlaneReconciler) renderedHostname(ctx context.Context, m *clusterv1.Machine) (string, error) {
	ref := m.Spec.Bootstrap.ConfigRef
	if ref.Name == "" || ref.Kind != "KairosConfig" || ref.APIGroup != bootstrapv1beta2.GroupVersion.Group {
		return m.Name, nil
	}
	kc := &bootstrapv1beta2.KairosConfig{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: ref.Name}, kc); err != nil {
		if apierrors.IsNotFound(err) {
			return m.Name, nil
		}
		return "", fmt.Errorf("read KairosConfig %s/%s of Machine %s for its node name: %w", m.Namespace, ref.Name, m.Name, err)
	}
	return bootstrapv1beta2.RenderedHostname(&kc.Spec, m.Name), nil
}

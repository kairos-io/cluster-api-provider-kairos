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

	"github.com/go-logr/logr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// renderedNodeNameAnnotation records, on a control-plane Machine, the
// hostname the KCP built into its KairosConfig: the name its node, its k0s
// EtcdMember, its etcd-leave key and its etcd-status report go by (KD-91, an
// amendment to ADR 0015 PR-3).
//
// createControlPlaneMachineNamed sets it in the same Create that stamps the
// etcd-leave hook, and reconcileRenderedNodeNames sets it on Machines created
// before it existed. The KCP never overwrites or removes it, and
// machineTemplate.metadata never copies it (reservedMachineAnnotations).
//
// It lives on the Machine, next to the hook, so that the leave target is in
// the same trust domain as the hook itself: whoever can change it can already
// remove the hook. Before it, the name came from the KairosConfig, which can
// be edited, deleted (a foreground delete of the Machine removes it before
// pre-terminate) or recreated (CAPI adopts the new one), and was recomputed
// on every pass of a leave.
const renderedNodeNameAnnotation = "controlplane.cluster.x-k8s.io/rendered-node-name"

// leaveNodeName returns the name a control-plane Machine's member goes by in
// the workload cluster: the key of its etcd-leave request, of its etcd-status
// report, and the name of its k0s EtcdMember.
//
// With a NodeRef it is the Node name. Without one it is the name recorded on
// the Machine at creation (renderedNodeNameAnnotation), returned as written:
// inspectUnregisteredLeave checks it before it is used to look anything up.
// A Machine created before the record existed, and not backfilled, falls back
// to the hostname its KairosConfig renders (renderedNodeName). k0s creates
// the EtcdMember, and the node writes its reports, under that name long
// before the Machine gets a NodeRef: etcd membership comes first (ADR 0015).
func (r *KairosControlPlaneReconciler) leaveNodeName(ctx context.Context, m *clusterv1.Machine) (string, error) {
	if m.Status.NodeRef.IsDefined() {
		return m.Status.NodeRef.Name, nil
	}
	if name, ok := recordedNodeName(m); ok {
		return name, nil
	}
	return r.renderedNodeName(ctx, m)
}

// recordedNodeName returns m's renderedNodeNameAnnotation, as written. ok
// reports whether the annotation is present: a present but empty or
// malformed value still decides the path, and is then refused as a name.
func recordedNodeName(m *clusterv1.Machine) (name string, ok bool) {
	name, ok = m.Annotations[renderedNodeNameAnnotation]
	return name, ok
}

// nodeNameRecordFor returns the renderedNodeNameAnnotation value for a
// control-plane Machine named machineName whose KairosConfig has spec: the
// hostname the bootstrap renderer will give its node. It refuses a value that
// is not a lowercase DNS-1123 label without "{{", so the KCP never writes a
// record that inspectUnregisteredLeave would refuse; an invalid record can
// then only mean that it was changed afterwards.
func nodeNameRecordFor(spec *bootstrapv1beta2.KairosConfigSpec, machineName string) (string, error) {
	name := bootstrapv1beta2.RenderedHostname(spec, machineName)
	if errs := bootstrapv1beta2.NodeHostnameErrors(name); len(errs) > 0 {
		return "", fmt.Errorf("the node name %q of control-plane Machine %s is not a lowercase DNS-1123 label: %s",
			name, machineName, strings.Join(errs, "; "))
	}
	return name, nil
}

// reconcileRenderedNodeNames records the node name on every Machine in
// machines that has no record yet and is not being deleted: control-plane
// Machines created before the record existed (KD-91).
//
// The name is the NodeRef name of a registered Machine, which is what the
// kubelet registered, else the hostname its KairosConfig renders. A name that
// is not a valid node hostname is not recorded: the leave keeps holding on it
// as before (inspectUnregisteredLeave), and an uppercase hostname is never
// lowercased into a record. A Machine whose KairosConfig is gone or cannot be
// read is skipped and keeps the old lookup; a read or write failure is
// logged and never fails the reconcile.
//
// A Machine already being deleted is never recorded: its leave may have
// started under the old lookup, and must not change name halfway.
func (r *KairosControlPlaneReconciler) reconcileRenderedNodeNames(ctx context.Context, log logr.Logger, machines []*clusterv1.Machine) {
	for _, m := range machines {
		if _, ok := recordedNodeName(m); ok || !m.DeletionTimestamp.IsZero() {
			continue
		}
		name, err := r.backfillNodeName(ctx, m)
		if err != nil {
			log.Info("Could not determine the node name of a control-plane Machine to record it; the etcd leave keeps the old lookup",
				"machine", m.Name, "reason", err.Error())
			continue
		}
		if name == "" || !bootstrapv1beta2.ValidNodeHostname(name) {
			continue
		}
		if err := r.annotateOnce(ctx, m, map[string]string{renderedNodeNameAnnotation: name}); err != nil {
			log.Info("Could not record the node name of a control-plane Machine; the etcd leave keeps the old lookup",
				"machine", m.Name, "reason", err.Error())
			continue
		}
		log.Info("Recorded the node name of a control-plane Machine", "machine", m.Name, "node", name)
	}
}

// backfillNodeName is the name reconcileRenderedNodeNames records for m: its
// NodeRef name, else the hostname its KairosConfig renders, as written. It
// returns "" when m has no readable KairosConfig.
func (r *KairosControlPlaneReconciler) backfillNodeName(ctx context.Context, m *clusterv1.Machine) (string, error) {
	if m.Status.NodeRef.IsDefined() {
		return m.Status.NodeRef.Name, nil
	}
	kc, err := r.machineKairosConfig(ctx, m)
	if err != nil {
		return "", fmt.Errorf("read KairosConfig %s/%s: %w", m.Namespace, m.Spec.Bootstrap.ConfigRef.Name, err)
	}
	if kc == nil {
		return "", nil
	}
	return bootstrapv1beta2.RenderedHostname(&kc.Spec, m.Name), nil
}

// renderedNodeName returns the hostname the bootstrap renderer gives m's node
// (bootstrapv1beta2.RenderedHostname), lowercased the way k0s and the kubelet
// lowercase it into the EtcdMember and Node names. A Machine whose
// KairosConfig is gone, or that has none, renders its own name. Any other
// read error is returned: the caller must not guess a name.
//
// It is the legacy lookup, for Machines without a record (leaveNodeName),
// and one of the names expectedEtcdMembers protects.
func (r *KairosControlPlaneReconciler) renderedNodeName(ctx context.Context, m *clusterv1.Machine) (string, error) {
	h, err := r.renderedHostname(ctx, m)
	return strings.ToLower(h), err
}

// renderedHostname is the hostname the renderer gives m's node, as written
// (not lowercased): spec.hostname of its KairosConfig, else the Machine name.
// The legacy lookup, like renderedNodeName.
func (r *KairosControlPlaneReconciler) renderedHostname(ctx context.Context, m *clusterv1.Machine) (string, error) {
	kc, err := r.machineKairosConfig(ctx, m)
	if err != nil {
		return "", fmt.Errorf("read KairosConfig %s/%s of Machine %s for its node name: %w", m.Namespace, m.Spec.Bootstrap.ConfigRef.Name, m.Name, err)
	}
	if kc == nil {
		return m.Name, nil
	}
	return bootstrapv1beta2.RenderedHostname(&kc.Spec, m.Name), nil
}

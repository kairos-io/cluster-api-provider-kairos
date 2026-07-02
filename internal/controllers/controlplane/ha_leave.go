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
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// The etcd-leave handshake (ADR 0005 §E.3). These identifiers are a wire
// contract shared with the node-side responder rendered by
// internal/bootstrap/templates/k0s_kairos_cloud_config_capv.yaml.tmpl — the
// ConfigMap name/namespace, the member key, and the two sentinel values MUST
// match byte-for-byte or the node never sees its leave request.
const (
	// etcdLeaveHookName is the SUFFIX of the CAPI pre-terminate lifecycle-hook
	// annotation stamped on k0s HA control-plane Machines. CAPI pauses Machine
	// termination *after drain, before infrastructure teardown* (node kubelet +
	// workload API still up) for as long as any annotation with the
	// pre-terminate prefix is present, giving the controller a window to drive a
	// clean `k0s etcd leave`.
	etcdLeaveHookName = "kairos-etcd-leave"

	// etcdLeaveConfigMapName / -Namespace name the workload-cluster ConfigMap the
	// controller writes the leave request into and the node acks on. Matches the
	// node responder.
	etcdLeaveConfigMapName      = "kairos-etcd-leave"
	etcdLeaveConfigMapNamespace = "kube-system"

	// etcdLeaveRequestedValue is the fixed sentinel the controller writes for a
	// member; etcdLeftValue is the ack the node writes back after `k0s etcd
	// leave`. Compared by exact string equality on both ends — never eval'd.
	etcdLeaveRequestedValue = "leave-requested"
	etcdLeftValue           = "left"

	// etcdLeaveRequestedAtAnnotation records (RFC3339) when the controller FIRST
	// wrote leave-requested for a member. It anchors the timeout so an
	// unreachable/stuck node cannot wedge Machine deletion forever.
	etcdLeaveRequestedAtAnnotation = "controlplane.cluster.x-k8s.io/etcd-leave-requested-at"

	// memberLeaveTimeout bounds the wait for a node to ack its etcd leave. The
	// node responder polls every 30s (OnUnitActiveSec), so this allows ~10
	// attempts before the controller gives up on the clean leave, warns that the
	// member may be orphaned, and lets the delete proceed (quorum was already
	// proven safe by canRemoveMember before the delete).
	memberLeaveTimeout = 5 * time.Minute
)

// etcdLeaveHookAnnotation is the full pre-terminate hook annotation key
// (`<prefix>/<name>`) CAPI recognizes.
func etcdLeaveHookAnnotation() string {
	return clusterv1.PreTerminateDeleteHookAnnotationPrefix + "/" + etcdLeaveHookName
}

// hasEtcdLeaveHook reports whether the Machine carries the etcd-leave
// pre-terminate hook.
func hasEtcdLeaveHook(m *clusterv1.Machine) bool {
	_, ok := m.Annotations[etcdLeaveHookAnnotation()]
	return ok
}

// distributionOf returns the effective distribution for a KCP, defaulting the
// empty value to k0s (mirrors createControlPlaneMachine).
func distributionOf(kcp *controlplanev1beta2.KairosControlPlane) string {
	if kcp.Spec.Distribution == "" {
		return "k0s"
	}
	return kcp.Spec.Distribution
}

// shouldStampEtcdLeaveHook decides whether a newly-created control-plane Machine
// gets the etcd-leave pre-terminate hook: only k0s init/join members. Single-node
// has no etcd cluster to leave; k3s embedded etcd has no supported member-remove
// (KD-5d), so neither is hooked.
func shouldStampEtcdLeaveHook(kcp *controlplanev1beta2.KairosControlPlane, role bootstrapv1beta2.ControlPlaneRole) bool {
	if distributionOf(kcp) != "k0s" {
		return false
	}
	return role == bootstrapv1beta2.ControlPlaneRoleInit || role == bootstrapv1beta2.ControlPlaneRoleJoin
}

// defaultWorkloadClient builds a client for the workload cluster from its
// `<cluster>-kubeconfig` Secret (the KD-3b node-push payload; its server URL
// already points at the VIP, which quorum guarantees is up). Used when the
// reconciler's WorkloadClientFactory seam is not overridden (tests inject a fake).
func (r *KairosControlPlaneReconciler) defaultWorkloadClient(ctx context.Context, cluster *clusterv1.Cluster) (client.Client, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: cluster.Namespace, Name: fmt.Sprintf("%s-kubeconfig", cluster.Name)}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("get workload kubeconfig secret %s: %w", key, err)
	}
	data, ok := secret.Data["value"]
	if !ok || len(data) == 0 {
		return nil, fmt.Errorf("workload kubeconfig secret %s has no non-empty 'value' key", key)
	}
	restCfg, err := clientcmd.RESTConfigFromKubeConfig(data)
	if err != nil {
		return nil, fmt.Errorf("parse workload kubeconfig %s: %w", key, err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	wc, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("build workload client for cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	return wc, nil
}

// reconcileMemberLeave drives the clean k0s etcd-leave handshake for a
// control-plane Machine that is being removed and still carries the etcd-leave
// pre-terminate hook (ADR 0005 §E.3). It is called from the reconcileMachines
// sweep for any owned, terminating, hooked Machine — which covers the
// controller's own quorum-approved rollout/scale-down deletes, operator/MHC
// deletes, and crash recovery. It returns done=true only once it is safe for
// CAPI to proceed (the hook has been removed); done=false requeues.
//
// State machine:
//  1. NodeRef == nil → the Machine never registered a Node: no member key to
//     signal and no live node to run the leave. Remove the hook, done. (We key
//     ONLY on NodeRef, never Machine phase: by the time the sweep runs the
//     Machine is already `Deleting`, so a phase check would fire for every
//     target and skip the leave for healthy members too.)
//  2. Non-k0s carrying the hook (should never happen) → remove hook, done.
//  3. Node acked `left` in the workload ConfigMap → remove hook, done.
//  4. Already-requested AND (member gone from etcd-status OR past
//     memberLeaveTimeout) → remove hook, done (timeout warns: member may be
//     orphaned).
//  5. Otherwise → write the leave-requested sentinel, stamp the first-request
//     timestamp, requeue (done=false).
//
// SECURITY / fail-safe (ADR 0005 §E.5): a workload-client BUILD error or a
// ConfigMap READ error returns the error with the hook STILL SET — the delete
// stays blocked and quorum is preserved. The hook is never removed on an error
// path.
func (r *KairosControlPlaneReconciler) reconcileMemberLeave(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, target *clusterv1.Machine) (bool, error) {
	// (1) Never-registered node: nothing to leave, nothing alive to run it.
	if target.Status.NodeRef == nil {
		log.Info("etcd-leave: target has no NodeRef; removing hook without leave handshake", "machine", target.Name)
		return true, r.removeEtcdLeaveHook(ctx, target)
	}
	// (2) Defensive: only k0s stamps the hook.
	if distributionOf(kcp) != "k0s" {
		return true, r.removeEtcdLeaveHook(ctx, target)
	}

	nodeName := target.Status.NodeRef.Name

	// Build the workload client on demand. Build error is fail-safe: hook retained.
	factory := r.WorkloadClientFactory
	if factory == nil {
		factory = r.defaultWorkloadClient
	}
	wc, err := factory(ctx, cluster)
	if err != nil {
		return false, fmt.Errorf("etcd-leave: build workload client: %w", err)
	}

	cm := &corev1.ConfigMap{}
	cmKey := types.NamespacedName{Namespace: etcdLeaveConfigMapNamespace, Name: etcdLeaveConfigMapName}
	if err := wc.Get(ctx, cmKey, cm); err != nil && !apierrors.IsNotFound(err) {
		// Read error is fail-safe: hook retained, requeue.
		return false, fmt.Errorf("etcd-leave: read workload ConfigMap %s: %w", cmKey, err)
	}

	// (3) Node acked the leave — unambiguous terminal, checked before anything
	// that depends on prior state.
	if cm.Data[nodeName] == etcdLeftValue {
		log.Info("etcd-leave: node acked left; removing hook", "machine", target.Name, "node", nodeName)
		return true, r.removeEtcdLeaveHook(ctx, target)
	}

	// (4) Terminal checks that only apply once we have actually asked the node to
	// leave. Gating on the stamped timestamp prevents a premature skip for a member
	// that simply has not reported etcd-status yet (the guard already fails closed
	// pre-delete for a live unreported member, so this is belt-and-braces).
	if _, requested := target.Annotations[etcdLeaveRequestedAtAnnotation]; requested {
		// Member gone from node-reported etcd-status → treat as left. Best-effort:
		// a read error leaves the member "present" so we keep waiting, never skip.
		if status, serr := r.readEtcdStatus(ctx, cluster); serr == nil {
			if _, present := status[nodeName]; !present {
				log.Info("etcd-leave: member absent from etcd-status; treating as left", "machine", target.Name, "node", nodeName)
				return true, r.removeEtcdLeaveHook(ctx, target)
			}
		}
		// Timeout backstop: an unreachable/stuck node must not wedge deletion.
		if requestedAt, perr := time.Parse(time.RFC3339, target.Annotations[etcdLeaveRequestedAtAnnotation]); perr == nil {
			if time.Since(requestedAt) > memberLeaveTimeout {
				log.Info("etcd-leave: timed out waiting for node ack; removing hook (etcd member may be orphaned)",
					"machine", target.Name, "node", nodeName, "timeout", memberLeaveTimeout)
				if r.Recorder != nil {
					r.Recorder.Eventf(kcp, corev1.EventTypeWarning, "EtcdMemberLeaveTimedOut",
						"Control-plane node %q did not leave etcd within %s; proceeding with deletion — the etcd member may need manual removal", nodeName, memberLeaveTimeout)
				}
				return true, r.removeEtcdLeaveHook(ctx, target)
			}
		}
	}

	// (5) Ask the node to leave and record when we first asked, then wait.
	if err := r.ensureLeaveRequested(ctx, wc, cmKey, nodeName); err != nil {
		return false, err
	}
	if err := r.stampLeaveRequestedAt(ctx, target); err != nil {
		return false, err
	}
	log.Info("etcd-leave: leave-requested written; awaiting node ack", "machine", target.Name, "node", nodeName)
	return false, nil
}

// ensureLeaveRequested upserts the workload-cluster ConfigMap and sets this
// member's key to the leave-requested sentinel. It never clobbers an existing
// `left` ack (a racy reconcile must not un-ack a node that already left).
func (r *KairosControlPlaneReconciler) ensureLeaveRequested(ctx context.Context, wc client.Client, cmKey types.NamespacedName, nodeName string) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmKey.Name, Namespace: cmKey.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, wc, cm, func() error {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if cm.Data[nodeName] == etcdLeftValue {
			return nil // already left; keep the ack
		}
		cm.Data[nodeName] = etcdLeaveRequestedValue
		return nil
	})
	if err != nil {
		return fmt.Errorf("etcd-leave: write leave-requested to %s: %w", cmKey, err)
	}
	return nil
}

// stampLeaveRequestedAt records the first-request timestamp on the Machine, once.
// The ORIGINAL timestamp is preserved on subsequent calls so the timeout window
// is anchored to when we first asked, not the latest reconcile.
func (r *KairosControlPlaneReconciler) stampLeaveRequestedAt(ctx context.Context, m *clusterv1.Machine) error {
	if _, ok := m.Annotations[etcdLeaveRequestedAtAnnotation]; ok {
		return nil
	}
	helper, err := patch.NewHelper(m, r.Client)
	if err != nil {
		return err
	}
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations[etcdLeaveRequestedAtAnnotation] = time.Now().UTC().Format(time.RFC3339)
	if err := helper.Patch(ctx, m); err != nil {
		return fmt.Errorf("stamp etcd-leave-requested-at on machine %s: %w", m.Name, err)
	}
	return nil
}

// removeEtcdLeaveHook drops the etcd-leave pre-terminate hook from the Machine so
// CAPI can proceed with termination. Idempotent: a no-op if the hook is absent.
func (r *KairosControlPlaneReconciler) removeEtcdLeaveHook(ctx context.Context, m *clusterv1.Machine) error {
	if !hasEtcdLeaveHook(m) {
		return nil
	}
	helper, err := patch.NewHelper(m, r.Client)
	if err != nil {
		return err
	}
	delete(m.Annotations, etcdLeaveHookAnnotation())
	if err := helper.Patch(ctx, m); err != nil {
		return fmt.Errorf("remove etcd-leave pre-terminate hook from machine %s: %w", m.Name, err)
	}
	return nil
}

// warnIfK3sEtcdLimitation emits a Warning event when a k3s HA control-plane
// member is removed: k3s embedded etcd has no supported member-remove, so the
// member stays registered after the node is gone (KD-5d). No-op for k0s (which
// removes members cleanly via the leave handshake) and for single-node (no etcd
// cluster).
func (r *KairosControlPlaneReconciler) warnIfK3sEtcdLimitation(kcp *controlplanev1beta2.KairosControlPlane, target *clusterv1.Machine) {
	if r.Recorder == nil || distributionOf(kcp) == "k0s" {
		return
	}
	desired := int32(1)
	if kcp.Spec.Replicas != nil {
		desired = *kcp.Spec.Replicas
	}
	if desired <= 1 {
		return
	}
	r.Recorder.Eventf(kcp, corev1.EventTypeWarning, "EtcdMemberRemoveUnsupportedForK3s",
		"Removing control-plane Machine %q: k3s embedded etcd has no supported member-remove; the etcd member remains registered after the node is gone and may require manual `etcdctl member remove` (KD-5d)", target.Name)
}

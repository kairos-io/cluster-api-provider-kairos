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
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
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
//
// The controller resolves + persists spec.distribution early in Reconcile (see
// resolveEffectiveDistribution + the resolve block in Reconcile), so by the time
// any of the machine-create / etcd-leave paths call distributionOf the field is
// already populated with the inherited-or-explicit value. The k0s fallback here
// is the last-resort default used only when the referenced KairosConfigTemplate
// has not been observed yet (the template read failed / does not exist), in which
// case a later reconcile resolves it.
func distributionOf(kcp *controlplanev1beta2.KairosControlPlane) string {
	if kcp.Spec.Distribution == "" {
		return "k0s"
	}
	return kcp.Spec.Distribution
}

// resolveEffectiveDistribution reads the KairosConfigTemplate referenced by the
// KCP and returns its spec.template.spec.distribution ("" when the template does
// not set one). It is the inherit source for KCP.spec.distribution: the KCP's
// explicit value always wins over this, and this in turn wins over the k0s
// fallback in distributionOf.
//
// It is intentionally read-only and side-effect free (no persistence): the
// caller in Reconcile decides whether to persist the resolved value. A NotFound
// on the template is NOT an error — it returns ("", nil) so the reconcile can
// proceed and resolve on a later pass once the template exists. All other read
// errors are surfaced.
func (r *KairosControlPlaneReconciler) resolveEffectiveDistribution(ctx context.Context, kcp *controlplanev1beta2.KairosControlPlane) (string, error) {
	if kcp.Spec.KairosConfigTemplate.Name == "" {
		return "", nil
	}
	template := &bootstrapv1beta2.KairosConfigTemplate{}
	key := types.NamespacedName{Namespace: kcp.Namespace, Name: kcp.Spec.KairosConfigTemplate.Name}
	if err := r.Get(ctx, key, template); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("resolve effective distribution: get KairosConfigTemplate %s: %w", key, err)
	}
	return template.Spec.Template.Spec.Distribution, nil
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

// workloadRequestTimeout bounds every request the KCP's workload client sends
// through the control-plane endpoint. Without it, one request to an apiserver
// that accepts the connection but never answers blocks the reconcile for as
// long as the TCP connection lives.
const workloadRequestTimeout = 10 * time.Second

// defaultWorkloadClient builds a client for the workload cluster from its
// `<cluster>-kubeconfig` Secret (the KD-3b node-push payload; its server URL
// points at the control-plane endpoint). Used when the reconciler's
// WorkloadClientFactory seam is not overridden (tests inject a fake). It backs
// the k0s join-token path, and the etcd-leave handshake when no member answers
// directly (connectForLeave). Every request is bounded by
// workloadRequestTimeout.
//
// ADR 0011: the rest.Config is built from the S5-rebuilt output of the
// contract bytes, NEVER the raw bytes (workloadRESTConfig ->
// trustedWorkloadKubeconfig). The contract Secret is normally already S5 output
// (the publish path is its only sanctioned writer), but re-sanitizing here
// closes the window where something overwrote it between reconciles: nothing
// client-go would honor as file-path credentials, exec/auth-provider plugins,
// impersonation or insecure TLS survives Sanitize, so the client uses only the
// inline credential and CA, the controller-resolved server and the pinned CA. The server is
// the one the controller resolves itself, not the one in the Secret, and the CA
// must match the pin on the KairosControlPlane. Any failure returns an error,
// which the callers treat as fail-safe (the etcd-leave hook stays set, quorum
// preserved; no join token is minted).
func (r *KairosControlPlaneReconciler) defaultWorkloadClient(ctx context.Context, cluster *clusterv1.Cluster) (client.Client, error) {
	restCfg, err := r.endpointRESTConfig(ctx, cluster)
	if err != nil {
		return nil, err
	}
	wc, err := newWorkloadClient(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build workload client for cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	return wc, nil
}

// endpointRESTConfig is the endpoint client's rest.Config: the workload
// kubeconfig, with every request bounded by the client timeout,
// workloadRequestTimeout.
func (r *KairosControlPlaneReconciler) endpointRESTConfig(ctx context.Context, cluster *clusterv1.Cluster) (*rest.Config, error) {
	restCfg, err := r.workloadRESTConfig(ctx, cluster)
	if err != nil {
		return nil, err
	}
	restCfg.Timeout = workloadRequestTimeout
	return restCfg, nil
}

// workloadRESTConfig builds a fresh rest.Config for the workload cluster: the
// server the controller itself resolves, the cluster CA pinned on the
// KairosControlPlane, and the inline credential. It is the ONLY place the KCP
// turns the `<cluster>-kubeconfig` Secret into a rest.Config; the endpoint
// client (defaultWorkloadClient), every etcd-leave member client
// (defaultMemberClient) and the k0s join-token CA (workloadClusterCA) all
// derive from it. The bytes it parses are trustedWorkloadKubeconfig's S5
// output, never the Secret's raw value.
func (r *KairosControlPlaneReconciler) workloadRESTConfig(ctx context.Context, cluster *clusterv1.Cluster) (*rest.Config, error) {
	sanitized, err := r.trustedWorkloadKubeconfig(ctx, cluster)
	if err != nil {
		return nil, err
	}
	restCfg, err := clientcmd.RESTConfigFromKubeConfig(sanitized)
	if err != nil {
		return nil, fmt.Errorf("parse sanitized workload kubeconfig for cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	return restCfg, nil
}

// newWorkloadClient builds a workload-cluster client with the types the KCP
// reads and writes there.
func newWorkloadClient(restCfg *rest.Config) (client.Client, error) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	// discovery/v1: the k0s join-token path reads the default/kubernetes
	// EndpointSlices to find live members (ADR 0013).
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return client.New(restCfg, client.Options{Scheme: scheme})
}

// reconcileMemberLeave drives the clean k0s etcd-leave handshake for a
// control-plane Machine that is being removed and still carries the etcd-leave
// pre-terminate hook (ADR 0005 §E.3). It is called from the reconcileMachines
// sweep for any owned, terminating, hooked Machine — which covers the
// controller's own quorum-approved rollout/scale-down deletes, operator/MHC
// deletes, and crash recovery. machines is the KCP-owned set that sweep works
// on. It returns done=true only once it is safe for CAPI to proceed (the hook
// has been removed); done=false requeues.
//
// The workload-cluster traffic goes through one member that is staying, not
// through the control-plane endpoint (connectForLeave, ADR 0014).
//
// State machine:
//  1. NodeRef == nil → the Machine never registered a Node: no member key to
//     signal and no live node to run the leave. Remove the hook, done. (We key
//     ONLY on NodeRef, never Machine phase: by the time the sweep runs the
//     Machine is already `Deleting`, so a phase check would fire for every
//     target and skip the leave for healthy members too.)
//  2. Non-k0s carrying the hook (should never happen) → remove hook, done.
//     Then, until a leave has been requested, wait (done=false, no workload
//     traffic) while CAPI is still draining the Node or detaching its
//     volumes (machineAtPreTerminate).
//  3. Node acked `left` in the workload ConfigMap → remove hook, done.
//  4. Already-requested AND member gone from etcd-status → remove hook, done.
//  5. Already-requested AND past memberLeaveTimeout → delegateMemberLeave:
//     set spec.leave on the member's k0s EtcdMember and remove the hook once
//     k0s reports Joined=False. Without that API or object, or after
//     etcdMemberLeaveCap, remove the hook with a warning (the member may be
//     orphaned).
//  6. Otherwise → write the leave-requested sentinel, stamp the first-request
//     timestamp, requeue (done=false).
//
// SECURITY / fail-safe (ADR 0005 §E.5): a workload-client BUILD error or a
// ConfigMap READ error, on every apiserver connectForLeave tries, or a failed
// write, returns the error with the hook STILL SET — the delete stays blocked
// and quorum is preserved. The one exception is the timeout of step 5, which
// is bounded whether or not the workload cluster answers: before, a failed
// read returned ahead of it, so a leave whose reads kept failing was never
// bounded.
func (r *KairosControlPlaneReconciler) reconcileMemberLeave(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine, target *clusterv1.Machine) (bool, error) {
	// (1) Never-registered node: nothing to leave, nothing alive to run it.
	if !target.Status.NodeRef.IsDefined() {
		log.Info("etcd-leave: target has no NodeRef; removing hook without leave handshake", "machine", target.Name)
		return true, r.removeEtcdLeaveHook(ctx, target)
	}
	// (2) Defensive: only k0s stamps the hook.
	if distributionOf(kcp) != "k0s" {
		return true, r.removeEtcdLeaveHook(ctx, target)
	}

	nodeName := target.Status.NodeRef.Name
	_, requested := target.Annotations[etcdLeaveRequestedAtAnnotation]

	// (2b) Not before CAPI has finished with the Node. The drain and the volume
	// detach go through the workload apiserver: once this member has left
	// etcd its own apiserver cannot serve them, and while it holds the VIP
	// neither can the endpoint. So nothing is requested, and nothing is sent
	// to the workload cluster, until CAPI waits on the pre-terminate hooks. A
	// leave already requested (by an earlier pass, or by a KCP without this
	// gate) carries on whatever CAPI reports.
	if !requested && !machineAtPreTerminate(target) {
		log.Info("etcd-leave: waiting for CAPI to finish with the node before the leave", "machine", target.Name, "node", nodeName)
		return false, nil
	}

	// Find a workload apiserver that answers, and read the leave ConfigMap
	// through it. Nothing answering is fail-safe: hook retained, except that
	// the timeout backstop below is still evaluated once a leave was requested.
	conn, connErr := r.connectForLeave(ctx, log, kcp, cluster, machines, target)

	// (3) First entry for THIS eviction cycle (no timestamp stamped yet). FORCE the
	// leave-requested sentinel, overwriting any STALE `left` a prior eviction of a
	// same-named Machine left behind: Machine indices/names are reused, and the
	// workload ConfigMap key == the Node name, so a previous cycle's ack can linger
	// and would otherwise short-circuit this eviction into removing the hook
	// without the (new) member ever leaving etcd → orphan. The force-write MUST
	// precede the timestamp stamp, so a stamp failure re-forces (clears the stale
	// ack) next reconcile rather than leaving a stale `left` visible once
	// requested==true. Terminal checks are deliberately NOT evaluated on this pass.
	if !requested {
		if connErr != nil {
			return false, fmt.Errorf("etcd-leave: no workload apiserver answered: %w", connErr)
		}
		if err := r.ensureLeaveRequested(ctx, conn.client, leaveConfigMapKey, nodeName, true /*force*/); err != nil {
			return false, err
		}
		if err := r.stampLeaveRequestedAt(ctx, target); err != nil {
			return false, err
		}
		log.Info("etcd-leave: leave-requested written; awaiting node ack", "machine", target.Name, "node", nodeName, "via", conn.via)
		return false, nil
	}

	// (4) This cycle has requested a leave — terminal checks below are now valid for
	// THIS cycle (a `left` seen here was written by the node in response to our
	// request, not a stale prior ack). The ack and etcd-status checks need an
	// answer from the workload cluster; the timeout does not.
	if connErr == nil {
		// Node acked the leave → done.
		if conn.cm.Data[nodeName] == etcdLeftValue {
			log.Info("etcd-leave: node acked left; removing hook", "machine", target.Name, "node", nodeName, "via", conn.via)
			return true, r.removeEtcdLeaveHook(ctx, target)
		}
		// Member gone from node-reported etcd-status → treat as left. Best-effort: a
		// read error leaves the member "present" so we keep waiting, never skip.
		if status, serr := r.readEtcdStatus(ctx, cluster); serr == nil {
			if _, present := status[nodeName]; !present {
				log.Info("etcd-leave: member absent from etcd-status; treating as left", "machine", target.Name, "node", nodeName)
				return true, r.removeEtcdLeaveHook(ctx, target)
			}
		}
	}
	// Timeout: the node has not acked in time. Hand the leave to k0s's
	// EtcdMember, falling back to removing the hook with a warning. This is
	// bounded whether or not the workload cluster answers: an unreachable or
	// stuck node must not wedge deletion, and neither must an unreachable
	// workload cluster.
	if leaveTimedOut(target) {
		return r.delegateMemberLeave(ctx, log, kcp, machines, target, conn, connErr)
	}
	if connErr != nil {
		return false, fmt.Errorf("etcd-leave: no workload apiserver answered: %w", connErr)
	}

	// (6) Still waiting: keep the sentinel present (preserving a just-written `left`
	// against a racy overwrite), then requeue.
	if err := r.ensureLeaveRequested(ctx, conn.client, leaveConfigMapKey, nodeName, false); err != nil {
		return false, err
	}
	log.Info("etcd-leave: awaiting node ack", "machine", target.Name, "node", nodeName, "via", conn.via)
	return false, nil
}

// machineAtPreTerminate reports whether CAPI's Machine controller has
// finished with m's Node (pre-drain hooks, drain, volume detach) and is
// waiting on its pre-terminate hooks: the v1beta2 Deleting condition is True
// with reason WaitingForPreTerminateHook. The kubeadm control plane in CAPI
// waits for the same state before it touches etcd.
func machineAtPreTerminate(m *clusterv1.Machine) bool {
	c := apimeta.FindStatusCondition(m.Status.Conditions, clusterv1.MachineDeletingCondition)
	return c != nil && c.Status == metav1.ConditionTrue && c.Reason == clusterv1.MachineDeletingWaitingForPreTerminateHookReason
}

// leaveTimedOut reports whether the leave of m was first requested more than
// memberLeaveTimeout ago. An unparsable stamp never times out.
func leaveTimedOut(m *clusterv1.Machine) bool {
	requestedAt, err := time.Parse(time.RFC3339, m.Annotations[etcdLeaveRequestedAtAnnotation])
	return err == nil && time.Since(requestedAt) > memberLeaveTimeout
}

// ensureLeaveRequested upserts the workload-cluster ConfigMap and sets this
// member's key to the leave-requested sentinel. When force is false it never
// clobbers an existing `left` ack (a racy reconcile within a cycle must not
// un-ack a node that already left). When force is true (the first entry of an
// eviction cycle) it overwrites any value, including a STALE `left` left behind
// by a prior eviction of a same-named Machine.
func (r *KairosControlPlaneReconciler) ensureLeaveRequested(ctx context.Context, wc client.Client, cmKey types.NamespacedName, nodeName string, force bool) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmKey.Name, Namespace: cmKey.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, wc, cm, func() error {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if !force && cm.Data[nodeName] == etcdLeftValue {
			return nil // already left this cycle; keep the ack
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
	return r.stampAnnotationOnce(ctx, m, etcdLeaveRequestedAtAnnotation)
}

// stampAnnotationOnce sets the annotation key on the Machine to the current
// time (RFC3339), unless it is already set.
func (r *KairosControlPlaneReconciler) stampAnnotationOnce(ctx context.Context, m *clusterv1.Machine, key string) error {
	return r.annotateOnce(ctx, m, map[string]string{key: time.Now().UTC().Format(time.RFC3339)})
}

// annotateOnce sets, in one patch, each of values on the Machine that is not
// set yet. Values already present are kept.
func (r *KairosControlPlaneReconciler) annotateOnce(ctx context.Context, m *clusterv1.Machine, values map[string]string) error {
	var keys []string
	for k := range values {
		if _, ok := m.Annotations[k]; !ok {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	helper, err := patch.NewHelper(m, r.Client)
	if err != nil {
		return err
	}
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	for _, k := range keys {
		m.Annotations[k] = values[k]
	}
	if err := helper.Patch(ctx, m); err != nil {
		return fmt.Errorf("stamp %s on machine %s: %w", strings.Join(keys, ", "), m.Name, err)
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

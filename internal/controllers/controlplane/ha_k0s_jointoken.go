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

// The KairosControlPlane mints the k0s controller-join token (ADR 0013).
//
// A k0s controller joins with a "join token": a kubeconfig, gzipped and base64
// encoded, whose server is an existing controller's join API on :9443 and whose
// bearer is a bootstrap token "<id>.<secret>". The join API looks the bootstrap
// token up in the workload kube-system namespace on every request and requires
// the controller-join usage.
//
// The token used to be minted once, on the init node, by `k0s token create`
// with a 24h lifetime, and pushed into the management cluster. That broke two
// ways: its server was the init node's own address, so nothing could join once
// the init node was replaced, and it expired after a day, so nothing could join
// once the cluster was 24h old. This file replaces it. The KCP creates the
// bootstrap token itself through the workload-cluster API, points the join token
// at a member that is live right now, keeps it short-lived, and extends it only
// while a joiner that was rendered with it is still joining.
//
// The token and Secret format live in internal/k0sjointoken; this file is the
// policy: when to mint, which member to point at, when to extend.
//
// Nothing here may log the token. Log the token ID (the public half), the
// Secret identity and the server host only.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/k0sjointoken"
)

const (
	// k0sJoinTokenIDAnnotation records the token ID on the join-token Secret and
	// on every join KairosConfig the KCP creates.
	k0sJoinTokenIDAnnotation = bootstrapv1beta2.K0sControllerJoinTokenIDAnnotation

	// k0sJoinTokenTTL is the lifetime of a new token, and what an extension
	// gives a pending joiner from the moment it is extended.
	k0sJoinTokenTTL = time.Hour

	// k0sJoinTokenRenewWithin is the remaining lifetime below which a token is
	// extended (a joiner is pending) or replaced (no joiner is pending).
	k0sJoinTokenRenewWithin = 30 * time.Minute

	// k0sJoinTokenExtendCap bounds how long the KCP keeps a joiner's token alive.
	// It is measured from the joining Machine's creation and mirrors the kubeadm
	// worker path's refresh cap. A joiner that has not joined within a day is
	// stuck for another reason, and its token is allowed to lapse.
	k0sJoinTokenExtendCap = 24 * time.Hour

	// k0sJoinTokenRemovedAtAnnotation is set on a join KairosConfig once the
	// token issued for it has been deleted from the workload cluster, because its
	// Machine joined or is being deleted. It keeps the cleanup to one workload
	// call per joiner.
	k0sJoinTokenRemovedAtAnnotation = "controlplane.cluster.x-k8s.io/k0s-join-token-removed-at"

	// k0sJoinTokenRecheckInterval is the longest the KCP waits between two
	// reconciles while a joiner is pending, so a token is always looked at well
	// inside its k0sJoinTokenRenewWithin window.
	k0sJoinTokenRecheckInterval = 5 * time.Minute
)

// k0sJoinServerURL returns the join API URL for a member address.
// net.JoinHostPort brackets IPv6 literals.
func k0sJoinServerURL(ip net.IP) string {
	return "https://" + net.JoinHostPort(ip.String(), k0sjointoken.JoinAPIPort)
}

// normalizeK0sJoinServer canonicalises a token's server URL so it compares
// equal to the URLs liveK0sJoinServers builds. ok is false for anything that is
// not https://<ip>:9443.
func normalizeK0sJoinServer(server string) (string, bool) {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Port() != k0sjointoken.JoinAPIPort {
		return "", false
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil {
		return "", false
	}
	return k0sJoinServerURL(ip), true
}

// k0sJoinServerHost returns only the host of a join URL, for logs and events.
func k0sJoinServerHost(server string) string {
	u, err := url.Parse(server)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// k0sJoinServers is the outcome of choosing a server for a join token.
type k0sJoinServers struct {
	// urls are the join API URLs of the live members that are Machines of this
	// control plane, lowest address first.
	urls []string
	// unmatched counts the ready addresses the apiserver lists that are not
	// the address of any Machine of this control plane.
	unmatched int
}

// kubernetesEndpointSliceKey names the EndpointSlice kube-apiserver itself
// maintains for the default/kubernetes Service (endpointsadapter.go names it
// after the Service).
var kubernetesEndpointSliceKey = types.NamespacedName{Namespace: metav1.NamespaceDefault, Name: "kubernetes"}

// liveK0sJoinServers returns the join API URLs of the live control-plane
// members, lowest address first.
//
// Liveness comes from the EndpointSlice kube-apiserver writes for the
// default/kubernetes Service, read by name. kube-apiserver advertises
// api.address, and with externalAddress unset k0s leaves the apiserver's own
// lease-based endpoint reconciler in charge, so that slice lists exactly the
// members whose apiserver is up (ADR 0013 GT-6). `k0s token create` builds its
// URL from the same api.address. Other slices carrying the
// kubernetes.io/service-name=kubernetes label are not read.
//
// Membership comes from the management cluster: an address counts only if it
// is in status.addresses of one of this control plane's Machines that is not
// being deleted. A join token therefore always points at one of our own
// members. An address also held by a Machine being deleted is left out, since
// a member on its way out may still be listed.
//
// The address must also be on a ready, non-terminating endpoint and parse as a
// unicast IP; loopback and unspecified addresses are dropped.
func liveK0sJoinServers(ctx context.Context, wc client.Client, machines []*clusterv1.Machine) (k0sJoinServers, error) {
	ours := ownMemberAddresses(machines)
	slice := &discoveryv1.EndpointSlice{}
	if err := wc.Get(ctx, kubernetesEndpointSliceKey, slice); err != nil {
		if apierrors.IsNotFound(err) {
			return k0sJoinServers{}, nil
		}
		return k0sJoinServers{}, fmt.Errorf("get the workload EndpointSlice %s: %w", kubernetesEndpointSliceKey, err)
	}
	if slice.AddressType != discoveryv1.AddressTypeIPv4 && slice.AddressType != discoveryv1.AddressTypeIPv6 {
		return k0sJoinServers{}, nil
	}
	var out k0sJoinServers
	seen := map[string]bool{}
	var ips []net.IP
	for _, ep := range slice.Endpoints {
		if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
			continue
		}
		if ep.Conditions.Terminating != nil && *ep.Conditions.Terminating {
			continue
		}
		for _, addr := range ep.Addresses {
			ip := net.ParseIP(addr)
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
				continue
			}
			key := ip.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			if !ours[key] {
				out.unmatched++
				continue
			}
			ips = append(ips, ip)
		}
	}
	sort.Slice(ips, func(i, j int) bool { return bytes.Compare(ips[i].To16(), ips[j].To16()) < 0 })
	for _, ip := range ips {
		out.urls = append(out.urls, k0sJoinServerURL(ip))
	}
	return out, nil
}

// ownMemberAddresses returns the canonical IPs in status.addresses of this
// control plane's Machines that are not being deleted, minus any IP a Machine
// being deleted also reports. machines is the KCP-owned set reconcileMachines
// works on.
func ownMemberAddresses(machines []*clusterv1.Machine) map[string]bool {
	live, leaving := map[string]bool{}, map[string]bool{}
	for _, m := range machines {
		into := live
		if !m.DeletionTimestamp.IsZero() {
			into = leaving
		}
		for _, a := range m.Status.Addresses {
			if ip := net.ParseIP(a.Address); ip != nil {
				into[ip.String()] = true
			}
		}
	}
	for ip := range leaving {
		delete(live, ip)
	}
	return live
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// k0sMemberJoined is the per-node "finished joining" test the join gate
// applies on k0s: the Machine has a Node and that node reported a healthy,
// voting etcd member.
func k0sMemberJoined(m *clusterv1.Machine, etcdStatus map[string]etcdMemberStatus) bool {
	if !m.Status.NodeRef.IsDefined() {
		return false
	}
	st, ok := etcdStatus[m.Status.NodeRef.Name]
	return ok && st.Healthy && st.Voting
}

// k0sJoinMember is a control-plane Machine whose KairosConfig has the join
// role. tokenID is the ID stamped on that KairosConfig when the KCP admitted
// the Machine, or "" when an older KCP created it and the init node's token was
// rendered into it. Each admitted joiner gets its own token, so tokenID binds
// the token to this one Machine.
type k0sJoinMember struct {
	machine  *clusterv1.Machine
	config   *bootstrapv1beta2.KairosConfig
	tokenID  string
	joined   bool
	deleting bool
}

// pending reports whether the member is still joining.
func (m k0sJoinMember) pending() bool { return !m.deleting && !m.joined }

// tokenRemoved reports whether the member's token was already deleted.
func (m k0sJoinMember) tokenRemoved() bool {
	_, ok := m.config.Annotations[k0sJoinTokenRemovedAtAnnotation]
	return ok
}

// k0sJoinMembers returns the control-plane Machines whose KairosConfig has the
// join role, with their token binding and state.
func (r *KairosControlPlaneReconciler) k0sJoinMembers(ctx context.Context, machines []*clusterv1.Machine, etcdStatus map[string]etcdMemberStatus) ([]k0sJoinMember, error) {
	var out []k0sJoinMember
	for _, m := range machines {
		ref := m.Spec.Bootstrap.ConfigRef
		if ref.Kind != "KairosConfig" || ref.Name == "" {
			continue
		}
		kc := &bootstrapv1beta2.KairosConfig{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: ref.Name}, kc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get KairosConfig %s/%s: %w", m.Namespace, ref.Name, err)
		}
		if kc.Spec.ControlPlaneRole != bootstrapv1beta2.ControlPlaneRoleJoin {
			continue
		}
		out = append(out, k0sJoinMember{
			machine:  m,
			config:   kc,
			tokenID:  kc.Annotations[k0sJoinTokenIDAnnotation],
			joined:   k0sMemberJoined(m, etcdStatus),
			deleting: !m.DeletionTimestamp.IsZero(),
		})
	}
	return out, nil
}

// workloadClientFor returns a client for the workload cluster through the
// WorkloadClientFactory seam.
func (r *KairosControlPlaneReconciler) workloadClientFor(ctx context.Context, cluster *clusterv1.Cluster) (client.Client, error) {
	factory := r.WorkloadClientFactory
	if factory == nil {
		factory = r.defaultWorkloadClient
	}
	return factory(ctx, cluster)
}

// workloadClusterCA returns the cluster CA of the workload kubeconfig. The KCP's
// workload client already verifies the apiserver against it, and on k0s it is
// the same CA `k0s token create` embeds, so joiners verify the :9443 join API
// against it too.
//
// It comes from workloadRESTConfig, so it is the CA of the sanitized kubeconfig
// and matches the fingerprint pinned on the KairosControlPlane (ADR 0011): a
// CA that was swapped into the Secret is never handed to a joiner.
func (r *KairosControlPlaneReconciler) workloadClusterCA(ctx context.Context, cluster *clusterv1.Cluster) ([]byte, error) {
	restCfg, err := r.workloadRESTConfig(ctx, cluster)
	if err != nil {
		return nil, err
	}
	if len(restCfg.CAData) == 0 {
		return nil, fmt.Errorf("workload kubeconfig for cluster %s/%s carries no cluster CA data", cluster.Namespace, cluster.Name)
	}
	return restCfg.CAData, nil
}

// storedK0sJoinToken is what the management join-token Secret currently holds.
type storedK0sJoinToken struct {
	annotatedID string
	empty       bool
	decoded     *k0sjointoken.Decoded
}

func inspectStoredK0sJoinToken(secret *corev1.Secret) storedK0sJoinToken {
	out := storedK0sJoinToken{annotatedID: secret.Annotations[k0sJoinTokenIDAnnotation]}
	raw := secret.Data[joinTokenSecretDataKey]
	if len(raw) == 0 {
		out.empty = true
		return out
	}
	if d, err := k0sjointoken.Decode(string(raw)); err == nil {
		out.decoded = &d
	}
	return out
}

// supersededIDs are the bootstrap-token IDs a new mint replaces: the annotated
// ID and the ID inside the stored token. They differ only for a token an older
// init node pushed (no annotation) or a Secret someone edited by hand.
func (s storedK0sJoinToken) supersededIDs() []string {
	var ids []string
	if s.annotatedID != "" {
		ids = append(ids, s.annotatedID)
	}
	if s.decoded != nil && s.decoded.Token.ID() != s.annotatedID {
		ids = append(ids, s.decoded.Token.ID())
	}
	return ids
}

// ensureK0sControllerJoinToken issues the controller-join token for the joiner
// about to be created and returns its ID. It runs once the join gate has opened
// and before the join Machine and KairosConfig are created; the ID is stamped
// on the new KairosConfig. Every admitted joiner gets its own token. The stored
// token is reused only when no Machine was created with it yet, which is the
// case when a create is retried after a partial failure.
//
// The join gate admits one joiner at a time, so one Secret slot is enough: by
// the time the next joiner is admitted the previous one has joined, and its
// token lives on only in its own bootstrap data until the KCP deletes it.
//
// When it cannot produce a token it returns "" and a reason, and the caller
// holds the joiner and requeues. Workload-cluster failures are reported that
// way, not as errors, so a flaky workload API delays a join instead of marking
// the control plane failed.
func (r *KairosControlPlaneReconciler) ensureK0sControllerJoinToken(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine) (string, string, error) {
	return r.ensureK0sControllerJoinTokenAt(ctx, log, kcp, cluster, machines, time.Now())
}

func (r *KairosControlPlaneReconciler) ensureK0sControllerJoinTokenAt(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine, now time.Time) (string, string, error) {
	// ADR 0015 PR-2: no token is minted while the workload identity is frozen.
	if err := errIfIdentityFrozen(kcp, "issue a k0s join token"); err != nil {
		return "", "", err
	}
	// Never replace the token while a joiner rendered with it is still joining.
	// The gate already guarantees this; checking again keeps the invariant local.
	etcdStatus, err := r.readEtcdStatus(ctx, cluster)
	if err != nil {
		return "", "", err
	}
	members, err := r.k0sJoinMembers(ctx, machines, etcdStatus)
	if err != nil {
		return "", "", err
	}
	// boundTo records which Machine each issued token was given to. A token is
	// handed to exactly one joiner: once a Machine exists with it, the next
	// joiner gets a new one.
	boundTo := map[string]string{}
	for _, m := range members {
		if m.pending() {
			return "", fmt.Sprintf("joiner %s has not finished joining", m.machine.Name), nil
		}
		if m.tokenID != "" {
			boundTo[m.tokenID] = m.machine.Name
		}
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: cluster.Namespace, Name: joinTokenSecretName(cluster.Name)}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Sprintf("join-token Secret %s is not visible yet", secretKey), nil
		}
		return "", "", fmt.Errorf("get join-token secret %s: %w", secretKey, err)
	}

	ca, err := r.workloadClusterCA(ctx, cluster)
	if err != nil {
		return "", fmt.Sprintf("cannot read the cluster CA for the k0s join token: %v", err), nil
	}
	wc, err := r.workloadClientFor(ctx, cluster)
	if err != nil {
		return "", fmt.Sprintf("cannot reach the workload cluster to issue a k0s join token: %v", err), nil
	}
	// ADR 0015 PR-2: admit a joiner only into the pinned cluster, checked on
	// the client the token is minted through. A token minted in another
	// cluster would let the joiner join it.
	if err := r.verifyWorkloadIdentity(ctx, log, kcp, wc, leaveViaEndpoint); err != nil {
		if errors.Is(err, errWorkloadIdentityChanged) {
			return "", "", err
		}
		return "", fmt.Sprintf("cannot verify the workload cluster identity before issuing a k0s join token: %v", err), nil
	}
	servers, err := liveK0sJoinServers(ctx, wc, machines)
	if err != nil {
		return "", fmt.Sprintf("cannot list live control-plane members: %v", err), nil
	}
	live := servers.urls
	if len(live) == 0 {
		reason := fmt.Sprintf("no live control-plane member to issue a k0s join token for: the apiserver lists %d ready address(es) "+
			"in the workload default/kubernetes EndpointSlice and none is in status.addresses of a control-plane Machine of this cluster",
			servers.unmatched)
		r.k0sJoinTokenWarning(kcp, "K0sJoinTokenNoLiveMember", "No join token issued for the next control-plane member: %s.", reason)
		return "", reason, nil
	}

	stored := inspectStoredK0sJoinToken(secret)
	var why string
	switch {
	case stored.empty:
		why = "no token stored"
	case stored.decoded == nil:
		why = "the stored value is not a k0s controller join token"
	case stored.annotatedID == "":
		why = "the stored token was minted by the init node"
	case stored.decoded.Token.ID() != stored.annotatedID:
		why = "the stored token does not match its ID annotation"
	case boundTo[stored.annotatedID] != "":
		why = "the stored token was issued to control-plane Machine " + boundTo[stored.annotatedID]
	default:
		// Not yet given to any Machine: an earlier admission minted it, then the
		// Machine create failed and is being retried. It is still this joiner's.
		why, err = checkIssuedK0sJoinToken(ctx, wc, *stored.decoded, ca, live, now)
		if err != nil {
			return "", fmt.Sprintf("cannot read the issued k0s join token from the workload cluster: %v", err), nil
		}
		if why == "" {
			log.V(4).Info("Reusing k0s controller-join token", "tokenID", stored.annotatedID,
				"server", k0sJoinServerHost(stored.decoded.Server))
			return stored.annotatedID, "", nil
		}
	}

	id, err := r.mintK0sControllerJoinToken(ctx, log, kcp, wc, secret, ca, live[0], now, why)
	if err != nil {
		if apierrors.IsConflict(err) {
			return "", "the join-token Secret changed while a new token was being stored; retrying", nil
		}
		return "", fmt.Sprintf("issuing a k0s join token failed: %v", err), nil
	}
	// ADR 0015 PR-2: narrow the verify-then-act window. The identity was read
	// before the mint; read it again on the same client now the token exists.
	// If the cluster changed in between, delete the token (best effort; it
	// also lapses with its TTL) and freeze, so no joiner is ever created with
	// it. The management Secret keeps the encoded token, but no KairosConfig is
	// stamped with its ID, and after an operator clears the freeze the next
	// admission re-checks it against the pinned cluster before reusing it.
	//
	// This narrows the window but cannot close it: through the VIP, each
	// request may reach a different apiserver, so the mint itself may land in
	// a cluster that neither read saw. The node-side join check (ADR 0015
	// PR-1: a joiner that sees itself as the only etcd member refuses to
	// serve) is the backstop for that residue.
	if verr := r.verifyWorkloadIdentity(ctx, log, kcp, wc, leaveViaEndpoint); verr != nil {
		if errors.Is(verr, errWorkloadIdentityChanged) {
			deleteK0sJoinToken(ctx, log, wc, id)
			return "", "", verr
		}
		return "", fmt.Sprintf("cannot re-verify the workload cluster identity after issuing a k0s join token: %v", verr), nil
	}
	for _, old := range stored.supersededIDs() {
		if old != id {
			deleteSupersededK0sJoinToken(ctx, log, wc, old)
		}
	}
	return id, "", nil
}

// checkIssuedK0sJoinToken reports why the token in the management Secret must
// be replaced, or "" when the joiner being admitted can still use it.
func checkIssuedK0sJoinToken(ctx context.Context, wc client.Client, stored k0sjointoken.Decoded, ca []byte, live []string, now time.Time) (string, error) {
	ws := &corev1.Secret{}
	if err := wc.Get(ctx, k0sjointoken.SecretKeyFor(stored.Token.ID()), ws); err != nil {
		if apierrors.IsNotFound(err) {
			return "the issued token no longer exists in the workload cluster", nil
		}
		return "", err
	}
	if !k0sjointoken.IsControllerJoinOnly(ws) {
		return "the workload token Secret is not a controller-join-only bootstrap token", nil
	}
	if subtle.ConstantTimeCompare(ws.Data[k0sjointoken.SecretKey], []byte(stored.Token.Secret())) != 1 {
		return "the workload token Secret does not match the stored token", nil
	}
	expires, ok := k0sjointoken.Expiry(ws)
	if !ok {
		return "the issued token has no valid expiration", nil
	}
	if expires.Sub(now) < k0sJoinTokenRenewWithin {
		return "the issued token expires within " + k0sJoinTokenRenewWithin.String(), nil
	}
	server, ok := normalizeK0sJoinServer(stored.Server)
	if !ok || !containsString(live, server) {
		return "the token's server is no longer a live control-plane member", nil
	}
	if !bytes.Equal(stored.CAData, ca) {
		return "the token's CA no longer matches the cluster CA", nil
	}
	return "", nil
}

// mintK0sControllerJoinToken creates a bootstrap token in the workload cluster
// and stores the encoded join token, with its ID annotation, in the management
// join-token Secret in one write. The write carries the Secret's
// resourceVersion: a reconcile working from a stale cache gets a conflict
// instead of overwriting a token another reconcile just issued.
func (r *KairosControlPlaneReconciler) mintK0sControllerJoinToken(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, wc client.Client, mgmtSecret *corev1.Secret, ca []byte, server string, now time.Time, why string) (string, error) {
	tok, err := k0sjointoken.Generate()
	if err != nil {
		return "", err
	}
	expires := now.Add(k0sJoinTokenTTL)
	ws := k0sjointoken.NewSecret(tok, expires)
	if err := wc.Create(ctx, ws); err != nil {
		return "", fmt.Errorf("create bootstrap token Secret %s/%s in the workload cluster: %w", ws.Namespace, ws.Name, err)
	}
	encoded, err := k0sjointoken.Encode(server, ca, tok)
	if err != nil {
		deleteK0sJoinTokenSecretBestEffort(ctx, log, wc, ws)
		return "", err
	}

	base := mgmtSecret.DeepCopy()
	if mgmtSecret.Data == nil {
		mgmtSecret.Data = map[string][]byte{}
	}
	mgmtSecret.Data[joinTokenSecretDataKey] = []byte(encoded)
	if mgmtSecret.Annotations == nil {
		mgmtSecret.Annotations = map[string]string{}
	}
	mgmtSecret.Annotations[k0sJoinTokenIDAnnotation] = tok.ID()
	if err := r.Patch(ctx, mgmtSecret, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		deleteK0sJoinTokenSecretBestEffort(ctx, log, wc, ws)
		return "", fmt.Errorf("store the k0s join token in %s/%s: %w", mgmtSecret.Namespace, mgmtSecret.Name, err)
	}

	host := k0sJoinServerHost(server)
	log.Info("Minted k0s controller-join token",
		"tokenID", tok.ID(), "server", host, "expires", expires.UTC().Format(time.RFC3339),
		"secret", mgmtSecret.Namespace+"/"+mgmtSecret.Name, "reason", why)
	if r.Recorder != nil {
		r.Recorder.Eventf(kcp, corev1.EventTypeNormal, "K0sJoinTokenMinted",
			"Issued k0s controller-join token %s for the next control-plane member; joiners reach %s:%s (%s)",
			tok.ID(), host, k0sjointoken.JoinAPIPort, why)
	}
	return tok.ID(), nil
}

// deleteSupersededK0sJoinToken removes a replaced token from the workload
// cluster, best effort.
func deleteSupersededK0sJoinToken(ctx context.Context, log logr.Logger, wc client.Client, id string) {
	deleteK0sJoinToken(ctx, log, wc, id)
}

// deleteK0sJoinToken deletes the workload Secret of token id. Only a
// controller-join-only bootstrap token is ever deleted, and the delete is
// pinned to the UID that was checked. It reports whether there is nothing left
// to do: deleted, already gone, or not a Secret this controller deletes. It
// reports false only when the workload cluster could not be read or written.
func deleteK0sJoinToken(ctx context.Context, log logr.Logger, wc client.Client, id string) bool {
	if !k0sjointoken.ValidID(id) {
		return true
	}
	ws := &corev1.Secret{}
	if err := wc.Get(ctx, k0sjointoken.SecretKeyFor(id), ws); err != nil {
		if apierrors.IsNotFound(err) {
			return true
		}
		log.V(1).Info("Could not read a k0s join token for cleanup", "tokenID", id, "error", err.Error())
		return false
	}
	if !k0sjointoken.IsControllerJoinOnly(ws) {
		log.Info("Leaving a token Secret in place: it is not a controller-join-only bootstrap token", "tokenID", id)
		return true
	}
	if !deleteK0sJoinTokenSecretBestEffort(ctx, log, wc, ws) {
		return false
	}
	log.Info("Deleted k0s controller-join token", "tokenID", id)
	return true
}

// deleteK0sJoinTokenSecretBestEffort deletes ws and reports whether it is gone.
func deleteK0sJoinTokenSecretBestEffort(ctx context.Context, log logr.Logger, wc client.Client, ws *corev1.Secret) bool {
	var opts []client.DeleteOption
	if ws.UID != "" {
		uid := ws.UID
		opts = append(opts, client.Preconditions{UID: &uid})
	}
	if err := wc.Delete(ctx, ws, opts...); err != nil && !apierrors.IsNotFound(err) {
		log.V(1).Info("Could not delete a k0s join token Secret; it expires on its own",
			"secret", ws.Namespace+"/"+ws.Name, "error", err.Error())
		return false
	}
	return true
}

// reconcileK0sJoinTokenLifetime keeps the token of every pending joiner alive,
// deletes the tokens of joiners that are done, and says why a joiner is stuck
// when the KCP cannot help it. It runs on every reconcile of a k0s HA control
// plane whose kubeconfig has been observed.
//
//   - A pending joiner is a non-deleting control-plane Machine whose join
//     KairosConfig carries the token-ID annotation and which has not finished
//     joining. Its own token is extended in place when less than 30m remain,
//     until 24h after the Machine was created.
//   - Once that Machine has joined (NodeRef plus a healthy voting etcd member)
//     or is being deleted, its token is deleted from the workload cluster, the
//     management join-token Secret is cleared if it still holds that token,
//     and the KairosConfig is marked so this happens once. A token that some
//     pending Machine also references is kept until that one is done too; only
//     tokens issued before one-token-per-joiner can be shared.
//   - A joiner whose KairosConfig has no annotation was rendered with the token
//     an older init node pushed. That token is never extended. The KCP says so
//     and leaves remediation to the operator or a MachineHealthCheck.
//
// Bootstrap data is never re-rendered, so a joiner keeps the token, and the
// server, it was rendered with. Replacing the token cannot help it.
//
// Every failure here is logged and skipped. Nothing it does gates the rest of
// the reconcile. It reports whether a pending joiner exists, so the caller can
// keep reconciling until it has joined; it also reports true when it cannot
// tell, so a transient read failure is retried rather than forgotten.
func (r *KairosControlPlaneReconciler) reconcileK0sJoinTokenLifetime(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine) bool {
	return r.reconcileK0sJoinTokenLifetimeAt(ctx, log, kcp, cluster, machines, time.Now())
}

func (r *KairosControlPlaneReconciler) reconcileK0sJoinTokenLifetimeAt(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine, now time.Time) bool {
	// ADR 0015 PR-2: no token is extended or deleted while the workload
	// identity is frozen. The freeze requeues the control plane by itself.
	if _, frozen := workloadIdentityFrozen(kcp); frozen {
		return false
	}
	etcdStatus, err := r.readEtcdStatus(ctx, cluster)
	if err != nil {
		log.Info("Cannot check pending k0s joiners", "error", err.Error())
		return true
	}
	members, err := r.k0sJoinMembers(ctx, machines, etcdStatus)
	if err != nil {
		log.Info("Cannot check pending k0s joiners", "error", err.Error())
		return true
	}

	pending := map[string][]*clusterv1.Machine{}
	var done []k0sJoinMember
	for _, m := range members {
		switch {
		case m.pending() && m.tokenID == "":
			log.Info("Control-plane joiner was bootstrapped with a token from the init node; it is not extended",
				"machine", m.machine.Name)
			r.k0sJoinTokenWarning(kcp, "K0sJoinTokenLegacy",
				"Control-plane Machine %s was bootstrapped with a k0s join token minted by the init node, which the control plane cannot extend or re-issue. "+
					"If it does not join, delete the Machine or let a MachineHealthCheck remediate it; its replacement gets a token from the control plane.",
				m.machine.Name)
		case m.pending():
			pending[m.tokenID] = append(pending[m.tokenID], m.machine)
		case m.tokenID != "" && !m.tokenRemoved():
			done = append(done, m)
		}
	}
	if len(pending) == 0 && len(done) == 0 {
		return false
	}

	wc, err := r.workloadClientFor(ctx, cluster)
	if err != nil {
		log.Info("Cannot reach the workload cluster to maintain k0s join tokens", "error", err.Error())
		return len(pending) > 0
	}
	// ADR 0015 PR-2: extend or delete tokens only in the pinned cluster. A
	// mismatch freezes the control plane; reconcileMachines then holds.
	if err := r.verifyWorkloadIdentity(ctx, log, kcp, wc, leaveViaEndpoint); err != nil {
		log.Info("Not maintaining k0s join tokens: cannot verify the workload cluster identity", "error", err.Error())
		return len(pending) > 0
	}

	for _, m := range done {
		if _, inUse := pending[m.tokenID]; inUse {
			continue
		}
		r.removeK0sJoinToken(ctx, log, wc, cluster, m)
	}
	if len(pending) == 0 {
		return false
	}

	// The joiner's server is the one in the stored token: the KCP never replaces
	// the token while a joiner is pending, so the IDs agree.
	var storedServer, storedID string
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: joinTokenSecretName(cluster.Name)}, secret); err == nil {
		if st := inspectStoredK0sJoinToken(secret); st.decoded != nil && st.annotatedID == st.decoded.Token.ID() {
			storedID = st.annotatedID
			storedServer, _ = normalizeK0sJoinServer(st.decoded.Server)
		}
	}
	servers, liveErr := liveK0sJoinServers(ctx, wc, machines)
	live := servers.urls

	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ms := pending[id]
		r.extendK0sJoinToken(ctx, log, kcp, wc, id, ms, now)
		if liveErr == nil && id == storedID && storedServer != "" && !containsString(live, storedServer) {
			for _, m := range ms {
				r.k0sJoinTokenWarning(kcp, "K0sJoinTokenServerUnavailable",
					"Control-plane Machine %s joins through %s, which is no longer a live control-plane member. "+
						"It stays pending until that member is back, or until the Machine is deleted or remediated.",
					m.Name, k0sJoinServerHost(storedServer))
			}
		}
	}
	return true
}

// removeK0sJoinToken deletes the token issued to a joiner that has joined or is
// being deleted, clears the management join-token Secret if it still holds that
// token, and marks the joiner's KairosConfig. Any step that fails leaves the
// mark off, so the next reconcile tries again; a token that is never deleted
// still lapses within its TTL, since nothing extends it.
func (r *KairosControlPlaneReconciler) removeK0sJoinToken(ctx context.Context, log logr.Logger, wc client.Client, cluster *clusterv1.Cluster, m k0sJoinMember) {
	if !deleteK0sJoinToken(ctx, log, wc, m.tokenID) {
		return
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: cluster.Namespace, Name: joinTokenSecretName(cluster.Name)}
	if err := r.Get(ctx, key, secret); err != nil && !apierrors.IsNotFound(err) {
		log.Info("Cannot read the join-token Secret to clear it", "error", err.Error())
		return
	} else if err == nil && secret.Annotations[k0sJoinTokenIDAnnotation] == m.tokenID {
		base := secret.DeepCopy()
		delete(secret.Data, joinTokenSecretDataKey)
		delete(secret.Annotations, k0sJoinTokenIDAnnotation)
		if err := r.Patch(ctx, secret, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			log.Info("Could not clear the join-token Secret; will retry", "tokenID", m.tokenID, "error", err.Error())
			return
		}
	}
	base := m.config.DeepCopy()
	if m.config.Annotations == nil {
		m.config.Annotations = map[string]string{}
	}
	m.config.Annotations[k0sJoinTokenRemovedAtAnnotation] = time.Now().UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, m.config, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
		log.Info("Could not mark the joiner's KairosConfig; will retry", "kairosConfig", m.config.Name, "error", err.Error())
		return
	}
	log.Info("Removed the k0s join token of a joiner that is done",
		"tokenID", m.tokenID, "machine", m.machine.Name, "joined", m.joined, "deleting", m.deleting)
}

// extendK0sJoinToken applies the extension rule to one token and the pending
// joiners that reference it.
func (r *KairosControlPlaneReconciler) extendK0sJoinToken(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, wc client.Client, id string, joiners []*clusterv1.Machine, now time.Time) {
	names := make([]string, 0, len(joiners))
	var newest time.Time
	for _, m := range joiners {
		names = append(names, m.Name)
		if m.CreationTimestamp.After(newest) {
			newest = m.CreationTimestamp.Time
		}
	}
	machinesStr := strings.Join(names, ",")

	if !k0sjointoken.ValidID(id) {
		log.Info("Pending joiner carries a malformed k0s join-token ID; not extending", "machines", machinesStr)
		return
	}
	ws := &corev1.Secret{}
	if err := wc.Get(ctx, k0sjointoken.SecretKeyFor(id), ws); err != nil {
		if apierrors.IsNotFound(err) {
			for _, n := range names {
				r.k0sJoinTokenWarning(kcp, "K0sJoinTokenMissing",
					"Control-plane Machine %s was bootstrapped with k0s join token %s, which no longer exists in the workload cluster. "+
						"It cannot join; delete the Machine or let a MachineHealthCheck remediate it.", n, id)
			}
			return
		}
		log.Info("Cannot read a pending joiner's k0s join token", "tokenID", id, "error", err.Error())
		return
	}
	if !k0sjointoken.IsControllerJoinOnly(ws) {
		log.Info("Not extending a token Secret that is not a controller-join-only bootstrap token", "tokenID", id)
		return
	}
	if now.Sub(newest) >= k0sJoinTokenExtendCap {
		for _, n := range names {
			r.k0sJoinTokenWarning(kcp, "K0sJoinTokenExtensionCapped",
				"Control-plane Machine %s has been joining for more than %s; its k0s join token %s is no longer extended. "+
					"Delete the Machine or let a MachineHealthCheck remediate it.", n, k0sJoinTokenExtendCap, id)
		}
		return
	}
	expires, ok := k0sjointoken.Expiry(ws)
	if !ok || expires.Sub(now) >= k0sJoinTokenRenewWithin {
		return
	}
	newExpiry := now.Add(k0sJoinTokenTTL)
	base := ws.DeepCopy()
	ws.Data[k0sjointoken.ExpirationKey] = []byte(newExpiry.UTC().Format(time.RFC3339))
	if err := wc.Patch(ctx, ws, client.MergeFrom(base)); err != nil {
		log.Info("Could not extend a pending joiner's k0s join token", "tokenID", id, "error", err.Error())
		return
	}
	log.Info("Extended k0s controller-join token for a pending joiner",
		"tokenID", id, "machines", machinesStr, "expires", newExpiry.UTC().Format(time.RFC3339))
}

func (r *KairosControlPlaneReconciler) k0sJoinTokenWarning(kcp *controlplanev1beta2.KairosControlPlane, reason, format string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(kcp, corev1.EventTypeWarning, reason, format, args...)
}

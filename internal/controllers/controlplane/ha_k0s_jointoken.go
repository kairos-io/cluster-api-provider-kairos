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
// Nothing here may log the token. Log the token ID (the public half), the
// Secret identity and the server host only.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
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

	// k0sJoinTokenRecheckInterval is the longest the KCP waits between two
	// reconciles while a joiner is pending, so a token is always looked at well
	// inside its k0sJoinTokenRenewWithin window.
	k0sJoinTokenRecheckInterval = 5 * time.Minute

	// k0sJoinAPIPort is the port of the k0s controller join API. `k0s token
	// create` uses the same port (APISpec.K0sControlPlaneAPIAddress).
	k0sJoinAPIPort = "9443"

	// k0sTokenKubeconfigName is the cluster and context name k0s uses inside a
	// join token, and k0sControllerBootstrapUser the user name. The joiner
	// refuses a controller token whose user is anything else.
	k0sTokenKubeconfigName     = "k0s"
	k0sControllerBootstrapUser = "controller-bootstrap"

	// Bootstrap-token Secret layout (k8s.io/cluster-bootstrap/token/api). Spelled
	// out here so this file needs no dependency beyond client-go and the stdlib.
	bootstrapTokenSecretPrefix      = "bootstrap-token-"
	bootstrapTokenIDKey             = "token-id"
	bootstrapTokenSecretKey         = "token-secret"
	bootstrapTokenExpirationKey     = "expiration"
	bootstrapTokenDescriptionKey    = "description"
	bootstrapTokenUsagePrefix       = "usage-bootstrap-"
	bootstrapTokenExtraGroupsKey    = "auth-extra-groups"
	bootstrapTokenUsageAuthKey      = bootstrapTokenUsagePrefix + "authentication"
	bootstrapTokenUsageSigningKey   = bootstrapTokenUsagePrefix + "signing"
	bootstrapTokenUsageCtrlJoinKey  = bootstrapTokenUsagePrefix + "controller-join"
	k0sLegacyUsageControllerJoinKey = "usage-controller-join"

	// k0sJoinTokenDescription is written into the bootstrap-token Secret so an
	// operator listing tokens can tell where it came from.
	k0sJoinTokenDescription = "k0s controller-join token minted by cluster-api-provider-kairos"

	bootstrapTokenIDLength     = 6
	bootstrapTokenSecretLength = 16
	bootstrapTokenCharset      = "abcdefghijklmnopqrstuvwxyz0123456789"

	// maxK0sJoinTokenKubeconfigBytes bounds the decompressed size of a stored
	// token we decode. A real one is about 1.5 KiB.
	maxK0sJoinTokenKubeconfigBytes = 64 << 10
)

var (
	bootstrapTokenIDPattern = regexp.MustCompile(`^[a-z0-9]{6}$`)
	bootstrapTokenPattern   = regexp.MustCompile(`^([a-z0-9]{6})\.([a-z0-9]{16})$`)
)

// k0sBootstrapToken is the "<id>.<secret>" bearer inside a k0s join token. The
// fields are unexported and String and MarshalLog redact the secret half, so an
// accidental log call prints the ID only.
type k0sBootstrapToken struct {
	id     string
	secret string
}

// bearer returns the full "<id>.<secret>" value. Never log it.
func (t k0sBootstrapToken) bearer() string { return t.id + "." + t.secret }

func (t k0sBootstrapToken) String() string { return t.id + ".<redacted>" }

// MarshalLog implements logr.Marshaler.
func (t k0sBootstrapToken) MarshalLog() any { return t.String() }

// generateK0sBootstrapToken returns a new bootstrap token from crypto/rand over
// [a-z0-9]: 6 characters of ID and 16 of secret, the format k0s and kubeadm use.
// It is stdlib only, so the release-0.1 line can share it without
// k8s.io/cluster-bootstrap.
func generateK0sBootstrapToken() (k0sBootstrapToken, error) {
	id, err := randomBootstrapTokenString(bootstrapTokenIDLength)
	if err != nil {
		return k0sBootstrapToken{}, err
	}
	secret, err := randomBootstrapTokenString(bootstrapTokenSecretLength)
	if err != nil {
		return k0sBootstrapToken{}, err
	}
	return k0sBootstrapToken{id: id, secret: secret}, nil
}

func randomBootstrapTokenString(n int) (string, error) {
	limit := big.NewInt(int64(len(bootstrapTokenCharset)))
	out := make([]byte, n)
	for i := range out {
		v, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("generate bootstrap token: %w", err)
		}
		out[i] = bootstrapTokenCharset[v.Int64()]
	}
	return string(out), nil
}

// encodeK0sControllerJoinToken builds a k0s controller join token: a kubeconfig
// with cluster and context "k0s" and user "controller-bootstrap", gzipped at
// BestCompression and base64-encoded with the standard alphabet. This is what
// `k0s token create --role=controller` and `k0s token pre-shared` produce
// (pkg/token/kubeconfig.go GenerateKubeconfig, pkg/token/joinencode.go).
func encodeK0sControllerJoinToken(server string, caPEM []byte, tok k0sBootstrapToken) (string, error) {
	kubeconfig, err := clientcmd.Write(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{k0sTokenKubeconfigName: {
			Server:                   server,
			CertificateAuthorityData: caPEM,
		}},
		Contexts: map[string]*clientcmdapi.Context{k0sTokenKubeconfigName: {
			Cluster:  k0sTokenKubeconfigName,
			AuthInfo: k0sControllerBootstrapUser,
		}},
		CurrentContext: k0sTokenKubeconfigName,
		AuthInfos: map[string]*clientcmdapi.AuthInfo{k0sControllerBootstrapUser: {
			Token: tok.bearer(),
		}},
	})
	if err != nil {
		return "", errors.New("encode k0s join token: cannot serialize the kubeconfig")
	}
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return "", fmt.Errorf("encode k0s join token: %w", err)
	}
	if _, err := gz.Write(kubeconfig); err != nil {
		return "", fmt.Errorf("encode k0s join token: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", fmt.Errorf("encode k0s join token: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// decodedK0sJoinToken is a parsed k0s controller join token.
type decodedK0sJoinToken struct {
	server string
	caData []byte
	user   string
	token  k0sBootstrapToken
}

// decodeK0sControllerJoinToken reverses encodeK0sControllerJoinToken. It also
// reads tokens minted by k0s itself, which is how the KCP recognises the token
// an older init node pushed. Errors never quote the input.
func decodeK0sControllerJoinToken(encoded string) (decodedK0sJoinToken, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return decodedK0sJoinToken{}, errors.New("decode k0s join token: not base64")
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return decodedK0sJoinToken{}, errors.New("decode k0s join token: not gzip")
	}
	data, err := io.ReadAll(io.LimitReader(gz, maxK0sJoinTokenKubeconfigBytes+1))
	if err != nil {
		return decodedK0sJoinToken{}, errors.New("decode k0s join token: corrupt gzip stream")
	}
	if len(data) > maxK0sJoinTokenKubeconfigBytes {
		return decodedK0sJoinToken{}, errors.New("decode k0s join token: kubeconfig too large")
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return decodedK0sJoinToken{}, errors.New("decode k0s join token: not a kubeconfig")
	}
	kctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok || kctx == nil {
		return decodedK0sJoinToken{}, errors.New("decode k0s join token: no current context")
	}
	cluster, ok := cfg.Clusters[kctx.Cluster]
	if !ok || cluster == nil {
		return decodedK0sJoinToken{}, errors.New("decode k0s join token: context names an unknown cluster")
	}
	auth, ok := cfg.AuthInfos[kctx.AuthInfo]
	if !ok || auth == nil {
		return decodedK0sJoinToken{}, errors.New("decode k0s join token: context names an unknown user")
	}
	m := bootstrapTokenPattern.FindStringSubmatch(auth.Token)
	if m == nil {
		return decodedK0sJoinToken{}, errors.New("decode k0s join token: bearer is not a bootstrap token")
	}
	return decodedK0sJoinToken{
		server: cluster.Server,
		caData: cluster.CertificateAuthorityData,
		user:   kctx.AuthInfo,
		token:  k0sBootstrapToken{id: m[1], secret: m[2]},
	}, nil
}

// newK0sControllerJoinTokenSecret returns the workload kube-system Secret that
// backs tok, in the shape k0s gives its own controller tokens
// (pkg/token/manager.go RandomBootstrapSecret): the controller-join usage in
// both its current and legacy key, and nothing else. In particular there is no
// authentication or signing usage and no extra group, so the token cannot be
// used against kube-apiserver at all.
func newK0sControllerJoinTokenSecret(tok k0sBootstrapToken, expires time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bootstrapTokenSecretPrefix + tok.id,
			Namespace: metav1.NamespaceSystem,
		},
		Type: corev1.SecretTypeBootstrapToken,
		Data: map[string][]byte{
			bootstrapTokenIDKey:             []byte(tok.id),
			bootstrapTokenSecretKey:         []byte(tok.secret),
			bootstrapTokenExpirationKey:     []byte(expires.UTC().Format(time.RFC3339)),
			bootstrapTokenDescriptionKey:    []byte(k0sJoinTokenDescription),
			bootstrapTokenUsageCtrlJoinKey:  []byte("true"),
			k0sLegacyUsageControllerJoinKey: []byte("true"),
		},
	}
}

// isK0sControllerJoinTokenSecret reports whether s is a bootstrap-token Secret
// that carries the controller-join usage and cannot authenticate to
// kube-apiserver. The KCP only extends, reuses or deletes Secrets that pass.
func isK0sControllerJoinTokenSecret(s *corev1.Secret) bool {
	if s.Type != corev1.SecretTypeBootstrapToken {
		return false
	}
	ctrlJoin := string(s.Data[bootstrapTokenUsageCtrlJoinKey]) == "true" ||
		string(s.Data[k0sLegacyUsageControllerJoinKey]) == "true"
	if !ctrlJoin {
		return false
	}
	if string(s.Data[bootstrapTokenUsageAuthKey]) == "true" || string(s.Data[bootstrapTokenUsageSigningKey]) == "true" {
		return false
	}
	return len(s.Data[bootstrapTokenExtraGroupsKey]) == 0
}

// bootstrapTokenSecretExpiry returns the expiration of a bootstrap-token Secret.
// ok is false when the key is missing or unparsable.
func bootstrapTokenSecretExpiry(s *corev1.Secret) (time.Time, bool) {
	raw, present := s.Data[bootstrapTokenExpirationKey]
	if !present || len(raw) == 0 {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, string(raw))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func bootstrapTokenSecretKeyFor(id string) types.NamespacedName {
	return types.NamespacedName{Namespace: metav1.NamespaceSystem, Name: bootstrapTokenSecretPrefix + id}
}

// k0sJoinServerURL returns the join API URL for a member address.
// net.JoinHostPort brackets IPv6 literals.
func k0sJoinServerURL(ip net.IP) string {
	return "https://" + net.JoinHostPort(ip.String(), k0sJoinAPIPort)
}

// normalizeK0sJoinServer canonicalises a token's server URL so it compares
// equal to the URLs liveK0sJoinServers builds. ok is false for anything that is
// not https://<ip>:9443.
func normalizeK0sJoinServer(server string) (string, bool) {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Port() != k0sJoinAPIPort {
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

// liveK0sJoinServers returns the join API URLs of the live control-plane
// members, lowest address first.
//
// The source is the workload default/kubernetes EndpointSlice. kube-apiserver
// advertises api.address, and with externalAddress unset k0s leaves the
// apiserver's own lease-based endpoint reconciler in charge, so the slice lists
// exactly the members whose apiserver is up (ADR 0013 GT-6). `k0s token create`
// builds its URL from the same api.address.
//
// An address counts only if its endpoint is ready and not terminating and it
// parses as a unicast IP. Loopback and unspecified addresses are dropped, as is
// any address in excluded (the addresses of control-plane Machines that are
// being deleted).
func liveK0sJoinServers(ctx context.Context, wc client.Client, excluded map[string]bool) ([]string, error) {
	slices := &discoveryv1.EndpointSliceList{}
	if err := wc.List(ctx, slices,
		client.InNamespace(metav1.NamespaceDefault),
		client.MatchingLabels{discoveryv1.LabelServiceName: "kubernetes"},
	); err != nil {
		return nil, fmt.Errorf("list the workload default/kubernetes EndpointSlices: %w", err)
	}
	seen := map[string]bool{}
	var ips []net.IP
	for i := range slices.Items {
		slice := &slices.Items[i]
		if slice.AddressType != discoveryv1.AddressTypeIPv4 && slice.AddressType != discoveryv1.AddressTypeIPv6 {
			continue
		}
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
				if excluded[key] || seen[key] {
					continue
				}
				seen[key] = true
				ips = append(ips, ip)
			}
		}
	}
	sort.Slice(ips, func(i, j int) bool { return bytes.Compare(ips[i].To16(), ips[j].To16()) < 0 })
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, k0sJoinServerURL(ip))
	}
	return out, nil
}

// deletingMachineAddresses returns the canonical IPs of every control-plane
// Machine that is being deleted. A member on its way out may still be in the
// EndpointSlice; a new joiner must not be pointed at it.
func deletingMachineAddresses(machines []*clusterv1.Machine) map[string]bool {
	out := map[string]bool{}
	for _, m := range machines {
		if m.DeletionTimestamp.IsZero() {
			continue
		}
		for _, a := range m.Status.Addresses {
			if ip := net.ParseIP(a.Address); ip != nil {
				out[ip.String()] = true
			}
		}
	}
	return out
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

// k0sJoiner is a control-plane joiner that has not finished joining.
// tokenID is the ID stamped on its KairosConfig, or "" when an older KCP created
// it and the init node's token was rendered into it.
type k0sJoiner struct {
	machine *clusterv1.Machine
	tokenID string
}

// k0sJoinersInFlight returns the non-deleting control-plane Machines whose
// KairosConfig has the join role and that have not finished joining.
func (r *KairosControlPlaneReconciler) k0sJoinersInFlight(ctx context.Context, machines []*clusterv1.Machine, etcdStatus map[string]etcdMemberStatus) ([]k0sJoiner, error) {
	var out []k0sJoiner
	for _, m := range machines {
		if !m.DeletionTimestamp.IsZero() || k0sMemberJoined(m, etcdStatus) {
			continue
		}
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
		out = append(out, k0sJoiner{machine: m, tokenID: kc.Annotations[k0sJoinTokenIDAnnotation]})
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

// workloadClusterCA returns the cluster CA from the workload kubeconfig Secret.
// The KCP's workload client already verifies the apiserver against it, and on
// k0s it is the same CA `k0s token create` embeds, so joiners verify the :9443
// join API against it too.
func (r *KairosControlPlaneReconciler) workloadClusterCA(ctx context.Context, cluster *clusterv1.Cluster) ([]byte, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: cluster.Namespace, Name: fmt.Sprintf("%s-kubeconfig", cluster.Name)}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("get workload kubeconfig secret %s: %w", key, err)
	}
	cfg, err := clientcmd.Load(secret.Data["value"])
	if err != nil {
		return nil, fmt.Errorf("workload kubeconfig secret %s does not parse", key)
	}
	kctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok || kctx == nil {
		return nil, fmt.Errorf("workload kubeconfig secret %s has no current context", key)
	}
	cl, ok := cfg.Clusters[kctx.Cluster]
	if !ok || cl == nil || len(cl.CertificateAuthorityData) == 0 {
		return nil, fmt.Errorf("workload kubeconfig secret %s carries no cluster CA data", key)
	}
	return cl.CertificateAuthorityData, nil
}

// storedK0sJoinToken is what the management join-token Secret currently holds.
type storedK0sJoinToken struct {
	annotatedID string
	empty       bool
	decoded     *decodedK0sJoinToken
}

func inspectStoredK0sJoinToken(secret *corev1.Secret) storedK0sJoinToken {
	out := storedK0sJoinToken{annotatedID: secret.Annotations[k0sJoinTokenIDAnnotation]}
	raw := secret.Data[joinTokenSecretDataKey]
	if len(raw) == 0 {
		out.empty = true
		return out
	}
	if d, err := decodeK0sControllerJoinToken(string(raw)); err == nil {
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
	if s.decoded != nil && s.decoded.token.id != s.annotatedID {
		ids = append(ids, s.decoded.token.id)
	}
	return ids
}

// ensureK0sControllerJoinToken makes sure the join-token Secret holds a usable
// KCP-minted token, minting one if needed, and returns its ID. It runs once the
// join gate has opened and before the join Machine and KairosConfig are created.
// The ID is stamped on the new KairosConfig.
//
// When it cannot produce a token it returns "" and a reason, and the caller
// holds the joiner and requeues. Workload-cluster failures are reported that
// way, not as errors, so a flaky workload API delays a join instead of marking
// the control plane failed.
func (r *KairosControlPlaneReconciler) ensureK0sControllerJoinToken(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine) (string, string, error) {
	return r.ensureK0sControllerJoinTokenAt(ctx, log, kcp, cluster, machines, time.Now())
}

func (r *KairosControlPlaneReconciler) ensureK0sControllerJoinTokenAt(ctx context.Context, log logr.Logger, kcp *controlplanev1beta2.KairosControlPlane, cluster *clusterv1.Cluster, machines []*clusterv1.Machine, now time.Time) (string, string, error) {
	// Never replace the token while a joiner rendered with it is still joining.
	// The gate already guarantees this; checking again keeps the invariant local.
	etcdStatus, err := r.readEtcdStatus(ctx, cluster)
	if err != nil {
		return "", "", err
	}
	joiners, err := r.k0sJoinersInFlight(ctx, machines, etcdStatus)
	if err != nil {
		return "", "", err
	}
	if len(joiners) > 0 {
		return "", fmt.Sprintf("joiner %s has not finished joining", joiners[0].machine.Name), nil
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
	live, err := liveK0sJoinServers(ctx, wc, deletingMachineAddresses(machines))
	if err != nil {
		return "", fmt.Sprintf("cannot list live control-plane members: %v", err), nil
	}
	if len(live) == 0 {
		return "", "no live control-plane member is listed in the workload default/kubernetes EndpointSlice; cannot issue a k0s join token", nil
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
	case stored.decoded.token.id != stored.annotatedID:
		why = "the stored token does not match its ID annotation"
	default:
		why, err = checkIssuedK0sJoinToken(ctx, wc, *stored.decoded, ca, live, now)
		if err != nil {
			return "", fmt.Sprintf("cannot read the issued k0s join token from the workload cluster: %v", err), nil
		}
		if why == "" {
			log.V(4).Info("Reusing k0s controller-join token", "tokenID", stored.annotatedID,
				"server", k0sJoinServerHost(stored.decoded.server))
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
	for _, old := range stored.supersededIDs() {
		if old != id {
			deleteSupersededK0sJoinToken(ctx, log, wc, old)
		}
	}
	return id, "", nil
}

// checkIssuedK0sJoinToken reports why the token in the management Secret must
// be replaced, or "" when it can be handed to the next joiner as is.
func checkIssuedK0sJoinToken(ctx context.Context, wc client.Client, stored decodedK0sJoinToken, ca []byte, live []string, now time.Time) (string, error) {
	ws := &corev1.Secret{}
	if err := wc.Get(ctx, bootstrapTokenSecretKeyFor(stored.token.id), ws); err != nil {
		if apierrors.IsNotFound(err) {
			return "the issued token no longer exists in the workload cluster", nil
		}
		return "", err
	}
	if !isK0sControllerJoinTokenSecret(ws) {
		return "the workload token Secret is not a controller-join-only bootstrap token", nil
	}
	if subtle.ConstantTimeCompare(ws.Data[bootstrapTokenSecretKey], []byte(stored.token.secret)) != 1 {
		return "the workload token Secret does not match the stored token", nil
	}
	expires, ok := bootstrapTokenSecretExpiry(ws)
	if !ok {
		return "the issued token has no valid expiration", nil
	}
	if expires.Sub(now) < k0sJoinTokenRenewWithin {
		return "the issued token expires within " + k0sJoinTokenRenewWithin.String(), nil
	}
	server, ok := normalizeK0sJoinServer(stored.server)
	if !ok || !containsString(live, server) {
		return "the token's server is no longer a live control-plane member", nil
	}
	if !bytes.Equal(stored.caData, ca) {
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
	tok, err := generateK0sBootstrapToken()
	if err != nil {
		return "", err
	}
	expires := now.Add(k0sJoinTokenTTL)
	ws := newK0sControllerJoinTokenSecret(tok, expires)
	if err := wc.Create(ctx, ws); err != nil {
		return "", fmt.Errorf("create bootstrap token Secret %s/%s in the workload cluster: %w", ws.Namespace, ws.Name, err)
	}
	encoded, err := encodeK0sControllerJoinToken(server, ca, tok)
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
	mgmtSecret.Annotations[k0sJoinTokenIDAnnotation] = tok.id
	if err := r.Patch(ctx, mgmtSecret, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		deleteK0sJoinTokenSecretBestEffort(ctx, log, wc, ws)
		return "", fmt.Errorf("store the k0s join token in %s/%s: %w", mgmtSecret.Namespace, mgmtSecret.Name, err)
	}

	host := k0sJoinServerHost(server)
	log.Info("Minted k0s controller-join token",
		"tokenID", tok.id, "server", host, "expires", expires.UTC().Format(time.RFC3339),
		"secret", mgmtSecret.Namespace+"/"+mgmtSecret.Name, "reason", why)
	if r.Recorder != nil {
		r.Recorder.Eventf(kcp, corev1.EventTypeNormal, "K0sJoinTokenMinted",
			"Issued k0s controller-join token %s for the next control-plane member; joiners reach %s:%s (%s)",
			tok.id, host, k0sJoinAPIPort, why)
	}
	return tok.id, nil
}

// deleteSupersededK0sJoinToken removes a replaced token from the workload
// cluster, best effort. Only a controller-join-only bootstrap token is ever
// deleted, and the delete is pinned to the UID that was checked.
func deleteSupersededK0sJoinToken(ctx context.Context, log logr.Logger, wc client.Client, id string) {
	if !bootstrapTokenIDPattern.MatchString(id) {
		return
	}
	ws := &corev1.Secret{}
	if err := wc.Get(ctx, bootstrapTokenSecretKeyFor(id), ws); err != nil {
		if !apierrors.IsNotFound(err) {
			log.V(1).Info("Could not read a superseded k0s join token for cleanup", "tokenID", id, "error", err.Error())
		}
		return
	}
	if !isK0sControllerJoinTokenSecret(ws) {
		log.Info("Leaving a superseded token Secret in place: it is not a controller-join-only bootstrap token", "tokenID", id)
		return
	}
	deleteK0sJoinTokenSecretBestEffort(ctx, log, wc, ws)
	log.Info("Deleted superseded k0s controller-join token", "tokenID", id)
}

func deleteK0sJoinTokenSecretBestEffort(ctx context.Context, log logr.Logger, wc client.Client, ws *corev1.Secret) {
	var opts []client.DeleteOption
	if ws.UID != "" {
		uid := ws.UID
		opts = append(opts, client.Preconditions{UID: &uid})
	}
	if err := wc.Delete(ctx, ws, opts...); err != nil && !apierrors.IsNotFound(err) {
		log.V(1).Info("Could not delete a k0s join token Secret; it expires on its own",
			"secret", ws.Namespace+"/"+ws.Name, "error", err.Error())
	}
}

// reconcileK0sJoinTokenLifetime keeps the token of every pending joiner alive
// and says why a joiner is stuck when the KCP cannot help it. It runs on every
// reconcile of a k0s HA control plane whose kubeconfig has been observed.
//
//   - A pending joiner is a non-deleting control-plane Machine whose join
//     KairosConfig carries the token-ID annotation and which has not finished
//     joining. Its token is extended in place when less than 30m remain, until
//     the Machine is 24h old.
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
	etcdStatus, err := r.readEtcdStatus(ctx, cluster)
	if err != nil {
		log.Info("Cannot check pending k0s joiners", "error", err.Error())
		return true
	}
	joiners, err := r.k0sJoinersInFlight(ctx, machines, etcdStatus)
	if err != nil {
		log.Info("Cannot check pending k0s joiners", "error", err.Error())
		return true
	}

	pending := map[string][]*clusterv1.Machine{}
	for _, j := range joiners {
		if j.tokenID == "" {
			log.Info("Control-plane joiner was bootstrapped with a token from the init node; it is not extended",
				"machine", j.machine.Name)
			r.k0sJoinTokenWarning(kcp, "K0sJoinTokenLegacy",
				"Control-plane Machine %s was bootstrapped with a k0s join token minted by the init node, which the control plane cannot extend or re-issue. "+
					"If it does not join, delete the Machine or let a MachineHealthCheck remediate it; its replacement gets a token from the control plane.",
				j.machine.Name)
			continue
		}
		pending[j.tokenID] = append(pending[j.tokenID], j.machine)
	}
	if len(pending) == 0 {
		return false
	}

	wc, err := r.workloadClientFor(ctx, cluster)
	if err != nil {
		log.Info("Cannot reach the workload cluster to extend k0s join tokens", "error", err.Error())
		return true
	}

	// The joiner's server is the one in the stored token: the KCP never replaces
	// the token while a joiner is pending, so the IDs agree.
	var storedServer, storedID string
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: joinTokenSecretName(cluster.Name)}, secret); err == nil {
		if st := inspectStoredK0sJoinToken(secret); st.decoded != nil && st.annotatedID == st.decoded.token.id {
			storedID = st.annotatedID
			storedServer, _ = normalizeK0sJoinServer(st.decoded.server)
		}
	}
	live, liveErr := liveK0sJoinServers(ctx, wc, deletingMachineAddresses(machines))

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

	if !bootstrapTokenIDPattern.MatchString(id) {
		log.Info("Pending joiner carries a malformed k0s join-token ID; not extending", "machines", machinesStr)
		return
	}
	ws := &corev1.Secret{}
	if err := wc.Get(ctx, bootstrapTokenSecretKeyFor(id), ws); err != nil {
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
	if !isK0sControllerJoinTokenSecret(ws) {
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
	expires, ok := bootstrapTokenSecretExpiry(ws)
	if !ok || expires.Sub(now) >= k0sJoinTokenRenewWithin {
		return
	}
	newExpiry := now.Add(k0sJoinTokenTTL)
	base := ws.DeepCopy()
	ws.Data[bootstrapTokenExpirationKey] = []byte(newExpiry.UTC().Format(time.RFC3339))
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

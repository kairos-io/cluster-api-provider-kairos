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

package bootstrap

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	bootstrapapi "k8s.io/cluster-bootstrap/token/api"
	bootstraputil "k8s.io/cluster-bootstrap/token/util"
	kubeadmv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	kubeadmtypes "sigs.k8s.io/cluster-api/bootstrap/kubeadm/types"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/kubeconfig"
)

const (
	// controlPlaneGroup is the API group every allowlisted control-plane kind lives
	// in. The allowlist matches on GROUP AND KIND (ADR 0010 OQ-9): a kind name alone
	// is not enough because anyone can define a CRD named "KubeadmControlPlane" in a
	// group they control.
	controlPlaneGroup = "controlplane.cluster.x-k8s.io"

	// defaultKubeadmTokenTTL is the bootstrap-token lifetime (ADR 0010 P1). Short by
	// design: the token is refreshed while the Machine has no nodeRef.
	defaultKubeadmTokenTTL = 15 * time.Minute

	// kubeadmTokenRefreshCap bounds how long the controller keeps refreshing a
	// worker's bootstrap token before giving up and raising a condition (ADR 0010 P1
	// item 7). Anchored on the Machine's creation time; the MHC nodeStartupTimeout is
	// the operational backstop.
	kubeadmTokenRefreshCap = 24 * time.Hour

	// minKubeadmTokenRecheckInterval floors TokenRecheckInterval so a very short
	// TTL cannot turn the requeue into a hot loop against the workload API.
	minKubeadmTokenRecheckInterval = 10 * time.Second

	// kubeadmBootstrapTokenGroups is the extra-groups value CABPK sets so the token
	// authenticates as a node bootstrapper (kubeadmconfig_controller token.go).
	kubeadmBootstrapTokenGroups = "system:bootstrappers:kubeadm:default-node-token"

	// kubeadmTokenOwnerLabel is set on every minted token Secret to the UID of the
	// KairosConfig it belongs to. It makes the token findable before its ID has
	// reached status (reuseOrMintBootstrapToken) and attributable afterwards.
	kubeadmTokenOwnerLabel = "bootstrap.cluster.x-k8s.io/kairosconfig-uid"

	// workloadKubeconfigSecretKey is the data key CAPI kubeconfig Secrets use.
	workloadKubeconfigSecretKey = "value"

	// clusterCASecretCrtKey is the certificate key in a <cluster>-ca Secret.
	clusterCASecretCrtKey = "tls.crt"

	// serverAddressDefaultScheme is assumed for a spec.serverAddress given as host[:port].
	serverAddressDefaultScheme = "https://"
	schemeSeparator            = "://"
)

// allowlistedControlPlaneKinds are the control-plane kinds a kubeadm worker may
// trust WITHOUT an operator flag, matched on group and kind. KairosControlPlane is
// included here for the FINE, per-request check only, where it additionally requires
// spec.distribution == kubeadm (a kubeadm KairosControlPlane is a later phase, so in
// this release that branch never matches — a k0s/k3s KairosControlPlane is refused).
var allowlistedControlPlaneKinds = map[schema.GroupKind]bool{
	{Group: controlPlaneGroup, Kind: "KubeadmControlPlane"}: true,
	{Group: controlPlaneGroup, Kind: "KamajiControlPlane"}:  true,
	{Group: controlPlaneGroup, Kind: "KairosControlPlane"}:  true,
}

// bootstrapNotReadyError is the typed not-ready signal for the kubeadm join path
// (ADR 0010 P1 item 6). reconcileBootstrapData maps it to a 10s requeue and leaves
// conditions untouched — the KairosConfig is NOT parked. Every transient failure
// (control plane not initialized yet, kubeconfig Secret absent, workload-cluster
// connect/API timeout) MUST be a bootstrapNotReadyError; anything else is terminal
// and surfaces as a failure condition. The message is user-visible and never
// carries secret material.
type bootstrapNotReadyError struct {
	reason string
	msg    string
}

func (e *bootstrapNotReadyError) Error() string { return e.msg }

// Reason returns the short machine-readable reason.
func (e *bootstrapNotReadyError) Reason() string { return e.reason }

// newNotReady builds a bootstrapNotReadyError.
func newNotReady(reason, format string, args ...any) error {
	return &bootstrapNotReadyError{reason: reason, msg: fmt.Sprintf(format, args...)}
}

// workloadClientFactory returns a client to the workload cluster identified by
// clusterKey, built from the kubeconfig the caller has already verified.
//
// Taking the kubeconfig as an argument is what keeps the connection on-demand and
// trust-gated. There is no resident accessor that could connect ahead of a join,
// and the only bytes a connection can be built from are the ones verifyTrustChain
// returned, so a caller that has not run the trust check has nothing to pass.
// Tests inject a fake.
type workloadClientFactory func(ctx context.Context, clusterKey client.ObjectKey, kubeconfig []byte) (client.Client, error)

// kubeadmJoinSource is the kubeadm worker JoinMaterialSource (ADR 0010 P1). It runs
// the owner-verified trust check, mints a short-lived bootstrap token in the
// workload cluster, and renders the JoinConfiguration. It implements
// JoinMaterialSource and workerTokenRefresher.
type kubeadmJoinSource struct {
	// mgmt is a management-cluster reader for the kubeconfig Secret and the
	// control-plane object (cached is fine; these are watched/stable).
	mgmt client.Reader
	// caReader is an UNCACHED management-cluster reader used only for the
	// <cluster>-ca cross-check, so a planted/cached value cannot be trusted
	// (ADR 0010 P1 step 2).
	caReader client.Reader
	// workloadClient produces a workload-cluster client for token operations.
	workloadClient workloadClientFactory
	// extraKinds widens the control-plane allowlist via an operator-level flag,
	// never a per-Cluster field (OQ-9).
	extraKinds map[schema.GroupKind]bool
	// tokenTTL is the minted-token lifetime (default defaultKubeadmTokenTTL).
	tokenTTL time.Duration
	// now is the clock (injectable for tests).
	now func() time.Time
	// resolveControlPlane resolves a ContractVersionedObjectReference to the live
	// control-plane object at its CONTRACT-advertised API version (so Kamaji's
	// non-v1beta2 KamajiControlPlane (e.g. v1alpha2) resolves correctly, not only v1beta2 kinds). Nil
	// means the production resolver, external.GetObjectFromContractVersionedRef over
	// s.mgmt; unit tests inject a stub so they need no CRD/contract labels.
	resolveControlPlane func(ctx context.Context, ref clusterv1.ContractVersionedObjectReference, namespace string) (*unstructured.Unstructured, error)
}

var (
	_ JoinMaterialSource   = (*kubeadmJoinSource)(nil)
	_ workerTokenRefresher = (*kubeadmJoinSource)(nil)
)

// newKubeadmJoinSource builds a kubeadmJoinSource with production defaults.
func newKubeadmJoinSource(mgmt, caReader client.Reader, workloadClient workloadClientFactory, extraKinds map[schema.GroupKind]bool) *kubeadmJoinSource {
	return &kubeadmJoinSource{
		mgmt:           mgmt,
		caReader:       caReader,
		workloadClient: workloadClient,
		extraKinds:     extraKinds,
		tokenTTL:       defaultKubeadmTokenTTL,
		now:            time.Now,
	}
}

// NewKubeadmJoinSource is the exported constructor main.go wires into
// KairosConfigReconciler.JoinSources["kubeadm"] (ADR 0010 P1). It returns the
// JoinMaterialSource interface; the reconciler finds the optional token-refresh
// capability by type assertion. mgmt is a cached management reader; caReader is an
// UNCACHED management reader for the <cluster>-ca cross-check; workloadClient is a
// clustercache-backed, ClusterFilter-gated workload-client factory; extraKinds is
// the operator-level allowlist widener.
func NewKubeadmJoinSource(mgmt, caReader client.Reader, workloadClient workloadClientFactory, extraKinds map[schema.GroupKind]bool) JoinMaterialSource {
	return newKubeadmJoinSource(mgmt, caReader, workloadClient, extraKinds)
}

// NewOnDemandWorkloadClient is the production workloadClientFactory main.go wires
// in. It builds a direct client from the verified kubeconfig, once, for the join
// that asked for it.
//
// It replaces a clustercache-backed factory. clustercache is not lazy: its own
// Cluster reconciler creates an accessor and calls Connect() for every cluster its
// ClusterFilter admits and whose infrastructure is provisioned, then health-probes
// it on a timer, whether or not a kubeadm KairosConfig exists. Because the filter
// could only see Cluster.spec.controlPlaneRef, installing this provider meant the
// bootstrap manager held admin connections to every KubeadmControlPlane and
// KamajiControlPlane cluster in the management cluster, Kairos or not, with no
// feature gate and no opt-out. Building the client here instead means a connection
// exists only while a worker of ours is actually joining, and only to a cluster
// whose kubeconfig passed the full ADR 0010 trust check.
//
// There is nothing to cache: the only workload objects this provider touches are
// bootstrap-token Secrets, which must be read live, and which the clustercache
// client already excluded from its cache.
func NewOnDemandWorkloadClient() workloadClientFactory {
	return func(_ context.Context, clusterKey client.ObjectKey, kubeconfig []byte) (client.Client, error) {
		if len(kubeconfig) == 0 {
			return nil, fmt.Errorf("refusing to connect to workload cluster %s without a verified kubeconfig", clusterKey)
		}
		restCfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("parse verified kubeconfig for workload cluster %s: %w", clusterKey, err)
		}
		restCfg.UserAgent = workloadClientUserAgent
		scheme := runtime.NewScheme()
		if err := corev1.AddToScheme(scheme); err != nil {
			return nil, err
		}
		wc, err := client.New(restCfg, client.Options{Scheme: scheme})
		if err != nil {
			return nil, fmt.Errorf("build workload client for cluster %s: %w", clusterKey, err)
		}
		return wc, nil
	}
}

// workloadClientUserAgent identifies this provider's outbound workload-cluster
// calls in a workload apiserver's audit log. It is the user agent the
// clustercache-backed client used, kept so audit trails survive the change.
const workloadClientUserAgent = "cluster-api-provider-kairos-bootstrap"

// WorkerJoin runs the trust check, reuses (or, if there is none, mints) the
// worker's bootstrap token, and renders the kubeadm JoinConfiguration for a worker
// join (ADR 0010 P1).
func (s *kubeadmJoinSource) WorkerJoin(ctx context.Context, req JoinRequest) (WorkerJoinMaterial, error) {
	trusted, err := s.verifyTrustChain(ctx, req)
	if err != nil {
		return WorkerJoinMaterial{}, err
	}

	wc, err := s.workloadClient(ctx, trusted.clusterKey, trusted.kubeconfig)
	if err != nil {
		return WorkerJoinMaterial{}, newNotReady("WorkloadClusterUnreachable",
			"waiting to reach the workload cluster to mint a bootstrap token: %v", err)
	}

	// Reuse the token recorded for this KairosConfig, so every render of it carries
	// the same token. The infrastructure provider copies the first render into the
	// VM's userdata and a booted VM never re-reads the bootstrap data Secret, so a
	// re-render (the providerID regeneration, a deleted data Secret) that minted a
	// new token would leave the node holding one nothing keeps alive. Mint only when
	// nothing is recorded, or the recorded token is gone from the workload cluster:
	// a VM holding a deleted token cannot join anyway.
	var storedID, ownerUID string
	if req.Config != nil {
		storedID = req.Config.Status.BootstrapTokenID
		ownerUID = string(req.Config.UID)
	}
	token, tokenID, err := s.reuseOrMintBootstrapToken(ctx, wc, storedID, ownerUID)
	if err != nil {
		return WorkerJoinMaterial{}, err
	}

	joinCfgYAML, patchesDir, err := s.renderJoinConfiguration(req, trusted, token)
	if err != nil {
		// A render failure is terminal: it is a bad spec or version, not transient.
		return WorkerJoinMaterial{}, err
	}

	mat := &KubeadmJoinMaterial{
		JoinConfiguration: joinCfgYAML,
		KubernetesVersion: req.Machine.Spec.Version,
		PatchesDirectory:  patchesDir,
		TokenID:           tokenID,
	}
	if storedID != "" && storedID != tokenID {
		mat.ReplacedTokenID = storedID
	}
	return WorkerJoinMaterial{Kubeadm: mat}, nil
}

// reuseOrMintBootstrapToken returns the full bootstrap token for storedID, read
// back from its Secret in the workload cluster and extended in place, or mints a
// new one when storedID is empty or no longer usable. The token is never logged.
//
// With nothing recorded it first looks for a token already minted for ownerUID
// (the KairosConfig UID label). Status can lag: a reconcile that reads the
// KairosConfig from a cache that has not yet seen the status write of the render
// that minted the token would otherwise mint a second one, and the VM may have
// captured either. The workload client reads Secrets uncached, so this is live.
func (s *kubeadmJoinSource) reuseOrMintBootstrapToken(ctx context.Context, wc client.Client, storedID, ownerUID string) (token, tokenID string, err error) {
	if storedID == "" && ownerUID != "" {
		secret, tok, id, err := findOwnedBootstrapToken(ctx, wc, ownerUID)
		if err != nil {
			return "", "", newNotReady("TokenLookupFailed",
				"waiting to look up this worker's bootstrap token in the workload cluster: %v", err)
		}
		if secret != nil {
			if err := extendBootstrapToken(ctx, wc, secret, s.ttl(), s.clock()); err != nil {
				return "", "", newNotReady("TokenExtendFailed",
					"waiting to extend this worker's bootstrap token: %v", err)
			}
			return tok, id, nil
		}
	}
	if storedID != "" {
		secret, found, err := getBootstrapTokenSecret(ctx, wc, storedID)
		if err != nil {
			return "", "", newNotReady("TokenLookupFailed",
				"waiting to read the recorded bootstrap token in the workload cluster: %v", err)
		}
		if found {
			if tok, ok := bootstrapTokenFromSecret(secret, storedID); ok {
				if err := extendBootstrapToken(ctx, wc, secret, s.ttl(), s.clock()); err != nil {
					return "", "", newNotReady("TokenExtendFailed",
						"waiting to extend the recorded bootstrap token: %v", err)
				}
				return tok, storedID, nil
			}
		}
		// Gone (expired and collected, or deleted) or not a usable token Secret:
		// fall through and mint. The caller reports the replacement.
	}
	token, tokenID, err = mintBootstrapToken(ctx, wc, s.ttl(), s.clock(), ownerUID)
	if err != nil {
		return "", "", newNotReady("TokenMintFailed",
			"waiting to mint a bootstrap token in the workload cluster: %v", err)
	}
	return token, tokenID, nil
}

// findOwnedBootstrapToken returns the usable bootstrap token Secret labelled with
// ownerUID, or a nil Secret when there is none. If several exist (minted before
// this lookup existed), the one that expires last wins, by name on a tie.
func findOwnedBootstrapToken(ctx context.Context, wc client.Client, ownerUID string) (*corev1.Secret, string, string, error) {
	list := &corev1.SecretList{}
	if err := wc.List(ctx, list, client.InNamespace(metav1.NamespaceSystem),
		client.MatchingLabels{kubeadmTokenOwnerLabel: ownerUID}); err != nil {
		return nil, "", "", err
	}
	var (
		best              *corev1.Secret
		bestToken, bestID string
	)
	for i := range list.Items {
		s := &list.Items[i]
		id := string(s.Data[bootstrapapi.BootstrapTokenIDKey])
		tok, ok := bootstrapTokenFromSecret(s, id)
		if !ok || s.Name != bootstraputil.BootstrapTokenSecretName(id) {
			continue
		}
		if best == nil || tokenExpiresAfter(s, best) {
			best, bestToken, bestID = s, tok, id
		}
	}
	return best, bestToken, bestID, nil
}

// tokenExpiresAfter orders bootstrap token Secrets by expiration (a missing or
// unparseable one sorts first), then by name.
func tokenExpiresAfter(a, b *corev1.Secret) bool {
	exp := func(s *corev1.Secret) time.Time {
		t, _ := time.Parse(time.RFC3339, string(s.Data[bootstrapapi.BootstrapTokenExpirationKey]))
		return t
	}
	ea, eb := exp(a), exp(b)
	if !ea.Equal(eb) {
		return ea.After(eb)
	}
	return a.Name > b.Name
}

// trustedControlPlane is the verified trust context carried from the trust check
// into token minting and rendering.
type trustedControlPlane struct {
	clusterKey   client.ObjectKey
	endpointHost string
	endpointPort int32
	caCertHashes []string
	// kubeconfig is the verified <cluster>-kubeconfig payload. It is the only
	// thing a workload-cluster connection may be built from: it has passed the
	// owner leg and one of the two independent anchors below, so the address it
	// steers a client at is not tenant-chosen.
	kubeconfig []byte
}

// verifyTrustChain implements ADR 0010 P1 step 1 (the security core). On any
// verification failure it returns a TERMINAL error (surfaced as a failure
// condition, no token minted); on a missing-but-expected dependency it returns a
// bootstrapNotReadyError (requeue).
func (s *kubeadmJoinSource) verifyTrustChain(ctx context.Context, req JoinRequest) (trustedControlPlane, error) {
	var zero trustedControlPlane
	if req.Machine == nil {
		return zero, fmt.Errorf("kubeadm worker join requires an owning Machine")
	}
	if req.Cluster == nil {
		return zero, newNotReady("WaitingForCluster", "waiting for the owning Cluster")
	}
	// MachineDeployment workers only: refuse a MachinePool owner (ADR 0010 P1).
	if isMachinePoolOwned(req.Config) || machineIsMachinePoolOwned(req.Machine) {
		return zero, fmt.Errorf("kubeadm bootstrap does not support MachinePool: use a MachineDeployment for Kairos kubeadm workers")
	}
	// Machine.spec.version is authoritative for the kubeadm path and required.
	if req.Machine.Spec.Version == "" {
		return zero, fmt.Errorf("kubeadm worker join requires Machine.spec.version to be set (the exact Kubernetes version)")
	}

	cluster := req.Cluster

	// (a) control plane must be initialized.
	if cluster.Status.Initialization.ControlPlaneInitialized == nil || !*cluster.Status.Initialization.ControlPlaneInitialized {
		return zero, newNotReady("WaitingForControlPlaneInitialized",
			"waiting for Cluster status.initialization.controlPlaneInitialized")
	}
	// (b) a valid control-plane endpoint.
	if !cluster.Spec.ControlPlaneEndpoint.IsValid() {
		return zero, newNotReady("WaitingForControlPlaneEndpoint",
			"waiting for a valid Cluster.spec.controlPlaneEndpoint")
	}

	// Resolve + allowlist-check the control-plane reference (group AND kind).
	cpRef := cluster.Spec.ControlPlaneRef
	if cpRef.Kind == "" || cpRef.APIGroup == "" || cpRef.Name == "" {
		return zero, newNotReady("WaitingForControlPlaneRef",
			"waiting for Cluster.spec.controlPlaneRef to be populated")
	}
	cpGK := schema.GroupKind{Group: cpRef.APIGroup, Kind: cpRef.Kind}
	if !allowlistedControlPlaneKinds[cpGK] && !s.extraKinds[cpGK] {
		return zero, fmt.Errorf("control-plane kind %s/%s is not on the kubeadm trust allowlist; "+
			"only KubeadmControlPlane, KamajiControlPlane, and a kubeadm KairosControlPlane are trusted by default "+
			"(widen with the operator-level allowlist flag if you control this control-plane provider)",
			cpRef.APIGroup, cpRef.Kind)
	}

	// Resolve the control-plane object to get its UID (the trust anchor). The
	// contract resolver reads the referenced kind's CRD contract label to pick the
	// served API version, so this works for Kamaji's non-v1beta2 KamajiControlPlane (e.g. v1alpha2) as
	// well as v1beta2 kinds — matching how CAPI core resolves the same ref.
	cp, err := s.resolveCP(ctx, cpRef, cluster.Namespace)
	if err != nil {
		return zero, newNotReady("WaitingForControlPlane", "waiting to resolve control-plane object %s/%s: %v", cpRef.Kind, cpRef.Name, err)
	}
	cpUID := cp.GetUID()
	if cpUID == "" {
		return zero, newNotReady("WaitingForControlPlane", "control-plane object %s/%s has no UID yet", cpRef.Kind, cpRef.Name)
	}

	// KairosControlPlane is only trusted when its distribution is kubeadm (P2).
	if cpGK == (schema.GroupKind{Group: controlPlaneGroup, Kind: "KairosControlPlane"}) {
		dist, _, _ := unstructured.NestedString(cp.Object, "spec", "distribution")
		if dist != bootstrapv1beta2.DistributionKubeadm {
			return zero, fmt.Errorf("KairosControlPlane %s has distribution %q, not kubeadm: "+
				"a kubeadm worker cannot join a k0s or k3s control plane",
				cpRef.Name, dist)
		}
	}

	// The <cluster>-kubeconfig Secret must exist and be controller-owned by the
	// resolved control-plane object.
	kubeconfigKey := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name + "-kubeconfig"}
	kcSecret := &corev1.Secret{}
	if err := s.mgmt.Get(ctx, kubeconfigKey, kcSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return zero, newNotReady("WaitingForKubeconfig", "waiting for the %s Secret", kubeconfigKey.Name)
		}
		return zero, newNotReady("WaitingForKubeconfig", "waiting to read the %s Secret: %v", kubeconfigKey.Name, err)
	}
	owner := metav1.GetControllerOf(kcSecret)
	if owner == nil || owner.UID != cpUID {
		return zero, fmt.Errorf("the %s Secret must be owned by the cluster's control plane (%s/%s); refusing to mint a token",
			kubeconfigKey.Name, cpRef.Kind, cpRef.Name)
	}

	// The CA pin comes from the kubeconfig. Its server may legitimately be an
	// internal address (Kamaji publishes admin.svc when advertiseAddress is set),
	// so it is not required to equal the Cluster endpoint -- but see the anchor
	// check below: one of the two must hold.
	kcData, ok := kcSecret.Data[workloadKubeconfigSecretKey]
	if !ok || len(kcData) == 0 {
		return zero, newNotReady("WaitingForKubeconfig", "the %s Secret has no %q payload yet", kubeconfigKey.Name, workloadKubeconfigSecretKey)
	}
	server, caData, err := serverAndCAFromKubeconfig(kcData)
	if err != nil {
		return zero, fmt.Errorf("the %s Secret is not a valid kubeconfig: %v", kubeconfigKey.Name, err)
	}

	hashes, kubeconfigCADER, err := caCertHashesFromPEM(caData)
	if err != nil {
		return zero, fmt.Errorf("the %s Secret has an unparseable certificate-authority: %v", kubeconfigKey.Name, err)
	}
	if len(hashes) == 0 {
		return zero, fmt.Errorf("the %s Secret carries no certificate-authority-data: refusing to mint a token without a CA pin", kubeconfigKey.Name)
	}

	caKey := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name + "-ca"}
	caCrossChecked, err := crossCheckClusterCA(ctx, s.caReader, cluster, kubeconfigCADER)
	if err != nil {
		return zero, err
	}

	// Exactly one of the two independent anchors must hold, because the owner
	// reference is not one: ownerReferences are user-settable and a UID is
	// readable, so anyone who can create Secrets in this namespace can forge the
	// owner leg. That leaves the server equalling the Cluster endpoint, and the CA
	// matching a <cluster>-ca this controller read separately. Requiring neither
	// would let a planted kubeconfig steer this controller's own client, which
	// holds workload-cluster admin credentials, at any address reachable from the
	// management network. Kamaji satisfies the second anchor: it publishes
	// <cluster>-ca alongside the kubeconfig (ADR 0010 P1 step 1, F4).
	wantServer := "https://" + cluster.Spec.ControlPlaneEndpoint.String()
	if !sameServer(server, wantServer) && !caCrossChecked {
		return zero, fmt.Errorf("the %s Secret's server %q does not equal the Cluster control-plane endpoint %q, and there is no %s Secret to cross-check its CA against: refusing to mint a token",
			kubeconfigKey.Name, server, wantServer, caKey.Name)
	}

	// The kubeconfig must also survive the sanitizer (ADR 0011): the workload
	// client that mints the token is built from its rebuilt (S5) form, never the
	// raw bytes (SanitizingKubeconfigReader), so a kubeconfig the sanitizer
	// refuses would never connect. Refuse here, before any token exists, with the
	// rule that failed (never a byte of the content).
	if _, err := kubeconfig.Sanitize(kcData, cluster.Name, anchoredServer(server, wantServer)); err != nil {
		return zero, fmt.Errorf("the %s Secret is not a kubeconfig this controller will connect with: %v; refusing to mint a token",
			kubeconfigKey.Name, err)
	}

	return trustedControlPlane{
		clusterKey:   client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name},
		endpointHost: cluster.Spec.ControlPlaneEndpoint.Host,
		endpointPort: cluster.Spec.ControlPlaneEndpoint.Port,
		caCertHashes: hashes,
		kubeconfig:   kcData,
	}, nil
}

// renderJoinConfiguration overlays the controller-owned fields onto the user's
// JoinConfiguration (if any) and marshals it with the CAPI version-aware
// marshaller, returning it with the kubeadm patches directory it names. The
// minted token lands in discovery.bootstrapToken.token; it is never logged.
func (s *kubeadmJoinSource) renderJoinConfiguration(req JoinRequest, trusted trustedControlPlane, token string) (joinConfig, patchesDir string, err error) {
	var jc kubeadmv1.JoinConfiguration
	if req.Config != nil && req.Config.Spec.Kubeadm != nil && req.Config.Spec.Kubeadm.JoinConfiguration != nil {
		jc = *req.Config.Spec.Kubeadm.JoinConfiguration.DeepCopy()
	}

	// nodeRegistration.name = the Machine hostname (its Node name). Honour an
	// explicit spec value if set (the webhook validated it as DNS-1123).
	if jc.NodeRegistration.Name == "" {
		jc.NodeRegistration.Name = req.Machine.Name
	}
	// Add the uninitialized taint CAPI core removes once the Machine is linked.
	jc.NodeRegistration.Taints = appendUninitializedTaint(jc.NodeRegistration.Taints)

	var serverAddress string
	if req.Config != nil {
		serverAddress = req.Config.Spec.ServerAddress
	}
	endpoint, err := kubeadmJoinEndpoint(serverAddress, trusted)
	if err != nil {
		return "", "", err
	}

	// Bootstrap-token discovery: the minted token, the endpoint, and the CA pin.
	jc.Discovery.BootstrapToken = kubeadmv1.BootstrapTokenDiscovery{
		Token:             token,
		APIServerEndpoint: endpoint,
		CACertHashes:      trusted.caCertHashes,
	}
	// Belt-and-braces: never emit file-based discovery or unsafe skip.
	jc.Discovery.File = kubeadmv1.FileDiscovery{}
	// tlsBootstrapToken defaults to the bootstrap token but may be overridden, so
	// a spec value would become the kubelet's TLS-bootstrap credential in
	// preference to the one minted here, escaping its TTL and rotation entirely.
	// The webhook forbids it; this is the render-time half of the same rule.
	jc.Discovery.TLSBootstrapToken = ""
	// A worker never carries control-plane join material. The webhook forbids this
	// too, but KairosConfigTemplate has no webhook registered, so without this the
	// only gate would be admission on the KairosConfig CAPI core derives from it.
	jc.ControlPlane = nil
	// kubeadm refuses --patches next to --config, so the directory the
	// providerID patch lands in is carried by the configuration itself.
	if jc.Patches.Directory == "" {
		jc.Patches.Directory = defaultKubeadmPatchesDir
	}

	version, err := semver.ParseTolerant(req.Machine.Spec.Version)
	if err != nil {
		return "", "", fmt.Errorf("invalid Machine.spec.version %q: %w", req.Machine.Spec.Version, err)
	}
	out, err := kubeadmtypes.MarshalJoinConfigurationForVersion(&jc, version)
	if err != nil {
		return "", "", fmt.Errorf("marshal kubeadm JoinConfiguration: %w", err)
	}
	return out, jc.Patches.Directory, nil
}

func (s *kubeadmJoinSource) ttl() time.Duration {
	if s.tokenTTL <= 0 {
		return defaultKubeadmTokenTTL
	}
	return s.tokenTTL
}

func (s *kubeadmJoinSource) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// resolveCP resolves the control-plane object at its contract-advertised API
// version. Production uses external.GetObjectFromContractVersionedRef; tests inject
// s.resolveControlPlane so they need no installed CRD / contract labels.
func (s *kubeadmJoinSource) resolveCP(ctx context.Context, ref clusterv1.ContractVersionedObjectReference, namespace string) (*unstructured.Unstructured, error) {
	if s.resolveControlPlane != nil {
		return s.resolveControlPlane(ctx, ref, namespace)
	}
	return external.GetObjectFromContractVersionedRef(ctx, s.mgmt, ref, namespace)
}

// ExtendWorkerToken implements workerTokenRefresher (ADR 0010 P1 item 7): CABPK's
// refreshBootstrapTokenIfNeeded for a Machine. It moves the expiry of the token
// tokenID in place and never mints; the node is holding that token's value, so
// only its expiry can be moved. found is false when the token Secret is gone (or is
// not a usable token), in which case the caller re-renders and WorkerJoin mints a
// replacement.
//
// It re-runs the full trust check before touching the workload cluster rather than
// relying on the one that ran at mint time: the kubeconfig Secret or its CA can
// change between the two. Transient failures are bootstrapNotReadyError.
func (s *kubeadmJoinSource) ExtendWorkerToken(ctx context.Context, req JoinRequest, tokenID string) (bool, error) {
	if !bootstraputil.IsValidBootstrapTokenID(tokenID) {
		// Nothing usable on record: let the caller re-render, which mints.
		return false, nil
	}
	wc, err := s.trustedWorkloadClient(ctx, req, "extend")
	if err != nil {
		return false, err
	}
	secret, found, err := getBootstrapTokenSecret(ctx, wc, tokenID)
	if err != nil {
		return false, newNotReady("TokenLookupFailed", "waiting to read the bootstrap token to extend it: %v", err)
	}
	if !found {
		return false, nil
	}
	if _, ok := bootstrapTokenFromSecret(secret, tokenID); !ok {
		return false, nil
	}
	if err := extendBootstrapToken(ctx, wc, secret, s.ttl(), s.clock()); err != nil {
		return false, newNotReady("TokenExtendFailed", "waiting to extend the bootstrap token expiry: %v", err)
	}
	return true, nil
}

// RevokeWorkerToken implements workerTokenRefresher. It deletes the token Secret
// tokenID points at, after the same trust check ExtendWorkerToken runs. An
// already-absent Secret is success, so a repeat after a failed status write is
// harmless.
func (s *kubeadmJoinSource) RevokeWorkerToken(ctx context.Context, req JoinRequest, tokenID string) error {
	if !bootstraputil.IsValidBootstrapTokenID(tokenID) {
		return nil
	}
	wc, err := s.trustedWorkloadClient(ctx, req, "revoke")
	if err != nil {
		return err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: metav1.NamespaceSystem,
		Name:      bootstraputil.BootstrapTokenSecretName(tokenID),
	}}
	if err := wc.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return newNotReady("TokenRevokeFailed", "waiting to revoke the bootstrap token: %v", err)
	}
	return nil
}

// TokenRecheckInterval implements workerTokenRefresher: a third of the TTL, as
// CABPK's tokenCheckRefreshOrRotationInterval, so two consecutive checks can be
// missed before the token lapses. Floored so a tiny TTL cannot become a hot loop.
func (s *kubeadmJoinSource) TokenRecheckInterval() time.Duration {
	if d := s.ttl() / 3; d > minKubeadmTokenRecheckInterval {
		return d
	}
	return minKubeadmTokenRecheckInterval
}

// trustedWorkloadClient runs the trust check for req and returns a workload-cluster
// client for a token operation. verb names the operation in the not-ready message.
func (s *kubeadmJoinSource) trustedWorkloadClient(ctx context.Context, req JoinRequest, verb string) (client.Client, error) {
	trusted, err := s.verifyTrustChain(ctx, req)
	if err != nil {
		return nil, err
	}
	wc, err := s.workloadClient(ctx, trusted.clusterKey, trusted.kubeconfig)
	if err != nil {
		return nil, newNotReady("WorkloadClusterUnreachable",
			"waiting to reach the workload cluster to %s the bootstrap token: %v", verb, err)
	}
	return wc, nil
}

// getBootstrapTokenSecret reads the bootstrap token Secret for tokenID from the
// workload cluster's kube-system namespace. found is false when it is absent.
func getBootstrapTokenSecret(ctx context.Context, wc client.Client, tokenID string) (*corev1.Secret, bool, error) {
	if !bootstraputil.IsValidBootstrapTokenID(tokenID) {
		return nil, false, nil
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: bootstraputil.BootstrapTokenSecretName(tokenID)}
	if err := wc.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return secret, true, nil
}

// bootstrapTokenFromSecret reassembles the full "<id>.<secret>" token from a
// bootstrap token Secret, and reports false unless the Secret is a well-formed
// bootstrap token for tokenID. The result goes into rendered bootstrap data, so
// it is shape-checked here and never logged.
func bootstrapTokenFromSecret(secret *corev1.Secret, tokenID string) (string, bool) {
	if secret == nil || secret.Type != bootstrapapi.SecretTypeBootstrapToken {
		return "", false
	}
	if string(secret.Data[bootstrapapi.BootstrapTokenIDKey]) != tokenID {
		return "", false
	}
	token := bootstraputil.TokenFromIDAndSecret(tokenID, string(secret.Data[bootstrapapi.BootstrapTokenSecretKey]))
	if !bootstraputil.IsValidBootstrapToken(token) {
		return "", false
	}
	return token, true
}

// extendBootstrapToken moves the token Secret's expiry to now+ttl unless it is
// already good for more than five sixths of the TTL (CABPK's
// skipTokenRefreshIfExpiringAfter). A missing or unparseable expiration is
// rewritten rather than trusted. Only the expiration key changes: the token's ID
// and secret half are untouched.
func extendBootstrapToken(ctx context.Context, wc client.Client, secret *corev1.Secret, ttl time.Duration, now time.Time) error {
	now = now.UTC()
	if raw := secret.Data[bootstrapapi.BootstrapTokenExpirationKey]; len(raw) > 0 {
		if exp, err := time.Parse(time.RFC3339, string(raw)); err == nil && exp.After(now.Add(ttl*5/6)) {
			return nil
		}
	}
	base := secret.DeepCopy()
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[bootstrapapi.BootstrapTokenExpirationKey] = []byte(now.Add(ttl).Format(time.RFC3339))
	return wc.Patch(ctx, secret, client.MergeFrom(base))
}

// appendUninitializedTaint adds the node.cluster.x-k8s.io/uninitialized taint
// (as CABPK does) without duplicating it. A nil input becomes a one-element slice;
// an explicit empty slice is preserved as empty-plus-the-taint.
func appendUninitializedTaint(in *[]corev1.Taint) *[]corev1.Taint {
	taint := clusterv1.NodeUninitializedTaint
	var out []corev1.Taint
	if in != nil {
		out = append(out, *in...)
		for _, t := range out {
			if t.Key == taint.Key && t.Effect == taint.Effect {
				return in
			}
		}
	}
	out = append(out, taint)
	return &out
}

// mintBootstrapToken creates a short-lived bootstrap token Secret in the workload
// cluster's kube-system namespace and returns the full token and its non-secret ID.
// It mirrors CABPK's createToken (groups/usages/keys/type); the token value is
// never logged. (k8s.io/cluster-bootstrap is the canonical source and is already
// pulled in by the spec.kubeadm embedding.)
//
// ownerUID, when set, labels the Secret with the KairosConfig it was minted for, so
// a later render that has not yet seen the recorded ID finds and reuses it.
func mintBootstrapToken(ctx context.Context, wc client.Client, ttl time.Duration, now time.Time, ownerUID string) (token, tokenID string, err error) {
	token, err = bootstraputil.GenerateBootstrapToken()
	if err != nil {
		return "", "", fmt.Errorf("generate bootstrap token: %w", err)
	}
	subs := bootstraputil.BootstrapTokenRegexp.FindStringSubmatch(token)
	if len(subs) != 3 {
		return "", "", fmt.Errorf("generated bootstrap token has an unexpected shape")
	}
	tokenID = subs[1]
	tokenSecret := subs[2]

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bootstraputil.BootstrapTokenSecretName(tokenID),
			Namespace: metav1.NamespaceSystem,
		},
		Type: bootstrapapi.SecretTypeBootstrapToken,
		Data: map[string][]byte{
			bootstrapapi.BootstrapTokenIDKey:               []byte(tokenID),
			bootstrapapi.BootstrapTokenSecretKey:           []byte(tokenSecret),
			bootstrapapi.BootstrapTokenExpirationKey:       []byte(now.UTC().Add(ttl).Format(time.RFC3339)),
			bootstrapapi.BootstrapTokenUsageSigningKey:     []byte("true"),
			bootstrapapi.BootstrapTokenUsageAuthentication: []byte("true"),
			bootstrapapi.BootstrapTokenExtraGroupsKey:      []byte(kubeadmBootstrapTokenGroups),
			bootstrapapi.BootstrapTokenDescriptionKey:      []byte("token minted by cluster-api-provider-kairos (kubeadm worker join)"),
		},
	}
	if ownerUID != "" {
		secret.Labels = map[string]string{kubeadmTokenOwnerLabel: ownerUID}
	}
	if err := wc.Create(ctx, secret); err != nil {
		return "", "", fmt.Errorf("create bootstrap token Secret in workload cluster: %w", err)
	}
	return token, tokenID, nil
}

// kubeadmJoinEndpoint returns spec.serverAddress as host:port when set (the CA pin
// still authenticates it), otherwise the Cluster control-plane endpoint.
func kubeadmJoinEndpoint(serverAddress string, trusted trustedControlPlane) (string, error) {
	if serverAddress == "" {
		// APIEndpoint.String is net.JoinHostPort, so an IPv6 literal is bracketed
		// as kubeadm requires. Do not hand-format this as host:port.
		return clusterv1.APIEndpoint{Host: trusted.endpointHost, Port: trusted.endpointPort}.String(), nil
	}
	raw := serverAddress
	if !strings.Contains(raw, schemeSeparator) {
		raw = serverAddressDefaultScheme + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("invalid spec.serverAddress %q: expected host[:port] or https://host[:port]", serverAddress)
	}
	port := u.Port()
	if port == "" {
		port = strconv.Itoa(int(trusted.endpointPort))
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

// serverAndCAFromKubeconfig parses a kubeconfig and returns the current-context
// cluster's server URL and its certificate-authority-data. Both are needed: the
// CA is the pin handed to the node, the server is one of the two trust anchors.
func serverAndCAFromKubeconfig(data []byte) (server string, caData []byte, err error) {
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return "", nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	ctxName := cfg.CurrentContext
	kubeCtx, ok := cfg.Contexts[ctxName]
	if !ok || kubeCtx == nil {
		// Fall back to the sole cluster when there is exactly one.
		if len(cfg.Clusters) != 1 {
			return "", nil, fmt.Errorf("kubeconfig has no usable current-context")
		}
		for _, cl := range cfg.Clusters {
			return cl.Server, cl.CertificateAuthorityData, nil
		}
	}
	cl, ok := cfg.Clusters[kubeCtx.Cluster]
	if !ok || cl == nil {
		return "", nil, fmt.Errorf("kubeconfig context %q references an unknown cluster", ctxName)
	}
	return cl.Server, cl.CertificateAuthorityData, nil
}

// sameServer reports whether two API server URLs address the same endpoint.
//
// A byte comparison is wrong here. Cluster API writes the kubeconfig's server
// from APIEndpoint.String, which is net.JoinHostPort and therefore brackets an
// IPv6 literal, so "https://[fd00::1]:6443" and "https://fd00::1:6443" denote
// the same endpoint while differing as strings. Host comparison is also
// case-insensitive (DNS), a trailing slash is not part of the authority, and an
// absent port means the scheme's default. Getting any of these wrong refuses a
// legitimate join with a message that reads like a planted-kubeconfig incident.
func sameServer(got, want string) bool {
	g, err := url.Parse(got)
	if err != nil {
		return false
	}
	w, err := url.Parse(want)
	if err != nil {
		return false
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		return ""
	}
	return strings.EqualFold(g.Scheme, w.Scheme) &&
		strings.EqualFold(g.Hostname(), w.Hostname()) &&
		port(g) == port(w)
}

// caCertHashesFromPEM parses a PEM CA bundle and returns the kubeadm-style
// "sha256:<hex>" SPKI hashes (one per certificate) plus the parsed DER bytes.
func caCertHashesFromPEM(caPEM []byte) (hashes []string, der [][]byte, err error) {
	rest := caPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, perr := x509.ParseCertificate(block.Bytes)
		if perr != nil {
			return nil, nil, fmt.Errorf("parse CA certificate: %w", perr)
		}
		sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
		hashes = append(hashes, "sha256:"+hex.EncodeToString(sum[:]))
		der = append(der, append([]byte(nil), cert.Raw...))
	}
	if len(hashes) == 0 {
		return nil, nil, fmt.Errorf("no CERTIFICATE block found in CA data")
	}
	return hashes, der, nil
}

// derCertSetsEqual reports whether two sets of DER-encoded certificates are equal
// (order-independent), using a constant-time compare per pair.
func derCertSetsEqual(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	sortDER(a)
	sortDER(b)
	for i := range a {
		if subtle.ConstantTimeCompare(a[i], b[i]) != 1 {
			return false
		}
	}
	return true
}

func sortDER(s [][]byte) {
	sort.Slice(s, func(i, j int) bool {
		return hex.EncodeToString(s[i]) < hex.EncodeToString(s[j])
	})
}

// isMachinePoolOwned reports whether the KairosConfig is owned by a MachinePool.
func isMachinePoolOwned(kc *bootstrapv1beta2.KairosConfig) bool {
	if kc == nil {
		return false
	}
	return hasMachinePoolOwner(kc.OwnerReferences)
}

// machineIsMachinePoolOwned reports whether the Machine is part of a MachinePool.
func machineIsMachinePoolOwned(m *clusterv1.Machine) bool {
	if m == nil {
		return false
	}
	return hasMachinePoolOwner(m.OwnerReferences)
}

func hasMachinePoolOwner(refs []metav1.OwnerReference) bool {
	for _, r := range refs {
		if r.Kind == "MachinePool" {
			return true
		}
	}
	return false
}

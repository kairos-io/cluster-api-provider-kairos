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
	"sort"
	"time"

	"github.com/blang/semver/v4"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

	// kubeadmBootstrapTokenGroups is the extra-groups value CABPK sets so the token
	// authenticates as a node bootstrapper (kubeadmconfig_controller token.go).
	kubeadmBootstrapTokenGroups = "system:bootstrappers:kubeadm:default-node-token"

	// workloadKubeconfigSecretKey is the data key CAPI kubeconfig Secrets use.
	workloadKubeconfigSecretKey = "value"

	// clusterCASecretCrtKey is the certificate key in a <cluster>-ca Secret.
	clusterCASecretCrtKey = "tls.crt"
)

// allowlistedControlPlaneKinds are the control-plane kinds a kubeadm worker may
// trust WITHOUT an operator flag (ADR 0010 OQ-9), matched on group and kind.
// KairosControlPlane is included but additionally requires spec.distribution ==
// kubeadm (a kubeadm KairosControlPlane is P2, so in P1 this never matches — a
// k0s/k3s KairosControlPlane is refused, per the ADR interop matrix).
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
// clusterKey. The production implementation is clustercache-backed with a
// ClusterFilter, so it only ever connects to clusters that pass the coarse
// allowlist gate; tests inject a fake.
type workloadClientFactory func(ctx context.Context, clusterKey client.ObjectKey) (client.Client, error)

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
	// v1alpha1 KamajiControlPlane resolves correctly, not only v1beta2 kinds). Nil
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

// KubeadmTrustedClusterFilter returns a clustercache ClusterFilter that admits only
// clusters whose control-plane kind is on the kubeadm trust allowlist (built-in
// kinds plus the operator-level extraKinds). It is the COARSE gate so the bootstrap
// manager's workload-client cache never connects to a k0s/k3s cluster; the FINE,
// owner-verified trust check still runs per worker in WorkerJoin.
func KubeadmTrustedClusterFilter(extraKinds map[schema.GroupKind]bool) func(*clusterv1.Cluster) bool {
	return func(cluster *clusterv1.Cluster) bool {
		if cluster == nil {
			return false
		}
		gk := schema.GroupKind{Group: cluster.Spec.ControlPlaneRef.APIGroup, Kind: cluster.Spec.ControlPlaneRef.Kind}
		return allowlistedControlPlaneKinds[gk] || extraKinds[gk]
	}
}

// WorkerJoin runs the trust check, mints a bootstrap token, and renders the
// kubeadm JoinConfiguration for a worker join (ADR 0010 P1).
func (s *kubeadmJoinSource) WorkerJoin(ctx context.Context, req JoinRequest) (WorkerJoinMaterial, error) {
	trusted, err := s.verifyTrustChain(ctx, req)
	if err != nil {
		return WorkerJoinMaterial{}, err
	}

	// Mint a short-lived bootstrap token in the WORKLOAD cluster.
	wc, err := s.workloadClient(ctx, trusted.clusterKey)
	if err != nil {
		return WorkerJoinMaterial{}, newNotReady("WorkloadClusterUnreachable",
			"waiting to reach the workload cluster to mint a bootstrap token: %v", err)
	}
	token, tokenID, err := mintBootstrapToken(ctx, wc, s.ttl(), s.clock())
	if err != nil {
		return WorkerJoinMaterial{}, newNotReady("TokenMintFailed",
			"waiting to mint a bootstrap token in the workload cluster: %v", err)
	}

	joinCfgYAML, err := s.renderJoinConfiguration(req, trusted, token)
	if err != nil {
		// A render failure is terminal: it is a bad spec or version, not transient.
		return WorkerJoinMaterial{}, err
	}

	return WorkerJoinMaterial{Kubeadm: &KubeadmJoinMaterial{
		JoinConfiguration: joinCfgYAML,
		KubernetesVersion: req.Machine.Spec.Version,
		TokenID:           tokenID,
	}}, nil
}

// trustedControlPlane is the verified trust context carried from the trust check
// into token minting and rendering.
type trustedControlPlane struct {
	clusterKey   client.ObjectKey
	endpointHost string
	endpointPort int32
	caCertHashes []string
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
	// served API version, so this works for Kamaji's v1alpha1 KamajiControlPlane as
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
				"a kubeadm worker cannot join a k0s/k3s control plane (ADR 0010); a kubeadm KairosControlPlane is a later phase",
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
		return zero, fmt.Errorf("the %s Secret is not controller-owned by the cluster's control-plane object %s/%s: "+
			"refusing to mint a token (a node-pushed or planted kubeconfig is not trusted on the kubeadm path)",
			kubeconfigKey.Name, cpRef.Kind, cpRef.Name)
	}

	// Parse the kubeconfig: the server must equal the Cluster endpoint, and the CA
	// pin is taken from its certificate-authority-data.
	kcData, ok := kcSecret.Data[workloadKubeconfigSecretKey]
	if !ok || len(kcData) == 0 {
		return zero, newNotReady("WaitingForKubeconfig", "the %s Secret has no %q payload yet", kubeconfigKey.Name, workloadKubeconfigSecretKey)
	}
	server, caData, err := serverAndCAFromKubeconfig(kcData)
	if err != nil {
		return zero, fmt.Errorf("the %s Secret is not a valid kubeconfig: %v", kubeconfigKey.Name, err)
	}
	wantServer := fmt.Sprintf("https://%s:%d", cluster.Spec.ControlPlaneEndpoint.Host, cluster.Spec.ControlPlaneEndpoint.Port)
	if server != wantServer {
		return zero, fmt.Errorf("the %s Secret's server %q does not equal the Cluster control-plane endpoint %q: refusing to mint a token",
			kubeconfigKey.Name, server, wantServer)
	}

	hashes, kubeconfigCADER, err := caCertHashesFromPEM(caData)
	if err != nil {
		return zero, fmt.Errorf("the %s Secret has an unparseable certificate-authority: %v", kubeconfigKey.Name, err)
	}
	if len(hashes) == 0 {
		return zero, fmt.Errorf("the %s Secret carries no certificate-authority-data: refusing to mint a token without a CA pin", kubeconfigKey.Name)
	}

	// Cross-check against <cluster>-ca if it exists (UNCACHED read, cert only). Do
	// NOT require the Secret to exist or to be owned: Kamaji's default setup and an
	// upstream-KCP user-supplied CA legitimately have no CAPI controller owner
	// (ADR 0010 P1 step 1).
	caKey := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name + "-ca"}
	caSecret := &corev1.Secret{}
	if err := s.caReader.Get(ctx, caKey, caSecret); err == nil {
		crt, ok := caSecret.Data[clusterCASecretCrtKey]
		if !ok || len(crt) == 0 {
			return zero, fmt.Errorf("the %s Secret has no %q: cannot cross-check the CA pin", caKey.Name, clusterCASecretCrtKey)
		}
		_, caDER, perr := caCertHashesFromPEM(crt)
		if perr != nil {
			return zero, fmt.Errorf("the %s Secret has an unparseable %q: %v", caKey.Name, clusterCASecretCrtKey, perr)
		}
		if !derCertSetsEqual(kubeconfigCADER, caDER) {
			return zero, fmt.Errorf("the kubeconfig CA does not match the %s Secret's %q: refusing to mint a token (possible planted Secret)",
				caKey.Name, clusterCASecretCrtKey)
		}
	} else if !apierrors.IsNotFound(err) {
		return zero, newNotReady("WaitingForClusterCA", "waiting to cross-check the %s Secret: %v", caKey.Name, err)
	}

	return trustedControlPlane{
		clusterKey:   client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name},
		endpointHost: cluster.Spec.ControlPlaneEndpoint.Host,
		endpointPort: cluster.Spec.ControlPlaneEndpoint.Port,
		caCertHashes: hashes,
	}, nil
}

// renderJoinConfiguration overlays the controller-owned fields onto the user's
// JoinConfiguration (if any) and marshals it with the CAPI version-aware
// marshaller. The minted token lands in discovery.bootstrapToken.token; it is
// never logged.
func (s *kubeadmJoinSource) renderJoinConfiguration(req JoinRequest, trusted trustedControlPlane, token string) (string, error) {
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

	// Bootstrap-token discovery: the minted token, the endpoint, and the CA pin.
	jc.Discovery.BootstrapToken = kubeadmv1.BootstrapTokenDiscovery{
		Token:             token,
		APIServerEndpoint: fmt.Sprintf("%s:%d", trusted.endpointHost, trusted.endpointPort),
		CACertHashes:      trusted.caCertHashes,
	}
	// Belt-and-braces: never emit file-based discovery or unsafe skip.
	jc.Discovery.File = kubeadmv1.FileDiscovery{}

	version, err := semver.ParseTolerant(req.Machine.Spec.Version)
	if err != nil {
		return "", fmt.Errorf("invalid Machine.spec.version %q: %w", req.Machine.Spec.Version, err)
	}
	out, err := kubeadmtypes.MarshalJoinConfigurationForVersion(&jc, version)
	if err != nil {
		return "", fmt.Errorf("marshal kubeadm JoinConfiguration: %w", err)
	}
	return out, nil
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

// TokenNeedsRefresh implements workerTokenRefresher (ADR 0010 P1 item 7). It is
// READ-ONLY and is only ever reached AFTER a prior successful WorkerJoin: the
// reconciler calls it with the non-secret tokenID persisted in status, which is set
// only once the full trust check passed and a token was minted. It mints nothing and
// re-verifies no trust itself; the coarse clustercache ClusterFilter still gates the
// workload client it uses, and the next regeneration re-runs the full trust check. It
// reads the token Secret the ID points at and rotates when the token is gone or past
// half its TTL — CABPK's shouldRotate, keyed by the stored ID so we never need the
// secret half. Transient workload-cluster errors are returned as bootstrapNotReadyError.
func (s *kubeadmJoinSource) TokenNeedsRefresh(ctx context.Context, req JoinRequest, tokenID string) (bool, error) {
	if tokenID == "" {
		// No token on record (never minted, or lost the ID): (re)mint.
		return true, nil
	}
	if req.Cluster == nil {
		return false, newNotReady("WaitingForCluster", "waiting for the owning Cluster to check the bootstrap token")
	}
	clusterKey := client.ObjectKey{Namespace: req.Cluster.Namespace, Name: req.Cluster.Name}
	wc, err := s.workloadClient(ctx, clusterKey)
	if err != nil {
		return false, newNotReady("WorkloadClusterUnreachable", "waiting to reach the workload cluster to check the bootstrap token: %v", err)
	}
	expiry, found, err := bootstrapTokenExpiry(ctx, wc, tokenID)
	if err != nil {
		return false, newNotReady("TokenLookupFailed", "waiting to read the bootstrap token status: %v", err)
	}
	if !found {
		// Expired and garbage-collected, or deleted: re-mint.
		return true, nil
	}
	return expiry.Before(s.clock().Add(s.ttl() / 2)), nil
}

// bootstrapTokenExpiry returns the expiry of the bootstrap token Secret for tokenID
// in the workload cluster. found is false when the Secret is absent. A token Secret
// with no expiration key is treated as non-expiring (far-future expiry).
func bootstrapTokenExpiry(ctx context.Context, wc client.Client, tokenID string) (time.Time, bool, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: bootstraputil.BootstrapTokenSecretName(tokenID)}
	if err := wc.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	raw, ok := secret.Data[bootstrapapi.BootstrapTokenExpirationKey]
	if !ok || len(raw) == 0 {
		return time.Now().Add(100 * 365 * 24 * time.Hour), true, nil
	}
	t, err := time.Parse(time.RFC3339, string(raw))
	if err != nil {
		return time.Time{}, true, fmt.Errorf("parse token expiration: %w", err)
	}
	return t, true, nil
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
func mintBootstrapToken(ctx context.Context, wc client.Client, ttl time.Duration, now time.Time) (token, tokenID string, err error) {
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
	if err := wc.Create(ctx, secret); err != nil {
		return "", "", fmt.Errorf("create bootstrap token Secret in workload cluster: %w", err)
	}
	return token, tokenID, nil
}

// serverAndCAFromKubeconfig parses a kubeconfig and returns the current-context
// cluster's server URL and its certificate-authority-data.
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

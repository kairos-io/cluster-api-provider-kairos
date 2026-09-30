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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/kubeconfig"
)

const testExpectedServer = "https://1.2.3.4:6443"

// noDeleteClient wraps a client.Client and fails the test if any Delete is
// issued. The whole point of ADR 0011's empty-in-place neutralization is that
// the KCP publish path never deletes a Secret (no secrets:delete grant), so
// every publish-path test runs against this wrapper.
type noDeleteClient struct {
	client.Client
	t *testing.T
}

func (c *noDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.t.Errorf("unexpected Delete on %T %q — the KCP publish path must never delete a Secret", obj, obj.GetName())
	return nil
}

func (c *noDeleteClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	c.t.Errorf("unexpected DeleteAllOf on %T — the KCP publish path must never delete", obj)
	return nil
}

// --- crypto + kubeconfig fixtures --------------------------------------------

func genCACertPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func genClientCertKeyPEM(t *testing.T, notBefore, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen client key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano() + 1),
		Subject:      pkix.Name{CommonName: "test-admin"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create client cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// rawKubeconfig builds an unsanitized kubeconfig with the given CA and user
// credentials. Names are non-canonical so a caller can prove the sanitizer
// rebuilds them.
func rawKubeconfig(caPEM []byte, user *clientcmdapi.AuthInfo) []byte {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["input-cluster"] = &clientcmdapi.Cluster{
		Server:                   "https://127.0.0.1:6443",
		CertificateAuthorityData: caPEM,
	}
	cfg.AuthInfos["input-user"] = user
	cfg.Contexts["input-ctx"] = &clientcmdapi.Context{Cluster: "input-cluster", AuthInfo: "input-user"}
	cfg.CurrentContext = "input-ctx"
	out, _ := clientcmd.Write(*cfg)
	return out
}

// newValidKubeconfig returns S5 (sanitized) bytes for a fresh, unexpired
// cert-authenticated admin kubeconfig — a fixed point of the sanitizer for
// (clusterName, server).
func newValidKubeconfig(t *testing.T, clusterName, server string) []byte {
	t.Helper()
	ca := genCACertPEM(t)
	cert, key := genClientCertKeyPEM(t, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	raw := rawKubeconfig(ca, &clientcmdapi.AuthInfo{ClientCertificateData: cert, ClientKeyData: key})
	out, err := kubeconfig.Sanitize(raw, clusterName, server)
	if err != nil {
		t.Fatalf("sanitize valid fixture: %v", err)
	}
	return out
}

// newExpiredKubeconfig returns S5 bytes whose client certificate is already
// expired (structure clean; only S3 expiry fails at publish time).
func newExpiredKubeconfig(t *testing.T, clusterName, server string) []byte {
	t.Helper()
	ca := genCACertPEM(t)
	cert, key := genClientCertKeyPEM(t, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	raw := rawKubeconfig(ca, &clientcmdapi.AuthInfo{ClientCertificateData: cert, ClientKeyData: key})
	out, err := kubeconfig.Sanitize(raw, clusterName, server)
	if err != nil {
		t.Fatalf("sanitize expired fixture: %v", err)
	}
	return out
}

// newTokenFileKubeconfig returns RAW (unsanitized) bytes carrying an unsafe
// tokenFile — the A1 attack shape (client-go reads the file and sends it as the
// bearer token). Sanitize rejects it as UnsafeIdentity.
func newTokenFileKubeconfig(t *testing.T) []byte {
	t.Helper()
	ca := genCACertPEM(t)
	return rawKubeconfig(ca, &clientcmdapi.AuthInfo{TokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token"})
}

// TestCAFingerprint verifies the pin is deterministic and CA-dependent.
func TestCAFingerprint(t *testing.T) {
	g := NewWithT(t)
	kc1 := newValidKubeconfig(t, "c", testExpectedServer)
	kc2 := newValidKubeconfig(t, "c", testExpectedServer) // different CA (fresh gen)

	fp1a, err := caFingerprint(kc1)
	g.Expect(err).NotTo(HaveOccurred())
	fp1b, err := caFingerprint(kc1)
	g.Expect(err).NotTo(HaveOccurred())
	fp2, err := caFingerprint(kc2)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(fp1a).To(Equal(fp1b), "fingerprint must be deterministic")
	g.Expect(fp1a).NotTo(Equal(fp2), "a different CA must produce a different fingerprint")
	g.Expect(fp1a).To(HaveLen(64), "SHA-256 hex is 64 chars")
}

// TestNeutralizesContent pins the reason→action classification: unsafe/oversized/
// unparseable neutralize; structure-only/credential-shape-only do not.
func TestNeutralizesContent(t *testing.T) {
	g := NewWithT(t)
	cases := []struct {
		reason kubeconfig.RejectionReason
		want   bool
	}{
		{kubeconfig.ReasonOversized, true},
		{kubeconfig.ReasonUnparseable, true},
		{kubeconfig.ReasonUnsafeCluster, true},
		{kubeconfig.ReasonUnsafeIdentity, true},
		{kubeconfig.ReasonStructure, false},
		{kubeconfig.ReasonCredentials, false},
	}
	for _, tc := range cases {
		err := &kubeconfig.RejectedError{Reason: tc.reason, Message: "x"}
		g.Expect(neutralizesContent(err)).To(Equal(tc.want), "reason %s", tc.reason)
	}
}

// TestContractAdoptable covers the ADR 0011 adoption criteria a-c.
func TestContractAdoptable(t *testing.T) {
	g := NewWithT(t)
	clusterCreated := metav1.NewTime(time.Now())
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name: "c", Namespace: "ns", CreationTimestamp: clusterCreated,
	}}
	base := func() *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:              "c-kubeconfig",
			Namespace:         "ns",
			CreationTimestamp: metav1.NewTime(clusterCreated.Add(time.Minute)),
			Labels:            map[string]string{clusterv1.ClusterNameLabel: "c"},
		}}
	}
	g.Expect(contractAdoptable(base(), cluster)).To(BeTrue(), "clean node-created Secret is adoptable")

	// (a) a controller owner disqualifies.
	withCtl := base()
	ctlTrue := true
	withCtl.OwnerReferences = []metav1.OwnerReference{{APIVersion: "x/v1", Kind: "X", Name: "x", UID: "u", Controller: &ctlTrue}}
	g.Expect(contractAdoptable(withCtl, cluster)).To(BeFalse(), "a controller owner disqualifies adoption")

	// (b) older than the Cluster disqualifies (leftover from deleted same-name cluster).
	older := base()
	older.CreationTimestamp = metav1.NewTime(clusterCreated.Add(-time.Minute))
	g.Expect(contractAdoptable(older, cluster)).To(BeFalse(), "a Secret older than the Cluster is not adoptable")

	// (c) wrong/missing cluster-name label disqualifies.
	mislabeled := base()
	mislabeled.Labels = map[string]string{clusterv1.ClusterNameLabel: "other"}
	g.Expect(contractAdoptable(mislabeled, cluster)).To(BeFalse(), "wrong cluster-name label is not adoptable")
}

// TestClusterControlPlaneRefMatches guards the authoritative Cluster->KCP check.
func TestClusterControlPlaneRefMatches(t *testing.T) {
	g := NewWithT(t)
	kcp := &controlplanev1beta2.KairosControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "kcp"}}
	match := &clusterv1.Cluster{Spec: clusterv1.ClusterSpec{ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
		APIGroup: controlplanev1beta2.GroupVersion.Group, Kind: "KairosControlPlane", Name: "kcp",
	}}}
	g.Expect(clusterControlPlaneRefMatches(match, kcp)).To(BeTrue())

	wrongName := match.DeepCopy()
	wrongName.Spec.ControlPlaneRef.Name = "other"
	g.Expect(clusterControlPlaneRefMatches(wrongName, kcp)).To(BeFalse())

	wrongKind := match.DeepCopy()
	wrongKind.Spec.ControlPlaneRef.Kind = "KubeadmControlPlane"
	g.Expect(clusterControlPlaneRefMatches(wrongKind, kcp)).To(BeFalse())

	none := &clusterv1.Cluster{}
	g.Expect(clusterControlPlaneRefMatches(none, kcp)).To(BeFalse())
}

// --- reconcileKubeconfig orchestration (fake client) -------------------------

func publishScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	g := NewWithT(t)
	s := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(s)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(s)).To(Succeed())
	g.Expect(controlplanev1beta2.AddToScheme(s)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(s)).To(Succeed())
	return s
}

func publishKCPAndCluster() (*controlplanev1beta2.KairosControlPlane, *clusterv1.Cluster) {
	kcp := &controlplanev1beta2.KairosControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "kcp", Namespace: "ns", UID: types.UID("kcp-uid")},
	}
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: types.UID("cluster-uid")},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: controlplanev1beta2.GroupVersion.Group, Kind: "KairosControlPlane", Name: "kcp",
			},
			ControlPlaneEndpoint: clusterv1.APIEndpoint{Host: "1.2.3.4", Port: 6443},
		},
	}
	return kcp, cluster
}

func buildReconciler(t *testing.T, objs ...client.Object) (*KairosControlPlaneReconciler, *noDeleteClient) {
	t.Helper()
	s := publishScheme(t)
	fakeC := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	nd := &noDeleteClient{Client: fakeC, t: t}
	return &KairosControlPlaneReconciler{Client: nd, Scheme: s}, nd
}

func getSecret(t *testing.T, c client.Client, ns, name string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, s); err != nil {
		t.Fatalf("get secret %s/%s: %v", ns, name, err)
	}
	return s
}

func kubeconfigReason(kcp *controlplanev1beta2.KairosControlPlane) string {
	c := conditions.Get(kcp, controlplanev1beta2.KubeconfigReadyCondition)
	if c == nil {
		return ""
	}
	return c.Reason
}

// TestReconcileKubeconfig_AdoptsCleanLegacyContract: a clean, node-created
// contract Secret is adopted (KCP-owned, source=adopted), the CA is pinned, and
// KubeconfigReady=True. No delete.
func TestReconcileKubeconfig_AdoptsCleanLegacyContract(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	valid := newValidKubeconfig(t, cluster.Name, testExpectedServer)
	contract := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig", Namespace: "ns",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c"},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": valid},
	}
	r, nd := buildReconciler(t, kcp, cluster, contract)

	ready, err := r.reconcileKubeconfig(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
	g.Expect(kubeconfigReason(kcp)).To(Equal(controlplanev1beta2.KubeconfigReadyReason))

	got := getSecret(t, nd, "ns", "c-kubeconfig")
	g.Expect(metav1.IsControlledBy(got, kcp)).To(BeTrue(), "adoption must set the KCP as controller owner")
	g.Expect(got.Annotations[KubeconfigSourceAnnotation]).To(Equal(KubeconfigSourceAdopted))
	g.Expect(got.Data["value"]).To(Equal(valid), "content is already S5; adoption keeps it byte-identical")
	g.Expect(kcp.Annotations[KubeconfigCAPinAnnotation]).NotTo(BeEmpty(), "first accept pins the CA")
}

// TestReconcileKubeconfig_NeutralizesUnsafeContract: a tokenFile push is emptied
// in place (object retained, value cleared) with KubeconfigReady=WaitingForNodePush.
// No delete.
func TestReconcileKubeconfig_NeutralizesUnsafeContract(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	unsafe := newTokenFileKubeconfig(t)
	contract := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig", Namespace: "ns",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c"},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": unsafe},
	}
	r, nd := buildReconciler(t, kcp, cluster, contract)

	ready, err := r.reconcileKubeconfig(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
	g.Expect(kubeconfigReason(kcp)).To(Equal(controlplanev1beta2.WaitingForNodePushReason))

	got := getSecret(t, nd, "ns", "c-kubeconfig")
	g.Expect(got.Data["value"]).To(BeEmpty(), "unsafe content must be emptied in place")
	g.Expect(kcp.Annotations[KubeconfigCAPinAnnotation]).To(BeEmpty(), "an unsafe push must not set a pin")
}

// TestReconcileKubeconfig_NeutralizesOversizedContract: content over the 64 KiB
// S0 cap is emptied in place. No delete.
func TestReconcileKubeconfig_NeutralizesOversizedContract(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	oversized := make([]byte, kubeconfig.MaxSize+1)
	for i := range oversized {
		oversized[i] = 'a'
	}
	contract := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig", Namespace: "ns",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c"},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": oversized},
	}
	r, nd := buildReconciler(t, kcp, cluster, contract)

	ready, err := r.reconcileKubeconfig(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
	g.Expect(kubeconfigReason(kcp)).To(Equal(controlplanev1beta2.WaitingForNodePushReason))
	got := getSecret(t, nd, "ns", "c-kubeconfig")
	g.Expect(got.Data["value"]).To(BeEmpty())
}

// TestReconcileKubeconfig_ExpiredCertConditionOnly: an expired but otherwise
// clean, owned contract is NOT emptied — condition only. No delete.
func TestReconcileKubeconfig_ExpiredCertConditionOnly(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	expired := newExpiredKubeconfig(t, cluster.Name, testExpectedServer)
	contract := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig", Namespace: "ns",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c"},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": expired},
	}
	// Owned by the KCP so this is a steady-state fixed-point check.
	g.Expect(controllerRefTo(kcp, contract, r0Scheme(t))).To(Succeed())
	r, nd := buildReconciler(t, kcp, cluster, contract)

	ready, err := r.reconcileKubeconfig(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
	g.Expect(kubeconfigReason(kcp)).To(Equal(controlplanev1beta2.KubeconfigCertificateExpiredReason))
	got := getSecret(t, nd, "ns", "c-kubeconfig")
	g.Expect(got.Data["value"]).To(Equal(expired), "an expired cert must never empty the kubeconfig")
}

// TestReconcileKubeconfig_CAMismatchNeutralizes: an owned contract whose CA does
// not match the stored pin is emptied with NodePushCAMismatch; the pin is kept.
// No delete.
func TestReconcileKubeconfig_CAMismatchNeutralizes(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	// Pin CA-A.
	pinnedKC := newValidKubeconfig(t, cluster.Name, testExpectedServer)
	pinA, err := caFingerprint(pinnedKC)
	g.Expect(err).NotTo(HaveOccurred())
	kcp.Annotations = map[string]string{KubeconfigCAPinAnnotation: pinA}

	// Contract holds valid content but a DIFFERENT CA (CA-B), KCP-owned.
	otherCA := newValidKubeconfig(t, cluster.Name, testExpectedServer)
	contract := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig", Namespace: "ns",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c"},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": otherCA},
	}
	g.Expect(controllerRefTo(kcp, contract, r0Scheme(t))).To(Succeed())
	r, nd := buildReconciler(t, kcp, cluster, contract)

	ready, err := r.reconcileKubeconfig(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
	g.Expect(kubeconfigReason(kcp)).To(Equal(controlplanev1beta2.NodePushCAMismatchReason))
	got := getSecret(t, nd, "ns", "c-kubeconfig")
	g.Expect(got.Data["value"]).To(BeEmpty(), "a CA mismatch neutralizes the contract")
	g.Expect(kcp.Annotations[KubeconfigCAPinAnnotation]).To(Equal(pinA), "the pin survives neutralization")
}

// TestReconcileKubeconfig_NotOwnedNotAdoptable_AuthoritativeEmptied is the SF1
// (option b) flip of the former leave-untouched behaviour: a same-name Secret
// older than the Cluster (a leftover, not adoptable) is now EMPTIED in place with
// WaitingForNodePush because the Cluster controlPlaneRef points at this KCP (the
// fixture's controlPlaneRef already does). No staging exists, so it stays empty
// pending recovery. No delete, no owner change.
func TestReconcileKubeconfig_NotOwnedNotAdoptable_AuthoritativeEmptied(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	cluster.CreationTimestamp = metav1.NewTime(time.Now())
	valid := newValidKubeconfig(t, cluster.Name, testExpectedServer)
	contract := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig", Namespace: "ns",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)), // older than Cluster
			Labels:            map[string]string{clusterv1.ClusterNameLabel: "c"},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": valid},
	}
	r, nd := buildReconciler(t, kcp, cluster, contract)

	ready, err := r.reconcileKubeconfig(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
	g.Expect(kubeconfigReason(kcp)).To(Equal(controlplanev1beta2.WaitingForNodePushReason))
	got := getSecret(t, nd, "ns", "c-kubeconfig")
	g.Expect(got.Data["value"]).To(BeEmpty(), "SF1: an authoritative not-owned/not-adoptable Secret is emptied in place")
	g.Expect(metav1.IsControlledBy(got, kcp)).To(BeFalse(), "neutralize does not take ownership")
}

// TestReconcileKubeconfig_NotOwnedNotAdoptable_MismatchUntouched is the SF1
// sibling: with a controlPlaneRef that does NOT point at this KCP, the same
// leftover Secret is left untouched with KubeconfigSecretNotOwned. No delete.
func TestReconcileKubeconfig_NotOwnedNotAdoptable_MismatchUntouched(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	cluster.CreationTimestamp = metav1.NewTime(time.Now())
	// Point the Cluster controlPlaneRef at a DIFFERENT control plane.
	cluster.Spec.ControlPlaneRef.Name = "some-other-kcp"
	valid := newValidKubeconfig(t, cluster.Name, testExpectedServer)
	contract := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig", Namespace: "ns",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)), // older than Cluster
			Labels:            map[string]string{clusterv1.ClusterNameLabel: "c"},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": valid},
	}
	r, nd := buildReconciler(t, kcp, cluster, contract)

	ready, err := r.reconcileKubeconfig(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
	g.Expect(kubeconfigReason(kcp)).To(Equal(controlplanev1beta2.KubeconfigSecretNotOwnedReason))
	got := getSecret(t, nd, "ns", "c-kubeconfig")
	g.Expect(got.Data["value"]).To(Equal(valid), "controlPlaneRef mismatch leaves the Secret untouched")
	g.Expect(metav1.IsControlledBy(got, kcp)).To(BeFalse())
}

// TestReconcileKubeconfig_SF1_ForgedOwnerPlantEmptiedAndRepublished: a tokenFile
// plant carrying a FORGED controller ownerRef (so it is neither KCP-owned nor
// adoptable) is emptied in place and then republished from clean staging when the
// Cluster controlPlaneRef points at this KCP. The forged owner is dropped and the
// KCP takes ownership. No delete.
func TestReconcileKubeconfig_SF1_ForgedOwnerPlantEmptiedAndRepublished(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	// A plant with unsafe content (tokenFile — the A1 shape) AND a forged
	// controller ownerRef that is not this KCP.
	ctlTrue := true
	plant := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig", Namespace: "ns",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       "kcp",
				UID:        types.UID("FORGED-uid"), // not kcp.UID
				Controller: &ctlTrue,
			}},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": newTokenFileKubeconfig(t)},
	}
	// Clean staging so the SF1 branch republishes after emptying.
	valid := newValidKubeconfig(t, cluster.Name, testExpectedServer)
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig-push", Namespace: "ns",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"value": valid},
	}
	g.Expect(controllerRefTo(kcp, staging, r0Scheme(t))).To(Succeed())
	r, nd := buildReconciler(t, kcp, cluster, plant, staging)

	ready, err := r.reconcileKubeconfig(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue(), "SF1: after emptying the forged plant, clean staging republishes")
	g.Expect(kubeconfigReason(kcp)).To(Equal(controlplanev1beta2.KubeconfigReadyReason))
	got := getSecret(t, nd, "ns", "c-kubeconfig")
	g.Expect(got.Data["value"]).To(Equal(valid), "the forged plant is replaced by clean staging content")
	g.Expect(metav1.IsControlledBy(got, kcp)).To(BeTrue(), "the KCP takes ownership, dropping the forged owner")
	owner := metav1.GetControllerOfNoCopy(got)
	g.Expect(owner).NotTo(BeNil())
	g.Expect(owner.UID).To(Equal(kcp.UID))
}

// TestReconcileKubeconfig_SF1_PreClusterPlantEmptiedAndRepublished: a clean
// Secret with a creationTimestamp before the Cluster (not adoptable by rule b) is
// emptied and republished from clean staging when authoritative. No delete.
func TestReconcileKubeconfig_SF1_PreClusterPlantEmptiedAndRepublished(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	cluster.CreationTimestamp = metav1.NewTime(time.Now())
	// A clean (S5) contract but older than the Cluster: not adoptable (rule b),
	// no controller owner.
	preexisting := newValidKubeconfig(t, cluster.Name, testExpectedServer)
	contract := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig", Namespace: "ns",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
			Labels:            map[string]string{clusterv1.ClusterNameLabel: "c"},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": preexisting},
	}
	fresh := newValidKubeconfig(t, cluster.Name, testExpectedServer)
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig-push", Namespace: "ns",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"value": fresh},
	}
	g.Expect(controllerRefTo(kcp, staging, r0Scheme(t))).To(Succeed())
	r, nd := buildReconciler(t, kcp, cluster, contract, staging)

	ready, err := r.reconcileKubeconfig(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
	g.Expect(kubeconfigReason(kcp)).To(Equal(controlplanev1beta2.KubeconfigReadyReason))
	got := getSecret(t, nd, "ns", "c-kubeconfig")
	g.Expect(got.Data["value"]).To(Equal(fresh), "the pre-Cluster leftover is emptied then republished from staging")
	g.Expect(metav1.IsControlledBy(got, kcp)).To(BeTrue())
}

// TestReconcileKubeconfig_PublishesFromStaging: no contract yet; a KCP-owned
// staging Secret carries a valid candidate → published to the contract as
// node-push, pinned, True.
func TestReconcileKubeconfig_PublishesFromStaging(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	valid := newValidKubeconfig(t, cluster.Name, testExpectedServer)
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig-push", Namespace: "ns",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"value": valid},
	}
	g.Expect(controllerRefTo(kcp, staging, r0Scheme(t))).To(Succeed())
	r, nd := buildReconciler(t, kcp, cluster, staging)

	ready, err := r.reconcileKubeconfig(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
	g.Expect(kubeconfigReason(kcp)).To(Equal(controlplanev1beta2.KubeconfigReadyReason))

	got := getSecret(t, nd, "ns", "c-kubeconfig")
	g.Expect(got.Data["value"]).To(Equal(valid))
	g.Expect(metav1.IsControlledBy(got, kcp)).To(BeTrue())
	g.Expect(got.Annotations[KubeconfigSourceAnnotation]).To(Equal(KubeconfigSourceNodePush))
}

// TestReconcileKubeconfig_ForgedStagingOwnerRejected: a node-planted staging
// Secret with a forged (non-KCP) controller owner is never used as a candidate,
// and its content never reaches the contract. No delete.
func TestReconcileKubeconfig_ForgedStagingOwnerRejected(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	valid := newValidKubeconfig(t, cluster.Name, testExpectedServer)
	ctlTrue := true
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig-push", Namespace: "ns",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: controlplanev1beta2.GroupVersion.String(),
				Kind:       "KairosControlPlane",
				Name:       "kcp",
				UID:        types.UID("FORGED-uid"), // not kcp.UID
				Controller: &ctlTrue,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"value": valid},
	}
	r, nd := buildReconciler(t, kcp, cluster, staging)

	ready, err := r.reconcileKubeconfig(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "forged staging must not be published")

	// The contract Secret must not have been created from forged staging content.
	contract := &corev1.Secret{}
	err = nd.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "c-kubeconfig"}, contract)
	if err == nil {
		g.Expect(contract.Data["value"]).To(BeEmpty(), "forged staging content must never reach the contract")
	}
}

// TestReconcileKubeconfig_SSHFallbackSourcePreserved: a contract Secret written
// by the SSH worker (Cluster non-controller owner, source=ssh-fallback) is
// adopted, its source preserved, and the Reason routes to ViaSSHFallback.
func TestReconcileKubeconfig_SSHFallbackSourcePreserved(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	valid := newValidKubeconfig(t, cluster.Name, testExpectedServer)
	ctlFalse := false
	contract := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig", Namespace: "ns",
			Labels:      map[string]string{clusterv1.ClusterNameLabel: "c"},
			Annotations: map[string]string{KubeconfigSourceAnnotation: KubeconfigSourceSSHFallback},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "Cluster",
				Name:       "c",
				UID:        cluster.UID,
				Controller: &ctlFalse, // non-controller owner
			}},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": valid},
	}
	r, nd := buildReconciler(t, kcp, cluster, contract)

	ready, err := r.reconcileKubeconfig(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
	g.Expect(kubeconfigReason(kcp)).To(Equal(controlplanev1beta2.KubeconfigReadyViaSSHFallbackReason))
	got := getSecret(t, nd, "ns", "c-kubeconfig")
	g.Expect(got.Annotations[KubeconfigSourceAnnotation]).To(Equal(KubeconfigSourceSSHFallback))
	g.Expect(metav1.IsControlledBy(got, kcp)).To(BeTrue())
}

// TestReconcileKubeconfig_RepinAcceptsNewCA: with a pin already stored, an
// operator re-pin (annotation) lets the next candidate re-pin and publish.
func TestReconcileKubeconfig_RepinAcceptsNewCA(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	kcp.Annotations = map[string]string{
		KubeconfigCAPinAnnotation: "old-pin-that-will-not-match",
		KubeconfigRepinAnnotation: "true",
	}
	valid := newValidKubeconfig(t, cluster.Name, testExpectedServer)
	newPin, err := caFingerprint(valid)
	g.Expect(err).NotTo(HaveOccurred())
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig-push", Namespace: "ns",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"value": valid},
	}
	g.Expect(controllerRefTo(kcp, staging, r0Scheme(t))).To(Succeed())
	r, nd := buildReconciler(t, kcp, cluster, staging)

	ready, err := r.reconcileKubeconfig(context.Background(), log.Log, kcp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue(), "after re-pin the new CA candidate is accepted")
	g.Expect(kcp.Annotations[KubeconfigRepinAnnotation]).To(BeEmpty(), "re-pin flag is consumed once")
	g.Expect(kcp.Annotations[KubeconfigCAPinAnnotation]).To(Equal(newPin), "re-pin adopts the new CA fingerprint")
	got := getSecret(t, nd, "ns", "c-kubeconfig")
	g.Expect(got.Data["value"]).To(Equal(valid))
}

// TestEnsureStaging_RepairsNodeTampering: a node adds a finalizer and a stray
// label to the KCP-owned staging Secret; the controller reverts them.
func TestEnsureStaging_RepairsNodeTampering(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c-kubeconfig-push", Namespace: "ns",
			Finalizers: []string{"node.example.com/pin"},
			Labels:     map[string]string{clusterv1.ClusterNameLabel: "c"},
		},
		Type: corev1.SecretTypeOpaque,
	}
	g.Expect(controllerRefTo(kcp, staging, r0Scheme(t))).To(Succeed())
	r, nd := buildReconciler(t, kcp, cluster, staging)

	g.Expect(r.ensureStagingKubeconfigSecret(context.Background(), log.Log, kcp, cluster)).To(Succeed())
	got := getSecret(t, nd, "ns", "c-kubeconfig-push")
	g.Expect(got.Finalizers).To(BeEmpty(), "node-added finalizers must be stripped")
	g.Expect(got.Labels[bootstrapv1beta2.KubeconfigPushSecretTypeLabel]).To(Equal(bootstrapv1beta2.KubeconfigPushSecretTypeValue))
	g.Expect(metav1.IsControlledBy(got, kcp)).To(BeTrue())
}

// TestEnsureStaging_CreatesWhenMissing.
func TestEnsureStaging_CreatesWhenMissing(t *testing.T) {
	g := NewWithT(t)
	kcp, cluster := publishKCPAndCluster()
	r, nd := buildReconciler(t, kcp, cluster)
	g.Expect(r.ensureStagingKubeconfigSecret(context.Background(), log.Log, kcp, cluster)).To(Succeed())
	got := getSecret(t, nd, "ns", "c-kubeconfig-push")
	g.Expect(metav1.IsControlledBy(got, kcp)).To(BeTrue())
	g.Expect(got.Labels[clusterv1.ClusterNameLabel]).To(Equal("c"))
}

// r0Scheme + controllerRefTo are test helpers to stamp a controller owner ref on
// a fixture Secret before it goes into the fake client.
func r0Scheme(t *testing.T) *runtime.Scheme { return publishScheme(t) }

func controllerRefTo(owner client.Object, obj client.Object, s *runtime.Scheme) error {
	gvks, _, err := s.ObjectKinds(owner)
	if err != nil {
		return err
	}
	ctlTrue := true
	obj.SetOwnerReferences(append(obj.GetOwnerReferences(), metav1.OwnerReference{
		APIVersion: gvks[0].GroupVersion().String(),
		Kind:       gvks[0].Kind,
		Name:       owner.GetName(),
		UID:        owner.GetUID(),
		Controller: &ctlTrue,
	}))
	return nil
}

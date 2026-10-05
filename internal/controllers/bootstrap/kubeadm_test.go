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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"

	"fmt"
	"github.com/go-logr/logr"
	"math/big"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clientcmdlatest "k8s.io/client-go/tools/clientcmd/api/latest"
	bootstrapapi "k8s.io/cluster-bootstrap/token/api"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

const (
	testKubeadmCluster   = "wl"
	testKubeadmNamespace = "default"
	testCPUID            = types.UID("cp-uid-1234")
	testEndpointHost     = "10.0.0.42"
	testEndpointPort     = int32(6443)
)

// genTestCA returns a self-signed CA certificate in PEM form.
func genTestCA(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// makeKubeconfig builds a kubeconfig with the given server and CA data.
func makeKubeconfig(t *testing.T, server string, caPEM []byte) []byte {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["wl"] = &clientcmdapi.Cluster{Server: server, CertificateAuthorityData: caPEM}
	cfg.Contexts["wl"] = &clientcmdapi.Context{Cluster: "wl", AuthInfo: "admin"}
	cfg.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "unused-in-trust-check"}
	cfg.CurrentContext = "wl"
	data, err := runtime.Encode(clientcmdlatest.Codec, cfg)
	if err != nil {
		t.Fatalf("encode kubeconfig: %v", err)
	}
	return data
}

func kubeadmTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	g := NewWithT(t)
	g.Expect(corev1.AddToScheme(s)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(s)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(s)).To(Succeed())
	// Register the control-plane kinds the trust check resolves as unstructured so
	// the fake client can store/serve them.
	for _, kind := range []string{"KubeadmControlPlane", "KamajiControlPlane", "KairosControlPlane", "EvilControlPlane"} {
		s.AddKnownTypeWithName(schema.GroupVersionKind{Group: controlPlaneGroup, Version: "v1beta2", Kind: kind}, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(schema.GroupVersionKind{Group: controlPlaneGroup, Version: "v1beta2", Kind: kind + "List"}, &unstructured.UnstructuredList{})
	}
	// A look-alike KubeadmControlPlane in a DIFFERENT group (not on the allowlist).
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "evil.example.com", Version: "v1beta2", Kind: "KubeadmControlPlane"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "evil.example.com", Version: "v1beta2", Kind: "KubeadmControlPlaneList"}, &unstructured.UnstructuredList{})
	return s
}

type kubeadmFixture struct {
	cpGroup      string
	cpKind       string
	cpDist       string // spec.distribution on the CP object (for KairosControlPlane)
	ownerUID     types.UID
	server       string
	kubeconfigCA []byte
	clusterCA    []byte // if non-nil, a <cluster>-ca Secret is created with this tls.crt
	initialized  bool
	endpoint     bool // set a valid controlPlaneEndpoint
	noKubeconfig bool
}

// defaultFixture is a passing KubeadmControlPlane fixture.
func defaultFixture(ca []byte) kubeadmFixture {
	return kubeadmFixture{
		cpGroup:      controlPlaneGroup,
		cpKind:       "KubeadmControlPlane",
		ownerUID:     testCPUID,
		server:       fmt.Sprintf("https://%s:%d", testEndpointHost, testEndpointPort),
		kubeconfigCA: ca,
		initialized:  true,
		endpoint:     true,
	}
}

// buildSource wires a kubeadmJoinSource over fake clients for fx, plus the Cluster
// and (optionally) Machine. workloadClient mints into an in-memory fake workload
// cluster. Returns the source, the JoinRequest, and the workload client.
func buildSource(t *testing.T, fx kubeadmFixture, extraKinds map[schema.GroupKind]bool) (*kubeadmJoinSource, JoinRequest, client.Client) {
	t.Helper()
	s := kubeadmTestScheme(t)

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: testKubeadmCluster, Namespace: testKubeadmNamespace},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: fx.cpGroup, Kind: fx.cpKind, Name: "cp",
			},
		},
	}
	if fx.endpoint {
		cluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: testEndpointHost, Port: testEndpointPort}
	}
	if fx.initialized {
		cluster.Status.Initialization.ControlPlaneInitialized = ptrBool(true)
	}

	cp := &unstructured.Unstructured{}
	cp.SetGroupVersionKind(schema.GroupVersionKind{Group: fx.cpGroup, Version: "v1beta2", Kind: fx.cpKind})
	cp.SetName("cp")
	cp.SetNamespace(testKubeadmNamespace)
	cp.SetUID(testCPUID)
	if fx.cpDist != "" {
		_ = unstructured.SetNestedField(cp.Object, fx.cpDist, "spec", "distribution")
	}

	objs := []client.Object{cluster, cp}
	if !fx.noKubeconfig {
		kc := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testKubeadmCluster + "-kubeconfig",
				Namespace: testKubeadmNamespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: fx.cpGroup + "/v1beta2",
					Kind:       fx.cpKind,
					Name:       "cp",
					UID:        fx.ownerUID,
					Controller: ptrBool(true),
				}},
			},
			Data: map[string][]byte{workloadKubeconfigSecretKey: makeKubeconfig(t, fx.server, fx.kubeconfigCA)},
		}
		objs = append(objs, kc)
	}

	var caObjs []client.Object
	if fx.clusterCA != nil {
		caObjs = append(caObjs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testKubeadmCluster + "-ca", Namespace: testKubeadmNamespace},
			Data:       map[string][]byte{clusterCASecretCrtKey: fx.clusterCA},
		})
	}

	mgmt := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	caReader := fake.NewClientBuilder().WithScheme(s).WithObjects(caObjs...).Build()
	workload := fake.NewClientBuilder().WithScheme(s).Build()

	src := &kubeadmJoinSource{
		mgmt:           mgmt,
		caReader:       caReader,
		workloadClient: func(_ context.Context, _ client.ObjectKey) (client.Client, error) { return workload, nil },
		extraKinds:     extraKinds,
		tokenTTL:       defaultKubeadmTokenTTL,
		now:            func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
		// Stub the contract resolver: read the fixture CP (stored at v1beta2) from the
		// fake mgmt client, so unit tests need no installed CRD / contract labels.
		resolveControlPlane: func(ctx context.Context, ref clusterv1.ContractVersionedObjectReference, namespace string) (*unstructured.Unstructured, error) {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(schema.GroupVersionKind{Group: ref.APIGroup, Version: "v1beta2", Kind: ref.Kind})
			if err := mgmt.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, u); err != nil {
				return nil, err
			}
			return u, nil
		},
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: testKubeadmNamespace},
		Spec:       clusterv1.MachineSpec{Version: "v1.30.0"},
	}
	req := JoinRequest{Config: &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: testKubeadmNamespace},
		Spec:       bootstrapv1beta2.KairosConfigSpec{Distribution: bootstrapv1beta2.DistributionKubeadm, Role: "worker"},
	}, Machine: machine, Cluster: cluster}
	return src, req, workload
}

func ptrBool(b bool) *bool { return &b }

// TestKubeadmWorkerJoin_Accepts covers the three legitimate trust shapes and
// asserts a token is minted and the JoinConfiguration is well-formed.
func TestKubeadmWorkerJoin_Accepts(t *testing.T) {
	ca := genTestCA(t, "cluster-ca")
	cases := []struct {
		name  string
		fx    kubeadmFixture
		extra map[schema.GroupKind]bool
	}{
		{"KubeadmControlPlane, no <cluster>-ca", defaultFixture(ca), nil},
		{"Kamaji default: <cluster>-ca present, unowned, matches", func() kubeadmFixture {
			fx := defaultFixture(ca)
			fx.cpKind = "KamajiControlPlane"
			fx.clusterCA = ca
			return fx
		}(), nil},
		{"operator-flag kind", func() kubeadmFixture {
			fx := defaultFixture(ca)
			fx.cpGroup = "evil.example.com"
			fx.cpKind = "KubeadmControlPlane"
			return fx
		}(), map[schema.GroupKind]bool{{Group: "evil.example.com", Kind: "KubeadmControlPlane"}: true}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			src, req, workload := buildSource(t, tc.fx, tc.extra)
			mat, err := src.WorkerJoin(context.Background(), req)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(mat.Kubeadm).NotTo(BeNil())
			g.Expect(mat.Kubeadm.TokenID).To(MatchRegexp(`^[a-z0-9]{6}$`))
			g.Expect(mat.Kubeadm.KubernetesVersion).To(Equal("v1.30.0"))

			jc := mat.Kubeadm.JoinConfiguration
			g.Expect(jc).To(ContainSubstring("kind: JoinConfiguration"))
			g.Expect(jc).To(ContainSubstring("bootstrapToken"))
			g.Expect(jc).To(ContainSubstring("caCertHashes"))
			g.Expect(jc).To(ContainSubstring("sha256:"))
			g.Expect(jc).To(ContainSubstring(fmt.Sprintf("%s:%d", testEndpointHost, testEndpointPort)))
			g.Expect(jc).To(ContainSubstring("node.cluster.x-k8s.io/uninitialized"))
			g.Expect(jc).To(ContainSubstring("worker-0")) // nodeRegistration.name

			// A bootstrap-token Secret was minted in the workload cluster.
			secret := &corev1.Secret{}
			err = workload.Get(context.Background(), client.ObjectKey{
				Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-" + mat.Kubeadm.TokenID,
			}, secret)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(secret.Type).To(Equal(bootstrapapi.SecretTypeBootstrapToken))
			g.Expect(string(secret.Data[bootstrapapi.BootstrapTokenExtraGroupsKey])).To(Equal(kubeadmBootstrapTokenGroups))
		})
	}
}

// TestKubeadmWorkerJoin_TerminalRefusals covers every TERMINAL trust-check failure:
// no token minted, the error is NOT a bootstrapNotReadyError.
func TestKubeadmWorkerJoin_TerminalRefusals(t *testing.T) {
	ca := genTestCA(t, "cluster-ca")
	otherCA := genTestCA(t, "other-ca")
	cases := []struct {
		name    string
		mutate  func(fx *kubeadmFixture)
		extra   map[schema.GroupKind]bool
		wantSub string
	}{
		{"owner UID mismatch", func(fx *kubeadmFixture) { fx.ownerUID = types.UID("not-the-cp") }, nil, "not controller-owned"},
		{"server mismatch", func(fx *kubeadmFixture) { fx.server = "https://evil.example.com:6443" }, nil, "does not equal the Cluster control-plane endpoint"},
		{"CA cross-check mismatch", func(fx *kubeadmFixture) { fx.clusterCA = otherCA }, nil, "does not match"},
		{"lookalike kind in another group", func(fx *kubeadmFixture) { fx.cpGroup = "evil.example.com" }, nil, "not on the kubeadm trust allowlist"},
		{"k0s KairosControlPlane", func(fx *kubeadmFixture) { fx.cpKind = "KairosControlPlane"; fx.cpDist = "k0s" }, nil, "not kubeadm"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			fx := defaultFixture(ca)
			tc.mutate(&fx)
			src, req, _ := buildSource(t, fx, tc.extra)
			_, err := src.WorkerJoin(context.Background(), req)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSub))
			var nr *bootstrapNotReadyError
			g.Expect(errors.As(err, &nr)).To(BeFalse(), "refusal must be terminal, not not-ready")
		})
	}
}

// TestKubeadmWorkerJoin_MachinePoolRefused asserts a MachinePool-owned worker is
// refused terminally (MachineDeployment workers only).
func TestKubeadmWorkerJoin_MachinePoolRefused(t *testing.T) {
	g := NewWithT(t)
	ca := genTestCA(t, "cluster-ca")
	src, req, _ := buildSource(t, defaultFixture(ca), nil)
	req.Machine.OwnerReferences = []metav1.OwnerReference{{Kind: "MachinePool", Name: "mp", APIVersion: "cluster.x-k8s.io/v1beta2"}}
	_, err := src.WorkerJoin(context.Background(), req)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("MachinePool"))
	var nr *bootstrapNotReadyError
	g.Expect(errors.As(err, &nr)).To(BeFalse())
}

// TestKubeadmWorkerJoin_NotReady covers the transient waits: they wrap the typed
// not-ready error so the reconciler requeues instead of parking.
func TestKubeadmWorkerJoin_NotReady(t *testing.T) {
	ca := genTestCA(t, "cluster-ca")
	cases := []struct {
		name   string
		mutate func(fx *kubeadmFixture)
		src    func(s *kubeadmJoinSource)
	}{
		{"control plane not initialized", func(fx *kubeadmFixture) { fx.initialized = false }, nil},
		{"no control-plane endpoint", func(fx *kubeadmFixture) { fx.endpoint = false }, nil},
		{"kubeconfig Secret absent", func(fx *kubeadmFixture) { fx.noKubeconfig = true }, nil},
		{"workload cluster unreachable", nil, func(s *kubeadmJoinSource) {
			s.workloadClient = func(_ context.Context, _ client.ObjectKey) (client.Client, error) {
				return nil, errors.New("dial tcp: timeout")
			}
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			fx := defaultFixture(ca)
			if tc.mutate != nil {
				tc.mutate(&fx)
			}
			src, req, _ := buildSource(t, fx, nil)
			if tc.src != nil {
				tc.src(src)
			}
			_, err := src.WorkerJoin(context.Background(), req)
			g.Expect(err).To(HaveOccurred())
			var nr *bootstrapNotReadyError
			g.Expect(errors.As(err, &nr)).To(BeTrue(), "expected a bootstrapNotReadyError, got %v", err)
		})
	}
}

// TestKubeadmWorkerJoin_MissingVersionTerminal asserts an absent Machine.spec.version
// is a terminal refusal (version is authoritative for the kubeadm path).
func TestKubeadmWorkerJoin_MissingVersionTerminal(t *testing.T) {
	g := NewWithT(t)
	ca := genTestCA(t, "cluster-ca")
	src, req, _ := buildSource(t, defaultFixture(ca), nil)
	req.Machine.Spec.Version = ""
	_, err := src.WorkerJoin(context.Background(), req)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("Machine.spec.version"))
	var nr *bootstrapNotReadyError
	g.Expect(errors.As(err, &nr)).To(BeFalse())
}

// fakeRefresher is a JoinMaterialSource + workerTokenRefresher test double.
type fakeRefresher struct {
	need   bool
	err    error
	called int
}

func (f *fakeRefresher) WorkerJoin(context.Context, JoinRequest) (WorkerJoinMaterial, error) {
	return WorkerJoinMaterial{Kubeadm: &KubeadmJoinMaterial{JoinConfiguration: "x", TokenID: "abcdef"}}, nil
}
func (f *fakeRefresher) TokenNeedsRefresh(context.Context, JoinRequest, string) (bool, error) {
	f.called++
	return f.need, f.err
}

// TestDecideKubeadmTokenRefresh pins the reconciler's refresh gate: regenerate when
// the source says so, cap when the Machine is too old, and none when the node has
// joined or the distribution is not kubeadm (ADR 0010 P1 item 7).
func TestDecideKubeadmTokenRefresh(t *testing.T) {
	scheme := charScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	kubeadmWorker := func() *bootstrapv1beta2.KairosConfig {
		return &bootstrapv1beta2.KairosConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "default"},
			Spec:       bootstrapv1beta2.KairosConfigSpec{Distribution: bootstrapv1beta2.DistributionKubeadm, Role: "worker"},
		}
	}
	youngMachine := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: "w", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Hour))}}
	oldMachine := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: "w", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-48 * time.Hour))}}
	joinedMachine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Hour))},
		Status:     clusterv1.MachineStatus{NodeRef: clusterv1.MachineNodeReference{Name: "node-w"}},
	}

	t.Run("source says refresh -> regenerate", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{need: true}
		r := &KairosConfigReconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now },
			JoinSources: map[string]JoinMaterialSource{bootstrapv1beta2.DistributionKubeadm: ref}}
		d, err := r.decideKubeadmTokenRefresh(context.Background(), logr.Discard(), kubeadmWorker(), youngMachine, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshRegenerate))
		g.Expect(ref.called).To(Equal(1))
	})
	t.Run("machine past cap -> capped, source not consulted", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{need: true}
		r := &KairosConfigReconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now },
			JoinSources: map[string]JoinMaterialSource{bootstrapv1beta2.DistributionKubeadm: ref}}
		d, err := r.decideKubeadmTokenRefresh(context.Background(), logr.Discard(), kubeadmWorker(), oldMachine, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshCapped))
		g.Expect(ref.called).To(Equal(0), "cap must short-circuit before minting")
	})
	t.Run("node joined -> none", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{need: true}
		r := &KairosConfigReconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now },
			JoinSources: map[string]JoinMaterialSource{bootstrapv1beta2.DistributionKubeadm: ref}}
		d, err := r.decideKubeadmTokenRefresh(context.Background(), logr.Discard(), kubeadmWorker(), joinedMachine, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshNone))
		g.Expect(ref.called).To(Equal(0))
	})
	t.Run("k0s worker -> none", func(t *testing.T) {
		g := NewWithT(t)
		r := &KairosConfigReconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now }}
		kc := kubeadmWorker()
		kc.Spec.Distribution = bootstrapv1beta2.DistributionK0s
		d, err := r.decideKubeadmTokenRefresh(context.Background(), logr.Discard(), kc, youngMachine, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshNone))
	})
}

// TestKubeadmTokenNeedsRefresh pins the refresh decision against the workload token
// Secret: missing -> refresh; past half-TTL -> refresh; fresh -> no refresh.
func TestKubeadmTokenNeedsRefresh(t *testing.T) {
	ca := genTestCA(t, "cluster-ca")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seedToken := func(workload client.Client, id string, exp time.Time) {
		_ = workload.Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-" + id},
			Type:       bootstrapapi.SecretTypeBootstrapToken,
			Data:       map[string][]byte{bootstrapapi.BootstrapTokenExpirationKey: []byte(exp.Format(time.RFC3339))},
		})
	}

	t.Run("empty token id -> refresh", func(t *testing.T) {
		g := NewWithT(t)
		src, req, _ := buildSource(t, defaultFixture(ca), nil)
		src.now = func() time.Time { return now }
		need, err := src.TokenNeedsRefresh(context.Background(), req, "")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(need).To(BeTrue())
	})
	t.Run("token absent in workload -> refresh", func(t *testing.T) {
		g := NewWithT(t)
		src, req, _ := buildSource(t, defaultFixture(ca), nil)
		src.now = func() time.Time { return now }
		need, err := src.TokenNeedsRefresh(context.Background(), req, "abcdef")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(need).To(BeTrue())
	})
	t.Run("past half-TTL -> refresh", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload := buildSource(t, defaultFixture(ca), nil)
		src.now = func() time.Time { return now }
		// TTL 15m; half is 7m30s. Expire in 5m -> past half-TTL.
		seedToken(workload, "abcdef", now.Add(5*time.Minute))
		need, err := src.TokenNeedsRefresh(context.Background(), req, "abcdef")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(need).To(BeTrue())
	})
	t.Run("fresh -> no refresh", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload := buildSource(t, defaultFixture(ca), nil)
		src.now = func() time.Time { return now }
		// Expire in 14m -> well under half consumed.
		seedToken(workload, "abcdef", now.Add(14*time.Minute))
		need, err := src.TokenNeedsRefresh(context.Background(), req, "abcdef")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(need).To(BeFalse())
	})
}

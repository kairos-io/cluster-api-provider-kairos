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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	kubeadmv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"

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
	endpointHost string // overrides testEndpointHost when set
	endpointPort int32  // overrides testEndpointPort when non-zero
	noController bool   // the kubeconfig ownerRef exists but is not a controller ref
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
	epHost, epPort := testEndpointHost, testEndpointPort
	if fx.endpointHost != "" {
		epHost = fx.endpointHost
	}
	if fx.endpointPort != 0 {
		epPort = fx.endpointPort
	}
	if fx.endpoint {
		cluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: epHost, Port: epPort}
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
					Controller: ptrBool(!fx.noController),
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

// TestKubeadmTrustedClusterFilter pins the COARSE connect-level filter: it admits
// only KubeadmControlPlane and KamajiControlPlane (plus operator-flag kinds) and
// EXCLUDES KairosControlPlane, so clustercache never dials a k0s/k3s KairosControlPlane
// cluster. The fine per-request WorkerJoin check still refuses non-kubeadm anyway.
func TestKubeadmTrustedClusterFilter(t *testing.T) {
	cp := func(group, kind string) *clusterv1.Cluster {
		return &clusterv1.Cluster{Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: clusterv1.ContractVersionedObjectReference{APIGroup: group, Kind: kind, Name: "cp"},
		}}
	}
	cases := []struct {
		name    string
		cluster *clusterv1.Cluster
		extra   map[schema.GroupKind]bool
		want    bool
	}{
		{"KubeadmControlPlane admitted", cp(controlPlaneGroup, "KubeadmControlPlane"), nil, true},
		{"KamajiControlPlane admitted", cp(controlPlaneGroup, "KamajiControlPlane"), nil, true},
		{"KairosControlPlane excluded from coarse filter", cp(controlPlaneGroup, "KairosControlPlane"), nil, false},
		{"unknown kind rejected", cp(controlPlaneGroup, "SomethingElse"), nil, false},
		{"lookalike in another group rejected", cp("evil.example.com", "KubeadmControlPlane"), nil, false},
		{"operator-flag kind admitted", cp("x.example.com", "FooControlPlane"), map[schema.GroupKind]bool{{Group: "x.example.com", Kind: "FooControlPlane"}: true}, true},
		{"nil cluster rejected", nil, nil, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(KubeadmTrustedClusterFilter(tc.extra)(tc.cluster)).To(Equal(tc.want))
		})
	}
}

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
		// Kamaji control-plane provider with advertiseAddress set publishes admin.svc.
		{"Kamaji kubeconfig with the in-cluster Service server", func() kubeadmFixture {
			fx := defaultFixture(ca)
			fx.cpKind = "KamajiControlPlane"
			fx.clusterCA = ca
			fx.server = "https://my-cluster.default.svc:6443"
			return fx
		}(), nil},
		{"operator-flag kind", func() kubeadmFixture {
			fx := defaultFixture(ca)
			fx.cpGroup = "evil.example.com"
			fx.cpKind = "KubeadmControlPlane"
			return fx
		}(), map[schema.GroupKind]bool{{Group: "evil.example.com", Kind: "KubeadmControlPlane"}: true}},
		// Cluster API writes the kubeconfig server with net.JoinHostPort, so an
		// IPv6 endpoint arrives bracketed. Comparing the raw strings refused every
		// IPv6 cluster, with a message that reads like a planted kubeconfig.
		{"IPv6 endpoint, bracketed by CAPI", func() kubeadmFixture {
			fx := defaultFixture(ca)
			fx.endpointHost = "fd00::1"
			fx.server = "https://[fd00::1]:6443"
			return fx
		}(), nil},
		{"kubeconfig server has a trailing slash", func() kubeadmFixture {
			fx := defaultFixture(ca)
			fx.server = fmt.Sprintf("https://%s:%d/", testEndpointHost, testEndpointPort)
			return fx
		}(), nil},
		{"host differs only in case", func() kubeadmFixture {
			fx := defaultFixture(ca)
			fx.endpointHost = "CP.Example.com"
			fx.server = "https://cp.example.com:6443"
			return fx
		}(), nil},
		{"default https port omitted in the kubeconfig", func() kubeadmFixture {
			fx := defaultFixture(ca)
			fx.endpointPort = 443
			fx.server = fmt.Sprintf("https://%s", testEndpointHost)
			return fx
		}(), nil},
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
			wantEP := clusterv1.APIEndpoint{Host: testEndpointHost, Port: testEndpointPort}
			if tc.fx.endpointHost != "" {
				wantEP.Host = tc.fx.endpointHost
			}
			if tc.fx.endpointPort != 0 {
				wantEP.Port = tc.fx.endpointPort
			}
			// Must be the bracketed form for IPv6: kubeadm parses this as host:port.
			g.Expect(jc).To(ContainSubstring(wantEP.String()))
			g.Expect(jc).NotTo(ContainSubstring(".svc"), "the node must join the Cluster endpoint, not the kubeconfig server")
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

// TestKubeadmWorkerJoin_StripsSpecSuppliedJoinCredentials is the render-time half
// of two webhook rules. KairosConfigTemplate has no webhook registered, so for a
// config CAPI core derives from a template these fields are only refused one
// object later. The renderer must not emit them even if they reach it.
func TestKubeadmWorkerJoin_StripsSpecSuppliedJoinCredentials(t *testing.T) {
	g := NewWithT(t)
	ca := genTestCA(t, "cluster-ca")
	src, req, _ := buildSource(t, defaultFixture(ca), nil)

	req.Config.Spec.Kubeadm = &bootstrapv1beta2.KubeadmConfig{
		JoinConfiguration: &kubeadmv1.JoinConfiguration{
			Discovery:    kubeadmv1.Discovery{TLSBootstrapToken: "abcdef.0123456789abcdef"},
			ControlPlane: &kubeadmv1.JoinControlPlane{},
		},
	}

	mat, err := src.WorkerJoin(context.Background(), req)
	g.Expect(err).NotTo(HaveOccurred())
	jc := mat.Kubeadm.JoinConfiguration

	g.Expect(jc).NotTo(ContainSubstring("abcdef.0123456789abcdef"),
		"a spec-supplied tlsBootstrapToken must never reach the rendered userdata")
	g.Expect(jc).NotTo(ContainSubstring("tlsBootstrapToken"))
	g.Expect(jc).NotTo(ContainSubstring("controlPlane"),
		"a worker join must carry no control-plane join material")
	// The minted token is still there: stripping must not break the join.
	g.Expect(jc).To(ContainSubstring("bootstrapToken"))
	g.Expect(mat.Kubeadm.TokenID).To(MatchRegexp(`^[a-z0-9]{6}$`))
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
		{"owner UID mismatch", func(fx *kubeadmFixture) { fx.ownerUID = types.UID("not-the-cp") }, nil, "must be owned by the cluster's control plane"},
		// An ownerRef that is not a controller ref: GetControllerOf returns nil, so
		// the UID leg must refuse rather than fall through.
		{"ownerRef present but not a controller ref", func(fx *kubeadmFixture) { fx.noController = true }, nil, "must be owned by the cluster's control plane"},
		// The other half of the Kamaji accept case above. A server that differs from
		// the Cluster endpoint is allowed only when <cluster>-ca anchors the CA; with
		// neither anchor the owner reference is all that is left, and that is
		// forgeable by anyone who can create Secrets in the namespace.
		{"server differs and there is no <cluster>-ca to anchor it", func(fx *kubeadmFixture) {
			fx.server = "https://evil.example.com:6443"
			fx.clusterCA = nil
		}, nil, "Secret to cross-check its CA against"},
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

// TestKubeadmWorkerJoin_ServerAddressOverridesEndpoint asserts spec.serverAddress
// becomes the join endpoint while the CA pin is kept.
func TestKubeadmWorkerJoin_ServerAddressOverridesEndpoint(t *testing.T) {
	g := NewWithT(t)
	ca := genTestCA(t, "cluster-ca")
	src, req, _ := buildSource(t, defaultFixture(ca), nil)
	req.Config.Spec.ServerAddress = "192.0.2.10:6443"

	mat, err := src.WorkerJoin(context.Background(), req)
	g.Expect(err).NotTo(HaveOccurred())
	jc := mat.Kubeadm.JoinConfiguration
	g.Expect(jc).To(ContainSubstring("apiServerEndpoint: 192.0.2.10:6443"))
	g.Expect(jc).NotTo(ContainSubstring(testEndpointHost))
	g.Expect(jc).To(ContainSubstring("sha256:"))
}

func TestKubeadmJoinEndpoint(t *testing.T) {
	trusted := trustedControlPlane{endpointHost: testEndpointHost, endpointPort: testEndpointPort}
	cases := []struct {
		name, serverAddress, want string
		wantErr                   bool
	}{
		{"unset uses the Cluster endpoint", "", "10.0.0.42:6443", false},
		{"host and port", "192.0.2.10:6443", "192.0.2.10:6443", false},
		{"https URL", "https://192.0.2.10:6443", "192.0.2.10:6443", false},
		{"host only takes the Cluster port", "cp.example.com", "cp.example.com:6443", false},
		{"IPv6", "[fd00::1]:6443", "[fd00::1]:6443", false},
		{"no host", "https://", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			got, err := kubeadmJoinEndpoint(tc.serverAddress, trusted)
			if tc.wantErr {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
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
	remint    bool
	nextCheck time.Duration
	err       error
	ensured   int
	revoked   int
	revokeErr error
	revokedID string
}

func (f *fakeRefresher) WorkerJoin(context.Context, JoinRequest) (WorkerJoinMaterial, error) {
	return WorkerJoinMaterial{Kubeadm: &KubeadmJoinMaterial{JoinConfiguration: "x", TokenID: "abcdef"}}, nil
}

func (f *fakeRefresher) EnsureWorkerToken(context.Context, JoinRequest, string) (bool, time.Duration, error) {
	f.ensured++
	return f.remint, f.nextCheck, f.err
}

func (f *fakeRefresher) RevokeWorkerToken(_ context.Context, _ JoinRequest, tokenID string) error {
	f.revoked++
	f.revokedID = tokenID
	return f.revokeErr
}

func (f *fakeRefresher) TokenRecheckInterval() time.Duration { return f.nextCheck }

// TestReconcileKubeadmToken pins the reconciler's token gate: extend (with the
// source's cadence) while the node is still booting, re-render only when there is
// nothing left to extend, revoke once the node has registered, cap when the Machine
// is too old, and do nothing for a non-kubeadm worker (ADR 0010 P1 item 7).
func TestReconcileKubeadmToken(t *testing.T) {
	scheme := charScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	kubeadmWorker := func() *bootstrapv1beta2.KairosConfig {
		kc := &bootstrapv1beta2.KairosConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "default"},
			Spec:       bootstrapv1beta2.KairosConfigSpec{Distribution: bootstrapv1beta2.DistributionKubeadm, Role: "worker"},
		}
		kc.Status.BootstrapTokenID = "abcdef"
		return kc
	}
	youngMachine := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: "w", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Hour))}}
	oldMachine := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: "w", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-48 * time.Hour))}}
	joinedMachine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "default", CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Hour))},
		Status:     clusterv1.MachineStatus{NodeRef: clusterv1.MachineNodeReference{Name: "node-w"}},
	}
	reconciler := func(ref JoinMaterialSource) *KairosConfigReconciler {
		return &KairosConfigReconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now },
			JoinSources: map[string]JoinMaterialSource{bootstrapv1beta2.DistributionKubeadm: ref}}
	}

	t.Run("still booting -> extend, carrying the source's cadence", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{nextCheck: 5 * time.Minute}
		d, next, err := reconciler(ref).reconcileKubeadmToken(context.Background(), logr.Discard(), kubeadmWorker(), youngMachine, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshExtended))
		g.Expect(next).To(Equal(5*time.Minute), "the requeue must come from the token's TTL")
		g.Expect(ref.ensured).To(Equal(1))
		g.Expect(ref.revoked).To(Equal(0), "a booting node's token must never be revoked")
	})
	t.Run("nothing left to extend -> regenerate", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{remint: true}
		d, next, err := reconciler(ref).reconcileKubeadmToken(context.Background(), logr.Discard(), kubeadmWorker(), youngMachine, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshRegenerate))
		g.Expect(next).To(BeZero())
	})
	t.Run("machine past cap -> capped, source not consulted", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{}
		d, _, err := reconciler(ref).reconcileKubeadmToken(context.Background(), logr.Discard(), kubeadmWorker(), oldMachine, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshCapped))
		g.Expect(ref.ensured).To(Equal(0), "the cap must short-circuit before touching the token")
	})
	t.Run("node joined -> revoke and forget the id", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{}
		kc := kubeadmWorker()
		d, _, err := reconciler(ref).reconcileKubeadmToken(context.Background(), logr.Discard(), kc, joinedMachine, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshRevoked))
		g.Expect(ref.revoked).To(Equal(1))
		g.Expect(ref.revokedID).To(Equal("abcdef"))
		g.Expect(kc.Status.BootstrapTokenID).To(BeEmpty(), "a revoked token must not be revoked again")
		g.Expect(ref.ensured).To(Equal(0))
	})
	t.Run("node joined with nothing on record -> none", func(t *testing.T) {
		g := NewWithT(t)
		ref := &fakeRefresher{}
		kc := kubeadmWorker()
		kc.Status.BootstrapTokenID = ""
		d, _, err := reconciler(ref).reconcileKubeadmToken(context.Background(), logr.Discard(), kc, joinedMachine, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshNone))
		g.Expect(ref.revoked).To(Equal(0))
	})
	t.Run("k0s worker -> none", func(t *testing.T) {
		g := NewWithT(t)
		r := &KairosConfigReconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now }}
		kc := kubeadmWorker()
		kc.Spec.Distribution = bootstrapv1beta2.DistributionK0s
		d, _, err := r.reconcileKubeadmToken(context.Background(), logr.Discard(), kc, youngMachine, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(d).To(Equal(kubeadmRefreshNone))
	})
}

// TestKubeadmEnsureWorkerToken pins the extension against the workload token
// Secret. The behaviour under test is that the token the node is ALREADY HOLDING
// has its expiry moved: the Secret keeps its name, its ID and its secret half, so
// a node that boots late still joins with the token baked into its userdata.
func TestKubeadmEnsureWorkerToken(t *testing.T) {
	ca := genTestCA(t, "cluster-ca")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seedToken := func(workload client.Client, id string, data map[string][]byte) {
		_ = workload.Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-" + id},
			Type:       bootstrapapi.SecretTypeBootstrapToken,
			Data:       data,
		})
	}
	withExpiry := func(exp time.Time) map[string][]byte {
		return map[string][]byte{
			bootstrapapi.BootstrapTokenIDKey:         []byte("abcdef"),
			bootstrapapi.BootstrapTokenSecretKey:     []byte("0123456789abcdef"),
			bootstrapapi.BootstrapTokenExpirationKey: []byte(exp.Format(time.RFC3339)),
		}
	}
	readToken := func(g *WithT, workload client.Client, id string) *corev1.Secret {
		s := &corev1.Secret{}
		g.Expect(workload.Get(context.Background(),
			client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-" + id}, s)).To(Succeed())
		return s
	}

	t.Run("empty token id -> remint", func(t *testing.T) {
		g := NewWithT(t)
		src, req, _ := buildSource(t, defaultFixture(ca), nil)
		src.now = func() time.Time { return now }
		remint, next, err := src.EnsureWorkerToken(context.Background(), req, "")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(remint).To(BeTrue())
		g.Expect(next).To(BeZero())
	})
	t.Run("token absent in workload -> remint", func(t *testing.T) {
		g := NewWithT(t)
		src, req, _ := buildSource(t, defaultFixture(ca), nil)
		src.now = func() time.Time { return now }
		remint, _, err := src.EnsureWorkerToken(context.Background(), req, "abcdef")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(remint).To(BeTrue())
	})
	t.Run("about to expire -> expiry moved, token value untouched", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload := buildSource(t, defaultFixture(ca), nil)
		src.now = func() time.Time { return now }
		// TTL 15m. The node is still booting and its token expires in one minute:
		// this is the case a slow install dies on today.
		seedToken(workload, "abcdef", withExpiry(now.Add(1*time.Minute)))

		remint, next, err := src.EnsureWorkerToken(context.Background(), req, "abcdef")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(remint).To(BeFalse(), "an extendable token must never be reminted")
		g.Expect(next).To(Equal(defaultKubeadmTokenTTL / 3))

		s := readToken(g, workload, "abcdef")
		g.Expect(string(s.Data[bootstrapapi.BootstrapTokenExpirationKey])).
			To(Equal(now.Add(defaultKubeadmTokenTTL).UTC().Format(time.RFC3339)))
		g.Expect(string(s.Data[bootstrapapi.BootstrapTokenSecretKey])).To(Equal("0123456789abcdef"),
			"the node is holding this value; extending must not change it")
		g.Expect(string(s.Data[bootstrapapi.BootstrapTokenIDKey])).To(Equal("abcdef"))
	})
	t.Run("already good for a full TTL -> left alone", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload := buildSource(t, defaultFixture(ca), nil)
		src.now = func() time.Time { return now }
		far := now.Add(2 * time.Hour)
		seedToken(workload, "abcdef", withExpiry(far))

		remint, next, err := src.EnsureWorkerToken(context.Background(), req, "abcdef")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(remint).To(BeFalse())
		g.Expect(next).To(Equal(defaultKubeadmTokenTTL / 3))
		g.Expect(string(readToken(g, workload, "abcdef").Data[bootstrapapi.BootstrapTokenExpirationKey])).
			To(Equal(far.Format(time.RFC3339)), "an expiry further out than ours must not be pulled in")
	})
	t.Run("no expiration key -> treated as due, ours is written", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload := buildSource(t, defaultFixture(ca), nil)
		src.now = func() time.Time { return now }
		seedToken(workload, "abcdef", map[string][]byte{bootstrapapi.BootstrapTokenIDKey: []byte("abcdef")})

		remint, _, err := src.EnsureWorkerToken(context.Background(), req, "abcdef")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(remint).To(BeFalse())
		g.Expect(string(readToken(g, workload, "abcdef").Data[bootstrapapi.BootstrapTokenExpirationKey])).
			To(Equal(now.Add(defaultKubeadmTokenTTL).UTC().Format(time.RFC3339)))
	})
	t.Run("unparsable expiration -> treated as due, ours is written", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload := buildSource(t, defaultFixture(ca), nil)
		src.now = func() time.Time { return now }
		seedToken(workload, "abcdef", map[string][]byte{
			bootstrapapi.BootstrapTokenExpirationKey: []byte("not-a-timestamp")})

		remint, _, err := src.EnsureWorkerToken(context.Background(), req, "abcdef")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(remint).To(BeFalse())
		g.Expect(string(readToken(g, workload, "abcdef").Data[bootstrapapi.BootstrapTokenExpirationKey])).
			To(Equal(now.Add(defaultKubeadmTokenTTL).UTC().Format(time.RFC3339)))
	})
}

// TestKubeadmRevokeWorkerToken pins revocation: the Secret is deleted, an empty ID
// and an already-absent Secret are both no-ops.
func TestKubeadmRevokeWorkerToken(t *testing.T) {
	ca := genTestCA(t, "cluster-ca")

	t.Run("deletes the token secret", func(t *testing.T) {
		g := NewWithT(t)
		src, req, workload := buildSource(t, defaultFixture(ca), nil)
		g.Expect(workload.Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-abcdef"},
			Type:       bootstrapapi.SecretTypeBootstrapToken,
		})).To(Succeed())

		g.Expect(src.RevokeWorkerToken(context.Background(), req, "abcdef")).To(Succeed())

		err := workload.Get(context.Background(),
			client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-abcdef"}, &corev1.Secret{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
	t.Run("empty id and an absent secret are no-ops", func(t *testing.T) {
		g := NewWithT(t)
		src, req, _ := buildSource(t, defaultFixture(ca), nil)
		g.Expect(src.RevokeWorkerToken(context.Background(), req, "")).To(Succeed())
		g.Expect(src.RevokeWorkerToken(context.Background(), req, "abcdef")).To(Succeed())
	})
}

// TestKubeadmWorkerJoin_TokenCarriesOwnerLabel pins the owner label on a minted
// token: a token whose ID never reaches status is otherwise unattributable.
func TestKubeadmWorkerJoin_TokenCarriesOwnerLabel(t *testing.T) {
	g := NewWithT(t)
	ca := genTestCA(t, "cluster-ca")
	src, req, workload := buildSource(t, defaultFixture(ca), nil)
	req.Config.UID = "config-uid-1"

	mat, err := src.WorkerJoin(context.Background(), req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mat.Kubeadm).NotTo(BeNil())

	s := &corev1.Secret{}
	g.Expect(workload.Get(context.Background(), client.ObjectKey{
		Namespace: metav1.NamespaceSystem,
		Name:      "bootstrap-token-" + mat.Kubeadm.TokenID,
	}, s)).To(Succeed())
	g.Expect(s.Labels).To(HaveKeyWithValue(kubeadmTokenOwnerLabel, "config-uid-1"))
}

// TestMintBootstrapTokenNilOwner pins the nil-Config guard: WorkerJoin tolerates a
// nil req.Config everywhere else, so minting must not panic on one either.
func TestMintBootstrapTokenNilOwner(t *testing.T) {
	g := NewWithT(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	workload := fake.NewClientBuilder().WithScheme(charScheme(t)).Build()

	_, tokenID, err := mintBootstrapToken(context.Background(), workload, defaultKubeadmTokenTTL, now, "")
	g.Expect(err).NotTo(HaveOccurred())

	s := &corev1.Secret{}
	g.Expect(workload.Get(context.Background(), client.ObjectKey{
		Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-" + tokenID}, s)).To(Succeed())
	g.Expect(s.Labels).To(BeEmpty())
}

// TestTokenRecheckInterval pins the cadence: a third of the TTL, floored so a very
// short TTL cannot turn the requeue into a hot loop.
func TestTokenRecheckInterval(t *testing.T) {
	g := NewWithT(t)
	g.Expect(tokenRecheckInterval(15 * time.Minute)).To(Equal(5 * time.Minute))
	g.Expect(tokenRecheckInterval(30 * time.Second)).To(Equal(minTokenRecheckInterval))
	g.Expect(tokenRecheckInterval(0)).To(Equal(minTokenRecheckInterval))
}

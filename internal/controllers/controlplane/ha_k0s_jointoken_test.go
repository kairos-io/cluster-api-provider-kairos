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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// The fixture under testdata/k0s-preshared-controller was produced by the real
// k0s binary at the documented baseline (v1.34.8+k0s.0, taken from the image
// quay.io/kairos/hadron:v0.4.0-standard-amd64-generic-v4.1.2-k0s-v1.34.8-k0s.0),
// with no network and a throwaway CA:
//
//	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
//	  -keyout ca.key -out ca.crt -days 1 -subj "/CN=kairos-capi-k0s-token-fixture-ca"
//	docker run --rm --network none --user "$(id -u):$(id -g)" \
//	  -v "$PWD:/in:ro" -v "$PWD/out:/out" --entrypoint /usr/bin/k0s <image> \
//	  token pre-shared --role controller --cert /in/ca.crt \
//	  --url https://192.0.2.10:9443 --valid 1s --out /out
//
// `token` is out/token_<id>, `bootstrap-token-secret.yaml` is
// out/bootstrap-token-<id>.yaml, and ca.key was discarded. The bootstrap token
// expired one second after it was created and was never applied to any
// cluster. Regenerate the fixture on every k0s baseline bump.
const (
	k0sFixtureDir    = "testdata/k0s-preshared-controller"
	k0sFixtureServer = "https://192.0.2.10:9443"
)

func readK0sFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(k0sFixtureDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func fixtureCA(t *testing.T) []byte { return readK0sFixture(t, "ca.crt") }

// gunzipBase64 returns the kubeconfig inside an encoded k0s token.
func gunzipBase64(t *testing.T, encoded string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	out, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

func TestEncodeK0sControllerJoinToken_RoundTrip(t *testing.T) {
	g := NewWithT(t)
	tok, err := generateK0sBootstrapToken()
	g.Expect(err).ToNot(HaveOccurred())
	ca := fixtureCA(t)

	for _, server := range []string{"https://10.0.0.5:9443", "https://[fd00::5]:9443"} {
		enc, err := encodeK0sControllerJoinToken(server, ca, tok)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(enc).To(MatchRegexp(`^[A-Za-z0-9+/]+=*$`), "standard base64, the alphabet k0s uses")

		dec, err := decodeK0sControllerJoinToken(enc)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(dec.server).To(Equal(server))
		g.Expect(dec.caData).To(Equal(ca))
		g.Expect(dec.user).To(Equal("controller-bootstrap"))
		g.Expect(dec.token.bearer()).To(MatchRegexp(`^[a-z0-9]{6}\.[a-z0-9]{16}$`))
		g.Expect(dec.token).To(Equal(tok))
	}
}

// TestEncodeK0sControllerJoinToken_MatchesRealK0s compares our encoding with a
// token real k0s produced. The decoded kubeconfigs must be structurally equal
// when built from the same server, CA and bootstrap token.
func TestEncodeK0sControllerJoinToken_MatchesRealK0s(t *testing.T) {
	g := NewWithT(t)
	fixture := string(readK0sFixture(t, "token"))
	ca := fixtureCA(t)

	want, err := clientcmd.Load(gunzipBase64(t, fixture))
	g.Expect(err).ToNot(HaveOccurred())

	// Our decoder reads the real token.
	dec, err := decodeK0sControllerJoinToken(fixture)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(dec.server).To(Equal(k0sFixtureServer))
	g.Expect(dec.caData).To(Equal(ca))
	g.Expect(dec.user).To(Equal(k0sControllerBootstrapUser))

	// Our encoder, given the same inputs, produces the same kubeconfig.
	ours, err := encodeK0sControllerJoinToken(k0sFixtureServer, ca, dec.token)
	g.Expect(err).ToNot(HaveOccurred())
	got, err := clientcmd.Load(gunzipBase64(t, ours))
	g.Expect(err).ToNot(HaveOccurred())

	g.Expect(got.CurrentContext).To(Equal(want.CurrentContext))
	g.Expect(got.CurrentContext).To(Equal("k0s"))
	g.Expect(keys(got.Clusters)).To(Equal(keys(want.Clusters)))
	g.Expect(keys(got.Contexts)).To(Equal(keys(want.Contexts)))
	g.Expect(keys(got.AuthInfos)).To(Equal(keys(want.AuthInfos)))
	for name, wc := range want.Clusters {
		g.Expect(got.Clusters[name].Server).To(Equal(wc.Server))
		g.Expect(got.Clusters[name].CertificateAuthorityData).To(Equal(wc.CertificateAuthorityData))
		g.Expect(got.Clusters[name].InsecureSkipTLSVerify).To(Equal(wc.InsecureSkipTLSVerify))
	}
	for name, wctx := range want.Contexts {
		g.Expect(got.Contexts[name].Cluster).To(Equal(wctx.Cluster))
		g.Expect(got.Contexts[name].AuthInfo).To(Equal(wctx.AuthInfo))
	}
	for name, wa := range want.AuthInfos {
		g.Expect(got.AuthInfos[name].Token).To(Equal(wa.Token))
	}

	// gzip at BestCompression has the same header k0s writes (no name, no mtime,
	// XFL=2 for maximum compression).
	rawOurs, _ := base64.StdEncoding.DecodeString(ours)
	rawFixture, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(fixture))
	g.Expect(rawOurs[:10]).To(Equal(rawFixture[:10]))
}

// TestNewK0sControllerJoinTokenSecret_MatchesRealK0s compares the workload
// bootstrap-token Secret with the one `k0s token pre-shared` writes.
func TestNewK0sControllerJoinTokenSecret_MatchesRealK0s(t *testing.T) {
	g := NewWithT(t)
	want := &corev1.Secret{}
	g.Expect(yaml.Unmarshal(readK0sFixture(t, "bootstrap-token-secret.yaml"), want)).To(Succeed())

	tok, err := generateK0sBootstrapToken()
	g.Expect(err).ToNot(HaveOccurred())
	got := newK0sControllerJoinTokenSecret(tok, time.Now().Add(time.Hour))

	g.Expect(got.Type).To(Equal(want.Type))
	g.Expect(got.Namespace).To(Equal(want.Namespace))
	g.Expect(got.Name).To(HavePrefix("bootstrap-token-"))
	g.Expect(want.Name).To(Equal("bootstrap-token-" + string(want.Data["token-id"])))
	g.Expect(got.Name).To(Equal("bootstrap-token-" + tok.id))
	g.Expect(keys(got.Data)).To(Equal(keys(want.Data)), "same data keys as k0s's own controller token")
	for _, usage := range []string{"usage-bootstrap-controller-join", "usage-controller-join"} {
		g.Expect(string(got.Data[usage])).To(Equal(string(want.Data[usage])))
	}
	_, err = time.Parse(time.RFC3339, string(want.Data["expiration"]))
	g.Expect(err).ToNot(HaveOccurred())
}

func TestNewK0sControllerJoinTokenSecret_Shape(t *testing.T) {
	g := NewWithT(t)
	tok := k0sBootstrapToken{id: "abcdef", secret: "0123456789abcdef"}
	expires := time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)
	s := newK0sControllerJoinTokenSecret(tok, expires)

	g.Expect(s.Namespace).To(Equal("kube-system"))
	g.Expect(s.Name).To(Equal("bootstrap-token-abcdef"))
	g.Expect(s.Type).To(Equal(corev1.SecretType("bootstrap.kubernetes.io/token")))
	g.Expect(s.Data).To(Equal(map[string][]byte{
		"token-id":                        []byte("abcdef"),
		"token-secret":                    []byte("0123456789abcdef"),
		"expiration":                      []byte("2026-10-16T13:00:00Z"),
		"description":                     []byte("k0s controller-join token minted by cluster-api-provider-kairos"),
		"usage-bootstrap-controller-join": []byte("true"),
		"usage-controller-join":           []byte("true"),
	}))
	// The token must not be able to authenticate to kube-apiserver at all.
	for _, forbidden := range []string{"usage-bootstrap-authentication", "usage-bootstrap-signing", "auth-extra-groups"} {
		g.Expect(s.Data).ToNot(HaveKey(forbidden))
	}
	g.Expect(isK0sControllerJoinTokenSecret(s)).To(BeTrue())

	// The guard used before extending, reusing or deleting rejects anything else.
	auth := s.DeepCopy()
	auth.Data["usage-bootstrap-authentication"] = []byte("true")
	g.Expect(isK0sControllerJoinTokenSecret(auth)).To(BeFalse())
	groups := s.DeepCopy()
	groups.Data["auth-extra-groups"] = []byte("system:bootstrappers:kubeadm:default-node-token")
	g.Expect(isK0sControllerJoinTokenSecret(groups)).To(BeFalse())
	noJoin := s.DeepCopy()
	delete(noJoin.Data, "usage-bootstrap-controller-join")
	delete(noJoin.Data, "usage-controller-join")
	g.Expect(isK0sControllerJoinTokenSecret(noJoin)).To(BeFalse())
	opaque := s.DeepCopy()
	opaque.Type = corev1.SecretTypeOpaque
	g.Expect(isK0sControllerJoinTokenSecret(opaque)).To(BeFalse())
}

func TestGenerateK0sBootstrapToken(t *testing.T) {
	g := NewWithT(t)
	re := regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		tok, err := generateK0sBootstrapToken()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(re.MatchString(tok.bearer())).To(BeTrue(), tok.String())
		g.Expect(seen[tok.bearer()]).To(BeFalse())
		seen[tok.bearer()] = true
	}
	tok := k0sBootstrapToken{id: "abcdef", secret: "0123456789abcdef"}
	g.Expect(tok.String()).To(Equal("abcdef.<redacted>"))
	g.Expect(fmt.Sprintf("%v %s %+v", tok, tok, tok)).ToNot(ContainSubstring("0123456789abcdef"))
}

func TestDecodeK0sControllerJoinToken_RejectsGarbageWithoutEchoingIt(t *testing.T) {
	g := NewWithT(t)
	gz := func(s string) string {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
		return base64.StdEncoding.EncodeToString(b.Bytes())
	}
	for name, in := range map[string]string{
		"not base64":     "!!!SECRETVALUE!!!",
		"not gzip":       base64.StdEncoding.EncodeToString([]byte("SECRETVALUE")),
		"not kubeconfig": gz("SECRETVALUE: [unclosed"),
		"no context":     gz("apiVersion: v1\nkind: Config\nusers:\n- name: u\n  user:\n    token: SECRETVALUE\n"),
		"bad bearer": gz("apiVersion: v1\nkind: Config\ncurrent-context: k0s\nclusters:\n- name: k0s\n  cluster:\n    server: https://10.0.0.1:9443\n" +
			"contexts:\n- name: k0s\n  context:\n    cluster: k0s\n    user: controller-bootstrap\nusers:\n- name: controller-bootstrap\n  user:\n    token: SECRETVALUE\n"),
	} {
		_, err := decodeK0sControllerJoinToken(in)
		g.Expect(err).To(HaveOccurred(), name)
		g.Expect(err.Error()).ToNot(ContainSubstring("SECRETVALUE"), name)
	}
}

func TestNormalizeK0sJoinServer(t *testing.T) {
	g := NewWithT(t)
	for in, want := range map[string]string{
		"https://10.0.0.5:9443":          "https://10.0.0.5:9443",
		"https://[fd00:0::5]:9443":       "https://[fd00::5]:9443",
		"https://[::ffff:10.0.0.5]:9443": "https://10.0.0.5:9443",
	} {
		got, ok := normalizeK0sJoinServer(in)
		g.Expect(ok).To(BeTrue(), in)
		g.Expect(got).To(Equal(want), in)
	}
	for _, in := range []string{"http://10.0.0.5:9443", "https://10.0.0.5:6443", "https://cp.example.com:9443", "::"} {
		_, ok := normalizeK0sJoinServer(in)
		g.Expect(ok).To(BeFalse(), in)
	}
}

// --- workload-cluster fixtures ---------------------------------------------

func workloadTestScheme(g *WithT) *runtime.Scheme {
	s := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(s)).To(Succeed())
	g.Expect(discoveryv1.AddToScheme(s)).To(Succeed())
	return s
}

func epReady(addrs ...string) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{Addresses: addrs, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}
}

func kubernetesSlice(name string, addressType discoveryv1.AddressType, eps ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{discoveryv1.LabelServiceName: "kubernetes"},
		},
		AddressType: addressType,
		Endpoints:   eps,
	}
}

func TestLiveK0sJoinServers(t *testing.T) {
	notReady := discoveryv1.Endpoint{Addresses: []string{"10.0.0.7"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false)}}
	terminating := discoveryv1.Endpoint{Addresses: []string{"10.0.0.8"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true), Terminating: ptr.To(true)}}
	nilReady := discoveryv1.Endpoint{Addresses: []string{"10.0.0.9"}} // nil Ready means ready

	otherService := kubernetesSlice("other", discoveryv1.AddressTypeIPv4, epReady("10.0.0.1"))
	otherService.Labels[discoveryv1.LabelServiceName] = "not-kubernetes"
	otherNamespace := kubernetesSlice("elsewhere", discoveryv1.AddressTypeIPv4, epReady("10.0.0.2"))
	otherNamespace.Namespace = "kube-system"

	tests := []struct {
		name     string
		objs     []client.Object
		excluded map[string]bool
		want     []string
	}{
		{
			name: "sorted lowest first, de-duplicated",
			objs: []client.Object{kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv4,
				epReady("10.0.0.30"), epReady("10.0.0.4"), epReady("10.0.0.30"), nilReady)},
			want: []string{"https://10.0.0.4:9443", "https://10.0.0.9:9443", "https://10.0.0.30:9443"},
		},
		{
			name: "IPv6 is bracketed",
			objs: []client.Object{kubernetesSlice("kubernetes-v6", discoveryv1.AddressTypeIPv6, epReady("fd00:0::5"))},
			want: []string{"https://[fd00::5]:9443"},
		},
		{
			name: "not ready and terminating are skipped",
			objs: []client.Object{kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv4, notReady, terminating, epReady("10.0.0.5"))},
			want: []string{"https://10.0.0.5:9443"},
		},
		{
			name:     "a deleting Machine's address is skipped",
			objs:     []client.Object{kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv4, epReady("10.0.0.5", "10.0.0.6"))},
			excluded: map[string]bool{"10.0.0.5": true},
			want:     []string{"https://10.0.0.6:9443"},
		},
		{
			name: "non-IP, loopback and unspecified values are rejected",
			objs: []client.Object{kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv4,
				epReady("cp.example.com", "127.0.0.1", "0.0.0.0", "10.0.0.5; rm -rf /", "10.0.0.6"))},
			want: []string{"https://10.0.0.6:9443"},
		},
		{
			name: "FQDN slices are ignored",
			objs: []client.Object{kubernetesSlice("fqdn", discoveryv1.AddressTypeFQDN, epReady("cp.example.com"))},
			want: []string{},
		},
		{
			name: "only default/kubernetes slices count",
			objs: []client.Object{otherService, otherNamespace},
			want: []string{},
		},
		{
			name: "empty",
			want: []string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			wc := fake.NewClientBuilder().WithScheme(workloadTestScheme(g)).WithObjects(tc.objs...).Build()
			got, err := liveK0sJoinServers(context.Background(), wc, tc.excluded)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

func TestDeletingMachineAddresses(t *testing.T) {
	g := NewWithT(t)
	live := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "live"}}
	live.Status.Addresses = clusterv1.MachineAddresses{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.1"}}
	now := metav1.Now()
	gone := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "gone", DeletionTimestamp: &now}}
	gone.Status.Addresses = clusterv1.MachineAddresses{
		{Type: clusterv1.MachineInternalIP, Address: "10.0.0.2"},
		{Type: clusterv1.MachineInternalIP, Address: "fd00:0::2"},
		{Type: clusterv1.MachineHostName, Address: "gone"},
	}
	g.Expect(deletingMachineAddresses([]*clusterv1.Machine{live, gone})).To(Equal(map[string]bool{"10.0.0.2": true, "fd00::2": true}))
}

// --- policy fixture ----------------------------------------------------------

var policyNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// policyEnv is a management cluster plus a workload cluster for one k0s HA
// control plane named "c" with three replicas.
type policyEnv struct {
	t        *testing.T
	mgmt     client.Client
	wc       client.Client
	r        *KairosControlPlaneReconciler
	kcp      *controlplanev1beta2.KairosControlPlane
	cluster  *clusterv1.Cluster
	machines []*clusterv1.Machine
	recorder *record.FakeRecorder
	logs     *logSink
	// factoryCalls counts workload-client builds.
	factoryCalls int
}

type policyOpts struct {
	storedToken  string          // management data.token
	storedAnno   string          // management ID annotation
	workload     []client.Object // extra workload objects
	liveAddrs    []string        // default/kubernetes ready addresses
	machines     []*clusterv1.Machine
	configs      []client.Object // KairosConfigs
	etcdHealthy  []string        // node names reporting healthy+voting
	mgmtIntercep *interceptor.Funcs
}

type logSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *logSink) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.buf.WriteString(prefix + " " + args + "\n")
	}, funcr.Options{Verbosity: 10})
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func mgmtKubeconfigSecret(t *testing.T, ca []byte) *corev1.Secret {
	t.Helper()
	data, err := clientcmd.Write(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"c": {Server: "https://192.0.2.100:6443", CertificateAuthorityData: ca}},
		Contexts:       map[string]*clientcmdapi.Context{"c-admin@c": {Cluster: "c", AuthInfo: "c-admin"}},
		CurrentContext: "c-admin@c",
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"c-admin": {Token: "admin-credential"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "c-kubeconfig", Namespace: "default", Labels: map[string]string{clusterv1.ClusterNameLabel: "c"}},
		Type:       clusterv1.ClusterSecretType,
		Data:       map[string][]byte{"value": data},
	}
}

func newPolicyEnv(t *testing.T, o policyOpts) *policyEnv {
	t.Helper()
	g := NewWithT(t)
	scheme := haTestScheme(g)
	ca := fixtureCA(t)

	kcp := k0sKCP()
	conditions.MarkTrue(kcp, controlplanev1beta2.KubeconfigReadyCondition)
	cluster := testCluster()

	jt := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: joinTokenSecretName("c"), Namespace: "default", ResourceVersion: "",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "c", controlPlaneJoinTokenSecretTypeLabel: controlPlaneJoinTokenSecretTypeValue}},
		Data: map[string][]byte{},
	}
	if o.storedToken != "" {
		jt.Data[joinTokenSecretDataKey] = []byte(o.storedToken)
	}
	if o.storedAnno != "" {
		jt.Annotations = map[string]string{k0sJoinTokenIDAnnotation: o.storedAnno}
	}
	es := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: etcdStatusSecretName("c"), Namespace: "default"}, Data: map[string][]byte{}}
	for _, n := range append([]string{"cp-0"}, o.etcdHealthy...) {
		es.Data[n] = []byte(`{"name":"` + n + `","healthy":true,"voting":true,"members":3,"reportedAt":"t"}`)
	}

	objs := append([]client.Object{jt, es, mgmtKubeconfigSecret(t, ca)}, o.configs...)
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...)
	if o.mgmtIntercep != nil {
		b = b.WithInterceptorFuncs(*o.mgmtIntercep)
	}
	mgmt := b.Build()

	wobjs := append([]client.Object{}, o.workload...)
	if len(o.liveAddrs) > 0 {
		var eps []discoveryv1.Endpoint
		for _, a := range o.liveAddrs {
			eps = append(eps, epReady(a))
		}
		wobjs = append(wobjs, kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv4, eps...))
	}
	wc := fake.NewClientBuilder().WithScheme(workloadTestScheme(g)).WithObjects(wobjs...).Build()

	env := &policyEnv{t: t, mgmt: mgmt, wc: wc, kcp: kcp, cluster: cluster, recorder: record.NewFakeRecorder(100), logs: &logSink{}}
	init := cpMachine("cp-0", "cp-0", policyNow.Add(-48*time.Hour), "")
	env.machines = append([]*clusterv1.Machine{init}, o.machines...)
	env.r = &KairosControlPlaneReconciler{
		Client: mgmt, Scheme: scheme, Recorder: env.recorder,
		WorkloadClientFactory: func(context.Context, *clusterv1.Cluster) (client.Client, error) {
			env.factoryCalls++
			return wc, nil
		},
	}
	return env
}

// cpMachine builds a KCP-owned control-plane Machine whose bootstrap config is
// the KairosConfig of the same name. node "" means no NodeRef yet.
func cpMachine(name, node string, created time.Time, addr string) *clusterv1.Machine {
	m := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", CreationTimestamp: metav1.NewTime(created)},
		Spec: clusterv1.MachineSpec{Bootstrap: clusterv1.Bootstrap{ConfigRef: clusterv1.ContractVersionedObjectReference{
			APIGroup: bootstrapv1beta2.GroupVersion.Group, Kind: "KairosConfig", Name: name,
		}}},
	}
	if node != "" {
		m.Status.NodeRef = clusterv1.MachineNodeReference{Name: node}
	}
	if addr != "" {
		m.Status.Addresses = clusterv1.MachineAddresses{{Type: clusterv1.MachineInternalIP, Address: addr}}
	}
	return m
}

func joinConfig(name, tokenID string) *bootstrapv1beta2.KairosConfig {
	kc := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       bootstrapv1beta2.KairosConfigSpec{Role: "control-plane", Distribution: "k0s", ControlPlaneRole: bootstrapv1beta2.ControlPlaneRoleJoin},
	}
	if tokenID != "" {
		kc.Annotations = map[string]string{k0sJoinTokenIDAnnotation: tokenID}
	}
	return kc
}

// issued returns an encoded token for server plus its workload Secret expiring
// at expires.
func issued(t *testing.T, id, server string, expires time.Time) (string, *corev1.Secret) {
	t.Helper()
	tok := k0sBootstrapToken{id: id, secret: "0123456789" + id}
	enc, err := encodeK0sControllerJoinToken(server, fixtureCA(t), tok)
	if err != nil {
		t.Fatal(err)
	}
	return enc, newK0sControllerJoinTokenSecret(tok, expires)
}

func (e *policyEnv) mgmtSecret() *corev1.Secret {
	s := &corev1.Secret{}
	NewWithT(e.t).Expect(e.mgmt.Get(context.Background(), types.NamespacedName{Name: joinTokenSecretName("c"), Namespace: "default"}, s)).To(Succeed())
	return s
}

func (e *policyEnv) workloadTokenIDs() []string {
	list := &corev1.SecretList{}
	NewWithT(e.t).Expect(e.wc.List(context.Background(), list, client.InNamespace("kube-system"))).To(Succeed())
	var ids []string
	for _, s := range list.Items {
		if s.Type == corev1.SecretTypeBootstrapToken {
			ids = append(ids, string(s.Data["token-id"]))
		}
	}
	sort.Strings(ids)
	return ids
}

func (e *policyEnv) workloadToken(id string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	return s, e.wc.Get(context.Background(), bootstrapTokenSecretKeyFor(id), s)
}

func (e *policyEnv) events() []string {
	var out []string
	for {
		select {
		case ev := <-e.recorder.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func (e *policyEnv) ensure() (string, string, error) {
	return e.r.ensureK0sControllerJoinTokenAt(context.Background(), e.logs.logger(), e.kcp, e.cluster, e.machines, policyNow)
}

func (e *policyEnv) lifetime() {
	e.r.reconcileK0sJoinTokenLifetimeAt(context.Background(), e.logs.logger(), e.kcp, e.cluster, e.machines, policyNow)
}

// --- policy: no joiner pending (ensureK0sControllerJoinToken) ---------------

func TestEnsureK0sJoinToken_MintsWhenAbsent(t *testing.T) {
	g := NewWithT(t)
	e := newPolicyEnv(t, policyOpts{liveAddrs: []string{"10.0.0.21", "10.0.0.20"}})

	id, reason, err := e.ensure()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(reason).To(BeEmpty())
	g.Expect(id).To(MatchRegexp(`^[a-z0-9]{6}$`))

	s := e.mgmtSecret()
	g.Expect(s.Annotations).To(HaveKeyWithValue(k0sJoinTokenIDAnnotation, id))
	dec, err := decodeK0sControllerJoinToken(string(s.Data[joinTokenSecretDataKey]))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(dec.server).To(Equal("https://10.0.0.20:9443"), "lowest live address")
	g.Expect(dec.caData).To(Equal(fixtureCA(t)), "the CA from the workload kubeconfig")
	g.Expect(dec.token.id).To(Equal(id))

	ws, err := e.workloadToken(id)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(ws.Data["token-secret"])).To(Equal(dec.token.secret))
	g.Expect(string(ws.Data["expiration"])).To(Equal("2026-10-16T13:00:00Z"), "1h TTL")
	g.Expect(isK0sControllerJoinTokenSecret(ws)).To(BeTrue())
	g.Expect(e.events()).To(ContainElement(HavePrefix("Normal K0sJoinTokenMinted")))
}

func TestEnsureK0sJoinToken_HealthyTokenIsReused(t *testing.T) {
	g := NewWithT(t)
	enc, ws := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(45*time.Minute))
	e := newPolicyEnv(t, policyOpts{storedToken: enc, storedAnno: "aaaaaa", workload: []client.Object{ws}, liveAddrs: []string{"10.0.0.21", "10.0.0.20"}})
	before := e.mgmtSecret()

	id, reason, err := e.ensure()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(reason).To(BeEmpty())
	g.Expect(id).To(Equal("aaaaaa"))
	after := e.mgmtSecret()
	g.Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "no write")
	g.Expect(e.workloadTokenIDs()).To(Equal([]string{"aaaaaa"}))
}

func TestEnsureK0sJoinToken_RotatesAndCleansUp(t *testing.T) {
	type tc struct {
		name     string
		opts     func(t *testing.T) policyOpts
		oldID    string
		wantGone bool
	}
	live := []string{"10.0.0.20", "10.0.0.21"}
	tests := []tc{
		{
			name: "legacy token from the init node",
			opts: func(t *testing.T) policyOpts {
				enc, ws := issued(t, "legacy", "https://10.0.0.20:9443", policyNow.Add(20*time.Hour))
				return policyOpts{storedToken: enc, workload: []client.Object{ws}, liveAddrs: live}
			},
			oldID: "legacy", wantGone: true,
		},
		{
			name: "less than 30m left",
			opts: func(t *testing.T) policyOpts {
				enc, ws := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(29*time.Minute))
				return policyOpts{storedToken: enc, storedAnno: "aaaaaa", workload: []client.Object{ws}, liveAddrs: live}
			},
			oldID: "aaaaaa", wantGone: true,
		},
		{
			name: "server is no longer a live member",
			opts: func(t *testing.T) policyOpts {
				enc, ws := issued(t, "aaaaaa", "https://10.0.0.9:9443", policyNow.Add(50*time.Minute))
				return policyOpts{storedToken: enc, storedAnno: "aaaaaa", workload: []client.Object{ws}, liveAddrs: live}
			},
			oldID: "aaaaaa", wantGone: true,
		},
		{
			name: "server belongs to a Machine being deleted",
			opts: func(t *testing.T) policyOpts {
				enc, ws := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(50*time.Minute))
				gone := cpMachine("cp-old", "cp-old", policyNow.Add(-72*time.Hour), "10.0.0.20")
				gone.DeletionTimestamp = ptr.To(metav1.NewTime(policyNow))
				return policyOpts{storedToken: enc, storedAnno: "aaaaaa", workload: []client.Object{ws}, liveAddrs: live, machines: []*clusterv1.Machine{gone}}
			},
			oldID: "aaaaaa", wantGone: true,
		},
		{
			name: "issued token expired and was cleaned up",
			opts: func(t *testing.T) policyOpts {
				enc, _ := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(-time.Minute))
				return policyOpts{storedToken: enc, storedAnno: "aaaaaa", liveAddrs: live}
			},
			oldID: "aaaaaa", wantGone: true,
		},
		{
			name: "annotation names a different token",
			opts: func(t *testing.T) policyOpts {
				enc, ws := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(50*time.Minute))
				return policyOpts{storedToken: enc, storedAnno: "bbbbbb", workload: []client.Object{ws}, liveAddrs: live}
			},
			oldID: "bbbbbb", wantGone: true,
		},
		{
			name: "workload Secret no longer matches the stored token",
			opts: func(t *testing.T) policyOpts {
				enc, ws := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(50*time.Minute))
				ws.Data["token-secret"] = []byte("ffffffffffffffff")
				return policyOpts{storedToken: enc, storedAnno: "aaaaaa", workload: []client.Object{ws}, liveAddrs: live}
			},
			oldID: "aaaaaa", wantGone: true,
		},
		{
			name: "stored value is not a k0s token",
			opts: func(t *testing.T) policyOpts {
				return policyOpts{storedToken: "not-a-token", storedAnno: "aaaaaa", liveAddrs: live}
			},
			oldID: "aaaaaa",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			e := newPolicyEnv(t, tc.opts(t))
			id, reason, err := e.ensure()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(reason).To(BeEmpty())
			g.Expect(id).To(MatchRegexp(`^[a-z0-9]{6}$`))
			g.Expect(id).ToNot(Equal(tc.oldID))

			s := e.mgmtSecret()
			g.Expect(s.Annotations).To(HaveKeyWithValue(k0sJoinTokenIDAnnotation, id))
			dec, err := decodeK0sControllerJoinToken(string(s.Data[joinTokenSecretDataKey]))
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(dec.token.id).To(Equal(id))
			g.Expect(dec.server).To(BeElementOf("https://10.0.0.20:9443", "https://10.0.0.21:9443"))
			g.Expect(e.workloadTokenIDs()).To(ContainElement(id))
			if tc.wantGone {
				_, err := e.workloadToken(tc.oldID)
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "superseded token %s must be deleted", tc.oldID)
			}
		})
	}
}

func TestEnsureK0sJoinToken_NoLiveMemberMintsNothing(t *testing.T) {
	g := NewWithT(t)
	e := newPolicyEnv(t, policyOpts{})
	before := e.mgmtSecret()

	id, reason, err := e.ensure()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(BeEmpty())
	g.Expect(reason).To(ContainSubstring("no live control-plane member"))
	g.Expect(e.mgmtSecret().ResourceVersion).To(Equal(before.ResourceVersion))
	g.Expect(e.workloadTokenIDs()).To(BeEmpty())
}

func TestEnsureK0sJoinToken_NeverMintsOverAPendingJoiner(t *testing.T) {
	g := NewWithT(t)
	enc, ws := issued(t, "aaaaaa", "https://10.0.0.9:9443", policyNow.Add(10*time.Minute))
	e := newPolicyEnv(t, policyOpts{
		storedToken: enc, storedAnno: "aaaaaa", workload: []client.Object{ws}, liveAddrs: []string{"10.0.0.20"},
		machines: []*clusterv1.Machine{cpMachine("cp-1", "", policyNow.Add(-time.Hour), "")},
		configs:  []client.Object{joinConfig("cp-1", "aaaaaa")},
	})
	before := e.mgmtSecret()

	id, reason, err := e.ensure()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(BeEmpty())
	g.Expect(reason).To(ContainSubstring("cp-1"))
	g.Expect(e.mgmtSecret().ResourceVersion).To(Equal(before.ResourceVersion))
	g.Expect(e.workloadTokenIDs()).To(Equal([]string{"aaaaaa"}), "the pending joiner's token is never deleted")
}

// TestEnsureK0sJoinToken_ConflictLeavesNoOrphan: a reconcile working from a
// stale view of the management Secret loses the optimistic-lock race. It must
// not report a token, and the bootstrap token it created is removed again.
func TestEnsureK0sJoinToken_ConflictLeavesNoOrphan(t *testing.T) {
	g := NewWithT(t)
	e := newPolicyEnv(t, policyOpts{
		liveAddrs: []string{"10.0.0.20"},
		mgmtIntercep: &interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, obj.GetName(), fmt.Errorf("the object has been modified"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		}},
	})

	id, reason, err := e.ensure()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(BeEmpty())
	g.Expect(reason).To(ContainSubstring("retrying"))
	g.Expect(e.mgmtSecret().Annotations).ToNot(HaveKey(k0sJoinTokenIDAnnotation))
	g.Expect(e.workloadTokenIDs()).To(BeEmpty())
}

// --- policy: pending joiners (reconcileK0sJoinTokenLifetime) ----------------

func TestK0sJoinTokenLifetime_ExtendsPendingJoinerInPlace(t *testing.T) {
	g := NewWithT(t)
	enc, ws := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(29*time.Minute))
	e := newPolicyEnv(t, policyOpts{
		storedToken: enc, storedAnno: "aaaaaa", workload: []client.Object{ws}, liveAddrs: []string{"10.0.0.20"},
		machines: []*clusterv1.Machine{cpMachine("cp-1", "", policyNow.Add(-2*time.Hour), "")},
		configs:  []client.Object{joinConfig("cp-1", "aaaaaa")},
	})
	before := e.mgmtSecret()

	e.lifetime()

	got, err := e.workloadToken("aaaaaa")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(got.Data["expiration"])).To(Equal("2026-10-16T13:00:00Z"), "extended to now+1h")
	g.Expect(got.Data["token-secret"]).To(Equal(ws.Data["token-secret"]), "same token, extended in place")
	after := e.mgmtSecret()
	g.Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
	g.Expect(after.Data[joinTokenSecretDataKey]).To(Equal(before.Data[joinTokenSecretDataKey]))
	g.Expect(e.workloadTokenIDs()).To(Equal([]string{"aaaaaa"}))
}

func TestK0sJoinTokenLifetime_LeavesTokenWithTimeLeft(t *testing.T) {
	g := NewWithT(t)
	enc, ws := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(31*time.Minute))
	e := newPolicyEnv(t, policyOpts{
		storedToken: enc, storedAnno: "aaaaaa", workload: []client.Object{ws}, liveAddrs: []string{"10.0.0.20"},
		machines: []*clusterv1.Machine{cpMachine("cp-1", "cp-1", policyNow.Add(-2*time.Hour), "")}, // NodeRef but no etcd report
		configs:  []client.Object{joinConfig("cp-1", "aaaaaa")},
	})
	before, err := e.workloadToken("aaaaaa")
	g.Expect(err).ToNot(HaveOccurred())
	e.lifetime()
	got, err := e.workloadToken("aaaaaa")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got.Data["expiration"]).To(Equal(ws.Data["expiration"]))
	g.Expect(got.ResourceVersion).To(Equal(before.ResourceVersion), "untouched")
}

func TestK0sJoinTokenLifetime_CapStopsExtending(t *testing.T) {
	g := NewWithT(t)
	enc, ws := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(10*time.Minute))
	e := newPolicyEnv(t, policyOpts{
		storedToken: enc, storedAnno: "aaaaaa", workload: []client.Object{ws}, liveAddrs: []string{"10.0.0.20"},
		machines: []*clusterv1.Machine{cpMachine("cp-1", "", policyNow.Add(-24*time.Hour), "")},
		configs:  []client.Object{joinConfig("cp-1", "aaaaaa")},
	})
	e.lifetime()
	got, err := e.workloadToken("aaaaaa")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got.Data["expiration"]).To(Equal(ws.Data["expiration"]), "not extended past the 24h cap")
	g.Expect(e.events()).To(ContainElement(SatisfyAll(HavePrefix("Warning K0sJoinTokenExtensionCapped"), ContainSubstring("cp-1"))))
}

func TestK0sJoinTokenLifetime_DeadServerKeepsTokenAndSaysWhy(t *testing.T) {
	g := NewWithT(t)
	enc, ws := issued(t, "aaaaaa", "https://10.0.0.9:9443", policyNow.Add(10*time.Minute))
	e := newPolicyEnv(t, policyOpts{
		storedToken: enc, storedAnno: "aaaaaa", workload: []client.Object{ws}, liveAddrs: []string{"10.0.0.20"},
		machines: []*clusterv1.Machine{cpMachine("cp-1", "", policyNow.Add(-time.Hour), "")},
		configs:  []client.Object{joinConfig("cp-1", "aaaaaa")},
	})
	before := e.mgmtSecret()
	e.lifetime()

	after := e.mgmtSecret()
	g.Expect(after.Data[joinTokenSecretDataKey]).To(Equal(before.Data[joinTokenSecretDataKey]), "the joiner's token is kept")
	g.Expect(after.Annotations).To(HaveKeyWithValue(k0sJoinTokenIDAnnotation, "aaaaaa"))
	g.Expect(e.workloadTokenIDs()).To(Equal([]string{"aaaaaa"}))
	g.Expect(e.events()).To(ContainElement(SatisfyAll(HavePrefix("Warning K0sJoinTokenServerUnavailable"),
		ContainSubstring("cp-1"), ContainSubstring("10.0.0.9"))))
}

func TestK0sJoinTokenLifetime_NeverExtendsLegacyJoiner(t *testing.T) {
	g := NewWithT(t)
	enc, ws := issued(t, "legacy", "https://10.0.0.9:9443", policyNow.Add(10*time.Minute))
	e := newPolicyEnv(t, policyOpts{
		storedToken: enc, workload: []client.Object{ws}, liveAddrs: []string{"10.0.0.20"},
		machines: []*clusterv1.Machine{cpMachine("cp-4", "", policyNow.Add(-time.Hour), "")},
		configs:  []client.Object{joinConfig("cp-4", "")},
	})
	e.lifetime()

	got, err := e.workloadToken("legacy")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got.Data["expiration"]).To(Equal(ws.Data["expiration"]))
	g.Expect(e.factoryCalls).To(BeZero(), "nothing to extend, so the workload cluster is not contacted")
	g.Expect(e.events()).To(ContainElement(SatisfyAll(HavePrefix("Warning K0sJoinTokenLegacy"), ContainSubstring("cp-4"))))
}

func TestK0sJoinTokenLifetime_MissingTokenSaysWhy(t *testing.T) {
	g := NewWithT(t)
	enc, _ := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(10*time.Minute))
	e := newPolicyEnv(t, policyOpts{
		storedToken: enc, storedAnno: "aaaaaa", liveAddrs: []string{"10.0.0.20"},
		machines: []*clusterv1.Machine{cpMachine("cp-1", "", policyNow.Add(-time.Hour), "")},
		configs:  []client.Object{joinConfig("cp-1", "aaaaaa")},
	})
	e.lifetime()
	g.Expect(e.events()).To(ContainElement(SatisfyAll(HavePrefix("Warning K0sJoinTokenMissing"), ContainSubstring("cp-1"))))
	g.Expect(e.workloadTokenIDs()).To(BeEmpty(), "a missing token is reported, not re-created")
}

func TestK0sJoinTokenLifetime_JoinedOrDeletingMembersAreNotPending(t *testing.T) {
	g := NewWithT(t)
	enc, ws := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(10*time.Minute))
	deleting := cpMachine("cp-2", "", policyNow.Add(-time.Hour), "")
	deleting.DeletionTimestamp = ptr.To(metav1.NewTime(policyNow))
	e := newPolicyEnv(t, policyOpts{
		storedToken: enc, storedAnno: "aaaaaa", workload: []client.Object{ws}, liveAddrs: []string{"10.0.0.20"},
		machines:    []*clusterv1.Machine{cpMachine("cp-1", "cp-1", policyNow.Add(-time.Hour), ""), deleting},
		configs:     []client.Object{joinConfig("cp-1", "aaaaaa"), joinConfig("cp-2", "aaaaaa")},
		etcdHealthy: []string{"cp-1"},
	})
	e.lifetime()
	got, err := e.workloadToken("aaaaaa")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got.Data["expiration"]).To(Equal(ws.Data["expiration"]), "no pending joiner, so the idle token lapses")
	g.Expect(e.factoryCalls).To(BeZero())
}

// TestReconcileMachines_RequeuesWhileAK0sJoinerIsPending: once every replica
// exists, the join gate no longer runs, so nothing else requeues the KCP while
// the last joiner boots and the KCP has no resync. reconcileMachines must keep
// coming back, often enough to extend that joiner's token, until it has joined.
func TestReconcileMachines_RequeuesWhileAK0sJoinerIsPending(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := machineMetaKCP("k0s", nil)
	kcp.Spec.Replicas = ptr.To(int32(3))
	conditions.MarkTrue(kcp, controlplanev1beta2.KubeconfigReadyCondition)
	cluster := rolloutCluster()

	withConfig := func(m *clusterv1.Machine) *clusterv1.Machine {
		m.Spec.Bootstrap.ConfigRef = clusterv1.ContractVersionedObjectReference{
			APIGroup: bootstrapv1beta2.GroupVersion.Group, Kind: "KairosConfig", Name: m.Name,
		}
		return m
	}
	m0 := withConfig(rolloutMachine(kcp, "test-kcp-0", kcp.Spec.Version, 3*time.Hour, true))
	m1 := withConfig(rolloutMachine(kcp, "test-kcp-1", kcp.Spec.Version, 2*time.Hour, true))
	m2 := withConfig(rolloutMachine(kcp, "test-kcp-2", kcp.Spec.Version, 10*time.Minute, false))
	initCfg := joinConfig("test-kcp-0", "")
	initCfg.Spec.ControlPlaneRole = bootstrapv1beta2.ControlPlaneRoleInit
	enc, ws := issued(t, "aaaaaa", "https://10.0.0.20:9443", time.Now().Add(50*time.Minute))
	jt := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: joinTokenSecretName(cluster.Name), Namespace: "default",
			Annotations: map[string]string{k0sJoinTokenIDAnnotation: "aaaaaa"}},
		Data: map[string][]byte{joinTokenSecretDataKey: []byte(enc)},
	}
	wc := fake.NewClientBuilder().WithScheme(workloadTestScheme(g)).
		WithObjects(ws, kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv4, epReady("10.0.0.20"))).Build()

	run := func(objs ...client.Object) ctrl.Result {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme, WorkloadClientFactory: staticWorkloadClient(wc)}
		res, err := r.reconcileMachines(context.Background(), logr.Discard(), kcp, cluster)
		g.Expect(err).NotTo(HaveOccurred())
		return res
	}
	common := []client.Object{m0, m1, initCfg, joinConfig("test-kcp-1", "aaaaaa"), joinConfig("test-kcp-2", "aaaaaa"), jt, machineMetaInfraTemplate()}

	// test-kcp-2 has no Node yet: keep reconciling.
	res := run(append(common, m2.DeepCopy(), rolloutEtcdStatus(cluster, m0, m1))...)
	g.Expect(res.RequeueAfter).To(BeNumerically(">", 0))
	g.Expect(res.RequeueAfter).To(BeNumerically("<=", k0sJoinTokenRecheckInterval))

	// Joined and reporting a healthy voting member: nothing left to watch.
	joined := m2.DeepCopy()
	joined.Status.NodeRef = clusterv1.MachineNodeReference{Name: "test-kcp-2-node"}
	joined.Status.Phase = string(clusterv1.MachinePhaseRunning)
	res = run(append(common, joined, rolloutEtcdStatus(cluster, m0, m1, joined))...)
	g.Expect(res.RequeueAfter).To(BeZero())
}

// TestK0sJoinToken_NeverLogged drives every path that handles token material
// (mint, rotate with cleanup, extend, a failed store) and checks that no log
// line and no event carries the token, its secret half, or the encoded value.
func TestK0sJoinToken_NeverLogged(t *testing.T) {
	g := NewWithT(t)
	var secrets []string
	collect := func(e *policyEnv) {
		s := e.mgmtSecret()
		if raw := string(s.Data[joinTokenSecretDataKey]); raw != "" {
			secrets = append(secrets, raw)
			if dec, err := decodeK0sControllerJoinToken(raw); err == nil {
				secrets = append(secrets, dec.token.bearer(), dec.token.secret)
			}
		}
		list := &corev1.SecretList{}
		g.Expect(e.wc.List(context.Background(), list)).To(Succeed())
		for _, ws := range list.Items {
			if v := string(ws.Data["token-secret"]); v != "" {
				secrets = append(secrets, v)
			}
		}
	}
	var output []string
	finish := func(e *policyEnv) {
		output = append(output, e.logs.String())
		output = append(output, e.events()...)
	}

	// Mint from scratch.
	e1 := newPolicyEnv(t, policyOpts{liveAddrs: []string{"10.0.0.20"}})
	_, _, err := e1.ensure()
	g.Expect(err).ToNot(HaveOccurred())
	collect(e1)
	finish(e1)

	// Rotate a legacy token, including the superseded-token cleanup.
	enc, ws := issued(t, "legacy", "https://10.0.0.9:9443", policyNow.Add(time.Hour))
	secrets = append(secrets, enc, string(ws.Data["token-secret"]))
	e2 := newPolicyEnv(t, policyOpts{storedToken: enc, workload: []client.Object{ws}, liveAddrs: []string{"10.0.0.20"}})
	_, _, err = e2.ensure()
	g.Expect(err).ToNot(HaveOccurred())
	collect(e2)
	finish(e2)

	// Extend a pending joiner's token, with a dead server and a legacy joiner.
	enc3, ws3 := issued(t, "aaaaaa", "https://10.0.0.9:9443", policyNow.Add(time.Minute))
	secrets = append(secrets, enc3, string(ws3.Data["token-secret"]))
	e3 := newPolicyEnv(t, policyOpts{
		storedToken: enc3, storedAnno: "aaaaaa", workload: []client.Object{ws3}, liveAddrs: []string{"10.0.0.20"},
		machines: []*clusterv1.Machine{cpMachine("cp-1", "", policyNow.Add(-time.Hour), ""), cpMachine("cp-2", "", policyNow.Add(-time.Hour), "")},
		configs:  []client.Object{joinConfig("cp-1", "aaaaaa"), joinConfig("cp-2", "")},
	})
	e3.lifetime()
	finish(e3)

	// A store that fails after the workload token was created.
	e4 := newPolicyEnv(t, policyOpts{
		liveAddrs: []string{"10.0.0.20"},
		mgmtIntercep: &interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if s, ok := obj.(*corev1.Secret); ok {
				for _, v := range s.Data {
					secrets = append(secrets, string(v))
					if dec, err := decodeK0sControllerJoinToken(string(v)); err == nil {
						secrets = append(secrets, dec.token.bearer(), dec.token.secret)
					}
				}
				return fmt.Errorf("injected failure")
			}
			return c.Patch(ctx, obj, patch, opts...)
		}},
	})
	_, reason, err := e4.ensure()
	g.Expect(err).ToNot(HaveOccurred())
	output = append(output, reason)
	finish(e4)

	g.Expect(secrets).ToNot(BeEmpty())
	all := strings.Join(output, "\n")
	g.Expect(all).To(ContainSubstring("Minted k0s controller-join token"), "the sink did capture the controller's logs")
	for _, s := range secrets {
		if len(s) < 8 {
			continue
		}
		g.Expect(all).ToNot(ContainSubstring(s))
	}
}

// --- gate and KairosConfig stamping ------------------------------------------

// TestAdmitJoiner_K0sStampsTheIssuedTokenOnTheJoinConfig runs the whole create
// path: the gate opens, a token is minted, and the new join KairosConfig names
// it. k3s joins get no annotation.
func TestAdmitJoiner_K0sStampsTheIssuedTokenOnTheJoinConfig(t *testing.T) {
	g := NewWithT(t)
	e := newPolicyEnv(t, policyOpts{liveAddrs: []string{"10.0.0.20"}})
	g.Expect(e.mgmt.Create(context.Background(), fleetMachineTemplate())).To(Succeed())
	e.kcp.Spec.MachineTemplate.InfrastructureRef = fleetTemplateRef()
	init := e.machines[0]
	init.Status.NodeRef = clusterv1.MachineNodeReference{Name: "cp-0"}

	admission, err := e.r.admitJoiner(context.Background(), e.logs.logger(), e.kcp, e.cluster, e.machines)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(admission.admitted).To(BeTrue(), admission.reason)
	g.Expect(admission.k0sJoinTokenID).To(MatchRegexp(`^[a-z0-9]{6}$`))

	g.Expect(e.r.createControlPlaneMachine(context.Background(), e.logs.logger(), e.kcp, e.cluster, 1,
		bootstrapv1beta2.ControlPlaneRoleJoin, admission.k0sJoinTokenID)).To(Succeed())
	kc := &bootstrapv1beta2.KairosConfig{}
	g.Expect(e.mgmt.Get(context.Background(), types.NamespacedName{Name: "kcp-1", Namespace: "default"}, kc)).To(Succeed())
	g.Expect(kc.Annotations).To(HaveKeyWithValue(k0sJoinTokenIDAnnotation, admission.k0sJoinTokenID))
	g.Expect(e.mgmtSecret().Annotations).To(HaveKeyWithValue(k0sJoinTokenIDAnnotation, admission.k0sJoinTokenID))

	// k3s: the gate admits without a token ID and nothing is minted.
	k3s := e.kcp.DeepCopy()
	k3s.Spec.Distribution = "k3s"
	admission, err = e.r.admitJoiner(context.Background(), e.logs.logger(), k3s, e.cluster, e.machines)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(admission.admitted).To(BeTrue())
	g.Expect(admission.k0sJoinTokenID).To(BeEmpty())
}

// TestCreateControlPlaneMachine_RestampsAnUnrenderedJoinConfig: a create that is
// retried after a partial failure finds its KairosConfig already there, possibly
// naming a token that has since been replaced. Until the config is rendered it
// follows the current token; once rendered it is left alone.
func TestCreateControlPlaneMachine_RestampsAnUnrenderedJoinConfig(t *testing.T) {
	for _, rendered := range []bool{false, true} {
		t.Run(fmt.Sprintf("rendered=%v", rendered), func(t *testing.T) {
			g := NewWithT(t)
			stale := joinConfig("kcp-1", "oldold")
			stale.Labels = map[string]string{clusterv1.ClusterNameLabel: "c"}
			if rendered {
				stale.Status.DataSecretName = ptr.To("kcp-1")
			}
			e := newPolicyEnv(t, policyOpts{configs: []client.Object{stale}})
			g.Expect(e.mgmt.Create(context.Background(), fleetMachineTemplate())).To(Succeed())
			e.kcp.Spec.MachineTemplate.InfrastructureRef = fleetTemplateRef()

			g.Expect(e.r.createControlPlaneMachine(context.Background(), e.logs.logger(), e.kcp, e.cluster, 1,
				bootstrapv1beta2.ControlPlaneRoleJoin, "newnew")).To(Succeed())
			kc := &bootstrapv1beta2.KairosConfig{}
			g.Expect(e.mgmt.Get(context.Background(), types.NamespacedName{Name: "kcp-1", Namespace: "default"}, kc)).To(Succeed())
			want := "newnew"
			if rendered {
				want = "oldold"
			}
			g.Expect(kc.Annotations).To(HaveKeyWithValue(k0sJoinTokenIDAnnotation, want))
		})
	}
}

// TestInitMachineJoinable_NoLongerReadsTheJoinToken: the gate opens on node,
// kubeconfig and etcd health alone. Whether a token exists is admitJoiner's
// concern, so an empty join-token Secret no longer holds a k0s joiner back.
func TestInitMachineJoinable_NoLongerReadsTheJoinToken(t *testing.T) {
	g := NewWithT(t)
	e := newPolicyEnv(t, policyOpts{})
	e.machines[0].Status.NodeRef = clusterv1.MachineNodeReference{Name: "cp-0"}
	joinable, reason, err := e.r.initMachineJoinable(context.Background(), e.kcp, e.cluster, e.machines)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(joinable).To(BeTrue(), reason)
	g.Expect(e.factoryCalls).To(BeZero(), "the gate itself never contacts the workload cluster")
}

func fleetTemplateRef() corev1.ObjectReference {
	return corev1.ObjectReference{
		APIVersion: "infrastructure.cluster.x-k8s.io/v1alpha1",
		Kind:       "KairosFleetMachineTemplate",
		Name:       "cp-tmpl",
		Namespace:  "default",
	}
}

func fleetMachineTemplate() *unstructured.Unstructured {
	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Kind: "KairosFleetMachineTemplate"})
	tmpl.SetName("cp-tmpl")
	tmpl.SetNamespace("default")
	_ = unstructured.SetNestedMap(tmpl.Object, map[string]interface{}{"group": "control-plane"}, "spec", "template", "spec")
	return tmpl
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

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

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/k0sjointoken"
)

// testCAPEM stands in for the cluster CA. The controller treats it as opaque
// bytes: it copies it from the workload kubeconfig into the join token.
var testCAPEM = []byte("-----BEGIN CERTIFICATE-----\nZml4dHVyZS1jYQ==\n-----END CERTIFICATE-----\n")

func fixtureCA(*testing.T) []byte { return testCAPEM }

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

// memberMachine is a control-plane Machine of this KCP reporting addrs.
func memberMachine(name string, deleting bool, addrs ...string) *clusterv1.Machine {
	m := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
	for _, a := range addrs {
		m.Status.Addresses = append(m.Status.Addresses, clusterv1.MachineAddress{Type: clusterv1.MachineInternalIP, Address: a})
	}
	if deleting {
		m.DeletionTimestamp = ptr.To(metav1.Now())
	}
	return m
}

func TestLiveK0sJoinServers(t *testing.T) {
	notReady := discoveryv1.Endpoint{Addresses: []string{"10.0.0.7"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false)}}
	terminating := discoveryv1.Endpoint{Addresses: []string{"10.0.0.8"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true), Terminating: ptr.To(true)}}
	nilReady := discoveryv1.Endpoint{Addresses: []string{"10.0.0.9"}} // nil Ready means ready

	// A slice some other controller writes, with the same service-name label
	// and a lower address that is even one of our Machines'.
	foreign := kubernetesSlice("kubernetes-extra", discoveryv1.AddressTypeIPv4, epReady("10.0.0.1"))
	otherNamespace := kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv4, epReady("10.0.0.2"))
	otherNamespace.Namespace = "kube-system"

	all := memberMachine("cp-0", false, "10.0.0.1", "10.0.0.2", "10.0.0.4", "10.0.0.5", "10.0.0.6",
		"10.0.0.7", "10.0.0.8", "10.0.0.9", "10.0.0.30", "fd00::5")

	tests := []struct {
		name          string
		objs          []client.Object
		machines      []*clusterv1.Machine
		want          []string
		wantUnmatched int
	}{
		{
			name: "sorted lowest first, de-duplicated",
			objs: []client.Object{kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv4,
				epReady("10.0.0.30"), epReady("10.0.0.4"), epReady("10.0.0.30"), nilReady)},
			machines: []*clusterv1.Machine{all},
			want:     []string{"https://10.0.0.4:9443", "https://10.0.0.9:9443", "https://10.0.0.30:9443"},
		},
		{
			name:     "IPv6 is bracketed",
			objs:     []client.Object{kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv6, epReady("fd00:0::5"))},
			machines: []*clusterv1.Machine{all},
			want:     []string{"https://[fd00::5]:9443"},
		},
		{
			name:     "not ready and terminating are skipped",
			objs:     []client.Object{kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv4, notReady, terminating, epReady("10.0.0.5"))},
			machines: []*clusterv1.Machine{all},
			want:     []string{"https://10.0.0.5:9443"},
		},
		{
			name:     "only the slice named kubernetes is read; a foreign slice with the same label is ignored",
			objs:     []client.Object{foreign, kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv4, epReady("10.0.0.5"))},
			machines: []*clusterv1.Machine{all},
			want:     []string{"https://10.0.0.5:9443"},
		},
		{
			name:          "an address that is no Machine of ours is ignored",
			objs:          []client.Object{kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv4, epReady("192.0.2.66", "10.0.0.5"))},
			machines:      []*clusterv1.Machine{memberMachine("cp-0", false, "10.0.0.5")},
			want:          []string{"https://10.0.0.5:9443"},
			wantUnmatched: 1,
		},
		{
			name:          "a Machine being deleted is not a target, even if another Machine reports its address",
			objs:          []client.Object{kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv4, epReady("10.0.0.5", "10.0.0.6"))},
			machines:      []*clusterv1.Machine{memberMachine("cp-0", false, "10.0.0.5", "10.0.0.6"), memberMachine("cp-old", true, "10.0.0.5")},
			want:          []string{"https://10.0.0.6:9443"},
			wantUnmatched: 1,
		},
		{
			name: "non-IP, loopback and unspecified values are rejected",
			objs: []client.Object{kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv4,
				epReady("cp.example.com", "127.0.0.1", "0.0.0.0", "10.0.0.5; rm -rf /", "10.0.0.6"))},
			machines: []*clusterv1.Machine{all},
			want:     []string{"https://10.0.0.6:9443"},
		},
		{
			name:          "no address is one of ours",
			objs:          []client.Object{kubernetesSlice("kubernetes", discoveryv1.AddressTypeIPv4, epReady("10.0.0.5", "10.0.0.6"))},
			machines:      []*clusterv1.Machine{memberMachine("cp-0", false)},
			wantUnmatched: 2,
		},
		{
			name:     "FQDN address type is ignored",
			objs:     []client.Object{kubernetesSlice("kubernetes", discoveryv1.AddressTypeFQDN, epReady("cp.example.com"))},
			machines: []*clusterv1.Machine{all},
		},
		{
			name:     "only default/kubernetes counts",
			objs:     []client.Object{foreign, otherNamespace},
			machines: []*clusterv1.Machine{all},
		},
		{
			name:     "no slice at all",
			machines: []*clusterv1.Machine{all},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			wc := fake.NewClientBuilder().WithScheme(workloadTestScheme(g)).WithObjects(tc.objs...).Build()
			got, err := liveK0sJoinServers(context.Background(), wc, tc.machines)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(got.urls).To(Equal(tc.want))
			g.Expect(got.unmatched).To(Equal(tc.wantUnmatched))
		})
	}
}

func TestOwnMemberAddresses(t *testing.T) {
	g := NewWithT(t)
	live := memberMachine("live", false, "10.0.0.1", "10.0.0.3")
	live.Status.Addresses = append(live.Status.Addresses, clusterv1.MachineAddress{Type: clusterv1.MachineHostName, Address: "live"})
	gone := memberMachine("gone", true, "10.0.0.2", "fd00:0::2", "10.0.0.3")
	g.Expect(ownMemberAddresses([]*clusterv1.Machine{live, gone})).To(Equal(map[string]bool{"10.0.0.1": true}))
}

func mustToken(t *testing.T, id, secret string) k0sjointoken.BootstrapToken {
	t.Helper()
	tok, err := k0sjointoken.NewBootstrapToken(id, secret)
	if err != nil {
		t.Fatalf("token %s: %v", id, err)
	}
	return tok
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
	memberAddrs  []string        // the init Machine's status.addresses; nil means liveAddrs
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
	members := o.memberAddrs
	if members == nil {
		members = o.liveAddrs
	}
	for _, a := range members {
		init.Status.Addresses = append(init.Status.Addresses, clusterv1.MachineAddress{Type: clusterv1.MachineInternalIP, Address: a})
	}
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
	tok := mustToken(t, id, "0123456789"+id)
	enc, err := k0sjointoken.Encode(server, fixtureCA(t), tok)
	if err != nil {
		t.Fatal(err)
	}
	return enc, k0sjointoken.NewSecret(tok, expires)
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
	return s, e.wc.Get(context.Background(), k0sjointoken.SecretKeyFor(id), s)
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
	dec, err := k0sjointoken.Decode(string(s.Data[joinTokenSecretDataKey]))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(dec.Server).To(Equal("https://10.0.0.20:9443"), "lowest live address")
	g.Expect(dec.CAData).To(Equal(fixtureCA(t)), "the CA from the workload kubeconfig")
	g.Expect(dec.Token.ID()).To(Equal(id))

	ws, err := e.workloadToken(id)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(ws.Data["token-secret"])).To(Equal(dec.Token.Secret()))
	g.Expect(string(ws.Data["expiration"])).To(Equal("2026-10-16T13:00:00Z"), "1h TTL")
	g.Expect(k0sjointoken.IsControllerJoinOnly(ws)).To(BeTrue())
	g.Expect(e.events()).To(ContainElement(HavePrefix("Normal K0sJoinTokenMinted")))
}

// TestEnsureK0sJoinToken_UnboundTokenIsReused: a healthy token that no Machine
// was created with yet (an admission whose Machine create failed and is being
// retried) is still this joiner's, so it is reused rather than re-minted.
func TestEnsureK0sJoinToken_UnboundTokenIsReused(t *testing.T) {
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

// TestEnsureK0sJoinToken_NeverReusesATokenGivenToAMachine: one token per
// joiner. Once a Machine exists with the stored token, the next joiner gets a
// new one even though the stored token is otherwise healthy.
func TestEnsureK0sJoinToken_NeverReusesATokenGivenToAMachine(t *testing.T) {
	for _, state := range []string{"joined", "deleting"} {
		t.Run(state, func(t *testing.T) {
			g := NewWithT(t)
			enc, ws := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(50*time.Minute))
			prev := cpMachine("cp-1", "cp-1", policyNow.Add(-time.Hour), "")
			healthy := []string{"cp-1"}
			if state == "deleting" {
				prev.DeletionTimestamp = ptr.To(metav1.NewTime(policyNow))
				healthy = nil
			}
			e := newPolicyEnv(t, policyOpts{
				storedToken: enc, storedAnno: "aaaaaa", workload: []client.Object{ws}, liveAddrs: []string{"10.0.0.20"},
				machines: []*clusterv1.Machine{prev}, configs: []client.Object{joinConfig("cp-1", "aaaaaa")}, etcdHealthy: healthy,
			})
			id, reason, err := e.ensure()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(reason).To(BeEmpty())
			g.Expect(id).To(MatchRegexp(`^[a-z0-9]{6}$`))
			g.Expect(id).ToNot(Equal("aaaaaa"), "a token is given to one joiner only")
			g.Expect(e.mgmtSecret().Annotations).To(HaveKeyWithValue(k0sJoinTokenIDAnnotation, id))
			g.Expect(e.workloadTokenIDs()).To(Equal([]string{id}), "the previous joiner's token is deleted")
		})
	}
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
			dec, err := k0sjointoken.Decode(string(s.Data[joinTokenSecretDataKey]))
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(dec.Token.ID()).To(Equal(id))
			g.Expect(dec.Server).To(BeElementOf("https://10.0.0.20:9443", "https://10.0.0.21:9443"))
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

// TestEnsureK0sJoinToken_NoOwnMemberMintsNothing: the apiserver lists live
// addresses, but none is the address of one of our Machines. No token is
// issued, and the KCP says why.
func TestEnsureK0sJoinToken_NoOwnMemberMintsNothing(t *testing.T) {
	g := NewWithT(t)
	e := newPolicyEnv(t, policyOpts{liveAddrs: []string{"10.0.0.20", "10.0.0.21"}, memberAddrs: []string{"10.0.0.99"}})
	before := e.mgmtSecret()

	id, reason, err := e.ensure()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(BeEmpty())
	g.Expect(reason).To(ContainSubstring("lists 2 ready address(es)"))
	g.Expect(e.mgmtSecret().ResourceVersion).To(Equal(before.ResourceVersion), "no write")
	g.Expect(e.workloadTokenIDs()).To(BeEmpty())
	g.Expect(e.events()).To(ContainElement(HavePrefix("Warning K0sJoinTokenNoLiveMember")))
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

// TestK0sJoinTokenLifetime_RemovesTheTokenOfAJoinerThatIsDone: once its
// Machine has joined (NodeRef plus a healthy voting member) or is being
// deleted, a joiner's token is deleted, the join-token Secret is cleared if it
// still holds it, and the KairosConfig is marked so this happens only once.
func TestK0sJoinTokenLifetime_RemovesTheTokenOfAJoinerThatIsDone(t *testing.T) {
	g := NewWithT(t)
	encA, wsA := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(40*time.Minute))
	_, wsB := issued(t, "bbbbbb", "https://10.0.0.20:9443", policyNow.Add(40*time.Minute))
	deleting := cpMachine("cp-2", "", policyNow.Add(-time.Hour), "")
	deleting.DeletionTimestamp = ptr.To(metav1.NewTime(policyNow))
	e := newPolicyEnv(t, policyOpts{
		storedToken: encA, storedAnno: "aaaaaa", workload: []client.Object{wsA, wsB}, liveAddrs: []string{"10.0.0.20"},
		machines:    []*clusterv1.Machine{cpMachine("cp-1", "cp-1", policyNow.Add(-time.Hour), ""), deleting},
		configs:     []client.Object{joinConfig("cp-1", "aaaaaa"), joinConfig("cp-2", "bbbbbb")},
		etcdHealthy: []string{"cp-1"},
	})

	g.Expect(e.r.reconcileK0sJoinTokenLifetimeAt(context.Background(), e.logs.logger(), e.kcp, e.cluster, e.machines, policyNow)).
		To(BeFalse(), "nothing pending, so no extra requeue")
	g.Expect(e.workloadTokenIDs()).To(BeEmpty(), "both done joiners' tokens are deleted")
	s := e.mgmtSecret()
	g.Expect(s.Data).ToNot(HaveKey(joinTokenSecretDataKey), "the Secret no longer holds the joined member's token")
	g.Expect(s.Annotations).ToNot(HaveKey(k0sJoinTokenIDAnnotation))
	for _, name := range []string{"cp-1", "cp-2"} {
		kc := &bootstrapv1beta2.KairosConfig{}
		g.Expect(e.mgmt.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, kc)).To(Succeed())
		g.Expect(kc.Annotations).To(HaveKey(k0sJoinTokenRemovedAtAnnotation), name)
		g.Expect(kc.Annotations).To(HaveKeyWithValue(k0sJoinTokenIDAnnotation, map[string]string{"cp-1": "aaaaaa", "cp-2": "bbbbbb"}[name]))
	}

	// Done once: a later reconcile does not go back to the workload cluster.
	calls := e.factoryCalls
	e.lifetime()
	g.Expect(e.factoryCalls).To(Equal(calls))
}

// TestK0sJoinTokenLifetime_KeepsASharedTokenWhileAJoinerUsesIt: tokens issued
// before one-token-per-joiner could be shared. A joined member's token is kept
// while another pending member references the same ID.
func TestK0sJoinTokenLifetime_KeepsASharedTokenWhileAJoinerUsesIt(t *testing.T) {
	g := NewWithT(t)
	enc, ws := issued(t, "aaaaaa", "https://10.0.0.20:9443", policyNow.Add(40*time.Minute))
	e := newPolicyEnv(t, policyOpts{
		storedToken: enc, storedAnno: "aaaaaa", workload: []client.Object{ws}, liveAddrs: []string{"10.0.0.20"},
		machines: []*clusterv1.Machine{
			cpMachine("cp-1", "cp-1", policyNow.Add(-2*time.Hour), ""),
			cpMachine("cp-2", "", policyNow.Add(-time.Hour), ""),
		},
		configs:     []client.Object{joinConfig("cp-1", "aaaaaa"), joinConfig("cp-2", "aaaaaa")},
		etcdHealthy: []string{"cp-1"},
	})
	g.Expect(e.r.reconcileK0sJoinTokenLifetimeAt(context.Background(), e.logs.logger(), e.kcp, e.cluster, e.machines, policyNow)).To(BeTrue())
	g.Expect(e.workloadTokenIDs()).To(Equal([]string{"aaaaaa"}))
	kc := &bootstrapv1beta2.KairosConfig{}
	g.Expect(e.mgmt.Get(context.Background(), types.NamespacedName{Name: "cp-1", Namespace: "default"}, kc)).To(Succeed())
	g.Expect(kc.Annotations).ToNot(HaveKey(k0sJoinTokenRemovedAtAnnotation))
}

// TestK0sJoinToken_OneTokenPerJoinerAcrossAScaleUp walks two sequential joins:
// each joiner gets its own token, only that token is extended while it joins,
// and it is gone once the joiner has joined.
func TestK0sJoinToken_OneTokenPerJoinerAcrossAScaleUp(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	e := newPolicyEnv(t, policyOpts{liveAddrs: []string{"10.0.0.20"}})

	admitAndJoin := func(name string, joinedAt time.Time) string {
		id, reason, err := e.ensure()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(reason).To(BeEmpty())
		g.Expect(e.mgmt.Create(ctx, joinConfig(name, id))).To(Succeed())
		m := cpMachine(name, "", policyNow, "")
		e.machines = append(e.machines, m)

		// Pending, 45m in: its own token is extended.
		e.r.reconcileK0sJoinTokenLifetimeAt(ctx, e.logs.logger(), e.kcp, e.cluster, e.machines, policyNow.Add(45*time.Minute))
		ws, err := e.workloadToken(id)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(string(ws.Data["expiration"])).To(Equal(policyNow.Add(105 * time.Minute).UTC().Format(time.RFC3339)))
		// A second admission is refused while it is pending.
		_, reason, err = e.ensure()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(reason).To(ContainSubstring(name))

		// It joins: its token goes away.
		m.Status.NodeRef = clusterv1.MachineNodeReference{Name: name}
		es := &corev1.Secret{}
		g.Expect(e.mgmt.Get(ctx, types.NamespacedName{Name: etcdStatusSecretName("c"), Namespace: "default"}, es)).To(Succeed())
		es.Data[name] = []byte(`{"name":"` + name + `","healthy":true,"voting":true,"members":3,"reportedAt":"t"}`)
		g.Expect(e.mgmt.Update(ctx, es)).To(Succeed())
		e.r.reconcileK0sJoinTokenLifetimeAt(ctx, e.logs.logger(), e.kcp, e.cluster, e.machines, joinedAt)
		_, err = e.workloadToken(id)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the joined member's token is deleted")
		return id
	}

	first := admitAndJoin("cp-1", policyNow.Add(50*time.Minute))
	second := admitAndJoin("cp-2", policyNow.Add(50*time.Minute))
	g.Expect(second).ToNot(Equal(first))
	g.Expect(e.workloadTokenIDs()).To(BeEmpty())
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
	m0.Status.Addresses = clusterv1.MachineAddresses{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.20"}}
	m1 := withConfig(rolloutMachine(kcp, "test-kcp-1", kcp.Spec.Version, 2*time.Hour, true))
	m2 := withConfig(rolloutMachine(kcp, "test-kcp-2", kcp.Spec.Version, 10*time.Minute, false))
	initCfg := joinConfig("test-kcp-0", "")
	initCfg.Spec.ControlPlaneRole = bootstrapv1beta2.ControlPlaneRoleInit
	enc, ws := issued(t, "aaaaaa", "https://10.0.0.20:9443", time.Now().Add(50*time.Minute))
	jt := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: joinTokenSecretName(cluster.Name), Namespace: "default",
			Annotations: map[string]string{k0sJoinTokenIDAnnotation: "aaaaaa"},
			// The KCP created this Secret, so it controls it: ensureJoinTokenSecret
			// refuses a same-name Secret it does not control.
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(kcp, controlplanev1beta2.GroupVersion.WithKind("KairosControlPlane"))}},
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
	common := []client.Object{m0, m1, initCfg, joinConfig("test-kcp-1", "bbbbbb"), joinConfig("test-kcp-2", "aaaaaa"), jt, machineMetaInfraTemplate()}

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
			if dec, err := k0sjointoken.Decode(raw); err == nil {
				secrets = append(secrets, dec.Token.Bearer(), dec.Token.Secret())
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
	_, wsDone := issued(t, "cccccc", "https://10.0.0.20:9443", policyNow.Add(time.Hour))
	secrets = append(secrets, enc3, string(ws3.Data["token-secret"]), string(wsDone.Data["token-secret"]))
	e3 := newPolicyEnv(t, policyOpts{
		storedToken: enc3, storedAnno: "aaaaaa", workload: []client.Object{ws3, wsDone}, liveAddrs: []string{"10.0.0.20"},
		machines: []*clusterv1.Machine{cpMachine("cp-1", "", policyNow.Add(-time.Hour), ""), cpMachine("cp-2", "", policyNow.Add(-time.Hour), ""),
			cpMachine("cp-3", "cp-3", policyNow.Add(-2*time.Hour), "")},
		configs:     []client.Object{joinConfig("cp-1", "aaaaaa"), joinConfig("cp-2", ""), joinConfig("cp-3", "cccccc")},
		etcdHealthy: []string{"cp-3"},
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
					if dec, err := k0sjointoken.Decode(string(v)); err == nil {
						secrets = append(secrets, dec.Token.Bearer(), dec.Token.Secret())
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
	g.Expect(all).To(ContainSubstring("Removed the k0s join token of a joiner that is done"))
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

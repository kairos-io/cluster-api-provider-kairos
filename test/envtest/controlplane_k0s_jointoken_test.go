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

package envtest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"net"
	"regexp"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/controllers/controlplane"
)

// k0sEnvtestForeignAddr is an address a test publishes in an extra
// EndpointSlice labelled for the kubernetes Service. The KCP must never pick it.
const k0sEnvtestForeignAddr = "10.0.0.1"

// withEnvtestAsWorkloadCluster makes the KCP use the envtest apiserver as the
// workload cluster: the k0s join-token path then creates its bootstrap token
// and reads the member EndpointSlices there.
func withEnvtestAsWorkloadCluster(t *testing.T) func(*controlplane.KairosControlPlaneReconciler, *rest.Config) {
	return func(r *controlplane.KairosControlPlaneReconciler, cfg *rest.Config) {
		r.WorkloadClientFactory = func(context.Context, *clusterv1.Cluster) (client.Client, error) {
			return envtestWorkloadClient(t, cfg), nil
		}
	}
}

func envtestWorkloadClient(t *testing.T, cfg *rest.Config) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	wc, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return wc
}

// k0sWorkloadKubeconfig is the `<cluster>-kubeconfig` payload: the KCP takes
// the cluster CA for the join token from it.
func k0sWorkloadKubeconfig(t *testing.T, cfg *rest.Config) []byte {
	t.Helper()
	if len(cfg.CAData) == 0 {
		t.Fatal("envtest rest.Config carries no CA data")
	}
	data, err := clientcmd.Write(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"wl": {Server: cfg.Host, CertificateAuthorityData: cfg.CAData}},
		Contexts:       map[string]*clientcmdapi.Context{"wl": {Cluster: "wl", AuthInfo: "admin"}},
		CurrentContext: "wl",
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"admin": {Token: "unused"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// envtestAPIServerAddress returns the address envtest's own kube-apiserver
// publishes in the default/kubernetes EndpointSlice: in these tests it plays
// the live init member, so the init Machine reports it as its address.
func envtestAPIServerAddress(t *testing.T, ctx context.Context, wc client.Client) string {
	t.Helper()
	var addr string
	NewWithT(t).Eventually(func() string {
		slice := &discoveryv1.EndpointSlice{}
		if err := wc.Get(ctx, types.NamespacedName{Namespace: metav1.NamespaceDefault, Name: "kubernetes"}, slice); err != nil {
			return ""
		}
		for _, ep := range slice.Endpoints {
			if len(ep.Addresses) > 0 {
				addr = ep.Addresses[0]
			}
		}
		return addr
	}, 15*time.Second, 200*time.Millisecond).ShouldNot(BeEmpty(), "envtest's apiserver publishes default/kubernetes")
	return addr
}

// publishForeignKubernetesSlice adds an EndpointSlice with the
// kubernetes.io/service-name=kubernetes label that kube-apiserver did not write.
func publishForeignKubernetesSlice(t *testing.T, ctx context.Context, wc client.Client, addr string) {
	t.Helper()
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kubernetes-extra",
			Namespace: metav1.NamespaceDefault,
			Labels:    map[string]string{discoveryv1.LabelServiceName: "kubernetes"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{addr},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		}},
	}
	NewWithT(t).Expect(wc.Create(ctx, slice)).To(Succeed())
}

// makeK0sInitJoinable puts the init machine of a haFixture cluster into the
// state the join gate waits for: a Machine with a NodeRef and the given
// addresses, the kubeconfig Secret, and a healthy voting etcd report.
func makeK0sInitJoinable(t *testing.T, ctx context.Context, c client.Client, nsName, clusterName, kcpName string, kubeconfig []byte, addrs ...string) {
	t.Helper()
	g := NewWithT(t)
	initMachine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcpName + "-0",
			Namespace: nsName,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         clusterName,
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(getKCP(g, ctx, c, nsName, kcpName), controlplanev1beta2.GroupVersion.WithKind("KairosControlPlane")),
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName:       clusterName,
			Version:           "v1.30.0+k0s.0",
			Bootstrap:         clusterv1.Bootstrap{DataSecretName: ptr.To("placeholder")},
			InfrastructureRef: testMachineInfraRef("infra"),
		},
	}
	g.Expect(c.Create(ctx, initMachine)).To(Succeed())
	initMachine.Status.NodeRef = clusterv1.MachineNodeReference{Name: "init-node"}
	initMachine.Status.Phase = string(clusterv1.MachinePhaseRunning)
	for _, a := range addrs {
		initMachine.Status.Addresses = append(initMachine.Status.Addresses,
			clusterv1.MachineAddress{Type: clusterv1.MachineInternalIP, Address: a})
	}
	g.Expect(c.Status().Update(ctx, initMachine)).To(Succeed())

	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-kubeconfig",
			Namespace: nsName,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
		},
		Type: clusterv1.ClusterSecretType,
		Data: map[string][]byte{"value": kubeconfig},
	})).To(Succeed())

	etcdStatusName := bootstrapv1beta2.EtcdStatusSecretName(clusterName)
	g.Eventually(func() error {
		es := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: etcdStatusName, Namespace: nsName}, es); err != nil {
			return err
		}
		if es.Data == nil {
			es.Data = map[string][]byte{}
		}
		es.Data["init-node"] = []byte(`{"name":"init-node","healthy":true,"voting":true,"members":1,"reportedAt":"t"}`)
		return c.Update(ctx, es)
	}, 15*time.Second, 500*time.Millisecond).Should(Succeed())
}

// decodedJoinToken is a k0s join token decoded independently of the
// controller's own codec.
type decodedJoinToken struct {
	server, user, bearer string
	ca                   []byte
}

func decodeJoinTokenForTest(t *testing.T, encoded []byte) decodedJoinToken {
	t.Helper()
	g := NewWithT(t)
	raw, err := base64.StdEncoding.DecodeString(string(encoded))
	g.Expect(err).ToNot(HaveOccurred())
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	g.Expect(err).ToNot(HaveOccurred())
	kubeconfig, err := io.ReadAll(gz)
	g.Expect(err).ToNot(HaveOccurred())
	cfg, err := clientcmd.Load(kubeconfig)
	g.Expect(err).ToNot(HaveOccurred())
	kctx := cfg.Contexts[cfg.CurrentContext]
	g.Expect(kctx).ToNot(BeNil())
	return decodedJoinToken{
		server: cfg.Clusters[kctx.Cluster].Server,
		ca:     cfg.Clusters[kctx.Cluster].CertificateAuthorityData,
		user:   kctx.AuthInfo,
		bearer: cfg.AuthInfos[kctx.AuthInfo].Token,
	}
}

// TestHA_K0sJoinToken_KCPMintsAndStampsJoinConfig: once the init machine is
// joinable, the KCP mints the controller-join token itself (ADR 0013). The
// workload cluster gets a controller-join-only bootstrap token with a 1h
// lifetime; the management join-token Secret gets the encoded token, pointing
// at the live member, and its ID; the new join KairosConfig names that ID.
func TestHA_K0sJoinToken_KCPMintsAndStampsJoinConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	ctx, c, cfg, _, teardown := startKCPEnvtest(t, withEnvtestAsWorkloadCluster(t))
	defer teardown()
	wc := envtestWorkloadClient(t, cfg)

	const (
		nsName      = "ha-k0s-mint"
		clusterName = "ha-k0s-mint-cluster"
		kcpName     = "ha-k0s-mint-kcp"
	)
	configs := recordKairosConfigs(t, ctx, cfg, nsName)
	haFixture(t, ctx, c, nsName, clusterName, kcpName, "k0s")
	// The init Machine reports the live apiserver's address and one more; an
	// extra slice lists that other, lower address. Only the apiserver's own
	// slice counts, so the token must point at the live apiserver.
	apiAddr := envtestAPIServerAddress(t, ctx, wc)
	publishForeignKubernetesSlice(t, ctx, wc, k0sEnvtestForeignAddr)
	makeK0sInitJoinable(t, ctx, c, nsName, clusterName, kcpName, k0sWorkloadKubeconfig(t, cfg), apiAddr, k0sEnvtestForeignAddr)

	// No infrastructure provider runs here, so each join create fails at the
	// infrastructure Machine and deletes its KairosConfig again (ADR 0015
	// PR-4); read the join KairosConfig from the record of what was created.
	var joinCfg *bootstrapv1beta2.KairosConfig
	g.Eventually(func() string {
		joins := configs.withRole(bootstrapv1beta2.ControlPlaneRoleJoin)
		if len(joins) == 0 {
			return ""
		}
		joinCfg = joins[0]
		return joinCfg.Annotations[bootstrapv1beta2.K0sControllerJoinTokenIDAnnotation]
	}, 30*time.Second, 500*time.Millisecond).Should(MatchRegexp(`^[a-z0-9]{6}$`),
		"the join KairosConfig must name the token the KCP minted for it")
	g.Expect(joinCfg.Name).To(MatchRegexp(`^` + kcpName + `-[a-z0-9]{5}$`))
	id := joinCfg.Annotations[bootstrapv1beta2.K0sControllerJoinTokenIDAnnotation]

	// The management Secret holds that token, written together with its ID.
	secret := &corev1.Secret{}
	g.Eventually(func() string {
		_ = c.Get(ctx, types.NamespacedName{Name: bootstrapv1beta2.ControlPlaneJoinTokenSecretName(clusterName), Namespace: nsName}, secret)
		return secret.Annotations[bootstrapv1beta2.K0sControllerJoinTokenIDAnnotation]
	}, 10*time.Second, 200*time.Millisecond).Should(Equal(id))
	tok := decodeJoinTokenForTest(t, secret.Data[bootstrapv1beta2.ControlPlaneJoinTokenSecretDataKey])
	g.Expect(tok.server).To(Equal("https://"+net.JoinHostPort(apiAddr, "9443")),
		"the live member's join API, never the endpoint or an address from another slice")
	g.Expect(tok.user).To(Equal("controller-bootstrap"))
	g.Expect(tok.ca).To(Equal(cfg.CAData), "the CA the workload kubeconfig verifies against")
	g.Expect(regexp.MustCompile(`^` + id + `\.[a-z0-9]{16}$`).MatchString(tok.bearer)).To(BeTrue())

	// The workload cluster holds the matching controller-join-only token.
	ws := &corev1.Secret{}
	g.Expect(wc.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: "bootstrap-token-" + id}, ws)).To(Succeed())
	g.Expect(ws.Type).To(Equal(corev1.SecretTypeBootstrapToken))
	g.Expect(id + "." + string(ws.Data["token-secret"])).To(Equal(tok.bearer))
	g.Expect(string(ws.Data["usage-bootstrap-controller-join"])).To(Equal("true"))
	g.Expect(string(ws.Data["usage-controller-join"])).To(Equal("true"))
	g.Expect(ws.Data).ToNot(HaveKey("usage-bootstrap-authentication"))
	g.Expect(ws.Data).ToNot(HaveKey("usage-bootstrap-signing"))
	g.Expect(ws.Data).ToNot(HaveKey("auth-extra-groups"))
	expires, err := time.Parse(time.RFC3339, string(ws.Data["expiration"]))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(time.Until(expires)).To(BeNumerically("~", time.Hour, 5*time.Minute))

	// The KCP keeps retrying the join Machine (no infrastructure provider runs
	// here). A healthy token is reused across those retries, not re-minted.
	g.Consistently(func() string {
		cur := &corev1.Secret{}
		_ = c.Get(ctx, types.NamespacedName{Name: bootstrapv1beta2.ControlPlaneJoinTokenSecretName(clusterName), Namespace: nsName}, cur)
		return cur.Annotations[bootstrapv1beta2.K0sControllerJoinTokenIDAnnotation]
	}, 4*time.Second, 500*time.Millisecond).Should(Equal(id))
	// Each retry is a new KairosConfig under a new name, stamped with that same
	// token: no Machine was ever created with it.
	joins := configs.withRole(bootstrapv1beta2.ControlPlaneRoleJoin)
	g.Expect(len(joins)).To(BeNumerically(">=", 2))
	names := map[string]bool{}
	for _, kc := range joins {
		g.Expect(names).NotTo(HaveKey(kc.Name))
		names[kc.Name] = true
		g.Expect(kc.Annotations).To(HaveKeyWithValue(bootstrapv1beta2.K0sControllerJoinTokenIDAnnotation, id))
	}
}

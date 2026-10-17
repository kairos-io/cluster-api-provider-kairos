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
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	kcfg "sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The bootstrap manager's workload ClusterCache loads `<cluster>-kubeconfig`
// only through NewSanitizingKubeconfigReader (clustercache.Options.SecretClient).
// These tests drive the reader the way ClusterCache does
// (cluster-api/util/kubeconfig.FromSecret, then
// clientcmd.RESTConfigFromKubeConfig) and check what reaches the rest.Config.

// clusterCacheRESTConfig reproduces clustercache's createRESTConfig over reader.
func clusterCacheRESTConfig(ctx context.Context, reader client.Reader) (*rest.Config, error) {
	data, err := kcfg.FromSecret(ctx, reader, client.ObjectKey{Namespace: testKubeadmNamespace, Name: testKubeadmCluster})
	if err != nil {
		return nil, err
	}
	return clientcmd.RESTConfigFromKubeConfig(data)
}

// readerFor builds the sanitizing reader over a buildSource fixture.
func readerFor(t *testing.T, fx kubeadmFixture) client.Reader {
	t.Helper()
	src, _, _ := buildSource(t, fx, nil)
	return NewSanitizingKubeconfigReader(src.mgmt, src.caReader)
}

func endpointURL() string {
	return fmt.Sprintf("https://%s:%d", testEndpointHost, testEndpointPort)
}

// TestSanitizingKubeconfigReader_CleanKubeconfig: a kubeconfig that names the
// Cluster endpoint yields a config with that server, its CA and its inline
// token, and nothing else.
func TestSanitizingKubeconfigReader_CleanKubeconfig(t *testing.T) {
	g := NewWithT(t)
	ca := genTestCA(t, "cluster-ca")
	fx := defaultFixture(ca)
	fx.kubeconfigData = kubeconfigWithUser(t, endpointURL(), ca, &clientcmdapi.AuthInfo{Token: "inline-admin-token"})

	cfg, err := clusterCacheRESTConfig(context.Background(), readerFor(t, fx))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(cfg.Host).To(Equal(endpointURL()))
	g.Expect(cfg.CAData).To(Equal(ca))
	g.Expect(cfg.BearerToken).To(Equal("inline-admin-token"))
	g.Expect(cfg.BearerTokenFile).To(BeEmpty())
	g.Expect(cfg.ExecProvider).To(BeNil())
	g.Expect(cfg.AuthProvider).To(BeNil())
	g.Expect(cfg.Proxy).To(BeNil())
	g.Expect(cfg.Insecure).To(BeFalse())
}

// TestSanitizingKubeconfigReader_RefusesUnsafeKubeconfig: content client-go
// would act on outside the kubeconfig never reaches clientcmd. That includes an
// entry the current context does not reference.
func TestSanitizingKubeconfigReader_RefusesUnsafeKubeconfig(t *testing.T) {
	ca := genTestCA(t, "cluster-ca")
	cases := map[string]func(*clientcmdapi.Config){
		"token file": func(c *clientcmdapi.Config) {
			c.AuthInfos["admin"] = &clientcmdapi.AuthInfo{TokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token"}
		},
		"exec plugin": func(c *clientcmdapi.Config) {
			c.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Exec: &clientcmdapi.ExecConfig{Command: "/bin/sh", APIVersion: "client.authentication.k8s.io/v1"}}
		},
		"client key file": func(c *clientcmdapi.Config) {
			c.AuthInfos["admin"] = &clientcmdapi.AuthInfo{ClientCertificate: "/etc/x.crt", ClientKey: "/etc/x.key"}
		},
		"proxy-url": func(c *clientcmdapi.Config) { c.Clusters["wl"].ProxyURL = "http://203.0.113.66:3128" },
		"insecure":  func(c *clientcmdapi.Config) { c.Clusters["wl"].InsecureSkipTLSVerify = true },
		"CA file": func(c *clientcmdapi.Config) {
			c.Clusters["wl"].CertificateAuthority = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
		},
		"unreferenced user with a token file": func(c *clientcmdapi.Config) {
			c.AuthInfos["spare"] = &clientcmdapi.AuthInfo{TokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			cfg := clientcmdapi.NewConfig()
			cfg.Clusters["wl"] = &clientcmdapi.Cluster{Server: endpointURL(), CertificateAuthorityData: ca}
			cfg.Contexts["wl"] = &clientcmdapi.Context{Cluster: "wl", AuthInfo: "admin"}
			cfg.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "inline-admin-token"}
			cfg.CurrentContext = "wl"
			mutate(cfg)
			data, err := clientcmd.Write(*cfg)
			g.Expect(err).ToNot(HaveOccurred())

			fx := defaultFixture(ca)
			fx.kubeconfigData = data
			restCfg, err := clusterCacheRESTConfig(context.Background(), readerFor(t, fx))
			g.Expect(err).To(MatchError(ContainSubstring("failed sanitization")))
			g.Expect(restCfg).To(BeNil())
		})
	}
}

// TestSanitizingKubeconfigReader_ServerAnchors: a server other than the Cluster
// endpoint is kept only when <cluster>-ca anchors the CA (Kamaji publishes an
// internal admin address); without that anchor nothing is returned.
func TestSanitizingKubeconfigReader_ServerAnchors(t *testing.T) {
	ca := genTestCA(t, "cluster-ca")
	const internal = "https://cp-admin.tenant.svc:6443"

	t.Run("anchored by <cluster>-ca", func(t *testing.T) {
		g := NewWithT(t)
		fx := defaultFixture(ca)
		fx.server = internal
		fx.clusterCA = ca
		cfg, err := clusterCacheRESTConfig(context.Background(), readerFor(t, fx))
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(cfg.Host).To(Equal(internal))
	})
	t.Run("no anchor", func(t *testing.T) {
		g := NewWithT(t)
		fx := defaultFixture(ca)
		fx.server = internal
		_, err := clusterCacheRESTConfig(context.Background(), readerFor(t, fx))
		g.Expect(err).To(MatchError(ContainSubstring("no " + testKubeadmCluster + "-ca Secret")))
	})
	t.Run("<cluster>-ca does not match", func(t *testing.T) {
		g := NewWithT(t)
		fx := defaultFixture(ca)
		fx.server = internal
		fx.clusterCA = genTestCA(t, "other-ca")
		_, err := clusterCacheRESTConfig(context.Background(), readerFor(t, fx))
		g.Expect(err).To(MatchError(ContainSubstring("does not match")))
	})
	t.Run("same endpoint, other spelling", func(t *testing.T) {
		g := NewWithT(t)
		fx := defaultFixture(ca)
		fx.server = fmt.Sprintf("HTTPS://%s:%d/", testEndpointHost, testEndpointPort)
		cfg, err := clusterCacheRESTConfig(context.Background(), readerFor(t, fx))
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(cfg.Host).To(Equal(endpointURL()))
	})
}

// TestSanitizingKubeconfigReader_FailsClosedForOtherReads: the reader serves
// only typed Gets of `<cluster>-kubeconfig` Secrets.
func TestSanitizingKubeconfigReader_FailsClosedForOtherReads(t *testing.T) {
	g := NewWithT(t)
	ca := genTestCA(t, "cluster-ca")
	fx := defaultFixture(ca)
	fx.clusterCA = ca
	r := readerFor(t, fx)
	ctx := context.Background()

	err := r.Get(ctx, client.ObjectKey{Namespace: testKubeadmNamespace, Name: testKubeadmCluster + "-ca"}, &corev1.Secret{})
	g.Expect(err).To(MatchError(errNotAKubeconfigRead))

	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("Secret")
	err = r.Get(ctx, client.ObjectKey{Namespace: testKubeadmNamespace, Name: testKubeadmCluster + "-kubeconfig"}, u)
	g.Expect(err).To(MatchError(errNotAKubeconfigRead))

	g.Expect(r.List(ctx, &corev1.SecretList{})).To(MatchError(errNotAKubeconfigRead))
}

// TestSanitizingKubeconfigReader_ReturnsOnlyTheRebuiltValue: no other key of
// the Secret leaves the reader.
func TestSanitizingKubeconfigReader_ReturnsOnlyTheRebuiltValue(t *testing.T) {
	g := NewWithT(t)
	ca := genTestCA(t, "cluster-ca")
	src, _, _ := buildSource(t, defaultFixture(ca), nil)
	key := client.ObjectKey{Namespace: testKubeadmNamespace, Name: testKubeadmCluster + "-kubeconfig"}
	stored := &corev1.Secret{}
	g.Expect(src.mgmt.Get(context.Background(), key, stored)).To(Succeed())
	stored.Data["extra"] = []byte("not for the cache")
	g.Expect(src.mgmt.(client.Client).Update(context.Background(), stored)).To(Succeed())

	got := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{}}
	g.Expect(NewSanitizingKubeconfigReader(src.mgmt, src.caReader).Get(context.Background(), key, got)).To(Succeed())
	g.Expect(got.Data).To(HaveLen(1))
	g.Expect(got.Data).To(HaveKey(workloadKubeconfigSecretKey))
}

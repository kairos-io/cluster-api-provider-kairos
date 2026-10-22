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
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// The kubeadm worker path builds its workload client from the kubeconfig
// verifyTrustChain returns, which is the sanitizer's rebuilt (S5) form of
// `<cluster>-kubeconfig`. These tests run the trust check over a fixture and
// check what reaches the rest.Config the on-demand client is built from.

// verifiedRESTConfig runs verifyTrustChain over fx and returns the rest.Config
// built from the kubeconfig it hands to the workload-client factory. It also
// asserts that kubeconfig is accepted by the production factory's own check.
func verifiedRESTConfig(t *testing.T, fx kubeadmFixture) (*rest.Config, error) {
	t.Helper()
	src, req, _ := buildSource(t, fx, nil)
	trusted, err := src.verifyTrustChain(context.Background(), req)
	if err != nil {
		return nil, err
	}
	if err := requireSanitizedKubeconfig(trusted.kubeconfig, trusted.clusterKey.Name); err != nil {
		t.Fatalf("verifyTrustChain returned a kubeconfig the workload-client factory refuses: %v", err)
	}
	return clientcmd.RESTConfigFromKubeConfig(trusted.kubeconfig)
}

func endpointURL() string {
	return fmt.Sprintf("https://%s:%d", testEndpointHost, testEndpointPort)
}

// TestVerifiedWorkloadKubeconfig_CleanKubeconfig: a kubeconfig that names the
// Cluster endpoint yields a config with that server, its CA and its inline
// token, and nothing else.
func TestVerifiedWorkloadKubeconfig_CleanKubeconfig(t *testing.T) {
	g := NewWithT(t)
	ca := genTestCA(t, "cluster-ca")
	fx := defaultFixture(ca)
	fx.kubeconfigData = kubeconfigWithUser(t, endpointURL(), ca, &clientcmdapi.AuthInfo{Token: "inline-admin-token"})

	cfg, err := verifiedRESTConfig(t, fx)
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

// TestVerifiedWorkloadKubeconfig_RefusesUnsafeKubeconfig: content client-go
// would act on outside the kubeconfig never reaches clientcmd. That includes an
// entry the current context does not reference.
func TestVerifiedWorkloadKubeconfig_RefusesUnsafeKubeconfig(t *testing.T) {
	ca := genTestCA(t, "cluster-ca")
	cases := map[string]func(*clientcmdapi.Config){
		"token file": func(c *clientcmdapi.Config) {
			c.AuthInfos["admin"] = &clientcmdapi.AuthInfo{TokenFile: "/etc/example/token"}
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
			c.AuthInfos["spare"] = &clientcmdapi.AuthInfo{TokenFile: "/etc/example/token"}
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
			restCfg, err := verifiedRESTConfig(t, fx)
			g.Expect(err).To(MatchError(ContainSubstring("not a kubeconfig this controller will connect with")))
			g.Expect(restCfg).To(BeNil())
		})
	}
}

// TestVerifiedWorkloadKubeconfig_ServerAnchors: a server other than the Cluster
// endpoint is kept only when <cluster>-ca anchors the CA (Kamaji publishes an
// internal admin address); without that anchor nothing is returned.
func TestVerifiedWorkloadKubeconfig_ServerAnchors(t *testing.T) {
	ca := genTestCA(t, "cluster-ca")
	const internal = "https://cp-admin.tenant.svc:6443"

	t.Run("anchored by <cluster>-ca", func(t *testing.T) {
		g := NewWithT(t)
		fx := defaultFixture(ca)
		fx.server = internal
		fx.clusterCA = ca
		cfg, err := verifiedRESTConfig(t, fx)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(cfg.Host).To(Equal(internal))
	})
	t.Run("no anchor", func(t *testing.T) {
		g := NewWithT(t)
		fx := defaultFixture(ca)
		fx.server = internal
		_, err := verifiedRESTConfig(t, fx)
		g.Expect(err).To(MatchError(ContainSubstring("no " + testKubeadmCluster + "-ca Secret")))
	})
	t.Run("<cluster>-ca does not match", func(t *testing.T) {
		g := NewWithT(t)
		fx := defaultFixture(ca)
		fx.server = internal
		fx.clusterCA = genTestCA(t, "other-ca")
		_, err := verifiedRESTConfig(t, fx)
		g.Expect(err).To(MatchError(ContainSubstring("does not match")))
	})
	t.Run("same endpoint, other spelling", func(t *testing.T) {
		g := NewWithT(t)
		fx := defaultFixture(ca)
		fx.server = fmt.Sprintf("HTTPS://%s:%d/", testEndpointHost, testEndpointPort)
		cfg, err := verifiedRESTConfig(t, fx)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(cfg.Host).To(Equal(endpointURL()))
	})
}

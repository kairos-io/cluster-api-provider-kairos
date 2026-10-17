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
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

// The KCP builds every workload-cluster client (the endpoint client, each
// etcd-leave member client) and reads the k0s join-token CA from one place,
// workloadRESTConfig. These tests pin what it accepts.

// pinnedWorkloadEnv returns a reconciler whose management client holds kcp and
// the `<cluster>-kubeconfig` Secret, with kcp pinned to that Secret's CA and
// the Cluster pointing at kcp with endpoint 192.0.2.10:6443.
func pinnedWorkloadEnv(t *testing.T, secret *corev1.Secret) (*KairosControlPlaneReconciler, *controlplanev1beta2.KairosControlPlane, *clusterv1.Cluster) {
	t.Helper()
	g := NewWithT(t)
	scheme := haTestScheme(g)
	kcp, cluster := k0sKCP(), testCluster()
	pinWorkloadKubeconfig(t, kcp, cluster, secret)
	mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp, secret).Build()
	return &KairosControlPlaneReconciler{Client: mgmt, Scheme: scheme}, kcp, cluster
}

// contractSecret wraps raw kubeconfig bytes as testCluster()'s contract Secret.
func contractSecret(data []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName + "-kubeconfig", Namespace: "default"},
		Data:       map[string][]byte{"value": data},
	}
}

// TestWorkloadRESTConfig_ServerIsTheResolvedEndpoint: the server written in the
// Secret is ignored. The client always dials the endpoint the controller
// resolves itself.
func TestWorkloadRESTConfig_ServerIsTheResolvedEndpoint(t *testing.T) {
	g := NewWithT(t)
	r, _, cluster := pinnedWorkloadEnv(t, workloadKubeconfigSecret("https://203.0.113.66:443"))

	cfg, err := r.workloadRESTConfig(context.Background(), cluster)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(cfg.Host).To(Equal("https://192.0.2.10:6443"))
	g.Expect(cfg.CAData).To(Equal([]byte(testWorkloadCA)))
	g.Expect(cfg.BearerToken).To(Equal("kcp-token"))
	g.Expect(cfg.Insecure).To(BeFalse())
}

// TestWorkloadRESTConfig_RefusesUnsafeKubeconfig: content client-go would act
// on outside the kubeconfig itself (a token file, an exec plugin, a proxy,
// skipped TLS verification) never becomes a rest.Config.
func TestWorkloadRESTConfig_RefusesUnsafeKubeconfig(t *testing.T) {
	ca := genCACertPEM(t)
	for name, mutate := range map[string]func(*clientcmdapi.Cluster, *clientcmdapi.AuthInfo){
		"tokenFile": func(_ *clientcmdapi.Cluster, u *clientcmdapi.AuthInfo) {
			u.Token = ""
			u.TokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
		},
		"exec": func(_ *clientcmdapi.Cluster, u *clientcmdapi.AuthInfo) {
			u.Exec = &clientcmdapi.ExecConfig{Command: "/bin/sh", APIVersion: "client.authentication.k8s.io/v1"}
		},
		"proxy-url": func(c *clientcmdapi.Cluster, _ *clientcmdapi.AuthInfo) { c.ProxyURL = "http://203.0.113.66:3128" },
		"insecure": func(c *clientcmdapi.Cluster, _ *clientcmdapi.AuthInfo) {
			c.InsecureSkipTLSVerify = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			// Pin the clean shape first, so the only thing wrong is the
			// unsafe field.
			clean := contractSecret(rawKubeconfig(ca, &clientcmdapi.AuthInfo{Token: "kcp-token"}))
			r, _, cluster := pinnedWorkloadEnv(t, clean)

			cfg := clientcmdapi.NewConfig()
			cl := &clientcmdapi.Cluster{Server: "https://203.0.113.66:6443", CertificateAuthorityData: ca}
			user := &clientcmdapi.AuthInfo{Token: "kcp-token"}
			mutate(cl, user)
			cfg.Clusters["input-cluster"] = cl
			cfg.AuthInfos["input-user"] = user
			cfg.Contexts["input-ctx"] = &clientcmdapi.Context{Cluster: "input-cluster", AuthInfo: "input-user"}
			cfg.CurrentContext = "input-ctx"
			g.Expect(r.Update(context.Background(), contractSecret(mustWriteKubeconfig(t, cfg)))).To(Succeed())

			_, err := r.workloadRESTConfig(context.Background(), cluster)
			g.Expect(err).To(MatchError(ContainSubstring("failed sanitization")))
		})
	}
}

// TestWorkloadRESTConfig_RequiresTheCAPin: with no pin on the KCP, or a pin for
// a different CA, no rest.Config is built.
func TestWorkloadRESTConfig_RequiresTheCAPin(t *testing.T) {
	t.Run("no pin", func(t *testing.T) {
		g := NewWithT(t)
		r, kcp, cluster := pinnedWorkloadEnv(t, workloadKubeconfigSecret("https://192.0.2.10:6443"))
		delete(kcp.Annotations, KubeconfigCAPinAnnotation)
		g.Expect(r.Update(context.Background(), kcp)).To(Succeed())

		_, err := r.workloadRESTConfig(context.Background(), cluster)
		g.Expect(err).To(MatchError(ContainSubstring("no CA is pinned")))
	})
	t.Run("different CA", func(t *testing.T) {
		g := NewWithT(t)
		r, _, cluster := pinnedWorkloadEnv(t, workloadKubeconfigSecret("https://192.0.2.10:6443"))
		swapped := contractSecret(rawKubeconfig(genCACertPEM(t), &clientcmdapi.AuthInfo{Token: "kcp-token"}))
		g.Expect(r.Update(context.Background(), swapped)).To(Succeed())

		_, err := r.workloadRESTConfig(context.Background(), cluster)
		g.Expect(err).To(MatchError(ContainSubstring("does not match the fingerprint pinned")))

		// The join-token CA comes from the same check.
		_, err = r.workloadClusterCA(context.Background(), cluster)
		g.Expect(err).To(MatchError(ContainSubstring("does not match the fingerprint pinned")))
	})
}

// TestWorkloadRESTConfig_RequiresAKairosControlPlaneRef: a Cluster whose
// control plane is not a KairosControlPlane gets no client from this
// controller.
func TestWorkloadRESTConfig_RequiresAKairosControlPlaneRef(t *testing.T) {
	g := NewWithT(t)
	r, _, cluster := pinnedWorkloadEnv(t, workloadKubeconfigSecret("https://192.0.2.10:6443"))
	cluster.Spec.ControlPlaneRef.Kind = "KubeadmControlPlane"

	_, err := r.workloadRESTConfig(context.Background(), cluster)
	g.Expect(err).To(MatchError(ContainSubstring("does not reference a KairosControlPlane")))
}

// TestWorkloadRESTConfig_ReadsThePinFromTheReferencedKCP: the pin is read from
// the KCP the Cluster names, not from any other KCP in the namespace.
func TestWorkloadRESTConfig_ReadsThePinFromTheReferencedKCP(t *testing.T) {
	g := NewWithT(t)
	r, kcp, cluster := pinnedWorkloadEnv(t, workloadKubeconfigSecret("https://192.0.2.10:6443"))
	other := kcp.DeepCopy()
	other.Name, other.ResourceVersion = "other", ""
	delete(other.Annotations, KubeconfigCAPinAnnotation)
	g.Expect(r.Create(context.Background(), other)).To(Succeed())
	cluster.Spec.ControlPlaneRef.Name = "other"

	_, err := r.workloadRESTConfig(context.Background(), cluster)
	g.Expect(err).To(MatchError(ContainSubstring("no CA is pinned on KairosControlPlane default/other")))
}

// TestDefaultMemberClient_RefusesUnsafeKubeconfig: an ADR 0014 etcd-leave
// member client is derived from the same checked config, so an unsafe Secret
// yields no member client either.
func TestDefaultMemberClient_RefusesUnsafeKubeconfig(t *testing.T) {
	g := NewWithT(t)
	r, _, cluster := pinnedWorkloadEnv(t, workloadKubeconfigSecret("https://192.0.2.10:6443"))
	g.Expect(r.Update(context.Background(), contractSecret(newTokenFileKubeconfig(t)))).To(Succeed())

	_, err := r.defaultMemberClient(context.Background(), cluster, "10.0.0.21")
	g.Expect(err).To(MatchError(ContainSubstring("failed sanitization")))
	_, err = r.defaultWorkloadClient(context.Background(), cluster)
	g.Expect(err).To(MatchError(ContainSubstring("failed sanitization")))
}

// mustWriteKubeconfig serializes cfg.
func mustWriteKubeconfig(t *testing.T, cfg *clientcmdapi.Config) []byte {
	t.Helper()
	out, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return out
}

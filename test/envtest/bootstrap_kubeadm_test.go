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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clientcmdlatest "k8s.io/client-go/tools/clientcmd/api/latest"
	bootstrapapi "k8s.io/cluster-bootstrap/token/api"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/controllers/bootstrap"
)

const kubeadmCPGroup = "controlplane.cluster.x-k8s.io"

// testControlPlaneKind is a throwaway control-plane kind the test trusts via the
// operator-level allowlist flag (extraKinds), so the trust anchor can be a simple
// schemaless object instead of a full KubeadmControlPlane. This also exercises the
// operator-flag trust path (ADR 0010 OQ-9).
const testControlPlaneKind = "TestControlPlane"

var kubeadmCPGVK = schema.GroupVersionKind{Group: kubeadmCPGroup, Version: "v1beta2", Kind: testControlPlaneKind}

// kubeadmControlPlaneCRD is a minimal, schemaless control-plane CRD (a throwaway
// TestControlPlane kind) so the trust-anchor object can be created in envtest
// without the full KubeadmControlPlane schema. The test trusts it via the operator
// allowlist flag.
func kubeadmControlPlaneCRD() *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testcontrolplanes." + kubeadmCPGroup,
			// CAPI contract label so external.GetObjectFromContractVersionedRef
			// resolves this kind's served version (the production trust-check resolver).
			Labels: map[string]string{"cluster.x-k8s.io/v1beta2": "v1beta2"},
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: kubeadmCPGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "testcontrolplanes", Singular: "testcontrolplane",
				Kind: testControlPlaneKind, ListKind: testControlPlaneKind + "List",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1beta2", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type:                   "object",
						XPreserveUnknownFields: &preserve,
					},
				},
			}},
		},
	}
}

// envtestCAPEM returns a self-signed CA certificate in PEM form for a test kubeconfig.
func envtestCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "envtest-cluster-ca"},
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

// envtestKubeconfig builds kubeconfig bytes for server with caPEM.
func envtestKubeconfig(t *testing.T, server string, caPEM []byte) []byte {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["wl"] = &clientcmdapi.Cluster{Server: server, CertificateAuthorityData: caPEM}
	cfg.Contexts["wl"] = &clientcmdapi.Context{Cluster: "wl", AuthInfo: "admin"}
	cfg.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "unused"}
	cfg.CurrentContext = "wl"
	data, err := runtime.Encode(clientcmdlatest.Codec, cfg)
	if err != nil {
		t.Fatalf("encode kubeconfig: %v", err)
	}
	return data
}

// TestBootstrapIntegration_KubeadmWorker exercises the kubeadm worker join against a
// real apiserver: a Cluster whose controlPlaneRef is a KubeadmControlPlane and whose
// owned <cluster>-kubeconfig passes the trust check mints a token in a fake workload
// cluster and renders the JoinConfiguration; a kubeconfig whose owner UID does not
// match the control plane is refused with no data Secret and a failure condition.
// (ADR 0010 P1.)
func TestBootstrapIntegration_KubeadmWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)

	crdPaths := []string{"../../config/crd/bases"}
	if _, err := os.Stat("../../test/crd/capi/cluster-api-components.yaml"); err == nil {
		crdPaths = append(crdPaths, "../../test/crd/capi")
	}
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     crdPaths,
		ErrorIfCRDPathMissing: false,
		CRDs:                  []*apiextensionsv1.CustomResourceDefinition{kubeadmControlPlaneCRD()},
	}
	cfg, err := testEnv.Start()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg).NotTo(BeNil())
	defer func() { g.Expect(testEnv.Stop()).To(Succeed()) }()

	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(bootstrapv1beta2.AddToScheme(scheme)).To(Succeed())

	mgr, err := manager.New(cfg, manager.Options{Scheme: scheme, Logger: log.Log,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)}})
	g.Expect(err).NotTo(HaveOccurred())

	// Uncached reader for the kubeadm source (mgmt reads + <cluster>-ca cross-check)
	// so the test does not race the manager cache.
	direct, err := client.New(cfg, client.Options{Scheme: scheme, Mapper: mgr.GetRESTMapper()})
	g.Expect(err).NotTo(HaveOccurred())

	// Fake workload cluster for token minting.
	workload := fake.NewClientBuilder().WithScheme(scheme).Build()
	extraKinds := map[schema.GroupKind]bool{{Group: kubeadmCPGroup, Kind: testControlPlaneKind}: true}
	kubeadmSource := bootstrap.NewKubeadmJoinSource(direct, direct,
		func(context.Context, client.ObjectKey) (client.Client, error) { return workload, nil }, extraKinds)

	reconciler := &bootstrap.KairosConfigReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		JoinSources: map[string]bootstrap.JoinMaterialSource{bootstrapv1beta2.DistributionKubeadm: kubeadmSource},
	}
	g.Expect(reconciler.SetupWithManager(mgr)).To(Succeed())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgrErrCh := make(chan error, 1)
	go func() { mgrErrCh <- mgr.Start(ctx) }()
	g.Eventually(func() bool { return mgr.GetCache().WaitForCacheSync(ctx) }, 10*time.Second).Should(BeTrue())

	const nsName = "kubeadm-worker"
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	g.Expect(mgr.GetClient().Create(ctx, ns)).To(Succeed())

	caPEM := envtestCAPEM(t)
	server := "https://172.16.56.45:6443" // matches testControlPlaneEndpoint()

	// setupCluster creates a KubeadmControlPlane + Cluster + kubeconfig Secret. When
	// matchingOwner is false the kubeconfig's controller owner UID is wrong, which the
	// trust check must refuse.
	setupCluster := func(name string, matchingOwner bool) {
		cp := &unstructured.Unstructured{}
		cp.SetGroupVersionKind(kubeadmCPGVK)
		cp.SetName(name + "-cp")
		cp.SetNamespace(nsName)
		g.Expect(mgr.GetClient().Create(ctx, cp)).To(Succeed())
		liveCP := &unstructured.Unstructured{}
		liveCP.SetGroupVersionKind(kubeadmCPGVK)
		g.Expect(direct.Get(ctx, types.NamespacedName{Name: name + "-cp", Namespace: nsName}, liveCP)).To(Succeed())
		ownerUID := liveCP.GetUID()
		g.Expect(ownerUID).NotTo(BeEmpty())
		if !matchingOwner {
			ownerUID = types.UID("00000000-0000-0000-0000-000000000000")
		}

		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName},
			Spec: clusterv1.ClusterSpec{
				ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: kubeadmCPGroup, Kind: testControlPlaneKind, Name: name + "-cp",
				},
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "DockerCluster", Name: name,
				},
				ControlPlaneEndpoint: testControlPlaneEndpoint(),
			},
		}
		g.Expect(mgr.GetClient().Create(ctx, cluster)).To(Succeed())
		live := &clusterv1.Cluster{}
		g.Eventually(func() error {
			if err := mgr.GetClient().Get(ctx, client.ObjectKeyFromObject(cluster), live); err != nil {
				return err
			}
			live.Status.Initialization.InfrastructureProvisioned = ptr.To(true)
			live.Status.Initialization.ControlPlaneInitialized = ptr.To(true)
			return mgr.GetClient().Status().Update(ctx, live)
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())

		kcSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-kubeconfig",
				Namespace: nsName,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: kubeadmCPGroup + "/v1beta2",
					Kind:       testControlPlaneKind,
					Name:       name + "-cp",
					UID:        ownerUID,
					Controller: ptr.To(true),
				}},
			},
			Data: map[string][]byte{"value": envtestKubeconfig(t, server, caPEM)},
		}
		g.Expect(mgr.GetClient().Create(ctx, kcSecret)).To(Succeed())
	}

	makeWorker := func(cluster, name string) {
		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName, Labels: map[string]string{clusterv1.ClusterNameLabel: cluster}},
			Spec: clusterv1.MachineSpec{
				ClusterName: cluster,
				Version:     "v1.30.0",
				Bootstrap: clusterv1.Bootstrap{ConfigRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: bootstrapv1beta2.GroupVersion.Group, Kind: "KairosConfig", Name: name,
				}},
				InfrastructureRef: testMachineInfraRef("infra"),
			},
		}
		g.Expect(mgr.GetClient().Create(ctx, machine)).To(Succeed())
		kc := &bootstrapv1beta2.KairosConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: nsName,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(machine, clusterv1.GroupVersion.WithKind("Machine")),
				},
			},
			Spec: bootstrapv1beta2.KairosConfigSpec{
				Role:              "worker",
				Distribution:      bootstrapv1beta2.DistributionKubeadm,
				KubernetesVersion: "v1.30.0",
				UserName:          "kairos",
				UserPassword:      "test-password",
				UserGroups:        []string{"admin"},
			},
		}
		g.Expect(mgr.GetClient().Create(ctx, kc)).To(Succeed())
	}

	// --- Accept case ---
	setupCluster("wl-ok", true)
	makeWorker("wl-ok", "worker-ok")

	g.Eventually(func() bool {
		got := &bootstrapv1beta2.KairosConfig{}
		if err := mgr.GetClient().Get(ctx, types.NamespacedName{Name: "worker-ok", Namespace: nsName}, got); err != nil {
			return false
		}
		return got.Status.DataSecretName != nil && *got.Status.DataSecretName != "" && got.Status.BootstrapTokenID != ""
	}, 40*time.Second, 1*time.Second).Should(BeTrue(), "kubeadm worker should mint a token and produce bootstrap data")

	got := &bootstrapv1beta2.KairosConfig{}
	g.Expect(mgr.GetClient().Get(ctx, types.NamespacedName{Name: "worker-ok", Namespace: nsName}, got)).To(Succeed())
	g.Expect(got.Status.BootstrapTokenID).To(MatchRegexp(`^[a-z0-9]{6}$`))

	secret := &corev1.Secret{}
	g.Expect(mgr.GetClient().Get(ctx, types.NamespacedName{Name: *got.Status.DataSecretName, Namespace: nsName}, secret)).To(Succeed())
	cloudConfig := string(secret.Data["value"])
	g.Expect(cloudConfig).To(ContainSubstring("#cloud-config"))
	g.Expect(cloudConfig).To(ContainSubstring("kind: JoinConfiguration"))
	g.Expect(cloudConfig).To(ContainSubstring("kairos-kubeadm-post-bootstrap.service"))

	// The token was minted in the (fake) workload cluster with the stored ID.
	tokenSecret := &corev1.Secret{}
	g.Expect(workload.Get(ctx, client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-" + got.Status.BootstrapTokenID}, tokenSecret)).To(Succeed())
	g.Expect(tokenSecret.Type).To(Equal(bootstrapapi.SecretTypeBootstrapToken))
	// The stored token id is the non-secret half (no ".secret").
	g.Expect(got.Status.BootstrapTokenID).NotTo(ContainSubstring("."))

	// --- Refuse case: kubeconfig owner UID does not match the control plane ---
	setupCluster("wl-bad", false)
	makeWorker("wl-bad", "worker-bad")

	g.Eventually(func() string {
		bad := &bootstrapv1beta2.KairosConfig{}
		if err := mgr.GetClient().Get(ctx, types.NamespacedName{Name: "worker-bad", Namespace: nsName}, bad); err != nil {
			return ""
		}
		return bad.Status.FailureReason
	}, 40*time.Second, 1*time.Second).Should(Equal(bootstrapv1beta2.BootstrapDataSecretGenerationFailedReason),
		"a kubeconfig not owned by the control plane must be refused")

	bad := &bootstrapv1beta2.KairosConfig{}
	g.Expect(mgr.GetClient().Get(ctx, types.NamespacedName{Name: "worker-bad", Namespace: nsName}, bad)).To(Succeed())
	g.Expect(bad.Status.DataSecretName).To(BeNil(), "no bootstrap data Secret for a refused trust check")
	g.Expect(bad.Status.BootstrapTokenID).To(BeEmpty(), "no token id for a refused trust check")
	g.Expect(bad.Status.FailureMessage).To(ContainSubstring("not controller-owned"))

	cancel()
	<-mgrErrCh
}

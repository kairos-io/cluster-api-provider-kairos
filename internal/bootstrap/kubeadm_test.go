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
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
	sigsyaml "sigs.k8s.io/yaml"
)

// testKubeadmCACert is a self-signed P-256 CA used only as a render fixture.
const testKubeadmCACert = `-----BEGIN CERTIFICATE-----
MIIBgDCCASegAwIBAgIUSy8sq9DIOjLcDRhMq3UAEF8tJdYwCgYIKoZIzj0EAwIw
FTETMBEGA1UEAwwKa3ViZXJuZXRlczAgFw0yNjA5MjkxMzU3NTNaGA8yMTI2MDkw
NTEzNTc1M1owFTETMBEGA1UEAwwKa3ViZXJuZXRlczBZMBMGByqGSM49AgEGCCqG
SM49AwEHA0IABKsijJ+JNqP85czWLfcZ5gFXxYImkvaurep8lv/NJYFfYeJ4Q+Qy
6sOuFnWbO3DPZ7i+zCLRNg9fnITFF8+EbwKjUzBRMB0GA1UdDgQWBBTbUwbMvDUI
9p00aNG97oA5yszqYjAfBgNVHSMEGDAWgBTbUwbMvDUI9p00aNG97oA5yszqYjAP
BgNVHRMBAf8EBTADAQH/MAoGCCqGSM49BAMCA0cAMEQCIA1NQU+hNSCEl06duHlr
pjPfjR7uO1Gj9YDltfCy063cAiBzgFTnX6t9hjKfudIccxVJ0DUwxaL6IhvOUEQg
OJyseQ==
-----END CERTIFICATE-----
`

const (
	testKubeadmClusterToken     = "0b6d2c9e-7a54-4f3c-9d1e-2f8a6b4c1d07"
	testKubeadmControlPlaneHost = "10.96.0.10:6443"
	testKubeadmVersion          = "v1.36.4"
	testKubeadmBootstrapToken   = "abcdef.0123456789abcdef"
	testKubeadmProviderID       = "kubevirt://md-0-abcde"
)

func testKubeadmWorkerParams() KubeadmWorkerParams {
	return KubeadmWorkerParams{
		ClusterToken:      testKubeadmClusterToken,
		ControlPlaneHost:  testKubeadmControlPlaneHost,
		CACertPEM:         testKubeadmCACert,
		KubernetesVersion: testKubeadmVersion,
		BootstrapToken:    testKubeadmBootstrapToken,
		ProviderID:        testKubeadmProviderID,
	}
}

// kubeadmWorkerTemplateData is the TemplateData the controller produces for a CAPK kubeadm worker.
func kubeadmWorkerTemplateData(cluster *KubeadmCluster) TemplateData {
	return TemplateData{
		Role:           kubeadmRoleWorker,
		Hostname:       "md-0-abcde",
		HostnamePrefix: "metal-",
		UserName:       "kairos",
		UserPassword:   "kairos",
		UserGroups:     []string{"admin"},
		DNSServers:     []string{"1.1.1.1", "8.8.8.8"},
		ProviderID:     testKubeadmProviderID,
		KubeadmCluster: cluster,
	}
}

func mustKubeadmWorkerCluster(t *testing.T, params KubeadmWorkerParams) *KubeadmCluster {
	t.Helper()
	c, err := NewKubeadmWorkerCluster(params)
	if err != nil {
		t.Fatalf("NewKubeadmWorkerCluster: %v", err)
	}
	return c
}

// providerKubernetesUserConfig mirrors the cluster.config shape provider-kubernetes unmarshals
// (internal/provider/useroptions.go), decoded with sigs.k8s.io/yaml as the provider does.
type providerKubernetesUserConfig struct {
	ClusterConfiguration struct {
		KubernetesVersion string `json:"kubernetesVersion"`
	} `json:"clusterConfiguration"`
	InitConfiguration struct {
		NodeRegistration struct {
			KubeletExtraArgs []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"kubeletExtraArgs"`
		} `json:"nodeRegistration"`
	} `json:"initConfiguration"`
	JoinConfiguration struct {
		Discovery struct {
			BootstrapToken struct {
				Token             string `json:"token"`
				APIServerEndpoint string `json:"apiServerEndpoint"`
			} `json:"bootstrapToken"`
		} `json:"discovery"`
	} `json:"joinConfiguration"`
}

func TestRenderKubeadmCloudConfig_WorkerMatchesProviderKubernetesContract(t *testing.T) {
	out, err := RenderKubeadmCloudConfig(kubeadmWorkerTemplateData(mustKubeadmWorkerCluster(t, testKubeadmWorkerParams())))
	if err != nil {
		t.Fatalf("RenderKubeadmCloudConfig: %v", err)
	}
	if strings.Contains(out, "unsafeSkipCAVerification") {
		t.Errorf("rendered cloud-config must not skip CA verification:\n%s", out)
	}

	var doc struct {
		Cluster KubeadmCluster `yaml:"cluster"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("rendered cloud-config is not valid YAML: %v\n%s", err, out)
	}
	c := doc.Cluster
	if c.ClusterToken != testKubeadmClusterToken || c.ControlPlaneHost != testKubeadmControlPlaneHost || c.Role != kubeadmRoleWorker {
		t.Errorf("unexpected cluster block: token=%q host=%q role=%q", c.ClusterToken, c.ControlPlaneHost, c.Role)
	}
	if len(c.CACerts) != 1 || c.CACerts[0] != testKubeadmCACert {
		t.Errorf("ca_certs did not round-trip the CA PEM: %q", c.CACerts)
	}

	var cfg providerKubernetesUserConfig
	if err := sigsyaml.Unmarshal([]byte(c.Config), &cfg); err != nil {
		t.Fatalf("cluster.config does not parse as provider-kubernetes config: %v\n%s", err, c.Config)
	}
	if cfg.ClusterConfiguration.KubernetesVersion != testKubeadmVersion {
		t.Errorf("kubernetesVersion = %q, want %q", cfg.ClusterConfiguration.KubernetesVersion, testKubeadmVersion)
	}
	bt := cfg.JoinConfiguration.Discovery.BootstrapToken
	if bt.Token != testKubeadmBootstrapToken || bt.APIServerEndpoint != testKubeadmControlPlaneHost {
		t.Errorf("unexpected bootstrapToken discovery: %+v", bt)
	}
	args := cfg.InitConfiguration.NodeRegistration.KubeletExtraArgs
	if len(args) != 1 || args[0].Name != kubeadmProviderIDArg || args[0].Value != testKubeadmProviderID {
		t.Errorf("kubeletExtraArgs = %+v, want provider-id=%s", args, testKubeadmProviderID)
	}
}

func TestRenderKubeadmCloudConfig_OmitsProviderIDWhenUnknown(t *testing.T) {
	params := testKubeadmWorkerParams()
	params.ProviderID = ""
	data := kubeadmWorkerTemplateData(mustKubeadmWorkerCluster(t, params))
	data.ProviderID = ""

	out, err := RenderKubeadmCloudConfig(data)
	if err != nil {
		t.Fatalf("RenderKubeadmCloudConfig: %v", err)
	}
	if strings.Contains(out, kubeadmProviderIDArg) {
		t.Errorf("expected no provider-id without a providerID:\n%s", out)
	}
}

func TestRenderKubeadmCloudConfig_RequiresCluster(t *testing.T) {
	if _, err := RenderKubeadmCloudConfig(kubeadmWorkerTemplateData(nil)); err == nil {
		t.Fatal("expected an error without a KubeadmCluster")
	}
}

func TestNewKubeadmWorkerCluster_RequiresInputs(t *testing.T) {
	for name, mutate := range map[string]func(*KubeadmWorkerParams){
		"no kubernetes version": func(p *KubeadmWorkerParams) { p.KubernetesVersion = "" },
		"no bootstrap token":    func(p *KubeadmWorkerParams) { p.BootstrapToken = "" },
	} {
		t.Run(name, func(t *testing.T) {
			params := testKubeadmWorkerParams()
			mutate(&params)
			if _, err := NewKubeadmWorkerCluster(params); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestRenderKubeadmCloudConfig_RejectsInvalidCluster(t *testing.T) {
	for name, mutate := range map[string]func(*KubeadmWorkerParams){
		"short cluster token":   func(p *KubeadmWorkerParams) { p.ClusterToken = "too-short" },
		"no control plane host": func(p *KubeadmWorkerParams) { p.ControlPlaneHost = "" },
		"newline in endpoint":   func(p *KubeadmWorkerParams) { p.ControlPlaneHost = "10.0.0.1:6443\nrole: init" },
		"CA is not PEM":         func(p *KubeadmWorkerParams) { p.CACertPEM = "not a certificate" },
		"CA is a non-cert PEM": func(p *KubeadmWorkerParams) {
			p.CACertPEM = "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n"
		},
		"CA has invalid DER inside": func(p *KubeadmWorkerParams) {
			p.CACertPEM = "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			params := testKubeadmWorkerParams()
			mutate(&params)
			c := mustKubeadmWorkerCluster(t, params)
			if _, err := RenderKubeadmCloudConfig(kubeadmWorkerTemplateData(c)); err == nil {
				t.Fatal("expected render to reject the cluster block")
			}
		})
	}
}

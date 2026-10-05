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
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"

	yaml "gopkg.in/yaml.v3"
)

// Kairos provider-kubernetes cluster block values.
const (
	kubeadmRoleWorker         = "worker"
	kubeadmProviderIDArg      = "provider-id"
	kubeadmMinClusterTokenLen = 16
	pemBlockTypeCertificate   = "CERTIFICATE"
)

// KubeadmCluster is the Kairos `cluster:` block consumed by provider-kubernetes.
type KubeadmCluster struct {
	ClusterToken     string   `yaml:"cluster_token"`
	ControlPlaneHost string   `yaml:"control_plane_host"`
	Role             string   `yaml:"role"`
	CACerts          []string `yaml:"ca_certs"`
	// Config is the kubeadm v1beta4 subset provider-kubernetes reads, as a YAML string.
	Config string `yaml:"config"`
}

// kubeadmUserConfig is the subset of kubeadm v1beta4 that provider-kubernetes reads from cluster.config.
type kubeadmUserConfig struct {
	ClusterConfiguration kubeadmClusterConfiguration `yaml:"clusterConfiguration"`
	InitConfiguration    kubeadmInitConfiguration    `yaml:"initConfiguration,omitempty"`
	JoinConfiguration    kubeadmJoinConfiguration    `yaml:"joinConfiguration"`
}

type kubeadmClusterConfiguration struct {
	KubernetesVersion string `yaml:"kubernetesVersion"`
}

// kubeadmInitConfiguration carries nodeRegistration, which provider-kubernetes also applies to joins.
type kubeadmInitConfiguration struct {
	NodeRegistration kubeadmNodeRegistration `yaml:"nodeRegistration,omitempty"`
}

type kubeadmNodeRegistration struct {
	KubeletExtraArgs []kubeadmArg `yaml:"kubeletExtraArgs,omitempty"`
}

type kubeadmArg struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type kubeadmJoinConfiguration struct {
	Discovery kubeadmDiscovery `yaml:"discovery"`
}

type kubeadmDiscovery struct {
	BootstrapToken kubeadmBootstrapTokenDiscovery `yaml:"bootstrapToken"`
}

type kubeadmBootstrapTokenDiscovery struct {
	Token             string `yaml:"token"`
	APIServerEndpoint string `yaml:"apiServerEndpoint"`
}

// KubeadmWorkerParams holds the inputs for a provider-kubernetes worker join.
type KubeadmWorkerParams struct {
	// ClusterToken is a stable correlation value (at least 16 characters), not a credential.
	ClusterToken string
	// ControlPlaneHost is the control-plane endpoint as host:port.
	ControlPlaneHost  string
	CACertPEM         string
	KubernetesVersion string
	BootstrapToken    string
	ProviderID        string
}

// NewKubeadmWorkerCluster builds the provider-kubernetes cluster block for a worker join.
func NewKubeadmWorkerCluster(params KubeadmWorkerParams) (*KubeadmCluster, error) {
	if params.KubernetesVersion == "" {
		return nil, errors.New("kubernetes version is required")
	}
	if params.BootstrapToken == "" {
		return nil, errors.New("bootstrap token is required")
	}

	cfg := kubeadmUserConfig{
		ClusterConfiguration: kubeadmClusterConfiguration{KubernetesVersion: params.KubernetesVersion},
		JoinConfiguration: kubeadmJoinConfiguration{
			Discovery: kubeadmDiscovery{
				BootstrapToken: kubeadmBootstrapTokenDiscovery{
					Token:             params.BootstrapToken,
					APIServerEndpoint: params.ControlPlaneHost,
				},
			},
		},
	}
	if params.ProviderID != "" {
		cfg.InitConfiguration.NodeRegistration.KubeletExtraArgs = []kubeadmArg{{Name: kubeadmProviderIDArg, Value: params.ProviderID}}
	}

	config, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal kubeadm config: %w", err)
	}

	return &KubeadmCluster{
		ClusterToken:     params.ClusterToken,
		ControlPlaneHost: params.ControlPlaneHost,
		Role:             kubeadmRoleWorker,
		CACerts:          []string{params.CACertPEM},
		Config:           string(config),
	}, nil
}

// validateKubeadmCluster enforces the inputs provider-kubernetes would otherwise reject at boot.
func validateKubeadmCluster(c *KubeadmCluster) error {
	if c == nil {
		return nil
	}

	var errs []error
	if len(c.ClusterToken) < kubeadmMinClusterTokenLen {
		errs = append(errs, fmt.Errorf("kubeadm cluster_token must be at least %d characters", kubeadmMinClusterTokenLen))
	}
	if c.ControlPlaneHost == "" {
		errs = append(errs, errors.New("kubeadm control_plane_host is required"))
	}
	for _, f := range []struct{ name, value string }{
		{"kubeadm cluster_token", c.ClusterToken},
		{"kubeadm control_plane_host", c.ControlPlaneHost},
		{"kubeadm role", c.Role},
	} {
		if err := rejectControlChars(f.name, f.value); err != nil {
			errs = append(errs, err)
		}
	}
	if len(c.CACerts) == 0 {
		errs = append(errs, errors.New("kubeadm ca_certs requires at least one CA certificate"))
	}
	for i, caPEM := range c.CACerts {
		if err := validateCertificatePEM(caPEM); err != nil {
			errs = append(errs, fmt.Errorf("kubeadm ca_certs[%d]: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

func validateCertificatePEM(s string) error {
	block, _ := pem.Decode([]byte(s))
	if block == nil || block.Type != pemBlockTypeCertificate {
		return errors.New("not a PEM encoded certificate")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return fmt.Errorf("invalid certificate: %w", err)
	}
	return nil
}

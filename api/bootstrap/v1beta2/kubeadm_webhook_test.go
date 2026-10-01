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

package v1beta2

import (
	"strings"
	"testing"

	"k8s.io/utils/ptr"
	kubeadmv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
)

// newKubeadmWorker returns a valid kubeadm worker KairosConfig: worker role, a
// credential set, and no secret-bearing kubeadm fields. The controller fills the
// join configuration, so an empty spec.kubeadm is valid.
func newKubeadmWorker() *KairosConfig {
	kc := newValidKairosConfig()
	kc.Spec.Role = "worker"
	kc.Spec.Distribution = DistributionKubeadm
	return kc
}

// TestValidateKubeadmConfig_Refusals pins every P1 kubeadm admission refusal.
func TestValidateKubeadmConfig_Refusals(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(kc *KairosConfig)
		wantSub string // substring the aggregate error must contain
	}{
		{
			name:    "control-plane role is refused",
			mutate:  func(kc *KairosConfig) { kc.Spec.Role = "control-plane" },
			wantSub: "kubeadm is supported only for worker nodes",
		},
		{
			name: "inline bootstrap token is refused",
			mutate: func(kc *KairosConfig) {
				kc.Spec.Kubeadm = &KubeadmConfig{JoinConfiguration: &kubeadmv1.JoinConfiguration{
					Discovery: kubeadmv1.Discovery{BootstrapToken: kubeadmv1.BootstrapTokenDiscovery{
						Token: "abcdef.0123456789abcdef",
					}},
				}}
			},
			wantSub: "inline bootstrap token is not allowed",
		},
		{
			name: "unsafeSkipCAVerification is refused",
			mutate: func(kc *KairosConfig) {
				kc.Spec.Kubeadm = &KubeadmConfig{JoinConfiguration: &kubeadmv1.JoinConfiguration{
					Discovery: kubeadmv1.Discovery{BootstrapToken: kubeadmv1.BootstrapTokenDiscovery{
						UnsafeSkipCAVerification: ptr.To(true),
					}},
				}}
			},
			wantSub: "unsafeSkipCAVerification is not allowed",
		},
		{
			name: "discovery.file is refused",
			mutate: func(kc *KairosConfig) {
				kc.Spec.Kubeadm = &KubeadmConfig{JoinConfiguration: &kubeadmv1.JoinConfiguration{
					Discovery: kubeadmv1.Discovery{File: kubeadmv1.FileDiscovery{
						KubeConfigPath: "/etc/kubernetes/admin.conf",
					}},
				}}
			},
			wantSub: "file-based discovery is not supported",
		},
		{
			name: "jinja placeholder anywhere in the kubeadm block is refused",
			mutate: func(kc *KairosConfig) {
				kc.Spec.Kubeadm = &KubeadmConfig{JoinConfiguration: &kubeadmv1.JoinConfiguration{
					NodeRegistration: kubeadmv1.NodeRegistrationOptions{
						CRISocket: "unix:///run/{{ ds.meta_data.hostname }}.sock",
					},
				}}
			},
			wantSub: "must not contain '{{'",
		},
		{
			name: "non-DNS-1123 nodeRegistration.name is refused",
			mutate: func(kc *KairosConfig) {
				kc.Spec.Kubeadm = &KubeadmConfig{JoinConfiguration: &kubeadmv1.JoinConfiguration{
					NodeRegistration: kubeadmv1.NodeRegistrationOptions{Name: "Not_A_Valid_Name"},
				}}
			},
			wantSub: "nodeRegistration.name must be a valid DNS-1123 subdomain",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			kc := newKubeadmWorker()
			tc.mutate(kc)
			err := kc.validate()
			if err == nil {
				t.Fatalf("validate() returned nil; expected an error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("validate() error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestValidateKubeadmConfig_Accepts proves the valid worker shapes pass: an empty
// spec.kubeadm (controller fills everything) and a JoinConfiguration that sets
// only non-secret, non-refused fields.
func TestValidateKubeadmConfig_Accepts(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(kc *KairosConfig)
	}{
		{"empty spec.kubeadm", func(kc *KairosConfig) {}},
		{"valid nodeRegistration.name", func(kc *KairosConfig) {
			kc.Spec.Kubeadm = &KubeadmConfig{JoinConfiguration: &kubeadmv1.JoinConfiguration{
				NodeRegistration: kubeadmv1.NodeRegistrationOptions{Name: "worker-0"},
			}}
		}},
		{"caCertHashes set without inline token is allowed", func(kc *KairosConfig) {
			kc.Spec.Kubeadm = &KubeadmConfig{JoinConfiguration: &kubeadmv1.JoinConfiguration{
				Discovery: kubeadmv1.Discovery{BootstrapToken: kubeadmv1.BootstrapTokenDiscovery{
					CACertHashes: []string{"sha256:deadbeef"},
				}},
			}}
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			kc := newKubeadmWorker()
			tc.mutate(kc)
			if err := kc.validate(); err != nil {
				t.Fatalf("validate() returned %v; expected nil", err)
			}
		})
	}
}

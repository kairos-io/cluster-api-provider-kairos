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
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func TestResolveSSHHost_KubevirtFallback(t *testing.T) {
	g := NewWithT(t)

	machine := &clusterv1.Machine{
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: corev1.ObjectReference{
				Kind: "KubevirtMachine",
			},
		},
	}
	cluster := &clusterv1.Cluster{
		Spec: clusterv1.ClusterSpec{
			ControlPlaneEndpoint: clusterv1.APIEndpoint{
				Host: "10.111.124.223",
			},
		},
	}

	host, err := resolveSSHHost(machine, cluster, "", errors.New("no ip in status"), log.Log)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(host).To(Equal("10.111.124.223"))
}

func TestResolveSSHHost_NoFallbackForVsphere(t *testing.T) {
	g := NewWithT(t)

	expectedErr := errors.New("no ip in status")
	machine := &clusterv1.Machine{
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: corev1.ObjectReference{
				Kind: "VSphereMachine",
			},
		},
	}
	cluster := &clusterv1.Cluster{
		Spec: clusterv1.ClusterSpec{
			ControlPlaneEndpoint: clusterv1.APIEndpoint{
				Host: "10.111.124.223",
			},
		},
	}

	_, err := resolveSSHHost(machine, cluster, "", expectedErr, log.Log)
	g.Expect(err).To(MatchError(expectedErr))
}

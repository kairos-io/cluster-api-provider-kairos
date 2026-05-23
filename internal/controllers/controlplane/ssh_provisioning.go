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

// Package controlplane: SSH-provisioning helpers.
//
// KD-3b (this PR) eliminated SSH from the controller's kubeconfig-retrieval
// path. The one remaining caller of resolveSSHHost is ensureProviderIDOnNodes,
// which PR-8 (the providerID-from-node-push follow-up) will remove. Isolating
// the SSH-only helper in its own file means PR-8 deletes this whole file in
// one shot rather than leaving SSH crumbs scattered across the main
// controller file.
//
// Do not add new SSH callers here. The KD-3b architectural decision is that
// the controller's hot path no longer performs synchronous SSH I/O; any
// future SSH need lives behind the PR-9 SSHFallback opt-in, with proper
// host-key verification.

package controlplane

import (
	"fmt"

	"github.com/go-logr/logr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// resolveSSHHost returns the address to use for SSH-based provisioning
// against a Machine. Preference order:
//
//  1. The node IP discovered from the infrastructure provider's status.
//  2. For KubeVirt control planes only, the Cluster's ControlPlaneEndpoint
//     host as a fallback when the per-machine IP isn't visible yet
//     (e.g. CAPK clusters where the LB IP is the only addressable surface).
//  3. The original nodeErr if neither path yields a host.
//
// Non-KubeVirt providers (CAPV / CAPM3 / etc.) deliberately do not fall
// back to the cluster endpoint — that endpoint targets the API server's
// reachable address, not necessarily a node SSH-listener.
func resolveSSHHost(machine *clusterv1.Machine, cluster *clusterv1.Cluster, nodeIP string, nodeErr error, log logr.Logger) (string, error) {
	if nodeIP != "" {
		return nodeIP, nil
	}

	if isKubevirtMachine(machine) && cluster != nil {
		fallbackHost := cluster.Spec.ControlPlaneEndpoint.Host
		if isValidEndpointHost(fallbackHost) {
			log.Info("Using controlPlaneEndpoint host for KubeVirt SSH fallback", "host", fallbackHost, "machine", machine.Name)
			return fallbackHost, nil
		}
	}

	if nodeErr != nil {
		return "", nodeErr
	}

	return "", fmt.Errorf("node IP not available yet")
}

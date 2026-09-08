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

// Package controlplane: infrastructure-IP lookup helpers.
//
// These helpers read the per-infra-provider Machine status (VSphereMachine,
// VSphereVM, KubevirtMachine, KubeVirt VMI, DockerMachine) to resolve a
// best-effort node IP. They are unstructured-client-based on purpose: the
// controlplane provider must not take a hard build-time dependency on every
// infrastructure provider's typed API.
//
// They live in their own file (extracted in PR-8 of the KD-3b sequence) so
// the surviving call sites — `updateClusterStatus` and the LB-endpoint
// reconciler — stay easy to find. PR-8 removed `ensureProviderIDOnNodes`,
// which had been the single largest caller; the helpers themselves stayed
// because the LB-endpoint path still needs them.

package controlplane

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/external"
)

// getNodeIP retrieves the node IP from the infrastructure provider.
// Supports CAPD (DockerMachine), CAPV (VSphereMachine/VSphereVM), CAPK (KubevirtMachine),
// CAPM3 (Metal3Machine), and the Kairos fleet provider (KairosFleetMachine).
func (r *KairosControlPlaneReconciler) getNodeIP(ctx context.Context, log logr.Logger, machine *clusterv1.Machine) (string, error) {
	switch machine.Spec.InfrastructureRef.Kind {
	case "VSphereMachine":
		// First, try to get IP from VSphereMachine status
		vsphereMachine, err := external.GetObjectFromContractVersionedRef(ctx, r.Client, machine.Spec.InfrastructureRef, machine.Namespace)
		if err != nil {
			return "", fmt.Errorf("failed to get VSphereMachine: %w", err)
		}

		// Try to get IP from VSphereMachine status.addresses
		if ip := r.extractIPFromUnstructured(vsphereMachine); ip != "" {
			return ip, nil
		}

		// Fallback: try VSphereVM (CAPV creates VSphereVM with the same name)
		vsphereVM := &unstructured.Unstructured{}
		vsphereVM.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "infrastructure.cluster.x-k8s.io",
			Version: "v1beta1",
			Kind:    "VSphereVM",
		})
		vsphereVMKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Namespace,
		}

		if err := r.Get(ctx, vsphereVMKey, vsphereVM); err != nil {
			return "", fmt.Errorf("failed to get VSphereVM: %w", err)
		}

		if ip := r.extractIPFromUnstructured(vsphereVM); ip != "" {
			return ip, nil
		}

		return "", fmt.Errorf("no IP address found in VSphereMachine or VSphereVM status")
	case "KubevirtMachine", "KubeVirtMachine":
		kubevirtMachine, err := external.GetObjectFromContractVersionedRef(ctx, r.Client, machine.Spec.InfrastructureRef, machine.Namespace)
		if err != nil {
			return "", fmt.Errorf("failed to get KubevirtMachine: %w", err)
		}

		if ip := r.extractIPFromUnstructured(kubevirtMachine); ip != "" {
			return ip, nil
		}

		if ip, err := r.getKubevirtVMIIP(ctx, log, machine); err == nil && ip != "" {
			log.Info("Resolved KubeVirt VMI IP", "machine", machine.Name, "ip", ip)
			return ip, nil
		}

		return "", fmt.Errorf("no IP address found in KubevirtMachine status")
	case "DockerMachine":
		dockerMachine, err := external.GetObjectFromContractVersionedRef(ctx, r.Client, machine.Spec.InfrastructureRef, machine.Namespace)
		if err != nil {
			return "", fmt.Errorf("failed to get DockerMachine: %w", err)
		}
		if ip := r.extractIPFromUnstructured(dockerMachine); ip != "" {
			return ip, nil
		}
		return "", fmt.Errorf("no IP address found in DockerMachine status")
	case "Metal3Machine":
		// Metal3Machine.status.addresses uses the same MachineAddresses shape as CAPV,
		// so the generic extractor handles it without a provider-specific fallback.
		metal3Machine, err := external.GetObjectFromContractVersionedRef(ctx, r.Client, machine.Spec.InfrastructureRef, machine.Namespace)
		if err != nil {
			return "", fmt.Errorf("failed to get Metal3Machine: %w", err)
		}
		if ip := r.extractIPFromUnstructured(metal3Machine); ip != "" {
			return ip, nil
		}
		return "", fmt.Errorf("no IP address found in Metal3Machine status")
	case "KairosFleetMachine":
		// The Kairos fleet provider (cluster-api-provider-kairos-fleet) reports
		// KairosFleetMachine.status.addresses as a Hostname only (no InternalIP in
		// v0.1), and a fleet node self-discovers its providerID on-node rather than
		// depending on an IP resolved here. There is thus no routable IP to return.
		// Report "no IP" WITHOUT an error: the only consumer is the opt-in,
		// non-fatal SSH-fallback path, which treats an empty address as "nothing to
		// dial" rather than a failure. Returning a non-error empty string also keeps
		// the switch from ever calling a supported provider "unsupported" if this
		// helper is rewired. See ADR 0008.
		return "", nil
	default:
		// Any other CAPI-conformant infrastructure provider. status.addresses is
		// contract-mandated on an infra machine and uses the standard
		// MachineAddresses shape, so the generic extractor already understands it —
		// there is nothing provider-specific left to special-case.
		//
		// Report "no IP" WITHOUT an error when nothing is published, for the same
		// reason as KairosFleetMachine above: the only consumer is the opt-in,
		// non-fatal SSH-fallback path, which treats an empty address as "nothing to
		// dial". Providers whose nodes self-discover their providerID on-node (the
		// Kairos fleet provider, Beskar7) legitimately publish no routable address,
		// and that must not be reported as a failure.
		infraMachine, err := external.GetObjectFromContractVersionedRef(ctx, r.Client, machine.Spec.InfrastructureRef, machine.Namespace)
		if err != nil {
			return "", fmt.Errorf("failed to get %s: %w", machine.Spec.InfrastructureRef.Kind, err)
		}
		return r.extractIPFromUnstructured(infraMachine), nil
	}
}

// extractIPFromUnstructured extracts IP address from an unstructured object's status
func (r *KairosControlPlaneReconciler) extractIPFromUnstructured(obj *unstructured.Unstructured) string {

	// Extract IP from status.addresses
	// VSphere status structure: status.addresses[].address or status.network[].ipAddrs[]
	addresses, found, err := unstructured.NestedSlice(obj.Object, "status", "addresses")
	if err == nil && found && len(addresses) > 0 {
		// Try to get IP from addresses array
		// Prefer InternalIP, then ExternalIP, then any address
		var internalIP, externalIP, anyIP string
		for _, addr := range addresses {
			if addrMap, ok := addr.(map[string]interface{}); ok {
				addrType, _ := addrMap["type"].(string)
				if ip, ok := addrMap["address"].(string); ok && ip != "" {
					switch addrType {
					case "InternalIP":
						internalIP = ip
					case "ExternalIP":
						externalIP = ip
					default:
						if anyIP == "" {
							anyIP = ip
						}
					}
				}
			}
		}
		// Return in priority order: InternalIP > ExternalIP > any IP
		if internalIP != "" {
			return internalIP
		}
		if externalIP != "" {
			return externalIP
		}
		if anyIP != "" {
			return anyIP
		}
	}

	// Fallback: try status.network[].ipAddrs[]
	network, found, err := unstructured.NestedSlice(obj.Object, "status", "network")
	if err == nil && found && len(network) > 0 {
		for _, net := range network {
			if netMap, ok := net.(map[string]interface{}); ok {
				if ipAddrs, ok := netMap["ipAddrs"].([]interface{}); ok && len(ipAddrs) > 0 {
					if ip, ok := ipAddrs[0].(string); ok && ip != "" {
						return ip
					}
				}
			}
		}
	}

	// Also check if there's a direct IP in status (some CAPV versions)
	if ip, found, err := unstructured.NestedString(obj.Object, "status", "vmIp"); err == nil && found && ip != "" {
		return ip
	}

	return ""
}

func (r *KairosControlPlaneReconciler) getKubevirtVMIIP(ctx context.Context, log logr.Logger, machine *clusterv1.Machine) (string, error) {
	if machine == nil {
		return "", fmt.Errorf("machine is nil")
	}

	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "kubevirt.io",
		Version: "v1",
		Kind:    "VirtualMachineInstance",
	})
	vmiKey := types.NamespacedName{
		Name:      machine.Spec.InfrastructureRef.Name,
		Namespace: machine.Namespace,
	}
	if err := r.Get(ctx, vmiKey, vmi); err != nil {
		return "", fmt.Errorf("failed to get VMI: %w", err)
	}

	interfaces, found, err := unstructured.NestedSlice(vmi.Object, "status", "interfaces")
	if err != nil || !found {
		return "", fmt.Errorf("VMI interfaces not found")
	}

	for _, iface := range interfaces {
		if ifaceMap, ok := iface.(map[string]interface{}); ok {
			if ip, ok := ifaceMap["ipAddress"].(string); ok && ip != "" {
				return ip, nil
			}
		}
	}

	return "", fmt.Errorf("no IP address found in VMI status.interfaces")
}

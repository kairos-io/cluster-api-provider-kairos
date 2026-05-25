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
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// getNodeIP retrieves the node IP from the infrastructure provider.
// Supports CAPD (DockerMachine), CAPV (VSphereMachine/VSphereVM), and CAPK (KubevirtMachine).
func (r *KairosControlPlaneReconciler) getNodeIP(ctx context.Context, log logr.Logger, machine *clusterv1.Machine) (string, error) {
	switch machine.Spec.InfrastructureRef.Kind {
	case "VSphereMachine":
		// First, try to get IP from VSphereMachine status
		vsphereMachine := &unstructured.Unstructured{}
		vsphereMachine.SetGroupVersionKind(machine.Spec.InfrastructureRef.GroupVersionKind())
		vsphereMachineKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}

		if err := r.Get(ctx, vsphereMachineKey, vsphereMachine); err != nil {
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
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}

		if err := r.Get(ctx, vsphereVMKey, vsphereVM); err != nil {
			return "", fmt.Errorf("failed to get VSphereVM: %w", err)
		}

		if ip := r.extractIPFromUnstructured(vsphereVM); ip != "" {
			return ip, nil
		}

		return "", fmt.Errorf("no IP address found in VSphereMachine or VSphereVM status")
	case "KubevirtMachine", "KubeVirtMachine":
		kubevirtMachine := &unstructured.Unstructured{}
		kubevirtMachine.SetGroupVersionKind(machine.Spec.InfrastructureRef.GroupVersionKind())
		kubevirtMachineKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}

		if err := r.Get(ctx, kubevirtMachineKey, kubevirtMachine); err != nil {
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
		dockerMachine := &unstructured.Unstructured{}
		dockerMachine.SetGroupVersionKind(machine.Spec.InfrastructureRef.GroupVersionKind())
		dockerMachineKey := types.NamespacedName{
			Name:      machine.Spec.InfrastructureRef.Name,
			Namespace: machine.Spec.InfrastructureRef.Namespace,
		}
		if err := r.Get(ctx, dockerMachineKey, dockerMachine); err != nil {
			return "", fmt.Errorf("failed to get DockerMachine: %w", err)
		}
		if ip := r.extractIPFromUnstructured(dockerMachine); ip != "" {
			return ip, nil
		}
		return "", fmt.Errorf("no IP address found in DockerMachine status")
	default:
		return "", fmt.Errorf("unsupported infrastructure provider: %s", machine.Spec.InfrastructureRef.Kind)
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
		Namespace: machine.Spec.InfrastructureRef.Namespace,
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

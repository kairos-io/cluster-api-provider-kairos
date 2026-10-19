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

// Condition types for KairosConfig
const (
	// BootstrapReadyCondition reports whether bootstrap data generation is ready
	BootstrapReadyCondition = "BootstrapReady"

	// DataSecretAvailableCondition reports whether the bootstrap data secret is available
	DataSecretAvailableCondition = "DataSecretAvailable"
)

// Condition reasons
const (
	// WaitingForClusterInfrastructureReason indicates that bootstrap is waiting for cluster infrastructure
	WaitingForClusterInfrastructureReason = "WaitingForClusterInfrastructure"

	// WaitingForControlPlaneInitializationReason indicates that bootstrap is waiting for control plane initialization
	WaitingForControlPlaneInitializationReason = "WaitingForControlPlaneInitialization"

	// BootstrapDataSecretGenerationFailedReason indicates that bootstrap data secret generation failed
	BootstrapDataSecretGenerationFailedReason = "BootstrapDataSecretGenerationFailed"

	// BootstrapDataSecretAvailableReason indicates that bootstrap data secret is available
	BootstrapDataSecretAvailableReason = "BootstrapDataSecretAvailable"

	// BootstrapSucceededReason indicates that bootstrap succeeded
	BootstrapSucceededReason = "BootstrapSucceeded"

	// BootstrapFailedReason indicates that bootstrap failed
	BootstrapFailedReason = "BootstrapFailed"

	// BootstrapTokenRefreshCapExceededReason is the False(Warning) reason on
	// BootstrapReadyCondition for a kubeadm worker whose Machine has still not
	// registered a Node after the token-refresh hard cap elapsed (ADR 0010 P1
	// item 7). The controller stops extending the bootstrap token past the cap; a
	// MachineHealthCheck with a nodeStartupTimeout is expected to replace the stuck
	// Machine. Resolution: check why the node never joined (image/version mismatch,
	// network, CNI), then let the MHC or a manual delete replace the Machine.
	BootstrapTokenRefreshCapExceededReason = "BootstrapTokenRefreshCapExceeded"

	// WaitingForKubeconfigPushTargetReason indicates that bootstrap rendering for
	// a control-plane KairosConfig is waiting on the KairosControlPlane: the
	// per-cluster staging kubeconfig Secret (<cluster>-kubeconfig-push) must
	// exist and be owned by the control plane before a control-plane node can be
	// handed a push token, because the rendered node only ever PATCHes that
	// controller-owned Secret. Clears on its own once the control plane has
	// created and taken ownership of the staging Secret; see ADR 0011.
	WaitingForKubeconfigPushTargetReason = "WaitingForKubeconfigPushTarget"

	// ManagementCAUnavailableReason indicates that bootstrap rendering for a
	// control-plane KairosConfig cannot proceed because no management API CA was
	// resolved to verify the node-push channel against (ADR 0011 PR-4). The
	// node-push TLS has no insecure fallback, so without a CA the push cannot be
	// rendered. Resolve on its own once the operator supplies a CA
	// (KAIROS_MANAGEMENT_API_CA_FILE) or the in-cluster trust root becomes
	// available; the manager normally fails fast at startup instead.
	ManagementCAUnavailableReason = "ManagementCAUnavailable"
)

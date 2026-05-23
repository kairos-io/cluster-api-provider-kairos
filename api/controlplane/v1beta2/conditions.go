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

// Condition types for KairosControlPlane
const (
	// AvailableCondition indicates that the control plane is available
	AvailableCondition = "Available"

	// KubeconfigReadyCondition indicates whether the workload-cluster
	// kubeconfig Secret has been observed in the management cluster. Under
	// KD-3b the control-plane controller no longer SSHes into nodes to fetch
	// the kubeconfig; instead, the node pushes its kubeconfig via curl using
	// a per-cluster ServiceAccount token and the controller waits for the
	// Secret to appear (watch-driven, no polling).
	//
	// Reasons:
	//   - KubeconfigReadyReason (True) — Secret exists, parses as a valid
	//     kubeconfig, has the expected cluster-name label.
	//   - WaitingForNodePushReason (False) — Secret is not present (or has
	//     no kubeconfig payload). Severity is Info while the elapsed
	//     since-first-observation is under kubeconfigReadyTimeout; escalates
	//     to Warning past that threshold so operators see the stall in the
	//     condition surface.
	KubeconfigReadyCondition = "KubeconfigReady"
)

// Condition reasons
const (
	// WaitingForMachinesReason indicates that the control plane is waiting for machines
	WaitingForMachinesReason = "WaitingForMachines"

	// WaitingForMachinesReadyReason indicates that the control plane is waiting for machines to be ready
	WaitingForMachinesReadyReason = "WaitingForMachinesReady"

	// ControlPlaneInitializationFailedReason indicates that control plane initialization failed
	ControlPlaneInitializationFailedReason = "ControlPlaneInitializationFailed"

	// ControlPlaneInitializationSucceededReason indicates that control plane initialization succeeded
	ControlPlaneInitializationSucceededReason = "ControlPlaneInitializationSucceeded"

	// ScalingUpReason indicates that the control plane is scaling up
	ScalingUpReason = "ScalingUp"

	// ScalingDownReason indicates that the control plane is scaling down
	ScalingDownReason = "ScalingDown"

	// WaitingForNodePushReason is the False reason for KubeconfigReadyCondition
	// while the workload-cluster kubeconfig Secret has not yet been observed
	// in the management cluster. Severity transitions from Info to Warning
	// once Status.LastNodePushObserved is older than kubeconfigReadyTimeout.
	WaitingForNodePushReason = "WaitingForNodePush"

	// KubeconfigReadyReason is the True reason for KubeconfigReadyCondition
	// once the workload-cluster kubeconfig Secret has been observed and
	// parses successfully.
	KubeconfigReadyReason = "KubeconfigReady"
)

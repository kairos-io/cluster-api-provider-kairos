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

	// KubeconfigReadyViaSSHFallbackReason is the True reason for
	// KubeconfigReadyCondition when the kubeconfig Secret was supplied
	// by the opt-in SSH fallback path (Spec.SSHFallback.Enabled=true)
	// rather than the node-push path. Operators auditing "how did this
	// cluster get its kubeconfig?" see this in `kubectl describe kcp`.
	KubeconfigReadyViaSSHFallbackReason = "KubeconfigReadyViaSSHFallback"

	// SSHFallbackDialingReason is the False reason for
	// KubeconfigReadyCondition while the SSH fallback path is dialing
	// the workload node. Severity Info. Transitions to True (one of
	// the two Ready reasons) on success, or to SSHFallbackFailedReason
	// on hard error.
	SSHFallbackDialingReason = "SSHFallbackDialing"

	// SSHFallbackFailedReason is the False reason for
	// KubeconfigReadyCondition when the SSH fallback attempt failed in
	// a way that prevented kubeconfig retrieval. Severity Warning.
	// Common causes: host-key mismatch, auth failure, file not found
	// on remote. The controller continues to honour any concurrent
	// node-push; a successful node-push will still transition to
	// KubeconfigReadyReason.
	SSHFallbackFailedReason = "SSHFallbackFailed"

	// SSHFallbackMisconfiguredReason is the False reason for
	// KubeconfigReadyCondition when SSHFallback.Enabled=true but a
	// referenced Secret is missing, empty, or unparseable. Severity
	// Warning. Resolution: fix the referenced Secret; the path retries
	// on next reconcile.
	SSHFallbackMisconfiguredReason = "SSHFallbackMisconfigured"

	// WaitingForInfrastructureControlPlaneEndpointReason is the False
	// reason for the KairosControlPlane AvailableCondition and the
	// derived ReadyCondition while `Cluster.Spec.ControlPlaneEndpoint`
	// is not yet populated. Severity Info.
	//
	// Per the CAPI v1beta2 contract, the infrastructure provider's
	// `<Infra>Cluster` resource is the source of truth for the
	// control-plane endpoint:
	//   - CAPV operators set `VSphereCluster.Spec.ControlPlaneEndpoint`.
	//   - CAPK operators set
	//     `KubevirtCluster.Spec.ControlPlaneServiceTemplate`; CAPK then
	//     creates the LoadBalancer Service and reflects its IP into
	//     `KubevirtCluster.Spec.ControlPlaneEndpoint`.
	//   - CAPD operators set `Cluster.Spec.ControlPlaneEndpoint`
	//     directly (CAPD does not auto-discover).
	// CAPI core then copies `<Infra>Cluster.Spec.ControlPlaneEndpoint`
	// into `Cluster.Spec.ControlPlaneEndpoint`. This Reason names that
	// upstream wait so operators see clearly that the control-plane
	// controller is correctly NOT writing the field (KD-12). Closely
	// related to CAPI's own
	// `InfrastructureReadyV1Beta1Condition` /
	// `WaitingForInfrastructureFallback` on the Cluster — when the
	// infrastructure provider is itself stalled, this Reason on KCP
	// and that condition on the Cluster show up together; operators
	// fix the InfraCluster spec and both clear.
	//
	// Resolution: set the endpoint on the InfraCluster per the
	// upstream provider's contract. For an in-flight cluster, also
	// valid to `kubectl edit cluster <name>` and populate
	// `spec.controlPlaneEndpoint.host`/`port` directly — CAPI core
	// does not overwrite a populated value.
	WaitingForInfrastructureControlPlaneEndpointReason = "WaitingForInfrastructureControlPlaneEndpoint"
)

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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

const (
	// KairosControlPlaneFinalizer allows the reconciler to clean up resources associated with KairosControlPlane before
	// removing it from the API server.
	KairosControlPlaneFinalizer = "kairoscontrolplane.controlplane.cluster.x-k8s.io"
)

// KairosControlPlaneSpec defines the desired state of KairosControlPlane
type KairosControlPlaneSpec struct {
	// Replicas is the number of control plane machines.
	// Contract: ControlPlane MUST expose replicas.
	//
	// Only `replicas: 1` is supported in this release. With `replicas: 1`,
	// k0s/k3s is configured with `--single` and the control plane operates in
	// single-node mode. HA control planes (both classic and P2P/decentralized)
	// are planned for a future release; until then, the validating webhook
	// rejects values greater than 1 because the current bootstrap logic would
	// otherwise produce N independent single-node clusters instead of an HA
	// cluster (see foundational-review item KD-5).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Version is the Kubernetes version to use
	// Contract: ControlPlane MUST expose version
	// +kubebuilder:validation:Required
	Version string `json:"version"`

	// Distribution specifies the Kubernetes distribution to install
	// +kubebuilder:validation:Enum=k0s;k3s
	// +kubebuilder:default=k0s
	// +optional
	Distribution string `json:"distribution,omitempty"`

	// MachineTemplate defines the template for creating control plane machines
	// Contract: ControlPlane MUST expose machineTemplate
	MachineTemplate KairosControlPlaneMachineTemplate `json:"machineTemplate"`

	// KairosConfigTemplate is a reference to a KairosConfigTemplate resource
	// Contract: ControlPlane MUST reference a BootstrapConfigTemplate
	KairosConfigTemplate KairosConfigTemplateReference `json:"kairosConfigTemplate"`

	// RolloutStrategy defines the strategy for rolling out updates
	// +optional
	RolloutStrategy *RolloutStrategy `json:"rolloutStrategy,omitempty"`

	// SSHFallback configures an opt-in SSH-pull fallback used to retrieve the
	// workload-cluster kubeconfig when the in-VM node-push path is unreachable
	// (typically: air-gapped CAPV deployments where the workload subnet has no
	// route back to the management apiserver).
	//
	// The fallback runs in a dedicated, non-Reconcile controller path: when
	// KubeconfigReadyCondition has been False(WaitingForNodePush) for longer
	// than spec.sshFallback.activateAfter (default 15m), and Enabled=true,
	// the SSH-fetch path dials the workload node, retrieves the local admin
	// kubeconfig from /var/lib/k0s/pki/admin.conf (k0s) or
	// /etc/rancher/k3s/k3s.yaml (k3s), and writes the cluster kubeconfig
	// Secret. KubeconfigReadyCondition then transitions to
	// True(KubeconfigReadyViaSSHFallback) so operators can audit which
	// path supplied the kubeconfig.
	//
	// SECURITY: host-key verification is mandatory. The controller refuses
	// to connect unless the workload node's host key matches an entry in
	// the referenced KnownHostsSecretRef. There is no trust-on-first-use
	// path.
	//
	// Off by default. Adding fields here is opt-in via Enabled=true.
	// +optional
	SSHFallback *SSHFallback `json:"sshFallback,omitempty"`
}

// SSHFallback configures the opt-in SSH-pull fallback for the
// workload-cluster kubeconfig. See KairosControlPlaneSpec.SSHFallback.
type SSHFallback struct {
	// Enabled toggles the SSH fallback path. When false (the default),
	// the controller never opens an SSH connection regardless of any
	// other field in this struct. When true, the other fields MUST be
	// set per the validating webhook — in particular KnownHostsSecretRef
	// and IdentitySecretRef are required.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// IdentitySecretRef references a Secret containing the SSH
	// private key the controller uses to authenticate to the workload
	// node. The Secret MUST contain a key named "ssh-privatekey"
	// holding a PEM-encoded private key (matches the Kubernetes
	// kubernetes.io/ssh-auth Secret convention). The Secret's type
	// SHOULD be kubernetes.io/ssh-auth; the webhook permits Opaque for
	// back-compat but warns.
	//
	// The corresponding public key MUST be installed in the workload
	// node's authorized_keys via KairosConfig.Spec.SSHPublicKey /
	// GitHubUser. The provider does not push the public key on the
	// operator's behalf.
	//
	// REQUIRED when Enabled=true. The webhook rejects Enabled=true with
	// a nil ref.
	// +optional
	IdentitySecretRef *SSHFallbackSecretReference `json:"identitySecretRef,omitempty"`

	// KnownHostsSecretRef references a Secret containing one or more
	// OpenSSH `known_hosts` lines. The controller verifies the workload
	// node's offered host key against this set BEFORE any data is
	// exchanged. There is no first-use-trust path.
	//
	// The Secret data key defaults to "known_hosts". Multiple lines
	// (one per host or wildcard entry) are supported per OpenSSH format.
	// Hashed entries (HashKnownHosts yes) are supported.
	//
	// REQUIRED when Enabled=true. The webhook rejects Enabled=true with
	// a nil ref.
	// +optional
	KnownHostsSecretRef *SSHFallbackSecretReference `json:"knownHostsSecretRef,omitempty"`

	// User is the SSH login user. Must be a Kairos OS user with read
	// access to the distribution's admin-kubeconfig file. Defaults to
	// "kairos" (matches the default user produced by the bootstrap
	// controller). Pattern matches POSIX-portable username syntax;
	// shell-injection in the user field is rejected at admission.
	// +kubebuilder:default=kairos
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_-]{0,31}$`
	// +optional
	User string `json:"user,omitempty"`

	// Port is the SSH port on the workload node. Defaults to 22.
	// +kubebuilder:default=22
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// ActivateAfter is the elapsed since KubeconfigReadyCondition first
	// became False(WaitingForNodePush) after which the SSH fallback path
	// becomes eligible to run. Defaults to 15m. Must be strictly greater
	// than the controller's kubeconfigReadyTimeout (10m) — the fallback
	// is meant to fire AFTER the Info→Warning escalation, not before.
	// The webhook enforces this lower bound.
	//
	// Format: Kubernetes Duration (e.g. "15m", "1h").
	// +kubebuilder:default="15m"
	// +optional
	ActivateAfter *metav1.Duration `json:"activateAfter,omitempty"`
}

// SSHFallbackSecretReference is a namespaced reference to a Secret used
// by the SSH fallback path. Namespace defaults to the KCP's namespace;
// the webhook rejects cross-namespace references in this release.
type SSHFallbackSecretReference struct {
	// Name of the Secret. Required.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Key within the Secret data. Per-field default (ssh-privatekey for
	// IdentitySecretRef, known_hosts for KnownHostsSecretRef) is
	// documented on the parent.
	// +optional
	Key string `json:"key,omitempty"`

	// Namespace of the Secret. Defaults to the KCP's namespace.
	// Cross-namespace references are rejected by the webhook in this
	// release.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// KairosControlPlaneMachineTemplate defines the template for control plane machines
type KairosControlPlaneMachineTemplate struct {
	// InfrastructureRef is a reference to a resource that provides infrastructure
	// Contract: ControlPlane MUST reference an infrastructure template
	InfrastructureRef corev1.ObjectReference `json:"infrastructureRef"`

	// NodeDrainTimeout is the total amount of time that the controller will spend
	// on draining a controlplane node
	// +optional
	NodeDrainTimeout *metav1.Duration `json:"nodeDrainTimeout,omitempty"`

	// Metadata is the metadata to apply to the machines
	// +optional
	Metadata clusterv1.ObjectMeta `json:"metadata,omitempty"`
}

// KairosConfigTemplateReference is a reference to a KairosConfigTemplate
type KairosConfigTemplateReference struct {
	// APIVersion is the API version of the referenced resource
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`

	// Kind is the kind of the referenced resource
	// +optional
	Kind string `json:"kind,omitempty"`

	// Name is the name of the referenced resource
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// RolloutStrategy defines the strategy for rolling out updates
type RolloutStrategy struct {
	// Type is the type of rollout strategy
	// +kubebuilder:validation:Enum=RollingUpdate
	// +kubebuilder:default=RollingUpdate
	Type string `json:"type,omitempty"`

	// RollingUpdate defines the rolling update configuration
	// +optional
	RollingUpdate *RollingUpdate `json:"rollingUpdate,omitempty"`
}

// RollingUpdate defines the rolling update configuration
type RollingUpdate struct {
	// MaxSurge is the maximum number of machines that can be created above the
	// desired number of machines
	// +optional
	MaxSurge *int32 `json:"maxSurge,omitempty"`
}

// KairosControlPlaneStatus defines the observed state of KairosControlPlane
// Contract: ControlPlane v1beta2 MUST expose initialized, readyReplicas, updatedReplicas, unavailableReplicas
type KairosControlPlaneStatus struct {
	// Initialized indicates whether the control plane has been initialized
	// Contract: ControlPlane MUST expose initialized
	// This field MUST be set to true when the first control plane machine is ready
	// and the control plane is functional.
	// +optional
	Initialized bool `json:"initialized,omitempty"`

	// Initialization provides observations of the control plane initialization process.
	// This is part of the Cluster API v1beta2 contract.
	// +optional
	Initialization KairosControlPlaneInitializationStatus `json:"initialization,omitempty,omitzero"`

	// ReadyReplicas is the number of control plane machines that are ready
	// Contract: ControlPlane MUST expose readyReplicas
	// A machine is considered ready when it has a NodeRef and the Node is ready.
	// Note: omitempty is removed to ensure the field is always present (even when 0),
	// as the Cluster controller checks this field and null vs 0 can cause issues.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas"`

	// Replicas is the total number of control plane machines
	// This includes machines in all states (pending, running, failed, etc.)
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// UpdatedReplicas is the number of control plane machines that have been updated
	// Contract: ControlPlane MUST expose updatedReplicas
	// A machine is considered updated when its spec matches the desired state.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`

	// UnavailableReplicas is the number of control plane machines that are unavailable
	// Contract: ControlPlane MUST expose unavailableReplicas
	// A machine is unavailable if it is not ready or if it is being deleted.
	// +optional
	UnavailableReplicas int32 `json:"unavailableReplicas,omitempty"`

	// Conditions defines current service state of the KairosControlPlane
	// Contract: ControlPlane SHOULD expose Conditions
	// Standard CAPI conditions: Ready, Available, Initialized
	// +optional
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`

	// ObservedGeneration is the most recent generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// FailureReason is a short machine-readable string indicating why the
	// control-plane controller failed on its last reconcile attempt. Cleared
	// automatically when a subsequent reconcile succeeds, so a non-empty
	// value indicates an ongoing failure, not a terminal one.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// FailureMessage is a human-readable description of the last control-plane
	// failure. Cleared automatically when a subsequent reconcile succeeds.
	// If non-empty, check the KairosControlPlane events and the owned Machine
	// events for context.
	// +optional
	FailureMessage string `json:"failureMessage,omitempty"`

	// Selector is the label selector for control plane machines
	// This is used to identify machines belonging to this control plane.
	// +optional
	Selector string `json:"selector,omitempty"`

	// LastNodePushObserved is the timestamp at which the controlplane
	// reconciler first observed that the workload-cluster kubeconfig Secret
	// was missing for this KairosControlPlane on the node-push path (KD-3b).
	// Cleared once the Secret is observed and KubeconfigReadyCondition
	// transitions to True. Used to escalate the condition severity from
	// Info to Warning once the elapsed exceeds kubeconfigReadyTimeout —
	// not a terminal state. The PR-9 SSHFallback option provides
	// operator-triggered recovery if a node never manages to push.
	// +optional
	LastNodePushObserved *metav1.Time `json:"lastNodePushObserved,omitempty"`
}

// KairosControlPlaneInitializationStatus provides observations of the control plane initialization process.
// +kubebuilder:validation:MinProperties=1
type KairosControlPlaneInitializationStatus struct {
	// ControlPlaneInitialized is true when the control plane is initialized and can accept requests.
	// +optional
	ControlPlaneInitialized *bool `json:"controlPlaneInitialized,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=kairoscontrolplanes,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Initialized",type="boolean",JSONPath=".status.initialized",description="Control plane initialized"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".status.replicas",description="Total replicas"
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas",description="Ready replicas"
// +kubebuilder:printcolumn:name="Updated",type="integer",JSONPath=".status.updatedReplicas",description="Updated replicas"
// +kubebuilder:printcolumn:name="Unavailable",type="integer",JSONPath=".status.unavailableReplicas",description="Unavailable replicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// KairosControlPlane is the Schema for the kairoscontrolplanes API
type KairosControlPlane struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KairosControlPlaneSpec   `json:"spec,omitempty"`
	Status KairosControlPlaneStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KairosControlPlaneList contains a list of KairosControlPlane
type KairosControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KairosControlPlane `json:"items"`
}

// GetConditions returns the set of conditions for this object.
func (c *KairosControlPlane) GetConditions() clusterv1.Conditions {
	return c.Status.Conditions
}

// SetConditions sets the conditions on this object.
func (c *KairosControlPlane) SetConditions(conditions clusterv1.Conditions) {
	c.Status.Conditions = conditions
}

func init() {
	SchemeBuilder.Register(&KairosControlPlane{}, &KairosControlPlaneList{})
}

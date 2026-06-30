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
	// Allowed values are 1, 3, and 5 — odd numbers only. Even counts provide
	// the same etcd fault tolerance as the next-lower odd count while increasing
	// the quorum requirement, so they are rejected at admission. Values above 5
	// are also rejected; beyond 5 etcd members the quorum cost outweighs the
	// additional fault tolerance for a control plane.
	//
	// replicas: 1 configures a single-node control plane (k0s --single /
	// k3s single-server). replicas: 3 or 5 configure an HA control plane using
	// the classic join path (ADR 0005, Phase 3+); kube-vip (ARP/L2 by default)
	// provides the stable VIP — configure spec.ha.vip for the VIP parameters.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
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

	// HA holds configuration for high-availability control planes
	// (spec.replicas in {3, 5}). Ignored when spec.replicas is 1.
	//
	// For single-node clusters (replicas: 1) this field must be left unset or
	// nil; kube-vip is not rendered for single-node control planes, and a
	// non-nil VIP block will trigger a validation warning.
	// +optional
	HA *HAConfig `json:"ha,omitempty"`

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

// HAConfig configures high-availability behaviour for a KairosControlPlane
// with spec.replicas > 1.
type HAConfig struct {
	// VIP configures the virtual IP (kube-vip) used as the stable control-plane
	// endpoint across HA nodes. Required when the infrastructure provider does
	// not supply a load-balanced endpoint automatically.
	//
	// CAPK exception: CAPK mints its own LoadBalancer Service and reflects the
	// IP into KubevirtCluster.Spec.ControlPlaneEndpoint. Do NOT set VIP for
	// CAPK clusters — it is redundant and will produce a conflicting ARP
	// announcement.
	//
	// For CAPV, CAPM3, and CAPD, set VIP.Address to the IP (or hostname)
	// pre-reserved for the control-plane endpoint. The same value must be set
	// on the InfraCluster's ControlPlaneEndpoint so that CAPI core copies it
	// into Cluster.Spec.ControlPlaneEndpoint. The controller does not enforce
	// this constraint at admission; a mismatch will cause the cluster endpoint
	// to point at the wrong address.
	// +optional
	VIP *KubeVIPConfig `json:"vip,omitempty"`
}

// KubeVIPMode is the kube-vip VIP advertisement mode.
// +kubebuilder:validation:Enum=ARP;BGP
type KubeVIPMode string

const (
	// KubeVIPModeARP uses ARP-based VIP advertisement (L2).
	// Requires the control-plane nodes to share an L2 network segment.
	KubeVIPModeARP KubeVIPMode = "ARP"

	// KubeVIPModeBGP uses BGP-based VIP advertisement (L3).
	// Requires BGP peering on the fabric; intended for routed bare-metal
	// deployments.
	KubeVIPModeBGP KubeVIPMode = "BGP"
)

// KubeVIPConfig holds the kube-vip parameters rendered into the
// control-plane cloud-config by the bootstrap provider (Phase 2).
// Security: Address and Interface are validated at admission (IP/hostname
// shape and interface-name regex) and must be further shquote'd at template
// render time (Phase 2 work, security-architect pre-merge required).
type KubeVIPConfig struct {
	// Address is the virtual IP address or DNS hostname for the control-plane
	// endpoint. Must be a valid IPv4 address, IPv6 address, or RFC-1123
	// hostname. For ARP mode this must be an IP address reachable on the
	// same L2 segment as the control-plane nodes.
	//
	// The value must match the host portion of the InfraCluster's
	// ControlPlaneEndpoint so that CAPI core copies the correct address into
	// Cluster.Spec.ControlPlaneEndpoint.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Address string `json:"address"`

	// Interface is the Linux network interface name on which kube-vip
	// advertises the VIP (e.g. "eth0", "ens3", "bond0").
	//
	// Must be a valid Linux interface name: 1–15 characters, starting with a
	// letter, followed by letters, digits, dots, underscores, or hyphens.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9._-]{0,14}$`
	Interface string `json:"interface"`

	// Mode selects the VIP advertisement mechanism.
	// ARP (default) requires the control-plane nodes to share an L2 segment.
	// BGP requires a BGP peer and is intended for routed bare-metal fabrics.
	// +kubebuilder:default=ARP
	// +optional
	Mode KubeVIPMode `json:"mode,omitempty"`
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

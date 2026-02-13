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
	// Replicas is the number of control plane machines
	// Contract: ControlPlane MUST expose replicas
	// When replicas == 1, the control plane operates in single-node mode and k0s will be
	// configured with --single flag. For HA setups, set replicas > 1 (full HA support is planned).
	// +kubebuilder:validation:Minimum=1
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

	// FailureReason indicates the reason for control plane failure
	// This field is set only when the control plane fails permanently.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// FailureMessage indicates the message for control plane failure
	// This field is set only when the control plane fails permanently.
	// +optional
	FailureMessage string `json:"failureMessage,omitempty"`

	// Selector is the label selector for control plane machines
	// This is used to identify machines belonging to this control plane.
	// +optional
	Selector string `json:"selector,omitempty"`
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

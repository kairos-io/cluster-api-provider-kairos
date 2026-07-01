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
	// KairosConfigFinalizer allows the reconciler to clean up resources associated with KairosConfig before
	// removing it from the API server.
	KairosConfigFinalizer = "kairosconfig.bootstrap.cluster.x-k8s.io"

	// ControlPlaneJoinTokenSecretSuffix is appended to the cluster name to form
	// the per-cluster HA control-plane join-token Secret name (ADR 0005 Phase 3).
	// Shared here so the controlplane controller (which creates + owns it) and
	// the bootstrap controller (which resolves + pushes into it) agree on one
	// name.
	ControlPlaneJoinTokenSecretSuffix = "control-plane-join-token"

	// ControlPlaneJoinTokenSecretTypeLabel + Value mark the join-token Secret so
	// controllers' Secret-watch predicates match it by label (KD-15), never by
	// name suffix.
	ControlPlaneJoinTokenSecretTypeLabel = "controlplane.cluster.x-k8s.io/secret-type"
	ControlPlaneJoinTokenSecretTypeValue = "control-plane-join-token"

	// ControlPlaneJoinTokenSecretDataKey is the data key holding the join token.
	ControlPlaneJoinTokenSecretDataKey = "token"

	// EtcdStatusSecretSuffix is appended to the cluster name to form the
	// per-cluster HA etcd-health Secret name (ADR 0005 §E.1). Every control-plane
	// node PATCHes its own member key over the node-push channel; the controlplane
	// controller pre-creates the (empty) Secret. Unlike the KCP-owned single-writer
	// join-token Secret above, the etcd-status Secret is owned by the CLUSTER
	// (multi-writer — one data key per etcd member).
	EtcdStatusSecretSuffix = "etcd-status"

	// EtcdStatusSecretTypeLabel + Value mark the etcd-status Secret so controllers'
	// Secret-watch predicates match it by label (KD-15), never by name suffix.
	EtcdStatusSecretTypeLabel = "controlplane.cluster.x-k8s.io/secret-type"
	EtcdStatusSecretTypeValue = "etcd-status"
)

// ControlPlaneJoinTokenSecretName returns the per-cluster HA join-token Secret
// name for the given cluster.
func ControlPlaneJoinTokenSecretName(clusterName string) string {
	return clusterName + "-" + ControlPlaneJoinTokenSecretSuffix
}

// EtcdStatusSecretName returns the per-cluster HA etcd-status Secret name for the
// given cluster.
func EtcdStatusSecretName(clusterName string) string {
	return clusterName + "-" + EtcdStatusSecretSuffix
}

// ControlPlaneRole is the per-machine role discriminator for control-plane
// nodes. It is assigned by the KairosControlPlane controller and must not be
// set by end users directly; the value drives which cloud-config shape the
// bootstrap provider renders (Phase 2+).
//
// +kubebuilder:validation:Enum=single;init;join
type ControlPlaneRole string

const (
	// ControlPlaneRoleSingle is the role for a single-node control plane.
	// The bootstrap provider renders the distribution's single-node mode
	// (k0s --single / k3s single-server). Assigned when spec.replicas == 1
	// on the owning KairosControlPlane.
	ControlPlaneRoleSingle ControlPlaneRole = "single"

	// ControlPlaneRoleInit is the role for the first (initialising) node of
	// an HA control plane. The bootstrap provider renders the distribution's
	// cluster-init path (k0s managed-etcd init / k3s --cluster-init).
	// Assigned to the oldest CP machine when spec.replicas > 1.
	ControlPlaneRoleInit ControlPlaneRole = "init"

	// ControlPlaneRoleJoin is the role for subsequent nodes of an HA control
	// plane. The bootstrap provider renders the distribution's join path
	// (k0s controller-join / k3s --server). Assigned to all CP machines
	// other than the init node when spec.replicas > 1.
	ControlPlaneRoleJoin ControlPlaneRole = "join"
)

// KairosConfigSpec defines the desired state of KairosConfig
type KairosConfigSpec struct {
	// Role indicates whether this is a control-plane or worker node
	// +kubebuilder:validation:Enum=control-plane;worker
	// +kubebuilder:default=worker
	Role string `json:"role,omitempty"`

	// Distribution specifies the Kubernetes distribution to install
	// +kubebuilder:validation:Enum=k0s;k3s
	// +kubebuilder:default=k0s
	Distribution string `json:"distribution,omitempty"`

	// KubernetesVersion specifies the Kubernetes version to install
	// +kubebuilder:validation:Required
	KubernetesVersion string `json:"kubernetesVersion"`

	// ServerAddress is the address of the Kubernetes API server (for worker nodes)
	// +optional
	ServerAddress string `json:"serverAddress,omitempty"`

	// Token is the join token for worker nodes (if required by distribution)
	// +optional
	Token string `json:"token,omitempty"`

	// TokenSecretRef is a reference to a Secret containing the join token
	// +optional
	TokenSecretRef *corev1.ObjectReference `json:"tokenSecretRef,omitempty"`

	// CACertHashes are the CA certificate hashes for secure join
	// +optional
	CACertHashes []string `json:"caCertHashes,omitempty"`

	// CACertSecretRef is a reference to a Secret containing the CA certificate
	// +optional
	CACertSecretRef *corev1.ObjectReference `json:"caCertSecretRef,omitempty"`

	// Files specifies additional files to write to the node's filesystem via
	// the cloud-config write_files: list. Files are rendered on all
	// distributions (k0s, k3s) and all infrastructure providers. Each entry
	// is serialized with yaml.v3, which automatically selects block-scalar
	// representation for multi-line content.
	//
	// Constraints: at most 32 files; each file's content is limited to 32 KiB
	// (32768 bytes); paths must be absolute and must not contain .. segments.
	//
	// This field was accepted but silently ignored before this release.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Files []File `json:"files,omitempty"`

	// PreCommands are commands to run before k0s/k3s installation
	// +optional
	PreCommands []string `json:"preCommands,omitempty"`

	// PostCommands are commands to run after k0s/k3s installation
	// +optional
	PostCommands []string `json:"postCommands,omitempty"`

	// Pause indicates that reconciliation should be paused
	// +optional
	Pause bool `json:"pause,omitempty"`

	// SingleNode indicates this is a single-node control plane cluster.
	// When true, k0s will be configured with --single flag.
	//
	// Deprecated: SingleNode is derived by the KairosControlPlane controller
	// from spec.replicas (true when replicas==1) and will be removed in a
	// future v1beta3 revision (KD-39). New code should read ControlPlaneRole
	// instead. Both fields coexist during the transition period; the controller
	// sets both for backward compatibility with templates that branch on
	// SingleNode.
	// +optional
	SingleNode bool `json:"singleNode,omitempty"`

	// ControlPlaneRole is the init/join/single discriminator for this
	// control-plane machine. Set by the KairosControlPlane controller;
	// must not be set directly by end users.
	//
	// The zero value ("") is treated as equivalent to "single" by the
	// bootstrap controller for backward compatibility with KairosConfig
	// objects that predate this field. Do not rely on the zero value in
	// new code — the controller sets an explicit value on every machine it
	// creates.
	//
	// Rendered into the cloud-config in Phase 2; ignored by the bootstrap
	// controller until Phase 2 ships (the SingleNode field covers the
	// single-node case in the interim).
	// +kubebuilder:validation:Enum=single;init;join
	// +optional
	ControlPlaneRole ControlPlaneRole `json:"controlPlaneRole,omitempty"`

	// UserName is the username for the default user
	// +kubebuilder:default=kairos
	// +optional
	UserName string `json:"userName,omitempty"`

	// UserPassword is the password for the default user (inline; discouraged).
	//
	// Inline values are stored in the cluster as part of this resource's spec
	// and are visible to anyone with read access to KairosConfigs. Prefer
	// UserPasswordSecretRef for any non-lab use — the password then lives in
	// a Kubernetes Secret which can be encrypted at rest and is subject to
	// separate RBAC.
	//
	// No default is applied. A KairosConfig MUST set at least one of
	// userPassword, userPasswordSecretRef, sshPublicKey, or gitHubUser; the
	// validating webhook enforces this. If both UserPassword and
	// UserPasswordSecretRef are set, UserPasswordSecretRef takes precedence.
	// +optional
	UserPassword string `json:"userPassword,omitempty"`

	// UserPasswordSecretRef is a reference to a Secret containing the user
	// password. The Secret must live in the same namespace as the KairosConfig
	// unless Namespace is set explicitly. The Secret's data key defaults to
	// "password". Recommended over inline UserPassword.
	// +optional
	UserPasswordSecretRef *UserPasswordSecretReference `json:"userPasswordSecretRef,omitempty"`

	// UserGroups are the groups for the default user
	// +kubebuilder:default={admin}
	// +optional
	UserGroups []string `json:"userGroups,omitempty"`

	// GitHubUser is the GitHub username for SSH key access (e.g., "octocat")
	// If set, SSH keys will be fetched from GitHub
	// +optional
	GitHubUser string `json:"githubUser,omitempty"`

	// SSHPublicKey is a raw SSH public key (alternative to GitHubUser)
	// +optional
	SSHPublicKey string `json:"sshPublicKey,omitempty"`

	// WorkerToken is the join token for worker nodes (inline specification)
	// For production use, prefer WorkerTokenSecretRef instead.
	// If both WorkerToken and WorkerTokenSecretRef are set, WorkerTokenSecretRef takes precedence.
	// +optional
	WorkerToken string `json:"workerToken,omitempty"`

	// WorkerTokenSecretRef is a reference to a Secret containing the worker join token
	// This is the recommended way to provide worker tokens for security.
	// The Secret must contain a key specified by WorkerTokenSecretRef.Key (defaults to "token").
	// +optional
	WorkerTokenSecretRef *WorkerTokenSecretReference `json:"workerTokenSecretRef,omitempty"`

	// K3sToken is the join token for k3s nodes (inline specification)
	// For production use, prefer K3sTokenSecretRef instead.
	// If both K3sToken and K3sTokenSecretRef are set, K3sTokenSecretRef takes precedence.
	// +optional
	K3sToken string `json:"k3sToken,omitempty"`

	// K3sTokenSecretRef is a reference to a Secret containing the k3s join token
	// The Secret must contain a key specified by K3sTokenSecretRef.Key (defaults to "token").
	// +optional
	K3sTokenSecretRef *WorkerTokenSecretReference `json:"k3sTokenSecretRef,omitempty"`

	// ControlPlaneJoinTokenSecretRef references a Secret holding the k0s
	// control-plane (controller-role) join token used by an HA "join" node
	// (ADR 0005 Phase 3). The token is minted on the init node via
	// `k0s token create --role=controller`, pushed back to the management cluster
	// over the node-push channel, and stored in an owner-ref'd Secret that the
	// KairosControlPlane controller points this ref at.
	//
	// Set by the KairosControlPlane controller; not user-set. There is NO inline
	// counterpart (TOKEN-INV / no new inline-secret fields): the controller-join
	// token is secret material and must live in a Secret, never in this spec.
	// The Secret must contain a key specified by Key (defaults to "token").
	//
	// k3s HA reuses K3sTokenSecretRef for the shared server token; this field is
	// k0s-specific.
	// +optional
	ControlPlaneJoinTokenSecretRef *WorkerTokenSecretReference `json:"controlPlaneJoinTokenSecretRef,omitempty"`

	// ControlPlaneVIP is the kube-vip configuration the KairosControlPlane
	// controller copies down from KairosControlPlane.Spec.HA.VIP so the bootstrap
	// renderer can emit the kube-vip static-pod manifest on HA control-plane nodes
	// (ADR 0005 Phase 3). It is consulted by the renderer ONLY on init/join
	// control-plane roles on non-KubeVirt infrastructure (RenderKubeVIP gate).
	//
	// Set by the KairosControlPlane controller; not user-set. Keeping a flat copy
	// here (rather than having the bootstrap controller reach into the
	// controlplane API group) preserves internal/bootstrap's API-server-unaware
	// contract: the controller marshals it into TemplateData.VIP.
	// +optional
	ControlPlaneVIP *ControlPlaneVIP `json:"controlPlaneVIP,omitempty"`

	// Manifests are Kubernetes manifests to be placed in the distribution manifests directory.
	// These will be automatically applied by the distribution at cluster startup.
	// k0s: /var/lib/k0s/manifests/{Name}/{File}
	// k3s: /var/lib/rancher/k3s/server/manifests/{Name}/{File}
	// +optional
	Manifests []Manifest `json:"manifests,omitempty"`

	// Hostname is the node hostname to set inside the VM
	// If set, it takes precedence over HostnamePrefix.
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// HostnamePrefix is the prefix for the hostname that will be set on the node
	// The final hostname will be: {HostnamePrefix}{{ trunc 4 .MachineID }}
	// For example, if HostnamePrefix is "metal-", the hostname will be "metal-{4-char-machine-id}"
	// Defaults to "metal-" if not specified
	// +kubebuilder:default=metal-
	// +optional
	HostnamePrefix string `json:"hostnamePrefix,omitempty"`

	// DNSServers configures DNS resolvers for early boot
	// This helps pulling CNI images before cluster DNS is ready.
	// +optional
	DNSServers []string `json:"dnsServers,omitempty"`

	// PodCIDR configures the pod network CIDR for k0s
	// Defaults to k0s defaults if not specified.
	// +optional
	PodCIDR string `json:"podCIDR,omitempty"`

	// ServiceCIDR configures the service network CIDR for k0s
	// Defaults to k0s defaults if not specified.
	// +optional
	ServiceCIDR string `json:"serviceCIDR,omitempty"`

	// PrimaryIP overrides the detected node IP for KubeVirt control-plane
	// certificates and endpoint configuration. This sets KAIROS_PRIMARY_IP.
	// +optional
	PrimaryIP string `json:"primaryIP,omitempty"`

	// Install specifies the Kairos installation configuration
	// This controls how Kairos OS is installed to disk
	// +optional
	Install *InstallConfig `json:"install,omitempty"`
}

// InstallConfig specifies the Kairos installation configuration
type InstallConfig struct {
	// Auto enables automatic installation to disk
	// When true, Kairos will automatically install to the specified device
	// +kubebuilder:default=true
	// +optional
	Auto *bool `json:"auto,omitempty"`

	// Device specifies the target device for installation
	// Use "auto" to automatically detect and use the first available disk
	// Or specify a device path like "/dev/sda" or "/dev/nvme0n1"
	// +kubebuilder:default=auto
	// +optional
	Device string `json:"device,omitempty"`

	// Reboot specifies whether to reboot after installation
	// When true, the system will reboot automatically after installation completes
	// +kubebuilder:default=true
	// +optional
	Reboot *bool `json:"reboot,omitempty"`
}

// WorkerTokenSecretReference is a reference to a Secret containing a worker join token
type WorkerTokenSecretReference struct {
	// Name is the name of the Secret
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Key is the key within the Secret that contains the token
	// Defaults to "token" if not specified
	// +kubebuilder:default=token
	// +optional
	Key string `json:"key,omitempty"`

	// Namespace is the namespace of the Secret
	// If not specified, defaults to the same namespace as the KairosConfig
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// UserPasswordSecretReference is a reference to a Secret containing the user
// password. Structurally identical to WorkerTokenSecretReference except for
// the default Key — "password" instead of "token" — so users following the
// most common convention can omit Key entirely.
type UserPasswordSecretReference struct {
	// Name is the name of the Secret
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Key is the key within the Secret that contains the password.
	// Defaults to "password" if not specified.
	// +kubebuilder:default=password
	// +optional
	Key string `json:"key,omitempty"`

	// Namespace is the namespace of the Secret.
	// If not specified, defaults to the same namespace as the KairosConfig.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ControlPlaneVIP is the flat, render-ready view of the kube-vip configuration
// (KairosControlPlane.Spec.HA.VIP) propagated onto a control-plane KairosConfig
// by the KairosControlPlane controller (ADR 0005 Phase 3). The bootstrap
// controller converts it into internal/bootstrap.VIPConfig so the renderer
// stays unaware of CAPI/controlplane types.
//
// Address and Interface mirror the validation on
// controlplane/v1beta2.KubeVIPConfig and are re-validated at render time
// (VIP-INV-3). This field is set by the controller, not by end users.
type ControlPlaneVIP struct {
	// Address is the virtual IP address or DNS hostname for the control-plane
	// endpoint. Must be a valid IPv4 address, IPv6 address, or RFC-1123 hostname.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Address string `json:"address"`

	// Interface is the Linux network interface name on which kube-vip advertises
	// the VIP (e.g. "eth0", "ens3", "bond0").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9._-]{0,14}$`
	Interface string `json:"interface"`

	// Mode selects the VIP advertisement mechanism: "ARP" (default, L2) or
	// "BGP" (L3). Empty is treated as ARP.
	// +kubebuilder:validation:Enum=ARP;BGP
	// +kubebuilder:default=ARP
	// +optional
	Mode string `json:"mode,omitempty"`
}

// Manifest represents a Kubernetes manifest file to be deployed by the
// workload distribution. The manifest is placed at:
//   - k0s: /var/lib/k0s/manifests/{Name}/{File}
//   - k3s: /var/lib/rancher/k3s/server/manifests/{Name}/{File}
//
// and is auto-applied by the distribution at server start. Manifests are
// applied on control-plane nodes only.
type Manifest struct {
	// Name is the directory name under the distribution's manifests directory.
	// This creates a directory structure:
	//   - k0s: /var/lib/k0s/manifests/{Name}/{File}
	//   - k3s: /var/lib/rancher/k3s/server/manifests/{Name}/{File}
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// File is the filename within the Name directory. The distribution's
	// manifest loader auto-applies it from:
	//   - k0s: /var/lib/k0s/manifests/{Name}/{File}
	//   - k3s: /var/lib/rancher/k3s/server/manifests/{Name}/{File}
	// +kubebuilder:validation:Required
	File string `json:"file"`

	// Content is the manifest YAML content
	// +kubebuilder:validation:Required
	Content string `json:"content"`
}

// File represents a file to be written via the cloud-config write_files: list.
// The entry is serialized with yaml.v3; multi-line Content is emitted as a
// block scalar automatically.
type File struct {
	// Path is the absolute path where the file is written on the node.
	// Must begin with '/' and must not contain '..' path segments. The
	// webhook rejects non-absolute paths and paths that contain '..'.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^/`
	Path string `json:"path"`

	// Content is the file content. Multi-line strings are accepted and are
	// emitted as a YAML block scalar. Maximum 32 KiB (32768 bytes).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=32768
	Content string `json:"content"`

	// Permissions is the file mode in octal notation. Accepts 3-digit or
	// 4-digit forms; the leading digit encodes setuid (4), setgid (2), and
	// sticky (1) bits. Examples: "0644" (world-readable regular file),
	// "0750" (group-accessible, others excluded), "4755" (setuid executable).
	// +optional
	// +kubebuilder:validation:Pattern=`^0?[0-7]{3,4}$`
	Permissions string `json:"permissions,omitempty"`

	// Owner is the file owner in user:group format (e.g., "root:root").
	// Both user and group must follow POSIX username conventions: start with
	// a letter or underscore, followed by letters, digits, underscores, or
	// hyphens. The group portion (including the colon) is optional.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_-]*(:[a-z_][a-z0-9_-]*)?$`
	Owner string `json:"owner,omitempty"`
}

// KairosConfigStatus defines the observed state of KairosConfig
// Contract: BootstrapConfig v1beta2 MUST expose a dataSecretName and ready status
type KairosConfigStatus struct {
	// Ready indicates the bootstrap data has been generated and is ready
	// Contract: BootstrapConfig MUST indicate bootstrap completion
	// This field MUST be set to true when bootstrap data is available and ready to use.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// DataSecretName is the name of the Secret containing the bootstrap data
	// Contract: BootstrapConfig MUST expose a dataSecretName
	// The Secret must be in the same namespace as the KairosConfig.
	// +optional
	DataSecretName *string `json:"dataSecretName,omitempty"`

	// Initialization provides observations of the KairosConfig initialization process.
	// NOTE: Fields in this struct are part of the Cluster API contract and are used to orchestrate initial Machine provisioning.
	// +optional
	Initialization *KairosConfigInitialization `json:"initialization,omitempty"`

	// Conditions defines current service state of the KairosConfig
	// Contract: BootstrapConfig SHOULD expose Conditions
	// Standard CAPI conditions: Ready, BootstrapReady, DataSecretAvailable
	// +optional
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`

	// ObservedGeneration is the most recent generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// FailureReason is a short machine-readable string indicating why the
	// bootstrap controller failed on its last reconcile attempt. Cleared
	// automatically when a subsequent reconcile succeeds, so a non-empty
	// value indicates an ongoing failure, not a terminal one.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// FailureMessage is a human-readable description of the last bootstrap
	// failure. Cleared automatically when a subsequent reconcile succeeds.
	// If non-empty, check the owning Machine's events for context.
	// +optional
	FailureMessage string `json:"failureMessage,omitempty"`
}

// KairosConfigInitialization provides observations of the KairosConfig initialization process.
// NOTE: Fields in this struct are part of the Cluster API contract, and they are used to orchestrate initial Machine provisioning.
type KairosConfigInitialization struct {
	// DataSecretCreated is true when the Machine's bootstrap secret is created.
	// NOTE: this field is part of the Cluster API contract, and it is used to orchestrate initial Machine provisioning.
	// +optional
	DataSecretCreated bool `json:"dataSecretCreated,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=kairosconfigs,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready",description="Bootstrap ready"
// +kubebuilder:printcolumn:name="DataSecretName",type="string",JSONPath=".status.dataSecretName",description="Secret containing bootstrap data"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// KairosConfig is the Schema for the kairosconfigs API
type KairosConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KairosConfigSpec   `json:"spec,omitempty"`
	Status KairosConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KairosConfigList contains a list of KairosConfig
type KairosConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KairosConfig `json:"items"`
}

// GetConditions returns the set of conditions for this object.
func (c *KairosConfig) GetConditions() clusterv1.Conditions {
	return c.Status.Conditions
}

// SetConditions sets the conditions on this object.
func (c *KairosConfig) SetConditions(conditions clusterv1.Conditions) {
	c.Status.Conditions = conditions
}

func init() {
	SchemeBuilder.Register(&KairosConfig{}, &KairosConfigList{})
}

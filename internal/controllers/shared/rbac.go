// Package sharedrbac holds the +kubebuilder:rbac markers that are common to
// BOTH the bootstrap and the control-plane managers. It contains no runtime
// code — it exists only so `make manifests` has a single source of truth for
// the grants that every provider role needs.
//
// Why a dedicated package: `make manifests` runs three controller-gen passes —
// a combined `manager-role` from `./...` (the flat/default overlay), a
// `bootstrap-manager-role` from `bootstrap/ + shared/`, and a
// `control-plane-manager-role` from `controlplane/ + shared/`. Any grant that
// both providers need must live in exactly one package that both the bootstrap
// pass and the control-plane pass include — this one. A naive path-keyed split
// that leaves a shared marker on only one controller drops it from the other
// role: e.g. if `customresourcedefinitions` stayed only on the control-plane
// controller, the bootstrap role would lose it and every contract-versioned
// infra reference resolution (getProviderID) would fail Forbidden.
//
// Keep this list minimal and shared-only. Provider-specific grants stay on the
// respective controller so the boundary (bootstrap read-only on machines/
// clusters, control-plane no roles/rolebindings) is legible at the source.
package sharedrbac

// Leader election: each manager runs its own namespaced, Lease-based election.
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=create;get;list;update;patch;watch

// CAPI v1beta2 contract-versioned reference resolution (ADR 0006): resolving a
// ContractVersionedObjectReference to a served apiVersion goes through
// external.GetObjectFromContractVersionedRef -> contract.GetGKMetadata, which
// Gets the target resource's CustomResourceDefinition to read its contract
// label. The bootstrap controller (getProviderID) and the control-plane
// controller (node-IP / kubeconfig resolution) both hit this path; without the
// grant those Gets fail Forbidden. Read-only, cluster-scoped (CRDs are), no
// write — matches upstream CAPI's own role.
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

// Event recording: both managers emit Events on the objects they reconcile.
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

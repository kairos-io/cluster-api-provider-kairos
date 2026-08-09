package kubevirtenv

// Manifest versions and URLs (keep aligned with historical kubevirt-env defaults).

const (
	CalicoVersion     = "v3.29.1"
	CalicoManifestURL = "https://raw.githubusercontent.com/projectcalico/calico/%s/manifests/calico.yaml"

	LocalPathManifestURL = "https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.28/deploy/local-path-storage.yaml"
	LocalPathNamespace   = "local-path-storage"
	LocalPathClassName   = "local-path"

	CDIOperatorURL = "https://github.com/kubevirt/containerized-data-importer/releases/latest/download/cdi-operator.yaml"
	CDICRURL       = "https://github.com/kubevirt/containerized-data-importer/releases/latest/download/cdi-cr.yaml"

	// KubeVirtVersion: v1.9.0 is the KubeVirt release built for Kubernetes 1.36
	// (it supports k8s 1.34-1.36). The e2e management cluster now runs k8s 1.36
	// (kindest/node:v1.36.1 below), which is outside v1.8.2's support window
	// (k8s 1.34-1.35), so the bump is required, not cosmetic. Recent KubeVirt
	// releases handle the dbus / machine-id setup in the kindest/node images
	// that broke the old v1.3.0 pin (virt-launcher "timed out waiting for domain
	// to be defined"). NOTE: v1.9.0 on kindest/node:v1.36.1 still needs a live
	// e2e run to confirm VMs boot before this is trusted in CI.
	KubeVirtVersion     = "v1.9.0"
	KubeVirtOperatorURL = "https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-operator.yaml"
	KubeVirtCRURL       = "https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-cr.yaml"

	// KindNodeImage pins kindest/node by digest so a re-tag of the `v1.36.1`
	// floating tag upstream cannot silently break our CI. v1.36.1 is shipped by
	// kind v0.32.0 (see binaries.go) and is the top of Cluster API v1.13's
	// supported management-cluster band. Bump this together with kind itself
	// (and confirm KubeVirt still supports the bundled k8s version).
	KindNodeImage = "kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5"

	CertManagerVersion = "v1.16.2"
	CertManagerURL     = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

	// KairosOperatorGitRef pins kairos-io/kairos-operator for kubectl apply -k (CRDs, controller, nginx).
	KairosOperatorGitRef = "v0.1.0-beta4"
)

// KairosOperatorKustomizeDefaultURL is operator + CRDs + RBAC (namespace operator-system).
func KairosOperatorKustomizeDefaultURL() string {
	return "https://github.com/kairos-io/kairos-operator/config/default?ref=" + KairosOperatorGitRef
}

// KairosOperatorKustomizeNginxURL is the optional nginx NodePort used for OSArtifact exporter uploads.
func KairosOperatorKustomizeNginxURL() string {
	return "https://github.com/kairos-io/kairos-operator/config/nginx?ref=" + KairosOperatorGitRef
}

const applyFieldManager = "kubevirt-env"

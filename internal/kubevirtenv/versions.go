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

	KubeVirtVersion     = "v1.3.0"
	KubeVirtOperatorURL = "https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-operator.yaml"
	KubeVirtCRURL       = "https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-cr.yaml"

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

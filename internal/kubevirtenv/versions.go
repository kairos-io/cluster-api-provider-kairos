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

	// KubeVirtVersion: bumped from v1.3.0 → v1.8.2 (latest stable as of 2026-05).
	// v1.3.0 was incompatible with the dbus / machine-id setup in the current
	// kindest/node images (virt-launcher panics with "timed out waiting for
	// domain to be defined" because the session bus cannot start). Newer
	// KubeVirt releases handle that case. v1.8.2 also tracks Kubernetes 1.35
	// which is what kindest/node:v1.35.0 provides.
	KubeVirtVersion     = "v1.8.2"
	KubeVirtOperatorURL = "https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-operator.yaml"
	KubeVirtCRURL       = "https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-cr.yaml"

	// KindNodeImage pins kindest/node by digest so a re-tag of the `v1.35.0`
	// floating tag upstream cannot silently break our CI. The digest was
	// fetched from Docker Hub on 2026-05-21. Bump this together with kind
	// itself (and confirm KubeVirt still supports the bundled k8s version).
	KindNodeImage = "kindest/node:v1.35.0@sha256:4613778f3cfcd10e615029370f5786704559103cf27bef934597ba562b269661"

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

# API Reference

Last verified against: Kairos v3.6.0+, CAPI v1.8.x (go.mod), provider release v0.1.0-alpha.1 / v0.1.0-alpha.2.

This document provides a reference for all Custom Resource Definitions (CRDs) provided by the Kairos CAPI Provider. See [Install guide](INSTALL.md) for development install. Quickstarts: [CAPD](QUICKSTART_CAPD.md), [CAPV](QUICKSTART_CAPV.md), [CAPK](QUICKSTART_CAPK.md).

## Table of Contents

- [KairosConfig](#kairosconfig)
- [KairosConfigTemplate](#kairosconfigtemplate)
- [KairosControlPlane](#kairoscontrolplane)
- [KairosControlPlaneTemplate](#kairoscontrolplanetemplate)
- [Persistence behavior](#persistence-behavior)
- [Notes](#notes)

---

## KairosConfig

**API Group:** `bootstrap.cluster.x-k8s.io`
**API Version:** `v1beta2`
**Kind:** `KairosConfig`

`KairosConfig` is a BootstrapConfig resource that generates Kairos cloud-config for bootstrapping Kubernetes nodes (control-plane or worker) using k0s or k3s.

### Spec Fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `role` | `string` | Yes | `"worker"` | Node role: `"control-plane"` or `"worker"`. |
| `distribution` | `string` | No | `"k0s"` | Kubernetes distribution: `"k0s"` or `"k3s"`. |
| `kubernetesVersion` | `string` | Yes | — | Kubernetes version string (e.g., `"v1.34.1+k0s.1"`). The value is informational — the actual version is pinned in the Kairos image at build time and cannot be changed by this field. See KD-24. |
| `singleNode` | `bool` | No | `false` | Signals single-node mode to the cloud-config renderer. For k0s, this adds `--single`; for k3s, it enables cluster-init mode. The KairosControlPlane controller derives this from `replicas==1`, so manual overrides are typically unnecessary. Applies to both k0s and k3s distributions. Tracked as a deprecation candidate in KD-39. |
| `userName` | `string` | No | `"kairos"` | Username for the default OS user. |
| `userPassword` | `string` | No | — | Password for the default OS user, specified inline. Inline values are stored in the resource and visible to anyone with read access to KairosConfig objects. Prefer `userPasswordSecretRef`. At least one of `userPassword`, `userPasswordSecretRef`, `sshPublicKey`, or `githubUser` must be set; the validating webhook enforces this. If both `userPassword` and `userPasswordSecretRef` are set, `userPasswordSecretRef` takes precedence. |
| `userPasswordSecretRef` | `UserPasswordSecretReference` | No | — | Reference to a Secret containing the OS user password. The Secret must have a key matching `userPasswordSecretRef.key` (default: `"password"`). Preferred over inline `userPassword`. |
| `userGroups` | `[]string` | No | `["admin"]` | Groups for the default OS user. |
| `githubUser` | `string` | No | — | GitHub username for SSH key access. The Kairos image fetches the user's public keys from `https://github.com/<githubUser>.keys` at boot. |
| `sshPublicKey` | `string` | No | — | Raw SSH public key (alternative to `githubUser`). |
| `serverAddress` | `string` | No | — | Kubernetes API server address for worker nodes to join (e.g., `"https://10.0.0.1:6443"`). |
| `token` | `string` | No | — | Generic join token for worker nodes (inline). Prefer `tokenSecretRef`. |
| `tokenSecretRef` | `ObjectReference` | No | — | Reference to a Secret containing a generic join token. |
| `workerToken` | `string` | No | — | k0s worker join token, inline. Prefer `workerTokenSecretRef`. If both are set, `workerTokenSecretRef` takes precedence. |
| `workerTokenSecretRef` | `WorkerTokenSecretReference` | No | — | Reference to a Secret containing the k0s worker join token. Required for k0s workers; prefer this over inline `workerToken`. |
| `k3sToken` | `string` | No | — | k3s join token, inline. Prefer `k3sTokenSecretRef`. If both are set, `k3sTokenSecretRef` takes precedence. |
| `k3sTokenSecretRef` | `WorkerTokenSecretReference` | No | — | Reference to a Secret containing the k3s join token. Required for k3s workers; prefer this over inline `k3sToken`. |
| `caCertHashes` | `[]string` | No | — | CA certificate hashes for secure node join. |
| `caCertSecretRef` | `ObjectReference` | No | — | Reference to a Secret containing the CA certificate. |
| `hostname` | `string` | No | — | Hostname to set on the node inside the VM. Takes precedence over `hostnamePrefix` when both are set. |
| `hostnamePrefix` | `string` | No | `"metal-"` | Prefix for the auto-generated hostname. The final hostname is `{hostnamePrefix}{4-char-machine-id}`. |
| `dnsServers` | `[]string` | No | — | DNS resolvers configured for early boot, before cluster DNS is ready (useful for pulling CNI images). |
| `podCIDR` | `string` | No | — | Pod network CIDR for k0s. Uses k0s defaults when unset. |
| `serviceCIDR` | `string` | No | — | Service network CIDR for k0s. Uses k0s defaults when unset. |
| `primaryIP` | `string` | No | — | Overrides the detected node IP used for TLS certificate SANs and endpoint configuration (sets `KAIROS_PRIMARY_IP`). Useful in KubeVirt environments where the detected IP is a pod network address rather than the VM's accessible address. |
| `install` | `InstallConfig` | No | — | Controls Kairos OS installation to disk. Required when using the 2-disk installer pattern (see `config/samples/capk/`). |
| `files` | `[]File` | No | — | Additional files to write in the cloud-config. |
| `manifests` | `[]Manifest` | No | — | Kubernetes manifests placed in the distribution's auto-apply directory. k0s: `/var/lib/k0s/manifests/{name}/{file}`. k3s: `/var/lib/rancher/k3s/server/manifests/{name}/{file}`. Applied automatically by the distribution at cluster startup. |
| `preCommands` | `[]string` | No | — | Reserved; not yet rendered into the cloud-config. |
| `postCommands` | `[]string` | No | — | Reserved; not yet rendered into the cloud-config. |
| `pause` | `bool` | No | `false` | When `true`, pauses reconciliation of this KairosConfig. |

#### UserPasswordSecretReference

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | `string` | Yes | — | Name of the Secret. |
| `key` | `string` | No | `"password"` | Key within the Secret that contains the password. |
| `namespace` | `string` | No | Same as KairosConfig | Namespace of the Secret. |

#### WorkerTokenSecretReference

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | `string` | Yes | — | Name of the Secret. |
| `key` | `string` | No | `"token"` | Key within the Secret containing the token. |
| `namespace` | `string` | No | Same as KairosConfig | Namespace of the Secret. |

#### InstallConfig

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `auto` | `*bool` | No | `true` | When `true`, Kairos installs to disk automatically at first boot. |
| `device` | `string` | No | `"auto"` | Target device for installation (e.g., `"/dev/vda"`). `"auto"` selects the first available disk. |
| `reboot` | `*bool` | No | `true` | When `true`, the system reboots automatically after installation completes. |

#### File

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `path` | `string` | Yes | Absolute path where the file is written on the node. |
| `content` | `string` | Yes | File content. |
| `permissions` | `string` | No | Octal permission string (e.g., `"0644"`). |
| `owner` | `string` | No | Owner in `user:group` format (e.g., `"root:root"`). |

#### Manifest

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Directory name under the distribution's manifest path. |
| `file` | `string` | Yes | Filename within the directory. |
| `content` | `string` | Yes | YAML content of the manifest. |

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `ready` | `bool` | `true` when bootstrap data has been generated and the bootstrap Secret is available for the CAPI Machine controller. |
| `dataSecretName` | `*string` | Name of the Secret containing the bootstrap cloud-config. |
| `initialization.dataSecretCreated` | `bool` | v1beta2 contract field: `true` when the bootstrap Secret has been created. |
| `conditions` | `[]Condition` | Standard CAPI conditions: `Ready`, `BootstrapReady`, `DataSecretAvailable`. |
| `observedGeneration` | `int64` | Most recent generation observed by the controller. |
| `failureReason` | `string` | Short machine-readable string indicating the last failure reason. Cleared automatically when the next reconcile succeeds — a non-empty value indicates an ongoing failure, not a terminal one. |
| `failureMessage` | `string` | Human-readable description of the last failure. Cleared automatically on the next successful reconcile. If non-empty, check the owning Machine's events for context. |

### Example

```yaml
# Secret referenced by userPasswordSecretRef below.
# Create before applying the KairosConfig:
#   kubectl create secret generic kairos-user-password \
#     --from-literal=password=$(openssl rand -base64 32)
apiVersion: v1
kind: Secret
metadata:
  name: kairos-user-password
  namespace: default
type: Opaque
stringData:
  password: REPLACE_WITH_A_STRONG_PASSWORD
---
apiVersion: bootstrap.cluster.x-k8s.io/v1beta2
kind: KairosConfig
metadata:
  name: kairos-config-control-plane
  namespace: default
spec:
  role: control-plane
  distribution: k0s
  kubernetesVersion: "v1.34.1+k0s.1"
  singleNode: true
  userName: kairos
  userPasswordSecretRef:
    name: kairos-user-password
  userGroups:
    - admin
```

For k3s workers, use `k3sTokenSecretRef`:

```yaml
apiVersion: bootstrap.cluster.x-k8s.io/v1beta2
kind: KairosConfig
metadata:
  name: kairos-config-worker
  namespace: default
spec:
  role: worker
  distribution: k3s
  kubernetesVersion: "v1.35.0+k3s1"
  userPasswordSecretRef:
    name: kairos-user-password
  k3sTokenSecretRef:
    name: k3s-worker-token
    key: token
```

---

## KairosConfigTemplate

**API Group:** `bootstrap.cluster.x-k8s.io`
**API Version:** `v1beta2`
**Kind:** `KairosConfigTemplate`

`KairosConfigTemplate` is a template for creating `KairosConfig` resources. Used by `MachineDeployment` and `KairosControlPlane` to create per-machine bootstrap configurations.

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `template` | `KairosConfigTemplateResource` | Yes | Template for creating `KairosConfig` resources. |

#### KairosConfigTemplateResource

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `metadata` | `ObjectMeta` | No | Metadata to apply to created `KairosConfig` resources. |
| `spec` | `KairosConfigSpec` | Yes | Spec applied to created `KairosConfig` resources. See [KairosConfig Spec](#spec-fields). |

### Example

```yaml
apiVersion: bootstrap.cluster.x-k8s.io/v1beta2
kind: KairosConfigTemplate
metadata:
  name: kairos-config-template-worker
  namespace: default
spec:
  template:
    spec:
      role: worker
      distribution: k0s
      kubernetesVersion: "v1.34.1+k0s.1"
      userPasswordSecretRef:
        name: kairos-user-password
      workerTokenSecretRef:
        name: worker-token
        key: token
```

---

## KairosControlPlane

**API Group:** `controlplane.cluster.x-k8s.io`
**API Version:** `v1beta2`
**Kind:** `KairosControlPlane`

`KairosControlPlane` manages the control plane machines for a Kubernetes cluster running on Kairos OS with k0s or k3s.

### Spec Fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `replicas` | `*int32` | No | `1` | Number of control plane machines. Must be exactly `1`. The validating webhook rejects values greater than 1 — the current bootstrap logic would otherwise produce N independent single-node clusters. HA control planes are tracked for a future release (KD-5b / KD-25). |
| `version` | `string` | Yes | — | Kubernetes version string (e.g., `"v1.34.1+k0s.1"`). Informational; the actual k8s version is pinned in the Kairos image. |
| `distribution` | `string` | No | `"k0s"` | Kubernetes distribution for this control plane: `"k0s"` or `"k3s"`. |
| `machineTemplate` | `KairosControlPlaneMachineTemplate` | Yes | — | Template for creating control plane Machines. |
| `kairosConfigTemplate` | `KairosConfigTemplateReference` | Yes | — | Reference to a `KairosConfigTemplate` that provides the bootstrap configuration for each Machine. |
| `rolloutStrategy` | `RolloutStrategy` | No | — | Strategy for rolling out updates. |

#### KairosControlPlaneMachineTemplate

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `infrastructureRef` | `ObjectReference` | Yes | Reference to the infrastructure template (e.g., `DockerMachineTemplate`, `VSphereMachineTemplate`, `KubevirtMachineTemplate`). |
| `nodeDrainTimeout` | `Duration` | No | Timeout for draining nodes during updates. |
| `metadata` | `ObjectMeta` | No | Metadata to apply to created Machines. |

#### KairosConfigTemplateReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Name of the `KairosConfigTemplate`. |
| `apiVersion` | `string` | No | API version (defaults to `bootstrap.cluster.x-k8s.io/v1beta2`). |
| `kind` | `string` | No | Kind (defaults to `KairosConfigTemplate`). |

The `namespace` field is not part of this reference. The namespace defaults to the same namespace as the `KairosControlPlane`.

#### RolloutStrategy

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `type` | `string` | No | `"RollingUpdate"` | Strategy type. Currently only `"RollingUpdate"` is accepted. |
| `rollingUpdate` | `RollingUpdate` | No | — | Rolling update configuration. |

#### RollingUpdate

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `maxSurge` | `*int32` | No | Maximum number of machines that can be created above the desired count during a rollout. |

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `initialized` | `bool` | `true` when the first control plane Machine is ready and the control plane is functional. |
| `initialization.controlPlaneInitialized` | `*bool` | v1beta2 contract field. `true` when the control plane has been initialized and can accept requests. |
| `readyReplicas` | `int32` | Number of control plane Machines that are ready. |
| `replicas` | `int32` | Total number of control plane Machines across all states. |
| `updatedReplicas` | `int32` | Number of Machines running the desired version. |
| `unavailableReplicas` | `int32` | Number of Machines that are unavailable (not ready or being deleted). |
| `conditions` | `[]Condition` | Standard CAPI conditions: `Ready`, `Available`, `Initialized`. |
| `observedGeneration` | `int64` | Most recent generation observed by the controller. |
| `failureReason` | `string` | Short machine-readable failure indicator. Cleared automatically when the next reconcile succeeds — a non-empty value indicates an ongoing failure, not a terminal one. |
| `failureMessage` | `string` | Human-readable failure description. Cleared automatically on the next successful reconcile. If non-empty, check KairosControlPlane events and owned Machine events for context. |
| `selector` | `string` | Label selector string identifying control plane Machines. |

### Example

```yaml
apiVersion: controlplane.cluster.x-k8s.io/v1beta2
kind: KairosControlPlane
metadata:
  name: kairos-control-plane
  namespace: default
spec:
  replicas: 1
  version: "v1.34.1+k0s.1"
  machineTemplate:
    infrastructureRef:
      apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
      kind: DockerMachineTemplate
      name: control-plane-template
  kairosConfigTemplate:
    name: kairos-config-template-control-plane
```

---

## KairosControlPlaneTemplate

**API Group:** `controlplane.cluster.x-k8s.io`
**API Version:** `v1beta2`
**Kind:** `KairosControlPlaneTemplate`

`KairosControlPlaneTemplate` is a template for creating `KairosControlPlane` resources. Intended for use with ClusterClass (planned; not yet exercised in samples).

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `template` | `KairosControlPlaneTemplateResource` | Yes | Template for creating `KairosControlPlane` resources. |

#### KairosControlPlaneTemplateResource

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `metadata` | `ObjectMeta` | No | Metadata to apply to created `KairosControlPlane` resources. |
| `spec` | `KairosControlPlaneSpec` | Yes | Spec applied to created `KairosControlPlane` resources. See [KairosControlPlane Spec](#spec-fields-1). |

---

## Persistence behavior

The provider injects `/system/oem/12_kairos-capi-persistency.yaml` into every node's cloud-config via `write_files`. The file uses immucore's `extra-layout.env` mechanism (not `cos-layout.env`), so its `PERSISTENT_STATE_PATHS` value is **unioned** with the stock image's persistent paths — never overwriting them. A custom or stock Kairos image is unaffected; the provider's persistent paths are added on top of whatever the image already persists.

The provider declares the following paths as persistent across reboots and A/B upgrades:

- `/etc/cni`
- `/etc/k0s`
- `/etc/kubernetes`
- `/etc/rancher`
- `/etc/ssh`
- `/etc/systemd`
- `/var/lib/cni`
- `/var/lib/containerd`
- `/var/lib/k0s`
- `/var/lib/kubelet`
- `/var/lib/rancher`
- `/var/log`

**Second-boot-onward semantic.** The first install boot consumes the cloud-config directly (the bootstrap Secret is rendered into userdata and applied by `kairos-agent`). From the second reboot onward, the on-disk `/system/oem/12_kairos-capi-persistency.yaml` is what immucore reads at every boot to assemble the persistent overlay.

**Adding your own persistent paths.** Users who need application-specific persistent paths can layer their own cloud-config snippet by writing a file via `Spec.Files` to `/oem/91_custom.yaml` (or any `/oem/9*` prefix). Lower numeric prefixes (`5x`–`8x`) are not recommended: future provider files may use those slots and silently shadow your override.

Tracked as KD-23 (persistence injection) and KD-34 (in-place upgrade persistence guarantees) in the punch list.

---

## Notes

### API Version Compatibility

- **Kairos CAPI Provider APIs**: `bootstrap.cluster.x-k8s.io/v1beta2` and `controlplane.cluster.x-k8s.io/v1beta2`.
- **CAPI Core Types**: The wire API version for `Cluster`, `Machine`, and related resources is `v1beta2` (`cluster.x-k8s.io/v1beta2`). However, the Go module currently imports `sigs.k8s.io/cluster-api/api/v1beta1` because the go.mod pins CAPI v1.8.0 which predates the v1beta2 Go package. Bumping to CAPI v1.11 is tracked as KD-13. This is a compile-time import detail; the CRD API group and version on the wire are not affected.
- **Infrastructure Providers**: Use their respective API versions (e.g., CAPD/CAPV use `infrastructure.cluster.x-k8s.io/v1beta1`, CAPK uses `infrastructure.cluster.x-k8s.io/v1alpha1`).

### Credential Requirements

At least one of the following must be set on every `KairosConfig` (enforced by the validating webhook):

- `userPassword` (inline; not recommended outside lab use)
- `userPasswordSecretRef` (recommended)
- `sshPublicKey`
- `githubUser`

### Worker Token Requirements

For `KairosConfig` with `role: worker`:

- **k0s**: Set `workerToken` or `workerTokenSecretRef`. `workerTokenSecretRef` is preferred.
- **k3s**: Set `k3sToken` or `k3sTokenSecretRef`. `k3sTokenSecretRef` is preferred.

The controller fails reconciliation if no token is provided for a worker.

### Single-Node Mode

When `KairosControlPlane.spec.replicas == 1`, the controller automatically sets `KairosConfig.spec.singleNode = true` on the created control-plane Machine's config. For k0s this adds the `--single` flag; for k3s it enables single-node cluster-init mode. The `singleNode` field applies to both distributions. Setting it manually on a `KairosConfigTemplate` is unnecessary when managed by `KairosControlPlane`.

### Multi-Node Control Planes

`KairosControlPlane.spec.replicas > 1` is currently rejected by the validating webhook. HA control planes are tracked for a future release (KD-5b / KD-25). Do not attempt to work around the webhook — setting replicas to 1 is the correct configuration for all current deployments.

### Security Considerations

- Provide credentials via `userPasswordSecretRef` (recommended) or `sshPublicKey` / `githubUser`. Inline `userPassword` is stored in the KairosConfig spec and readable by anyone who can `kubectl get kairosconfig`.
- Worker tokens should use the `*SecretRef` variants. Inline tokens in specs are readable without Secret RBAC.
- All Secrets referenced by `*SecretRef` fields must exist in the management cluster before the KairosConfig is reconciled. Missing Secrets cause a transient failure that clears automatically when the Secret is created.

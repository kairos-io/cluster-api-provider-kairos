# API Reference

This document provides a reference for all Custom Resource Definitions (CRDs) provided by the Kairos CAPI Provider. See [Install guide](INSTALL.md) for development install. Quickstarts: [CAPD](QUICKSTART_CAPD.md), [CAPV](QUICKSTART_CAPV.md), [CAPK](QUICKSTART_CAPK.md).

## Table of Contents

- [KairosConfig](#kairosconfig)
- [KairosConfigTemplate](#kairosconfigtemplate)
- [KairosControlPlane](#kairoscontrolplane)
- [KairosControlPlaneTemplate](#kairoscontrolplanetemplate)

---

## KairosConfig

**API Group:** `bootstrap.cluster.x-k8s.io`  
**API Version:** `v1beta2`  
**Kind:** `KairosConfig`

`KairosConfig` is a BootstrapConfig resource that generates Kairos cloud-config for bootstrapping Kubernetes nodes (control-plane or worker) using k0s.

### Spec Fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `role` | `string` | Yes | `"worker"` | Node role: `"control-plane"` or `"worker"` |
| `distribution` | `string` | No | `"k0s"` | Kubernetes distribution: `"k0s"` or `"k3s"` |
| `kubernetesVersion` | `string` | Yes | - | Kubernetes version to install (e.g., `"v1.30.0+k0s.0"`) |
| `singleNode` | `bool` | No | `false` | For control-plane: if `true`, configures k0s with `--single` flag for single-node mode |
| `userName` | `string` | No | `"kairos"` | Username for the default user |
| `userPassword` | `string` | No | `"kairos"` | Password for the default user. Change for non-dev use. |
| `userGroups` | `[]string` | No | `["admin"]` | Groups for the default user |
| `githubUser` | `string` | No | - | GitHub username for SSH key access (fetches keys from GitHub) |
| `sshPublicKey` | `string` | No | - | Raw SSH public key (alternative to `githubUser`) |
| `workerToken` | `string` | No* | - | Inline worker join token. *Required for worker nodes if `workerTokenSecretRef` is not set |
| `workerTokenSecretRef` | `WorkerTokenSecretReference` | No* | - | Reference to Secret containing worker token. *Required for worker nodes if `workerToken` is not set. Prefer this over inline token for security |
| `manifests` | `[]Manifest` | No | - | Kubernetes manifests to deploy via k0s (placed in `/var/lib/k0s/manifests/`) |
| `pause` | `bool` | No | `false` | If `true`, pauses reconciliation |

#### WorkerTokenSecretReference

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | `string` | Yes | - | Name of the Secret |
| `key` | `string` | No | `"token"` | Key within the Secret containing the token |
| `namespace` | `string` | No | Same as KairosConfig | Namespace of the Secret |

#### Manifest

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Directory name under `/var/lib/k0s/manifests/{name}/` |
| `file` | `string` | Yes | Filename within the directory |
| `content` | `string` | Yes | YAML content of the manifest |

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `ready` | `bool` | Indicates bootstrap data has been generated and is ready |
| `dataSecretName` | `string` | Name of the Secret containing bootstrap data (cloud-config) |
| `conditions` | `[]Condition` | Standard CAPI conditions: `Ready`, `BootstrapReady`, `DataSecretAvailable` |
| `observedGeneration` | `int64` | Most recent generation observed by the controller |
| `failureReason` | `string` | Reason for bootstrap failure (if any) |
| `failureMessage` | `string` | Human-readable failure message (if any) |

### Example

```yaml
apiVersion: bootstrap.cluster.x-k8s.io/v1beta2
kind: KairosConfig
metadata:
  name: kairos-config-control-plane
  namespace: default
spec:
  role: control-plane
  distribution: k0s
  kubernetesVersion: "v1.30.0+k0s.0"
  singleNode: true
  userName: kairos
  userPassword: kairos
  userGroups:
    - admin
  githubUser: "octocat"
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
| `template` | `KairosConfigTemplateResource` | Yes | Template for creating `KairosConfig` resources |

#### KairosConfigTemplateResource

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `metadata` | `ObjectMeta` | No | Metadata to apply to created `KairosConfig` resources |
| `spec` | `KairosConfigSpec` | Yes | Spec to apply to created `KairosConfig` resources (see [KairosConfig Spec](#spec-fields)) |

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
      kubernetesVersion: "v1.30.0+k0s.0"
      workerTokenSecretRef:
        name: worker-token
        key: token
```

---

## KairosControlPlane

**API Group:** `controlplane.cluster.x-k8s.io`  
**API Version:** `v1beta2`  
**Kind:** `KairosControlPlane`

`KairosControlPlane` manages the control plane machines for a Kubernetes cluster using Kairos and k0s.

### Spec Fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `replicas` | `*int32` | No | `1` | Number of control plane machines. Must be >= 1. When `replicas == 1`, single-node mode is enabled |
| `version` | `string` | Yes | - | Kubernetes version (e.g., `"v1.30.0+k0s.0"`) |
| `machineTemplate` | `KairosControlPlaneMachineTemplate` | Yes | - | Template for creating control plane machines |
| `kairosConfigTemplate` | `KairosConfigTemplateReference` | Yes | - | Reference to `KairosConfigTemplate` for bootstrap configuration |
| `rolloutStrategy` | `RolloutStrategy` | No | - | Strategy for rolling out updates (optional) |

#### KairosControlPlaneMachineTemplate

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `infrastructureRef` | `ObjectReference` | Yes | Reference to infrastructure template (e.g., `DockerMachineTemplate`, `VSphereMachineTemplate`) |
| `nodeDrainTimeout` | `Duration` | No | Timeout for draining nodes during updates |
| `metadata` | `ObjectMeta` | No | Metadata to apply to created machines |

#### KairosConfigTemplateReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Name of the `KairosConfigTemplate` |
| `apiVersion` | `string` | No | API version (defaults to `bootstrap.cluster.x-k8s.io/v1beta2`) |
| `kind` | `string` | No | Kind (defaults to `KairosConfigTemplate`) |

**Note:** The `namespace` field is not part of this reference. The namespace defaults to the same namespace as the `KairosControlPlane` resource.

#### RolloutStrategy

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `type` | `string` | No | `"RollingUpdate"` | Strategy type (currently only `"RollingUpdate"` supported) |
| `rollingUpdate` | `RollingUpdate` | No | - | Rolling update configuration |

#### RollingUpdate

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `maxSurge` | `*int32` | No | Maximum number of machines that can be created above desired count |

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `initialized` | `bool` | Indicates the control plane has been initialized (first machine ready) |
| `readyReplicas` | `int32` | Number of control plane machines that are ready |
| `replicas` | `int32` | Total number of control plane machines |
| `updatedReplicas` | `int32` | Number of machines with desired version |
| `unavailableReplicas` | `int32` | Number of unavailable machines |
| `conditions` | `[]Condition` | Standard CAPI conditions: `Ready`, `Available`, `Initialized` |
| `observedGeneration` | `int64` | Most recent generation observed by the controller |
| `failureReason` | `string` | Reason for control plane failure (if any) |
| `failureMessage` | `string` | Human-readable failure message (if any) |
| `selector` | `string` | Label selector for control plane machines |

### Example

```yaml
apiVersion: controlplane.cluster.x-k8s.io/v1beta2
kind: KairosControlPlane
metadata:
  name: kairos-control-plane
  namespace: default
spec:
  replicas: 1
  version: "v1.30.0+k0s.0"
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

`KairosControlPlaneTemplate` is a template for creating `KairosControlPlane` resources. Used by ClusterClass (planned).

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `template` | `KairosControlPlaneTemplateResource` | Yes | Template for creating `KairosControlPlane` resources |

#### KairosControlPlaneTemplateResource

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `metadata` | `ObjectMeta` | No | Metadata to apply to created `KairosControlPlane` resources |
| `spec` | `KairosControlPlaneSpec` | Yes | Spec to apply to created `KairosControlPlane` resources (see [KairosControlPlane Spec](#spec-fields)) |

---

## Notes

### API Version Compatibility

- **Kairos CAPI Provider APIs**: Use `v1beta2` (`bootstrap.cluster.x-k8s.io/v1beta2`, `controlplane.cluster.x-k8s.io/v1beta2`)
- **CAPI Core Types**: Currently use `v1beta1` (`cluster.x-k8s.io/v1beta1`) as `v1beta2` is not yet available in the CAPI Go module
- **Infrastructure Providers**: Use their respective API versions (e.g., CAPD/CAPV use `infrastructure.cluster.x-k8s.io/v1beta1`)

### Worker Token Requirements

For `KairosConfig` with `role: worker`, either `workerToken` or `workerTokenSecretRef` must be set. The controller will fail reconciliation if neither is provided.

### Single-Node Mode

When `KairosControlPlane.spec.replicas == 1`, the controller automatically sets `KairosConfig.spec.singleNode = true` for control plane machines, which configures k0s with the `--single` flag.

### Security Considerations

- **User Password**: Change the default `userPassword` for non-dev use
- **Worker Tokens**: Prefer `workerTokenSecretRef` over inline `workerToken` for better security
- **SSH Access**: Use `githubUser` or `sshPublicKey` instead of password-based access when possible


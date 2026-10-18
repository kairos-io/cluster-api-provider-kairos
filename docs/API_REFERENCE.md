# API Reference

Last verified against: Kairos v3.6.0+, CAPI v1.13.4 (v1beta2 contract), provider v0.1.3.

Kubeadm worker support (`distribution: kubeadm`, `spec.kubeadm`, `status.bootstrapTokenID`, and the `UnsupportedDistribution` reason) is on `main` and is not in v0.1.4. It is planned for v0.2.0. The v0.1.4 CRDs serve none of it.

This document provides a reference for all Custom Resource Definitions (CRDs) provided by the Kairos CAPI Provider. See [Install guide](INSTALL.md) for development install. Quickstarts: [CAPD](QUICKSTART_CAPD.md), [CAPV](QUICKSTART_CAPV.md), [CAPK](QUICKSTART_CAPK.md), [CAPM3](QUICKSTART_CAPM3.md). Joining Kairos workers to a Kamaji or `KubeadmControlPlane` control plane: [Kubeadm workers](KUBEADM_WORKERS.md).

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

`KairosConfig` is a BootstrapConfig resource that generates Kairos cloud-config for bootstrapping Kubernetes nodes (control-plane or worker) using k0s or k3s. With `distribution: kubeadm` it bootstraps worker nodes only, joining them to a kubeadm control plane that this provider does not run (see [Kubeadm workers](KUBEADM_WORKERS.md)).

### Spec Fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `role` | `string` | No | `"worker"` | Node role: `"control-plane"` or `"worker"`. `distribution: kubeadm` accepts `"worker"` only; the validating webhook rejects `"control-plane"`. |
| `distribution` | `string` | No | `"k0s"` | Kubernetes distribution: `"k0s"`, `"k3s"`, or `"kubeadm"`. `kubeadm` joins a Kairos worker to an existing kubeadm control plane (Kamaji `KamajiControlPlane` or `KubeadmControlPlane`) and is worker-only. `KairosControlPlane.spec.distribution` does not accept `kubeadm`. See [kubeadm workers](#kubeadm-workers). |
| `kubernetesVersion` | `string` | Yes | — | Kubernetes version string (e.g., `"v1.34.1+k0s.1"`). For k0s and k3s the value is informational — the actual version is pinned in the Kairos image at build time and cannot be changed by this field. See KD-24. For `distribution: kubeadm` the version that must match the image is `Machine.spec.version`, not this field; keep the two equal and use the plain upstream form (for example `"v1.36.1"`). |
| `singleNode` | `bool` | No | `false` | Signals single-node mode to the cloud-config renderer. The KairosControlPlane controller derives this from `replicas==1`, so manual overrides are typically unnecessary. What single-node mode renders is distribution- and infrastructure-specific: for k3s it enables cluster-init mode on every infrastructure provider; for k0s on CAPK it renders `--single`; for k0s on the generic / CAPV / CAPM3 / fleet render path it renders `--enable-worker` by default, or `--single` when `k0sSingleNode` is also `true` (see `k0sSingleNode` below). Tracked as a deprecation candidate in KD-39. |
| `k0sSingleNode` | `*bool` | No | `false` | For a single-node k0s control plane (`singleNode: true`, `distribution: k0s`) on the generic / CAPV / CAPM3 / fleet render path, selects which k0s single-node flag is rendered. `false` (the default) renders `--enable-worker`: a joinable, schedulable controller that runs workloads itself and accepts worker joins, so a control-plane-plus-worker cluster works out of the box. `true` renders `--single`: a standalone all-in-one node that refuses all joins ("cannot join into a single node cluster") — use this only for a node that will never gain workers. Ignored by k3s and by worker/join nodes, and has no effect on CAPK, which always renders `--single` for single-node k0s. |
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
| `caCertHashes` | `[]string` | No | — | Reserved; not implemented. The value is accepted and stored, but no controller reads it and it is rendered into no cloud-config, so setting it does not pin the join to a CA. Setting it produces an admission warning. Neither distribution can consume it: a k0s worker joins with the bundle `k0s token create` produces, which already carries the cluster CA, and a k3s agent takes no CA-hash flag. To get k3s to validate the server, pass the server's full node-token (`K10<hash>::server:<password>`) as `k3sToken` / `k3sTokenSecretRef`. See kairos-io/kairos#4937. |
| `caCertSecretRef` | `ObjectReference` | No | — | Reserved; not implemented. The reference is accepted and stored, but no controller reads the Secret and its contents reach no node. Setting it produces an admission warning. Use `files` to place a CA certificate on the node. See kairos-io/kairos#4937. |
| `hostname` | `string` | No | — | Hostname to set on the node inside the VM. Takes precedence over `hostnamePrefix` when both are set. On control-plane configs (`role: control-plane`) it must be a lowercase DNS-1123 label (at most 63 characters of `a-z`, `0-9` and `-`, starting and ending with a letter or digit) with no `{{` template markup: the control plane finds the node's k0s etcd member by this name. Enforced on create, and on update only when the field changes, so existing objects keep their value. Worker configs are not checked. A `KairosControlPlane` applies the same rule to the `KairosConfigTemplate` it references: it emits a Warning event `InvalidControlPlaneHostname` and creates no control-plane Machine from a template that breaks it. |
| `hostnamePrefix` | `string` | No | `"metal-"` | Prefix for the auto-generated hostname. The final hostname is `{hostnamePrefix}{4-char-machine-id}`. |
| `dnsServers` | `[]string` | No | — | DNS resolvers configured for early boot, before cluster DNS is ready (useful for pulling CNI images). |
| `podCIDR` | `string` | No | — | Pod network CIDR for k0s. Uses k0s defaults when unset. |
| `serviceCIDR` | `string` | No | — | Service network CIDR for k0s. Uses k0s defaults when unset. |
| `primaryIP` | `string` | No | — | Overrides the detected node IP used for TLS certificate SANs and endpoint configuration (sets `KAIROS_PRIMARY_IP`). Useful in KubeVirt environments where the detected IP is a pod network address rather than the VM's accessible address. |
| `install` | `InstallConfig` | No | — | Controls Kairos OS installation to disk. Required when using the 2-disk installer pattern (see `config/samples/capk/`). |
| `files` | `[]File` | No | — | Files to write on the node via the cloud-config `write_files:` list. Rendered on all distributions and all infrastructure providers. At most 32 entries; each file content is limited to 32 KiB. See [File](#file) for the sub-type and [Writing files to nodes](#writing-files-to-nodes) for usage guidance and the static-IP caveat. |
| `manifests` | `[]Manifest` | No | — | Kubernetes manifests placed in the distribution's auto-apply directory. k0s: `/var/lib/k0s/manifests/{name}/{file}`. k3s: `/var/lib/rancher/k3s/server/manifests/{name}/{file}`. Applied automatically by the distribution at cluster startup. |
| `preCommands` | `[]string` | No | — | Reserved; not yet rendered into the cloud-config. |
| `postCommands` | `[]string` | No | — | Reserved; not yet rendered into the cloud-config. |
| `pause` | `bool` | No | `false` | When `true`, pauses reconciliation of this KairosConfig. A paused Cluster pauses it as well; see [Pause](#pause). |
| `kubeadm` | `KubeadmConfig` | No | — | kubeadm-specific inputs for a worker join. Honoured only when `distribution` is `kubeadm` and `role` is `worker`; ignored for k0s and k3s. All fields are optional. See [KubeadmConfig](#kubeadmconfig). |

#### UserPasswordSecretReference

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | `string` | Yes | — | Name of the Secret. |
| `key` | `string` | No | `"password"` | Key within the Secret that contains the password. |
| `namespace` | `string` | No | Same as KairosConfig | Namespace of the Secret. It must be the KairosConfig's own namespace: a different one is rejected by the validating webhook and refused by the controller, because the value it points at is copied into the bootstrap Secret beside the KairosConfig. |

#### WorkerTokenSecretReference

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | `string` | Yes | — | Name of the Secret. |
| `key` | `string` | No | `"token"` | Key within the Secret containing the token. |
| `namespace` | `string` | No | Same as KairosConfig | Namespace of the Secret. It must be the KairosConfig's own namespace: a different one is rejected by the validating webhook and refused by the controller, because the value it points at is copied into the bootstrap Secret beside the KairosConfig. |

#### InstallConfig

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `auto` | `*bool` | No | `true` | When `true`, Kairos installs to disk automatically at first boot. |
| `device` | `string` | No | `"auto"` | Target device for installation (e.g., `"/dev/vda"`). `"auto"` selects the first available disk. |
| `reboot` | `*bool` | No | `true` | When `true`, the system reboots automatically after installation completes. |

#### File

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `path` | `string` | Yes | Absolute path where the file is written on the node. Must begin with `/`. Must not contain `..` path segments. |
| `content` | `string` | Yes | File content. Multi-line strings are accepted. Maximum 32 KiB (32768 bytes). |
| `permissions` | `string` | No | File mode in octal notation. Accepts 3-digit or 4-digit forms; the leading digit encodes setuid (4), setgid (2), and sticky (1) bits. Examples: `"0644"`, `"0750"`, `"4755"`. |
| `owner` | `string` | No | File owner in `user:group` format (e.g., `"root:root"`). The group portion including the colon is optional. |

#### Manifest

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Directory name under the distribution's manifest path. |
| `file` | `string` | Yes | Filename within the directory. |
| `content` | `string` | Yes | YAML content of the manifest. |

#### KubeadmConfig

`spec.kubeadm`. Honoured only for `distribution: kubeadm` with `role: worker`.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `joinConfiguration` | `JoinConfiguration` | No | — | The kubeadm `JoinConfiguration` for the worker join. The schema is the `JoinConfiguration` of Cluster API's kubeadm bootstrap provider (`bootstrap.cluster.x-k8s.io/v1beta2`); every field is optional. The controller sets or overwrites `nodeRegistration.name` (the Machine name, unless you set it), `discovery.bootstrapToken` (the minted token, the control-plane endpoint, and the cluster CA hash), and `discovery.file` (cleared); it adds the `node.cluster.x-k8s.io/uninitialized:NoSchedule` taint to `nodeRegistration.taints` and keeps any taints you set. All other fields pass through. Do not set `controlPlane`: control-plane joins are not supported in this release. Secret-bearing and trust-weakening fields are rejected at admission; see [Admission refusals](KUBEADM_WORKERS.md#admission-refusals). |

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `ready` | `bool` | `true` when bootstrap data has been generated and the bootstrap Secret is available for the CAPI Machine controller. |
| `dataSecretName` | `*string` | Name of the Secret containing the bootstrap cloud-config. |
| `initialization.dataSecretCreated` | `bool` | v1beta2 contract field: `true` when the bootstrap Secret has been created. |
| `bootstrapTokenID` | `string` | Non-secret ID (the six-character prefix) of the kubeadm bootstrap token this KairosConfig uses. The token Secret in the workload cluster is `kube-system/bootstrap-token-<id>`. Every render of the bootstrap data reuses this token, and the controller extends its expiry while the Machine has no `nodeRef`; it is revoked and cleared once the Machine has one, or when the KairosConfig is deleted. See [Token lifetime](KUBEADM_WORKERS.md#token-lifetime-and-failed-joins). The secret half of the token is never stored in `spec` or `status`. Empty for k0s and k3s, for a kubeadm worker that has not yet minted a token, and after revocation. |
| `conditions` | `[]Condition` | Standard CAPI conditions: `Ready`, `BootstrapReady`, `DataSecretAvailable`. See [Waiting for cluster infrastructure](#waiting-for-cluster-infrastructure) and, for control-plane roles, [Waiting for the kubeconfig push target](#waiting-for-the-kubeconfig-push-target) and [Management API CA unavailable](#management-api-ca-unavailable) for the reasons these carry before the bootstrap Secret is written, and [kubeadm workers](#kubeadm-workers) for the reasons a kubeadm worker adds. |
| `observedGeneration` | `int64` | Most recent generation observed by the controller. |
| `failureReason` | `string` | Short machine-readable string indicating the last failure reason. Cleared automatically when the next reconcile succeeds — a non-empty value indicates an ongoing failure, not a terminal one. |
| `failureMessage` | `string` | Human-readable description of the last failure. Cleared automatically on the next successful reconcile. If non-empty, check the owning Machine's events for context. |

### Waiting for cluster infrastructure

A `KairosConfig` does not render bootstrap data the moment its Machine appears.
It first waits for `Cluster.status.initialization.infrastructureProvisioned`,
and a control-plane role additionally waits for a usable
`Cluster.spec.controlPlaneEndpoint`. Until then `Ready`, `BootstrapReady` and
`DataSecretAvailable` are all `False` with reason
`WaitingForClusterInfrastructure` at severity `Info`, and the message says which
of the two it is waiting on.

This is normal during provisioning and clears on its own: the controller watches
the `Cluster` and re-reconciles the moment it is provisioned.

The wait exists because the endpoint is baked into the render and a node
installs from the first bootstrap Secret it is handed. Rendering early produced
a node whose k0s API SANs omitted the control-plane VIP and which pushed
`server: https://localhost:6443` into the cluster kubeconfig; regenerating the
Secret later did not help, because the node had already installed. Worker roles
are not gated on the endpoint, since their render tolerates its absence.

If a `KairosConfig` stays in this state, the cluster's infrastructure provider
has not reported `infrastructureProvisioned`, or (for a control plane) has
reported it without publishing `spec.controlPlaneEndpoint`. Check the
InfraCluster, not the `KairosConfig`.

### kubeadm workers

A `KairosConfig` with `distribution: kubeadm` and `role: worker` joins a control
plane that this provider does not run. The controller verifies the control plane,
mints a short-lived bootstrap token in the workload cluster, and renders a kubeadm
`JoinConfiguration`. The full procedure, prerequisites, and troubleshooting are in
[Kubeadm workers](KUBEADM_WORKERS.md). A worked manifest is in
`config/samples/kubeadm/kairos_kubeadm_workers.yaml`.

Requirements that surface as API behavior:

- `Machine.spec.version` must be set, in plain upstream form (`v1.36.1`), and must
  equal the kubeadm version in the node image exactly.
- The owner must be a `MachineDeployment`; a `MachinePool` owner is refused.
- The webhook rejects, on a kubeadm `KairosConfig`: `role: control-plane`, an inline
  bootstrap token, `unsafeSkipCAVerification`, `discovery.file`, any `{{` in
  `spec.kubeadm`, and a `nodeRegistration.name` that is not a DNS-1123 subdomain.
  It runs on `KairosConfig`, not on `KairosConfigTemplate`.
- The control-plane kinds a worker may join are `KubeadmControlPlane` and
  `KamajiControlPlane`. The operator can add kinds with the manager flag
  `--kubeadm-extra-controlplane-kinds` (see
  [INSTALL.md](INSTALL.md#optional-manager-flags)).

Reasons a kubeadm worker reports:

| Resource | Condition and status | Reason | Meaning |
|----------|----------------------|--------|---------|
| `KairosConfig` | `Ready`, `BootstrapReady`, `DataSecretAvailable`: `False` (Warning) | `BootstrapDataSecretGenerationFailed` | The control-plane check refused to mint a token, or rendering failed. The cause is in `status.failureMessage`. Not retried on a timer; delete the Machine after fixing the cause. |
| `KairosConfig` | `BootstrapReady`: `False` (Warning) | `BootstrapTokenRefreshCapExceeded` | The Machine still has no Node 24 hours after creation, so the controller stopped extending its bootstrap token. It does not delete the Machine; a `MachineHealthCheck` with `spec.checks.nodeStartupTimeoutSeconds` is what replaces it. |
| `KairosControlPlane` | `Ready`, `Available`: `False` (Warning) | `UnsupportedDistribution` | The effective distribution is `kubeadm`, which `KairosControlPlane` does not support in this release. See [KairosControlPlane refuses kubeadm](KUBEADM_WORKERS.md#kairoscontrolplane-refuses-kubeadm). |

While the control plane is not yet initialized, or the kubeconfig Secret or
workload cluster is not yet reachable, the controller requeues every 10 seconds
and leaves conditions unchanged; the reason is in the bootstrap manager log.

### Waiting for the kubeconfig push target

A control-plane `KairosConfig` waits on one further precondition before
rendering: the per-cluster staging kubeconfig Secret (`<cluster>-kubeconfig-push`)
must exist and be owned by the same `KairosControlPlane` that owns this
Machine. Until then, `BootstrapReady` is `False` with reason
`WaitingForKubeconfigPushTarget` at severity `Info`, and the controller
requeues every 10 seconds.

This exists because a control-plane node's rendered cloud-config PATCHes only
that staging Secret to publish its kubeconfig back to the management cluster
(see [KubeconfigReady condition](#kubeconfigready-condition)); rendering the
push block before the Secret exists, and is owned by the right control plane,
would hand the node a token with no valid target.

The wait clears on its own: the `KairosControlPlane` controller creates and
takes ownership of the staging Secret as part of every reconcile for a
cluster it manages, and the bootstrap controller re-reconciles once that
happens. If a `KairosConfig` stays in this state, confirm a
`KairosControlPlane` exists for the cluster and is reconciling — its own
conditions and events show why, if not.

Worker `KairosConfig`s never render a push block and are not affected.

### Management API CA unavailable

A control-plane `KairosConfig` also needs a certificate authority before it
can render a push block: the node-push channel verifies TLS against this CA
(`curl --cacert`) and has no insecure fallback. If none is available when a
Machine needs bootstrap data, `BootstrapReady` is `False` with reason
`ManagementCAUnavailable` at severity `Warning`, and the controller requeues
every 10 seconds.

In practice this state is rare to see on a running manager: the CA is
resolved once at manager startup — from the `KAIROS_MANAGEMENT_API_CA_FILE`
operator override, or otherwise from the manager's own in-cluster trust root
— and the manager fails to start at all if push is enabled and neither source
yields a CA. Seeing this condition means the resolver ran without either
source available, which is a configuration or environment problem rather than
a transient one.

To resolve: set `KAIROS_MANAGEMENT_API_CA_FILE` on the controller Deployment
to the management API server's CA (a filesystem path to a PEM bundle, or an
inline PEM), then restart the manager. See
[INSTALL.md — Management API TLS verification and the CA source](INSTALL.md#management-api-tls-verification-and-the-ca-source).

Worker `KairosConfig`s never render a push block and are not affected.

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

For kubeadm workers, no token is set: the controller mints one. `Machine.spec.version`
must be set on the owning `MachineDeployment`:

```yaml
apiVersion: bootstrap.cluster.x-k8s.io/v1beta2
kind: KairosConfigTemplate
metadata:
  name: kairos-kubeadm-worker
  namespace: default
spec:
  template:
    spec:
      role: worker
      distribution: kubeadm
      kubernetesVersion: "v1.36.1"
      userPasswordSecretRef:
        name: kairos-user-password
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
| `replicas` | `*int32` | No | `1` | Number of control plane machines. One of `1`, `3`, or `5` — the validating webhook rejects even counts (they provide the same etcd fault tolerance as the next-lower odd count while raising the quorum requirement) and values above `5` (beyond 5 members the quorum cost outweighs the added fault tolerance). `1` configures a single-node control plane. `3` or `5` configure a highly-available control plane; set `ha.vip` for infrastructure providers that do not supply a load-balanced endpoint (CAPV, CAPM3, CAPD). |
| `version` | `string` | Yes | — | Kubernetes version string (e.g., `"v1.34.1+k0s.1"`). Informational; the actual k8s version is pinned in the Kairos image. Changing it on an HA control plane replaces the control-plane Machines one at a time. On a single-node control plane it replaces nothing, because a replacement would start a separate, empty cluster; see [MachinesUpToDate condition](#machinesuptodate-condition). |
| `distribution` | `string` | No | `"k0s"` | Kubernetes distribution for this control plane: `"k0s"` or `"k3s"`. `"kubeadm"` is not accepted: it is rejected at admission, and a referenced `KairosConfigTemplate` with `distribution: kubeadm` sets `Ready` and `Available` to `False` with reason `UnsupportedDistribution` (see [kubeadm workers](#kubeadm-workers)). k0s is the fully-supported HA distribution; k3s HA bring-up is supported but replacing a k3s control-plane node afterward leaves an orphaned etcd member requiring manual cleanup (KD-5d — see [Multi-Node Control Planes](#multi-node-control-planes)). |
| `machineTemplate` | `KairosControlPlaneMachineTemplate` | Yes | — | Template for creating control plane Machines. |
| `kairosConfigTemplate` | `KairosConfigTemplateReference` | Yes | — | Reference to a `KairosConfigTemplate` that provides the bootstrap configuration for each Machine. |
| `rolloutStrategy` | `RolloutStrategy` | No | — | Strategy for rolling out updates. |
| `ha` | `HAConfig` | No | — | High-availability configuration, used when `replicas` is `3` or `5`. Ignored when `replicas` is `1`; setting it on a single-node control plane produces a non-blocking admission warning. See [HAConfig](#haconfig). |
| `sshFallback` | `SSHFallback` | No | — | Opt-in SSH-pull fallback for retrieving the workload-cluster kubeconfig when the in-VM node-push path is unreachable (typically air-gapped deployments). Off by default. See [SSHFallback](#sshfallback). |

#### KairosControlPlaneMachineTemplate

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `infrastructureRef` | `ObjectReference` | Yes | Reference to the infrastructure template (e.g., `DockerMachineTemplate`, `VSphereMachineTemplate`, `KubevirtMachineTemplate`, `Metal3MachineTemplate`, `KairosFleetMachineTemplate`, or any other CAPI-conformant `<Kind>MachineTemplate`). If `namespace` is omitted, it defaults to the `KairosControlPlane`'s own namespace, matching the CAPI convention that `clusterctl generate cluster` templates rely on. A different namespace is rejected: the template must live beside the `KairosControlPlane`. The control plane makes the Cluster an owner of this template; see [Template ownership](#template-ownership). |
| `nodeDrainTimeout` | `Duration` | No | Timeout for draining nodes during updates and scale-down. Propagated to `Machine.spec.deletion.nodeDrainTimeoutSeconds`, which is where Cluster API reads the deadline from, so the value is truncated to whole seconds. Unset means CAPI's default: drain with no deadline. A value of zero or less is the same as unset. A value above 2147483647 seconds (about 68 years) is capped at 2147483647 seconds. Editing it applies to the Machines that already exist, including one that is draining right now. |
| `metadata` | `ObjectMeta` | No | Labels and annotations to apply to the objects created for each control-plane replica: the `Machine`, and the infrastructure Machine cloned from `infrastructureRef`. Labels reach the Node through Cluster API, which syncs the `node-role.kubernetes.io/*`, `node-restriction.kubernetes.io/*` and `*.node.cluster.x-k8s.io/*` labels it finds on the `Machine`. Adding or changing an entry applies to the Machines that already exist; removing one leaves it in place on them, since the control plane does not own the whole metadata of a Machine. `cluster.x-k8s.io/cluster-name` and `cluster.x-k8s.io/control-plane` are set by the controller and cannot be overridden, because it selects its own Machines by them. |

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

#### HAConfig

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `vip` | `KubeVIPConfig` | No | The virtual IP (kube-vip) used as the stable control-plane endpoint across HA nodes. Required on CAPV, CAPM3, and CAPD when the infrastructure provider does not supply a load-balanced endpoint automatically. **Do not set on CAPK** — CAPK provisions its own LoadBalancer Service and reflects its IP into the control-plane endpoint; a VIP alongside it produces a conflicting ARP announcement. |

#### KubeVIPConfig

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `address` | `string` | Yes | — | The virtual IP address or DNS hostname for the control-plane endpoint. Must be a valid IPv4 address, IPv6 address, or RFC-1123 hostname (max 253 characters). For `mode: ARP`, must be an IP address reachable on the same L2 segment as the control-plane nodes. Must equal the host portion of the InfraCluster's `controlPlaneEndpoint` (e.g., `VSphereCluster.spec.controlPlaneEndpoint.host`) so that CAPI core copies the correct address into `Cluster.spec.controlPlaneEndpoint`. This cross-object match is not enforced at admission — a mismatch will not be rejected, but the cluster endpoint will point at the wrong address. |
| `interface` | `string` | Yes | — | The Linux network interface name on which kube-vip advertises the VIP (e.g., `"eth0"`, `"ens192"`, `"bond0"`). Must be 1-15 characters, starting with a letter, followed by letters, digits, dots, underscores, or hyphens. Verify the interface name against your Kairos image with `ip link` before setting this field. |
| `mode` | `string` | No | `"ARP"` | VIP advertisement mode: `"ARP"` (L2, requires the control-plane nodes to share an L2 segment) or `"BGP"` (L3, requires a BGP peer; intended for routed bare-metal fabrics). |

#### SSHFallback

Opt-in escape hatch for retrieving the workload-cluster kubeconfig by SSH when the normal in-VM node-push path cannot reach the management cluster's API server (typically an air-gapped CAPV deployment). Off by default; enabling it does not disable the node-push path — the fallback only activates after node-push has been failing for `activateAfter`. See [INSTALL.md — Network reachability requirement](INSTALL.md#network-reachability-requirement-for-non-capk-infrastructure) and [QUICKSTART_CAPV.md — Air-gapped fallback](QUICKSTART_CAPV.md#air-gapped-fallback-sshfallback) for the full walkthrough and the `KubeconfigReady` condition reasons this path produces.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `enabled` | `bool` | No | `false` | Toggles the SSH fallback path. When `false`, the controller never opens an SSH connection regardless of any other field in this block. When `true`, `identitySecretRef` and `knownHostsSecretRef` are required — the validating webhook rejects `enabled: true` with either unset. |
| `identitySecretRef` | `SSHFallbackSecretReference` | Required when `enabled: true` | — | Reference to a Secret containing the SSH private key used to authenticate to the workload node. Must contain a key named `ssh-privatekey` (matches the `kubernetes.io/ssh-auth` Secret convention) unless overridden via `key`. The matching public key must already be installed on the node via `KairosConfig.spec.sshPublicKey` / `githubUser` — this provider does not push it. |
| `knownHostsSecretRef` | `SSHFallbackSecretReference` | Required when `enabled: true` | — | Reference to a Secret containing one or more OpenSSH `known_hosts` lines. The controller verifies the workload node's offered host key against this set before any data is exchanged. **There is no trust-on-first-use path** — a node whose host key is not in this Secret is refused. Defaults to the `known_hosts` key unless overridden via `key`. |
| `user` | `string` | No | `"kairos"` | SSH login user. Must be a Kairos OS user with read access to the distribution's admin-kubeconfig file. Validated against a POSIX-username pattern at admission; shell-metacharacter values are rejected. |
| `port` | `int32` | No | `22` | SSH port on the workload node. Must be between `1` and `65535`. |
| `activateAfter` | `Duration` | No | `"15m"` | Elapsed time since `KubeconfigReady` first went `False(WaitingForNodePush)` after which the SSH fallback becomes eligible to run. Must be strictly greater than the controller's internal node-push timeout (10 minutes) — the webhook rejects a value at or below that bound, because the fallback is meant to fire after the Info-to-Warning escalation on `KubeconfigReady`, not before it. |

#### SSHFallbackSecretReference

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | `string` | Yes | — | Name of the Secret. |
| `key` | `string` | No | `ssh-privatekey` for `identitySecretRef`, `known_hosts` for `knownHostsSecretRef` | Key within the Secret that contains the value. |
| `namespace` | `string` | No | Same as `KairosControlPlane` | Namespace of the Secret. Cross-namespace references are rejected by the validating webhook in this release. |

Cross-field validation (enforced by the validating webhook, not expressible as kubebuilder markers): `enabled: true` requires both secret refs to be set with a non-empty `name`; both secret refs are rejected if they specify a `namespace` other than the `KairosControlPlane`'s own; `activateAfter` is rejected if it is not strictly greater than the controller's node-push timeout. `enabled: false` is always valid regardless of what else is set, so a configured-but-disabled block can be staged ahead of time.

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `initialized` | `bool` | `true` when the first control plane Machine is ready and the control plane is functional. |
| `initialization.controlPlaneInitialized` | `*bool` | v1beta2 contract field. `true` when the control plane has been initialized and can accept requests. |
| `readyReplicas` | `int32` | Number of control plane Machines that are ready. |
| `replicas` | `int32` | Total number of control plane Machines across all states. |
| `updatedReplicas` | `int32` | Number of Machines running the desired version. |
| `unavailableReplicas` | `int32` | Number of Machines that are unavailable (not ready or being deleted). |
| `conditions` | `[]Condition` | Standard CAPI conditions: `Ready`, `Available`, `Initialized`, `KubeconfigReady`, `MachinesUpToDate`, `ControlPlaneJoined` (HA only), `EtcdHealthy` (HA only). See [EtcdHealthy condition](#etcdhealthy-condition) and [MachinesUpToDate condition](#machinesuptodate-condition) below. `Ready` and `Available` are `False` with reason `UnsupportedDistribution` when the effective distribution is `kubeadm`; see [kubeadm workers](#kubeadm-workers). `Ready`, `Available` and `EtcdHealthy` are `False` with reason `WorkloadClusterIdentityChanged` while control-plane membership changes are frozen; see [Workload cluster identity](#workload-cluster-identity). |
| `observedGeneration` | `int64` | Most recent generation observed by the controller. |
| `failureReason` | `string` | Short machine-readable failure indicator. Cleared automatically when the next reconcile succeeds — a non-empty value indicates an ongoing failure, not a terminal one. |
| `failureMessage` | `string` | Human-readable failure description. Cleared automatically on the next successful reconcile. If non-empty, check KairosControlPlane events and owned Machine events for context. |
| `selector` | `string` | Label selector string identifying control plane Machines. |
| `lastNodePushObserved` | `*Time` | Timestamp at which the control-plane controller first observed that the workload-cluster kubeconfig Secret was absent on the node-push path (alpha-2+). Cleared once the Secret is present and `KubeconfigReady` condition transitions to `True`. Used to escalate condition severity from `Info` to `Warning` after 10 minutes — not a terminal state. |

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

A 3-node HA example with a kube-vip VIP:

```yaml
apiVersion: controlplane.cluster.x-k8s.io/v1beta2
kind: KairosControlPlane
metadata:
  name: kairos-control-plane
  namespace: default
spec:
  replicas: 3
  version: "v1.34.1+k0s.1"
  ha:
    vip:
      address: "192.168.1.50"   # must equal Cluster.spec.controlPlaneEndpoint.host
      interface: "ens192"
      mode: ARP
  machineTemplate:
    infrastructureRef:
      apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
      kind: VSphereMachineTemplate
      name: control-plane-template
  kairosConfigTemplate:
    name: kairos-config-template-control-plane
```

See the full worked sample at `config/samples/capv/kairos_cluster_k0s_ha.yaml` and the [CAPV HA quickstart](QUICKSTART_CAPV.md#high-availability-3-node-k0s-control-plane).

### EtcdHealthy condition

Surfaced on `KairosControlPlane.status.conditions` for HA control planes only (`replicas` is `3` or `5`); absent for single-node control planes.

| Status | Reason | Meaning |
|--------|--------|---------|
| `True` | `EtcdHealthy` | All desired etcd members report healthy and voting. |
| `False` (Info) | `EtcdQuorumDegraded` | Quorum still holds, but fewer than all desired members are healthy and voting. |
| `False` (Warning) | `EtcdQuorumAtRisk` | The healthy+voting member count is at or below the `(N/2)+1` quorum minimum — one more member loss breaks quorum. Also covers the bring-up window before any member has reported. |
| `False` (Info) | `WaitingForEtcdMember` | The init node has not yet reported a healthy voting etcd member. |
| `False` (Error) | `WorkloadClusterIdentityChanged` | A workload API server answered with a different cluster identity than the one pinned on this `KairosControlPlane`. Control-plane membership changes are frozen until an operator re-pins. `Ready` and `Available` carry the same reason. This overrides every other `EtcdHealthy` state, and is also set on a control plane that has been scaled down to one replica after it was pinned. See [Workload cluster identity](#workload-cluster-identity). |

The condition is derived from a per-cluster, node-reported etcd-status Secret (see [Security Considerations](#security-considerations) for the trust model of this signal). The controller uses this condition, together with the desired replica count, to refuse control-plane Machine deletions that would drop etcd below the quorum minimum — this quorum-safety decision is made independently of, and before, any single node's self-reported signal.

### MachinesUpToDate condition

Reports whether every control-plane Machine runs `spec.version`.

| Status | Reason | Meaning |
|--------|--------|---------|
| `True` | — | No control-plane Machine is outdated. |
| `False` (Info) | `RollingOut` | Outdated Machines are being replaced, one at a time. A replacement is added and must register a Node before an outdated Machine is removed, and the rollout waits while any control-plane Machine is still being deleted. |
| `False` (Warning) | `SingleNodeRolloutUnsupported` | A single-node control plane (`replicas: 1`) has a Machine whose version differs from `spec.version`. The Machine is left unchanged: it runs the whole cluster, so a replacement would start a separate, empty cluster. Set `spec.version` back to the version the Machine runs to clear the condition. |

### Workload cluster identity

A `KairosControlPlane` with more than one replica records which workload cluster it manages, and refuses to change control-plane membership through any other cluster. The identity is the UID of the workload cluster's `kube-system` Namespace. Every API server creates that Namespace the first time it starts against an empty etcd, so two clusters have different UIDs even when they share a certificate authority, as they do after a joining k0s member has started a new etcd cluster of its own.

**When the identity is pinned.** On the first reconcile after the control plane is highly available, `KubeconfigReady` is `True`, and a control-plane Machine has a Node, the controller reads the UID through the control-plane endpoint and through every control-plane member it can reach directly. It stores the UID only when at least one path answered, every answer is shaped like a Kubernetes UID, and every answer agrees. If the answers disagree, or one is not UID-shaped, the control plane is frozen at once; nothing a workload API server returns is copied into an annotation or an event unless it is UID-shaped. Existing clusters are pinned the same way on the first reconcile after the controller is upgraded. Once a control-plane Machine of an HA control plane has a Node, and until the identity is pinned, no joiner is admitted, no control-plane Machine is deleted, and no etcd-leave hook is removed. That hold has no time limit, and it also suspends the bounded etcd-leave timeout: a Machine being deleted stays at its etcd-leave hook. If the controller cannot pin (no path answers, or `KubeconfigReady` stays `False`), set the pin annotation by hand to the UID you verified; a pin that is already set is never replaced by first-use pinning. See [Before upgrading an existing HA cluster](HIGH_AVAILABILITY.md#before-upgrading-an-existing-ha-cluster).

**When it is checked.** The UID must match the pin:

- before a joiner is admitted, on the same connection the k0s join token is issued through (k3s: through the endpoint);
- before a k0s join token is extended or deleted;
- before the controller deletes a control-plane Machine, for a rollout or a scale-down;
- on every connection the k0s etcd-leave handshake and the orphan etcd-member cleanup use; these two paths use no connection at all until a pin exists;
- and through every reachable path at most once a minute, even with nothing to do.

An API server whose identity cannot be read is treated as not answering: the action waits and is retried.

**The freeze.** On a mismatch the controller sets the mismatch annotation and, until an operator clears it:

- creates and deletes no control-plane Machine, and removes no etcd-leave hook, so a Machine deleted by an operator or a MachineHealthCheck stays at its pre-terminate hook (k0s); k3s Machines carry no hook, so the controller cannot hold their deletion;
- writes nothing to the workload cluster: no etcd-leave request, no join token;
- sets `Ready`, `Available` and `EtcdHealthy` to `False` (Error) with reason `WorkloadClusterIdentityChanged`, and emits a Warning event `WorkloadClusterIdentityChanged` that names the pinned UID, the UID it read, and the address it read it through;
- requeues every 60 seconds.

Deleting the whole cluster is not affected: control-plane deletion strips the etcd-leave hooks and deletes the Machines whatever the identity.

| Annotation | Set by | Purpose |
|---|---|---|
| `controlplane.cluster.x-k8s.io/workload-cluster-uid` | Controller, once; operator to re-pin | The pinned `kube-system` Namespace UID. The controller sets it at first contact and never rewrites it. Changing it, or removing it so the controller pins again, clears a freeze. |
| `controlplane.cluster.x-k8s.io/workload-cluster-identity-mismatch` | Controller | The freeze marker: `<observed UID> via <member IP\|endpoint> at <RFC3339>; pinned <UID>`. It keeps the first mismatch seen. The `pinned` part records the pin the mismatch was measured against; when the pin annotation no longer equals it, the controller removes this annotation and emits a Normal event `WorkloadClusterIdentityFreezeCleared`. |

**Clearing the freeze.** Only an operator clears it; a changed identity does not heal by itself. Follow the runbook in [High availability: workload cluster identity](HIGH_AVAILABILITY.md#day-2-workload-cluster-identity-and-the-freeze), then do one of:

- **Re-pin to a verified UID** (recommended). Set the pin to the UID every member now answers with:
  ```bash
  kubectl annotate kairoscontrolplane <name> -n <namespace> --overwrite \
    controlplane.cluster.x-k8s.io/workload-cluster-uid=<verified-uid>
  ```
- **Let the controller pin again.** Remove the pin; the controller pins only if every path agrees, and freezes again if they do not:
  ```bash
  kubectl annotate kairoscontrolplane <name> -n <namespace> \
    controlplane.cluster.x-k8s.io/workload-cluster-uid-
  ```
- **Keep the pin.** If the cause is gone and every member answers with the pinned UID again, remove only the mismatch annotation. The next check freezes again if any path still disagrees:
  ```bash
  kubectl annotate kairoscontrolplane <name> -n <namespace> \
    controlplane.cluster.x-k8s.io/workload-cluster-identity-mismatch-
  ```

Write access to the `KairosControlPlane` is required for all three, the same access that can scale the control plane.

### k0s etcd leave and orphan members

On k0s HA control planes the controller removes a deleted control-plane Machine's etcd member before it lets the deletion proceed, including for a Machine that never got a Node, and it removes etcd members no control-plane Machine accounts for. The rules are in [High availability: etcd health and quorum-safe replacement](HIGH_AVAILABILITY.md#day-2-etcd-health-and-quorum-safe-replacement). The controller keeps its state in these annotations; none is meant to be set by hand.

| Annotation | On | Purpose |
|---|---|---|
| `controlplane.cluster.x-k8s.io/etcd-leave-requested-at` | Machine | When the leave was first requested (RFC3339). Starts the ~5-minute wait for the node's own ack. |
| `controlplane.cluster.x-k8s.io/etcd-member-leave-requested-at` | Machine | When the leave was handed to k0s. Starts the ~10-minute wait for k0s. |
| `controlplane.cluster.x-k8s.io/etcd-member-leave-member-id` | Machine | The etcd member ID first seen for the leaving node. k0s's `EtcdMember` is acted on only while it carries this ID. |
| `controlplane.cluster.x-k8s.io/etcd-leave-unreachable-since` | Machine | The first time, before any leave request, that no verified workload API server answered. After ~15 minutes the deletion proceeds with `EtcdMemberLeaveTimedOut`. |
| `controlplane.cluster.x-k8s.io/etcd-orphan-member-leave` | KairosControlPlane | The orphan etcd member being removed: `<EtcdMember name> <member ID> <RFC3339>`. While set, no other orphan is removed and no control-plane Machine is created or deleted. |

### Template ownership

The control plane makes the owning `Cluster` an owner of the templates it references: the machine template named by `machineTemplate.infrastructureRef`, and the `KairosConfigTemplate` named by `kairosConfigTemplate`. `clusterctl move` carries an object only if an owner-reference chain links it to the Cluster, so this is what moves the templates together with the cluster.

- It is a plain owner reference, not a controller reference, so several control planes can reference the same template.
- It applies to templates whose kind ends in `Template`, in the Cluster's own namespace. A template that does not exist yet is skipped on that reconcile.
- Kubernetes deletes an object once all of its owners are gone. Deleting the last Cluster that owns a template therefore deletes the template. A template shared by several Clusters stays until the last of them is deleted.

### KubeconfigReady condition

Reports whether the workload-cluster kubeconfig Secret (`<cluster>-kubeconfig`) is present, controlled by this `KairosControlPlane`, and holds canonical content that CAPI core and this provider's own controllers can safely build a client from.

Before publishing, the controller stages the candidate kubeconfig in a per-cluster Secret it owns, `<cluster>-kubeconfig-push` (label `controlplane.cluster.x-k8s.io/secret-type: kubeconfig-push`), and rebuilds it into a single canonical form: one cluster, one user, one context, named `<cluster>-admin@<cluster>`, with the `server` the controller already knows for this cluster. A pre-existing `<cluster>-kubeconfig` Secret not yet controlled by this `KairosControlPlane` is adopted the first time it is observed, provided it was created no earlier than the `Cluster` object and carries the `cluster.x-k8s.io/cluster-name` label; adoption rewrites its content into the same canonical form.

The control-plane controller creates and owns the staging Secret; a control-plane node publishes its kubeconfig by PATCHing only the Secret's `value` key, and never creates it or writes the contract Secret `<cluster>-kubeconfig` directly — the control plane is the contract Secret's sole writer. Operators do not create or edit the staging Secret; treat it as controller-internal state. See [Waiting for the kubeconfig push target](#waiting-for-the-kubeconfig-push-target) for the bootstrap-rendering precondition on this Secret.

| Status | Reason | Meaning |
|--------|--------|---------|
| `True` | `KubeconfigReady` | The contract Secret is controlled by this control plane, canonical, and its client certificate is unexpired. |
| `True` | `KubeconfigReadyViaSSHFallback` | Same as above, but the content arrived through the opt-in SSH fallback path rather than node-push. |
| `False` (Info → Warning) | `WaitingForNodePush` | No usable kubeconfig has been published yet. Severity escalates from Info to Warning once the wait exceeds the node-push timeout (10 minutes). See [Air-gapped fallback (SSHFallback)](QUICKSTART_CAPV.md#air-gapped-fallback-sshfallback) if the node cannot reach the management API server. |
| `False` (Warning) | `NodePushRejected` | A candidate kubeconfig — the one staged by a node, or the one already in the contract Secret — failed a structural or credential-shape check. The message names the failing rule, never any kubeconfig content. Whatever the contract Secret already held is left unchanged. Correct the source of the candidate (redeploy the affected node, or inspect its staging Secret); it is re-evaluated on the next reconcile. |
| `False` (Warning) | `NodePushCAMismatch` | A candidate kubeconfig presents a different certificate authority than the one already pinned for this cluster (see [Annotations](#annotations) below). The contract Secret's previous content, if any, is left in place; the controller refuses to publish the mismatched candidate until an operator re-pins. See the runbook below. |
| `False` (Warning) | `KubeconfigSecretNotOwned` | A Secret named `<cluster>-kubeconfig` exists but is neither controlled by this `KairosControlPlane` nor eligible for adoption, and `Cluster.spec.controlPlaneRef` does not point at this `KairosControlPlane` either. The controller has no authority over a Secret belonging to a different cluster (or predating this one) and does not touch its content. Delete the Secret so the controller can publish its own. |
| `False` (Warning) | `KubeconfigCertificateExpired` | The published kubeconfig's inline client certificate is outside its validity window. Its content is left in place — expiry does not clear a working Secret — but a client built from it will fail authentication. A control-plane Machine replacement, or an SSH-fallback fetch, refreshes the certificate. |
| `False` (Info) | `SSHFallbackDialing` | The SSH fallback path is dialing the workload node. |
| `False` (Warning) | `SSHFallbackFailed` | An SSH fallback attempt failed. Check Events for the categorized error. |
| `False` (Warning) | `SSHFallbackMisconfigured` | A Secret referenced by `spec.sshFallback` is missing, empty, or unparseable. |

When an unclaimed `<cluster>-kubeconfig` Secret's *cluster* still matches this
control plane — `Cluster.spec.controlPlaneRef` points at this
`KairosControlPlane`, even though the Secret itself is unowned or predates the
`Cluster` — the controller recovers on its own instead of surfacing
`KubeconfigSecretNotOwned`: it empties the Secret in place (never deletes it)
and republishes from staging, reporting `WaitingForNodePush` meanwhile. No
operator action is needed in that case. Manual deletion is only the right
response when the Secret genuinely belongs to a different cluster, or
predates this one entirely.

The controller also emits two Warning Events (not conditions) when the staging Secret's own metadata is unexpected — these indicate the staging Secret was modified by something other than the controller, and do not by themselves affect `KubeconfigReady`:

| Event reason | Meaning |
|---|---|
| `StagingKubeconfigNotOwned` | The staging Secret `<cluster>-kubeconfig-push` exists but is not controlled by this `KairosControlPlane`. Delete it so the controller can recreate it. |
| `StagingKubeconfigImmutable` | The staging Secret was marked `immutable: true`, which blocks the controller from repairing its metadata. Delete it so the controller can recreate it. |

#### Annotations

Two annotations on `KairosControlPlane` control the certificate-authority pin used by `NodePushCAMismatch`:

| Annotation | Set by | Purpose |
|---|---|---|
| `controlplane.cluster.x-k8s.io/kubeconfig-ca-pin` | Controller only | The certificate-authority fingerprint of the first kubeconfig the controller accepted for this cluster. Every later candidate must present the same CA or it is refused with `NodePushCAMismatch`. Do not set this by hand; the controller manages it and overwrites it on the next accepted publish. |
| `controlplane.cluster.x-k8s.io/kubeconfig-repin` | Operator | Set to `"true"` to clear the stored CA pin (both annotations are removed) and let the controller re-pin from the next candidate it accepts. This is the sanctioned way past a `NodePushCAMismatch`. The controller consumes the annotation once — you do not need to remove it yourself. |

**Runbook: `NodePushCAMismatch`.**

1. Inspect the kubeconfig currently offered on the staging Secret before doing anything else:
   ```bash
   kubectl get secret <cluster>-kubeconfig-push -n <cluster-namespace> \
     -o jsonpath='{.data.value}' | base64 -d
   ```
   Confirm whether the change in certificate authority is expected — for example, a control-plane node that reset and regenerated its cluster CA — or unexpected, in which case investigate the node and its staging Secret before proceeding.
2. If the change is expected, re-pin:
   ```bash
   kubectl annotate kairoscontrolplane <name> -n <cluster-namespace> \
     controlplane.cluster.x-k8s.io/kubeconfig-repin=true
   ```
3. On the next reconcile the controller drops the stored pin and accepts the next candidate's CA as the new pin. `KubeconfigReady` returns to `True` (or to `False(WaitingForNodePush)` if no candidate is available yet).
4. If the change is not expected, do not re-pin. Replace the affected control-plane node instead.

---

## KairosControlPlaneTemplate

**API Group:** `controlplane.cluster.x-k8s.io`
**API Version:** `v1beta2`
**Kind:** `KairosControlPlaneTemplate`

`KairosControlPlaneTemplate` is a template for creating `KairosControlPlane` resources. Intended for use with ClusterClass (planned; not yet exercised in samples).

Its validating webhook applies the same rules as the `KairosControlPlane` webhook, against the nested spec, so a template that would produce an invalid control plane is rejected when it is staged rather than when a cluster is created from it. Rules that compare against a namespace, `sshFallback` Secret references and `machineTemplate.infrastructureRef`, read the `KairosControlPlaneTemplate`'s own namespace, which is the namespace a stamped `KairosControlPlane` lives in.

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

The provider injects `/system/oem/12_kairos-capi-persistency.yaml` into every k0s and k3s node's cloud-config via `write_files`. The kubeadm worker cloud-config does not include it: with `distribution: kubeadm`, the node image must already persist the paths kubeadm writes (see [Kubeadm workers](KUBEADM_WORKERS.md#prerequisites)). The file uses immucore's `extra-layout.env` mechanism (not `cos-layout.env`), so its `PERSISTENT_STATE_PATHS` value is **unioned** with the stock image's persistent paths — never overwriting them. A custom or stock Kairos image is unaffected; the provider's persistent paths are added on top of whatever the image already persists.

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

**Adding your own persistent paths.** Users who need application-specific persistent paths can layer their own cloud-config snippet by writing a file via `Spec.Files` to `/system/oem/91_custom.yaml` (or any `/system/oem/9*` prefix). Lower numeric prefixes (`5x`–`8x`) are not recommended: future provider files may use those slots and silently shadow your override.

Tracked as KD-23 (persistence injection) and KD-34 (in-place upgrade persistence guarantees) in the punch list.

---

## Writing files to nodes

`KairosConfig.spec.files` (and the equivalent field inside `KairosConfigTemplate.spec.template.spec.files`) writes files onto the node's filesystem via the cloud-config `write_files:` list. The files are rendered at bootstrap time on all distributions (k0s, k3s, kubeadm) and all infrastructure providers (CAPV, CAPK, CAPD, CAPM3, and the Kairos fleet provider).

### Limits

- At most 32 files per `KairosConfig`.
- Content maximum: 32 KiB (32768 bytes) per file.
- `path` must be absolute (begin with `/`) and must not contain `..` segments.
- `permissions` must be a 3- or 4-digit octal string. 4-digit modes encode setuid/setgid/sticky: `"4755"` is valid. Leave unset to use the image default (typically `"0644"`).
- `owner` must follow `user:group` POSIX convention. The group portion is optional (`"root"` is valid as well as `"root:root"`).

The validating webhook rejects entries that violate path, permissions, or owner constraints. Validation errors appear in `KairosConfig.status.failureMessage`.

### Example

```yaml
apiVersion: bootstrap.cluster.x-k8s.io/v1beta2
kind: KairosConfigTemplate
metadata:
  name: kairos-config-template-control-plane
  namespace: default
spec:
  template:
    spec:
      role: control-plane
      distribution: k0s
      kubernetesVersion: "v1.34.1+k0s.1"
      userPasswordSecretRef:
        name: kairos-user-password
      userGroups:
        - admin
      files:
        - path: /etc/systemd/network/05-control-plane.network
          content: |
            [Match]
            Name=ens192

            [Network]
            Address=192.168.100.10/24
            Gateway=192.168.100.1
            DNS=192.168.100.1
          permissions: "0644"
          owner: "root:root"
```

A worked CAPV example (including the static-IP caveat comments) is in
`config/samples/capv/kairosconfig_files_static_ip.yaml`.

### Static IP via systemd-networkd — known limitation on pre-installed images

Writing `/etc/systemd/network/<name>.network` via `spec.files` is a common use case: a user wants to pin the control-plane node IP so it matches `Cluster.spec.controlPlaneEndpoint.host`. The file is written correctly. However, on a **pre-installed disk image** (such as a Hadron image deployed via CAPV), systemd-networkd may already hold a DHCP lease by the time the file lands in the filesystem. The new file does not take effect on its own: a `networkctl reload && networkctl reconfigure <iface>` must run before k0s or k3s starts.

The problem is that once k0s or k3s has bound the DHCP-assigned IP for API server listeners and TLS certificates, reconfiguring the interface on the same boot does not change those bindings. The static IP will be active from the next reboot onward, but the running k0s/k3s process will still use the DHCP address it saw at startup.

**Recommended approaches** (in preference order):

1. **Use infrastructure-layer IP management.** CAPV supports IPAM via `VSphereMachineTemplate.spec.template.spec.network.devices[].addressesFromPools`, or use a DHCP reservation at the network layer. With either approach the IP is set before the first boot and is consistent from the start — no race with networkd.

2. **If you write the `.network` file via `spec.files`**, pair it with an early reconfigure command. When `spec.preCommands` is rendered (currently not yet rendered — see API table), that field will be the right place. Until then, consider using a Kairos cloud-config `stages.initramfs` hook via an additional file in `spec.files` that calls `networkctl reload`. This is the user's responsibility; the provider does not perform the reload.

This is a known limitation of post-boot file writes for network configuration on pre-installed images, not a defect in the `spec.files` implementation.

---

## Notes

### API Version Compatibility

- **Kairos CAPI Provider APIs**: `bootstrap.cluster.x-k8s.io/v1beta2` and `controlplane.cluster.x-k8s.io/v1beta2`.
- **CAPI Core Types**: The wire API version for `Cluster`, `Machine`, and related resources is `v1beta2` (`cluster.x-k8s.io/v1beta2`). `go.mod` imports `sigs.k8s.io/cluster-api v1.13.4` and this provider's controllers use the `ContractVersionedObjectReference` / `MachineNodeReference` value types introduced by that contract. CAPI core v1.13.4+ is required at runtime.
- **Infrastructure Providers**: Use their respective API versions (e.g., CAPD/CAPV use `infrastructure.cluster.x-k8s.io/v1beta1`, CAPK and the Kairos fleet provider use `infrastructure.cluster.x-k8s.io/v1alpha1`). Any other CAPI-conformant infrastructure provider is cloned through a generic path that reads group and version from the `<Kind>MachineTemplate` object itself, so it is not limited to these versions.

### Credential Requirements

At least one of the following must be set on every `KairosConfig` (enforced by the validating webhook):

- `userPassword` (inline; discouraged — the value is stored in the resource spec and readable by anyone with read access to it)
- `userPasswordSecretRef` (recommended)
- `sshPublicKey`
- `githubUser`

### Worker Token Requirements

For `KairosConfig` with `role: worker`:

- **k0s**: Set `workerToken` or `workerTokenSecretRef`. `workerTokenSecretRef` is preferred.
- **k3s**: Set `k3sToken` or `k3sTokenSecretRef`. `k3sTokenSecretRef` is preferred.
- **kubeadm**: Set no token. The controller mints a short-lived bootstrap token in the workload cluster; an inline token in `spec.kubeadm.joinConfiguration` is rejected by the webhook.

The controller fails reconciliation if no token is provided for a k0s or k3s worker.

### Single-Node Mode

When `KairosControlPlane.spec.replicas == 1`, the controller automatically sets `KairosConfig.spec.singleNode = true` on the created control-plane Machine's config. Setting it manually on a `KairosConfigTemplate` is unnecessary when managed by `KairosControlPlane`.

`singleNode` decides *whether* single-node mode applies; for k0s, `k0sSingleNode` separately decides *which* k0s flag single-node mode renders:

- **k3s**, on every infrastructure provider: `singleNode` enables single-node cluster-init mode. `k0sSingleNode` has no effect.
- **k0s on CAPK**: `singleNode` renders `--single`, unconditionally. `k0sSingleNode` has no effect.
- **k0s on the generic / CAPV / CAPM3 / fleet render path**: `singleNode` alone renders `--enable-worker` — a joinable, schedulable controller, so a control-plane-plus-worker cluster works with no further configuration. Setting `k0sSingleNode: true` renders `--single` instead: a standalone all-in-one node that refuses all worker joins. Use `k0sSingleNode: true` only for a node that is meant to stay standalone forever.

See the `k0sSingleNode` row above for the field reference, and `config/samples/fleet/kairos_cluster_k0s_single_node.yaml` for a worked standalone-node example.

### Multi-Node Control Planes

`KairosControlPlane.spec.replicas` accepts `1`, `3`, or `5`. The validating webhook rejects even counts (they give the same etcd fault tolerance as the next-lower odd count while raising the quorum requirement — always use the next-higher odd number instead) and values above `5` (beyond 5 members the quorum cost outweighs the added fault tolerance for a control plane).

`3` and `5` configure a highly-available control plane. Set `spec.ha.vip` on CAPV, CAPM3, and CAPD clusters so kube-vip provides a stable, failover-capable endpoint — do not set it on CAPK, which supplies its own LoadBalancer-backed endpoint. See [HAConfig](#haconfig) and [EtcdHealthy condition](#etcdhealthy-condition) above, and [docs/HIGH_AVAILABILITY.md](HIGH_AVAILABILITY.md) for the full day-2 behavior (quorum-safe replacement, k0s clean etcd-leave, and the k3s orphaned-member limitation tracked as KD-5d).

### Pause

Both controllers stop reconciling an object while its Cluster is paused (`Cluster.spec.paused: true`) or the object carries the `cluster.x-k8s.io/paused` annotation. This is the Cluster API pause contract, and `clusterctl move` relies on it. `KairosConfig.spec.pause` is a separate switch for a single `KairosConfig`.

- **`KairosConfig`:** while paused, no bootstrap data is rendered.
- **`KairosControlPlane`:** while paused, the controller creates and deletes no Machines.
- **Deleting a `KairosControlPlane` while its Cluster is paused:** the object stays, with its finalizer, and teardown starts when the Cluster is unpaused. Teardown marks every control-plane Machine for deletion and cannot be undone, so it is held. If the Cluster no longer exists, only the annotation on the `KairosControlPlane` holds the deletion.

### Security Considerations

- Provide credentials via `userPasswordSecretRef` (recommended) or `sshPublicKey` / `githubUser`. Inline `userPassword` is stored in the KairosConfig spec and readable by anyone who can `kubectl get kairosconfig`.
- Worker tokens should use the `*SecretRef` variants. Inline tokens in specs are readable without Secret RBAC.
- For kubeadm workers, the bootstrap token (valid 15 minutes) is never written to `spec` or `status`, but it is part of the rendered bootstrap data Secret. Restrict read access to bootstrap data Secrets as for any other distribution.
- **HA etcd health/leave signals are node-self-reported and forgeable (KD-51).** Both day-2 HA signal channels — the per-cluster etcd-status Secret and the workload `kube-system/kairos-etcd-leave` ConfigMap leave acknowledgement — are written by control-plane nodes with vanilla RBAC on objects shared across the control plane, so a compromised control-plane node can forge them. This is not a privilege escalation (a compromised control-plane node already holds cluster-admin-equivalent access) and cannot force an unsafe deletion, because the quorum-safety decision is made independently of, and before, any node signal is consulted. A forged signal can only self-downgrade the clean-leave/health guarantee. Per-member-scoped signals are tracked as future hardening.
- **The workload kubeconfig Secret is always controller-rebuilt, never stored verbatim.** The control-plane controller stages a candidate kubeconfig, validates it, and rebuilds it into a single canonical form before publishing `<cluster>-kubeconfig`; it pins the certificate authority the first time a cluster's kubeconfig is accepted and refuses a later candidate presenting a different one. See [KubeconfigReady condition](#kubeconfigready-condition) for the full set of reasons this surfaces and the operator re-pin procedure.
- All Secrets referenced by `*SecretRef` fields must exist in the management cluster before the KairosConfig is reconciled. Missing Secrets cause a transient failure that clears automatically when the Secret is created.

# Changelog

All notable changes to the Kairos Cluster API provider are documented in this
file. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

This project is pre-1.0. Alpha releases may include breaking changes; those are
called out explicitly under **Breaking changes**. Per-scenario migration steps
live in [docs/UPGRADING.md](docs/UPGRADING.md).

## [v0.1.0] — 2026-08-10

The first non-beta 0.1.0 release. Adds the **Kairos fleet (AuroraBoot)
infrastructure provider** as a first-class peer to CAPD/CAPV/CAPK/CAPM3, and
fixes two multi-node bugs found by an integrated real-hardware end-to-end
run: worker `MachineDeployment`s never scaled up on any infrastructure
provider, and a single k0s control plane refused all worker joins. See the
[v0.1.0 release notes](docs/release-notes/v0.1.0.md) for full detail.

### Breaking changes

None in this release.

### Added

- **Kairos fleet infrastructure provider support** (ADR 0008). Fleet claims
  already-enrolled Kairos nodes from a named AuroraBoot group rather than
  creating machines on demand. New: `docs/QUICKSTART_FLEET.md`, k3s and k0s
  control-plane-plus-worker samples
  (`config/samples/fleet/kairos_cluster_{k0s,k3s}_with_workers.yaml`), a
  standalone-single-node k0s sample
  (`kairos_cluster_k0s_single_node.yaml`), and `clusterctl.yaml`
  registration guidance in `docs/INSTALL.md`. Requires fleet provider
  `v0.1.0-beta.2` or later — `v0.1.0-beta.1` crash-loops on a real
  management cluster.
- **`KairosFleetMachine` recognized as a supported infrastructure kind end
  to end**: `getNodeIP` extractor support, actionable endpoint-wait
  guidance, RBAC parity (`create;get` on `kairosfleetmachines`, `get` on
  their `status` and on `kairosfleetmachinetemplates`), template cloning,
  and node-push kubeconfig support.
- **`KairosConfig`/`KairosConfigTemplate.spec.k0sSingleNode`** (`*bool`,
  default `false`). Opts a single-node k0s control plane on the generic /
  CAPV / CAPM3 / fleet render path into the standalone k0s `--single` mode
  (refuses all joins), instead of the new default `--enable-worker`
  (joinable, schedulable). k0s control-plane machines only; k3s and
  worker/join nodes ignore it. CAPK is unchanged (always `--single` for
  single-node).
- **`KairosControlPlaneStatus.Version`**, part of the CAPI control-plane
  contract. Set once at least one control-plane member is ready.

### Fixed

- **Worker `MachineDeployment`s now scale up, on every infrastructure
  provider.** `status.version` was never set, so CAPI core's
  `ControlPlaneIsStable` preflight read every Kairos control plane as
  perpetually provisioning and refused to create worker Machines. Fixed by
  the new `status.version` field above. Found by the integrated multi-node
  e2e on real hardware — single-node kind runs never exercised a worker
  `MachineDeployment`, so the gap was invisible there.
- **A single k0s control plane now accepts workers.** It rendered k0s
  `--single` unconditionally, refusing every join. The generic / CAPV /
  CAPM3 / fleet render path now defaults to `--enable-worker`; the
  standalone behavior is preserved via the opt-in `spec.k0sSingleNode`
  field above. Validated on real hardware: a fleet-backed k0s cluster with
  a joined worker, both `Node`s `Ready` and providerIDs matched.

### Security

- **No new RBAC surface beyond the enumerated fleet infra-kind grants** —
  the same shape already granted for every other infrastructure provider's
  machine/template kinds; no cluster-wide or wildcard grant added.
- **The `kairos-fleet://<node-id>` providerID contract is a
  cross-repository string match with no compile-time link**, guarded today
  by cross-referencing doc comments and unit tests on each side; an
  integrated e2e is the intended long-term guard (tracked, not yet in CI).
- **AuroraBoot admin-token handling is out of this repo's scope.** The
  token Secret referenced by
  `KairosFleetCluster.spec.auroraboot.adminTokenSecretRef` is read and
  stored by the fleet provider. Samples and quickstart instruct creating it
  out of band and never committing a real token.

### Known limitations

- Fleet HA (`replicas: 3`/`5`) is mechanically reachable but not exercised
  — no fleet HA sample is shipped (ADR 0008).
- The integrated three-provider (bootstrap + control-plane + fleet) path is
  validated manually on real hardware, not yet in this repository's CI.
- No cosign signing or SBOM yet on release artifacts (carried forward from
  v0.1.0-beta.2).
- k3s HA day-2: control-plane replacement orphans an etcd member (KD-5d).
  k0s remains the fully-supported HA distribution.

## [v0.1.0-beta.2] — 2026-08-09

Beta-2 adds **`clusterctl`-installable packaging** (ADR 0007): one image,
packaged as two `clusterctl` providers (`bootstrap-kairos`,
`control-plane-kairos`), alongside the existing flat `kubectl apply`
manifest. Baseline moves to Kubernetes v1.36 (validated/recommended across
the CAPI v1.13 management band) and CAPI core v1.13.4. See the
[v0.1.0-beta.2 release notes](docs/release-notes/v0.1.0-beta.2.md) for full
detail.

### Breaking changes

- **The `clusterctl` install path uses new namespaces and labels, and is
  mutually exclusive with the flat manifest.** `clusterctl` providers use
  `capi-kairos-bootstrap-system`/`bootstrap-kairos` and
  `capi-kairos-control-plane-system`/`control-plane-kairos`, distinct from
  the flat manifest's `kairos-capi-system`/`kairos`. Because
  `Deployment.spec.selector` and `ClusterRoleBinding.roleRef` are
  immutable, applying one path over the other does not upgrade it in place
  — it stands up a second, independent set of controllers/webhooks
  watching the same CRDs, which is unsupported. **If you stay on the flat
  manifest, no action is required.** See
  [docs/UPGRADING.md — Moving from the flat install to clusterctl packaging](docs/UPGRADING.md#moving-from-the-flat-install-to-clusterctl-packaging).

### Added

- **Two `clusterctl` providers from one image** (ADR 0007), selected via a
  new `--controllers=bootstrap|control-plane|all` flag; a role-specific
  `LeaderElectionID` and `--namespace` flag bring single-namespace-watch
  compliance. A release now publishes `bootstrap-components.yaml`,
  `control-plane-components.yaml`, and `metadata.yaml` (digest-pinned)
  alongside `kairos-capi-provider.yaml`.
- **Per-provider least-privilege RBAC.** RBAC is split into two
  per-namespace sets; five dead cluster-wide grants are removed, including
  an unused `webhookconfigurations` `patch` permission.
- **CA injection via standard cert-manager `inject-ca-from`.**
  `hack/post-install-webhook-ca-injection.sh` is retired.
- **`clusterctl` plumbing fixed.** `config/clusterctl/metadata.yaml`
  reshaped to a valid `Metadata` document; the stray `provider.yaml`
  deleted.

### Changed

- Kubernetes v1.36 validated/recommended across the CAPI v1.13 management
  band (v1.30-v1.36); CAPI core v1.13.3 → v1.13.4; Kairos v3.6.0+ → v4.1.2;
  e2e stack moves to KubeVirt v1.9 / kindest/node v1.36.1 / k3s v1.36.1.

### Fixed

- **SSH-fallback `KubeconfigReady` condition no longer flaps** (#85). The
  condition is now written deterministically instead of racing a
  contention-sensitive worker round-trip.

### Security

- **RBAC reduction as attack-surface reduction**: five dead cluster-wide
  grants removed, including the unused webhook-config `patch` primitive.
- **Supply chain: digest-pinned, not yet signed.** Release artifacts remain
  image-digest-pinned but are not cosign-signed and ship no SBOM.

## [v0.1.0-beta.1] — 2026-07-04

Beta-1 adds **highly-available control planes**: `spec.replicas` accepts
`1` (single-node), `3`, or `5` on CAPK, CAPV, and CAPM3, for both k0s and
k3s. CAPI core dependency moves to v1.13.3, adopting the v1beta2
contract-versioned object-reference model (ADR 0006). See
[docs/UPGRADING.md](docs/UPGRADING.md#v010-alpha2--v010-beta1) for the
per-scenario upgrade impact.

### Breaking changes

- **`spec.replicas` validation changed.** Now accepts `1`, `3`, or `5`; the
  validating webhook rejects even counts (worse etcd fault tolerance per
  quorum cost than the next-lower odd count) and values above `5`. Existing
  `replicas: 1` deployments are unaffected. (KD-5b, ADR 0005)
- **CAPI core v1.13.3 / v1beta2 contract required.** The management-side
  CAPI dependency moves from v1.8 to v1.13.3, adopting
  `ContractVersionedObjectReference`/`MachineNodeReference` in place of
  v1beta1 pointer-based references (commit `736aeb3`, ADR 0006). Deploy
  this provider's CRDs via kustomize or `clusterctl`, never raw `kubectl
  apply -f config/crd/bases/` — the contract label is required for CAPI
  core to resolve object references. RBAC gains `get;list;watch` on
  `customresourcedefinitions.apiextensions.k8s.io` (commit `b31083f`).
- **`KairosControlPlane.spec.machineTemplate.metadata` is now a pointer**
  (commit `58bc284`), so an empty value is omitted from the API payload
  instead of round-tripping as `{}` — required by CAPI v1beta2's
  `ObjectMeta` `MinProperties=1` schema. No operator action needed unless
  external tooling depends on the Go field being a value type.

### Added

- **Highly-available control planes** (ADR 0005, Phases 1-4 + CAPK/CAPM3
  extension). `spec.replicas: 3` or `5` provisions a multi-node k0s or k3s
  control plane with etcd quorum. Endpoint mechanism is provider-specific:
  `spec.ha.vip` (kube-vip) on CAPV/CAPM3; CAPK's own LoadBalancer Service on
  CAPK (do not set `spec.ha.vip` there). CAPK control-plane VMs gain a
  routable secondary NIC for etcd peering, since KubeVirt's masquerade
  interface gives every VM the same self-address.
- **`EtcdHealthy` condition** on `KairosControlPlane`, derived from
  per-cluster node-reported etcd member health.
- **Quorum-safe replacement.** The controller refuses a rollout or
  scale-down delete that would drop etcd below `(N/2)+1` healthy voting
  members.
- **Clean etcd member eviction on k0s.** A CAPI pre-terminate hook pauses
  termination after drain; the departing node runs `k0s etcd leave` and
  acknowledges before the controller lets the delete finish.
- New samples: `config/samples/{capk,capv,capm3}/*_ha.yaml`. New quickstart
  sections for CAPK/CAPV/CAPM3 HA.

### Fixed

- **`KairosControlPlane.spec.distribution` now inherits from the
  referenced `KairosConfigTemplate` when unset**, instead of silently
  defaulting to `k0s` and overriding the template's distribution. A
  `DistributionOverride` warning Event fires if an explicit value disagrees
  with the template's.

### Security

- **KD-51**: HA etcd health/leave signals are node-self-reported over
  vanilla RBAC and forgeable by a compromised control-plane node. Not a
  privilege escalation (a compromised node already holds
  cluster-admin-equivalent access) and cannot force an unsafe deletion —
  the quorum-safety decision is made independently of any node signal.
- `spec.ha.vip.address` and `spec.ha.vip.interface` are validated at
  admission and shell-quoted at render time before being written into the
  kube-vip DaemonSet manifest.
- Controller RBAC gains a read-only, cluster-scoped `get;list;watch` on
  `customresourcedefinitions.apiextensions.k8s.io`.

## [v0.1.0-alpha.2] — 2026-06-18

Alpha-2 adds **Metal3 (CAPM3) bare-metal infrastructure support** and resolves
the two security notices that shipped with alpha-1: it removes **synchronous
SSH from the controller's reconcile path** (KD-3b) and removes the **insecure
`kairos:kairos` default credentials** (KD-3a). The full Kairos Hadron
end-to-end matrix — CAPV and CAPM3, each with k0s and k3s — is lab-validated.
See [docs/UPGRADING.md](docs/UPGRADING.md#v010-alpha1--v010-alpha2) for the
per-scenario upgrade impact before rolling existing clusters forward.

### Breaking changes

- **Default credentials removed.** The bootstrap provider no longer seeds the
  node user with `kairos:kairos`, and no longer injects `PasswordAuthentication yes`
  into `sshd_config`. A node now requires an explicit `spec.userPassword`,
  `spec.userPasswordSecretRef`, or SSH key. (#49, KD-3a)
- **`KairosControlPlane.spec.replicas > 1` is rejected** by the validating
  webhook. Alpha-1 silently accepted it and produced N independent single-node
  clusters rather than an HA control plane. HA join logic is still on the
  roadmap; stay on `replicas: 1`. (#48, KD-5a)
- **KCP no longer writes `Cluster.Spec.ControlPlaneEndpoint`.** The endpoint is
  now owned exclusively by CAPI core (copied from the infra cluster), per the
  v1beta2 contract. Clusters that relied on KCP auto-discovering the endpoint
  from Machine IPs must set it on the infrastructure cluster. (#60, KD-12)

### Security

- Eliminated synchronous SSH from the controller hot path. After k0s/k3s
  starts, the **node** POSTs its own workload kubeconfig back to a Secret in the
  management cluster using a short-lived, audience-scoped bearer token; the
  control-plane controller waits for that Secret via a label-filtered watch.
  CAPV is extended to the same node-push pattern as CAPK. (#54, #55, #58, KD-3b/KD-10)
- Removed `ssh.InsecureIgnoreHostKey()`. The new opt-in SSH fallback mandates
  `known_hosts` host-key verification and public-key-only authentication, runs
  off the reconcile path in a bounded worker pool, and never logs key material.
  (#59)
- Closed a shell-injection (RCE) vector in `Hostname` cloud-config rendering and
  routed every shell-context field through the `shquote` filter. (#53, KD-22/KD-28/KD-43)
- Minimized the per-cluster node ServiceAccount RBAC: dropped the dead
  `services:get` grant and scoped `virtualmachineinstances:get` to k0s
  control-plane nodes only (the sole config shape that consumes it). Replaced the
  `infrastructure.cluster.x-k8s.io` wildcard with an explicit kind enumeration
  (including Metal3). (#63, KD-46/KD-6)
- Added `spec.userPasswordSecretRef` so the node password no longer has to live
  inline in the `KairosConfig` spec. (#49, KD-3a)

### Added

- **Metal3 (CAPM3) bare-metal infrastructure support.** Single-node k0s and k3s
  control planes on bare metal via Cluster API Provider Metal3. The bootstrap
  provider recognises the Metal3 infrastructure kind and sets the
  `metal3://<uuid>` providerID from the Ironic config-drive metadata at first
  Node registration (rather than render time). Lab-validated end-to-end on
  Hadron. (#65, ADR 0004)
- Opt-in `spec.sshFallback` on `KairosControlPlane` for air-gapped CAPV/CAPM3
  environments where the VM cannot reach the management API server. Webhook
  defaults and validates the field (mandatory key + known-hosts Secret refs,
  no cross-namespace refs, `activateAfter` bounded above the kubeconfig-ready
  timeout). (#59)
- Validating + defaulting webhook for `KairosControlPlaneTemplate`, mirroring
  the `KairosControlPlane` webhook (including SSH-fallback validation). (#61)
- `spec.files` on `KairosConfig` / `KairosConfigTemplate` is now rendered into
  the node's cloud-config `write_files:` on every distribution and infrastructure
  provider (the field was accepted but silently ignored in alpha-1). Validated
  for absolute paths, no `..` traversal, octal permissions, and `user:group`
  owner. (#65)
- `ManagementEndpoint` type and `ManagementEndpointResolver` interface; the CAPK
  kubeconfig-push path is now resolver-driven and unit-testable without
  envtest. (#54, KD-33)
- Persistent-state-paths configuration injected via cloud-config so node state
  survives reboots. (#52, KD-23)

### Changed

- `Node.Spec.ProviderID` is now set exclusively in-VM: k3s via a kubelet
  `config.yaml.d` drop-in, k0s via a post-bootstrap systemd patch service plus a
  `--kubelet-extra-args=--provider-id` `ExecStartPre` drop-in. The controller no
  longer reaches into the workload cluster to patch it. (#58, #62, KD-3b/KD-3c)
- The release container image is built multi-arch (`linux/amd64` + `linux/arm64`)
  by cross-compiling per `TARGETARCH`, and the release manifest pins
  `imagePullPolicy: IfNotPresent` so the published artifact is reproducible.
  (#65, #67)

### Fixed

- `reconcileDelete` now enumerates and removes child resources (KairosConfigs,
  owned Secrets, InfraMachines) before stripping the finalizer, and latched
  `FailureReason`/`FailureMessage` recover on the success path. (#50, KD-4/KD-14)
- The bootstrap Secret is named deterministically and carries a GC-safe owner
  reference, so unattended Metal3 provisioning no longer races on duplicate
  Secrets. (#65, KD-48)
- Bootstrap cloud-config writes `write_files` entries with a named `owner: root`
  rather than numeric `0`, so musl-based Kairos (Hadron) `yip` resolves the
  owner; k0s providerID self-discovery on CAPV uses `k0s kubectl`, since Hadron
  ships no standalone `kubectl`. (#65)

### Removed

- `ensureProviderIDOnNodes` — the last piece of synchronous workload-cluster I/O
  in `Reconcile`. ProviderID is now owned entirely in-VM (see **Changed**).
  (#58, KD-10)
- The `kairos:kairos` default user and the `PasswordAuthentication yes` sshd
  injection (see **Breaking changes**). (#49, KD-3a)

## [v0.1.0-alpha.1] — 2026-05-18

Initial public alpha. Two providers ship together: the bootstrap provider
(`bootstrap.cluster.x-k8s.io/v1beta2`) and the control-plane provider
(`controlplane.cluster.x-k8s.io/v1beta2`). Single-node k0s and k3s clusters on
CAPD (Docker), CAPV (vSphere), and CAPK (KubeVirt). See the
[release notes](docs/release-notes/v0.1.0-alpha.1.md) for the full feature list
and the alpha-1 security notices.

[v0.1.0]: https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0
[v0.1.0-beta.2]: https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-beta.2
[v0.1.0-beta.1]: https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-beta.1
[v0.1.0-alpha.2]: https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-alpha.2
[v0.1.0-alpha.1]: https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-alpha.1

# Changelog

All notable changes to the Kairos Cluster API provider are documented in this
file. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

This project is pre-1.0. Alpha releases may include breaking changes; those are
called out explicitly under **Breaking changes**. Per-scenario migration steps
live in [docs/UPGRADING.md](docs/UPGRADING.md).

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

[v0.1.0-alpha.2]: https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-alpha.2
[v0.1.0-alpha.1]: https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-alpha.1

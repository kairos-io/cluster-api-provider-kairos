# Changelog

All notable changes to the Kairos Cluster API provider are documented in this
file. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

This project is pre-1.0. Alpha releases may include breaking changes; those are
called out explicitly under **Breaking changes**. Per-scenario migration steps
live in [docs/UPGRADING.md](docs/UPGRADING.md).

## [v0.1.0-alpha.2] — unreleased

> **Release gate.** This version is staged for review but **not yet tagged**.
> It will not be cut until a Kairos (Hadron) OS image that includes and enables
> `open-vm-tools` (`vmtoolsd` present and running) is published — CAPV relies on
> VMware Tools to report guest IP addresses. The entry below is frozen here
> until that image exists.

Alpha-2 resolves the two security notices that shipped with alpha-1: it removes
**synchronous SSH from the controller's reconcile path** (KD-3b) and removes the
**insecure `kairos:kairos` default credentials** (KD-3a). See
[docs/UPGRADING.md](docs/UPGRADING.md#v010-alpha1--v010-alpha2) for the
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
  control-plane nodes only (the sole config shape that consumes it). (#63, KD-46)
- Added `spec.userPasswordSecretRef` so the node password no longer has to live
  inline in the `KairosConfig` spec. (#49, KD-3a)

### Added

- Opt-in `spec.sshFallback` on `KairosControlPlane` for air-gapped CAPV
  environments where the VM cannot reach the management API server. Webhook
  defaults and validates the field (mandatory key + known-hosts Secret refs,
  no cross-namespace refs, `activateAfter` bounded above the kubeconfig-ready
  timeout). (#59)
- Validating + defaulting webhook for `KairosControlPlaneTemplate`, mirroring
  the `KairosControlPlane` webhook (including SSH-fallback validation). (#61)
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

### Fixed

- `reconcileDelete` now enumerates and removes child resources (KairosConfigs,
  owned Secrets, InfraMachines) before stripping the finalizer, and latched
  `FailureReason`/`FailureMessage` recover on the success path. (#50, KD-4/KD-14)

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

[v0.1.0-alpha.2]: https://github.com/kairos-io/cluster-api-provider-kairos/compare/v0.1.0-alpha.1...HEAD
[v0.1.0-alpha.1]: https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-alpha.1

# Kairos CAPI Provider

**Cluster API providers for Kairos OS.**

**Repository:** [github.com/kairos-io/cluster-api-provider-kairos](https://github.com/kairos-io/cluster-api-provider-kairos)

> **Found a bug, or want to request a feature?** Open it on
> [kairos-io/kairos](https://github.com/kairos-io/kairos/issues), including
> issues about this repository. Every Kairos issue lives in one place, so you
> never have to work out which repository to file against.

## Overview

This project provides two Cluster API (CAPI) providers for managing Kubernetes clusters on Kairos:

1. **Bootstrap Provider** (`bootstrap.cluster.x-k8s.io`) — generates Kairos cloud-config bootstrap data.
2. **Control Plane Provider** (`controlplane.cluster.x-k8s.io`) — manages Kairos-based Kubernetes control-plane machines.

## Status

**Latest release**: [`v0.1.2`](https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.2) — pre-1.0; API surface may still change before v1.0.

Supports single-node and highly-available k0s and k3s clusters with CAPD, CAPV, CAPK, and CAPM3 (Metal3 bare metal), plus single-control-plane clusters with the Kairos fleet infrastructure provider (AuroraBoot-claimed nodes; ADR 0008, maintained in the repository's internal decision records). HA control planes (`spec.replicas: 3` or `5`) are supported on CAPK, CAPV, and CAPM3 for both k0s and k3s. CAPK k0s HA additionally requires a Kairos image carrying a k0s start gate, which this project does not publish — see [the CAPK quickstart](docs/QUICKSTART_CAPK.md#k0s-ha-the-image-start-gate). CAPD is dev-only; HA is not exercised on CAPD. Fleet HA is not yet exercised — fleet is single-control-plane only today. `KairosControlPlane.spec.replicas` accepts `1` (single-node), `3`, or `5`; even counts and values above `5` are webhook-rejected. See [High-Availability control planes](#high-availability-control-planes) below. k0s is the fully-supported HA distribution; k3s HA has a known day-2 limitation (KD-5d).

Read the [v0.1.2 release notes](docs/release-notes/v0.1.2.md) before installing, and the [v0.1.0 release notes](docs/release-notes/v0.1.0.md) if you are coming from before v0.1.0. If you run `clusterctl` on your management cluster, the Breaking Changes section is required reading: the `clusterctl` and flat-manifest install paths are mutually exclusive and cannot be swapped in place.

The control plane provider is no longer limited to a fixed provider allowlist: any infrastructure provider that implements the standard CAPI `<Kind>MachineTemplate` contract works through a generic clone path, verified end-to-end against Beskar7 (a bare-metal provider outside the list above). CAPD, CAPV, CAPK, CAPM3, and the Kairos fleet provider remain the first-class path — HA support, worked samples, and a quickstart — because they carry behavior the generic path cannot infer (e.g. KubeVirt's cloud-init volume handling); other CAPI-conformant providers get the generic path with no dedicated sample or quickstart yet.

## Install (released version)

> Requires [cert-manager](https://cert-manager.io) v1.15+ and Cluster API core v1.13.4+ installed on the management cluster.

The provider ships two ways; pick one per management cluster, they are mutually exclusive:

- **clusterctl (recommended)**: register the provider, then run `clusterctl init --bootstrap kairos --control-plane kairos`.
- **Flat manifest**: `kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.2/kairos-capi-provider.yaml`.

See the [install guide](docs/INSTALL.md) for provider registration, the full procedure for both paths, and why they cannot be mixed on one management cluster.

## Credentials

Provide node credentials via `userPasswordSecretRef` (recommended) or `sshPublicKey` / `githubUser`. The validating webhook rejects any `KairosConfig` that specifies no credential. Inline `userPassword` is accepted but discouraged — the value is stored in the resource spec and readable by anyone with access to KairosConfig objects.

## High-Availability control planes

`KairosControlPlane.spec.replicas` accepts `1`, `3`, or `5`. k0s is the fully-supported HA distribution; k3s HA has a known day-2 limitation replacing a control-plane node (KD-5d). See [docs/HIGH_AVAILABILITY.md](docs/HIGH_AVAILABILITY.md) for the per-provider VIP configuration, worked samples, and the etcd health / quorum-safe replacement day-2 behavior.

## Target Versions

| Component | Supported |
| --- | --- |
| Kubernetes (management) | v1.30 - v1.36 (v1.36 recommended) |
| Kubernetes (workload) | v1.30 - v1.36 |
| Cluster API core | v1.13.4 (v1beta2 contract) |
| controller-runtime | v0.23.3 |
| CAPD | v1.8.x+ (dev only) |
| CAPV | v1.11.x+ |
| CAPK | KubeVirt v1.9.x / CAPK v0.1.x |
| CAPM3 | v1.13+; BMO/Ironic v0.13+ |
| kairos-fleet | v0.1.0+ (`cluster-api-provider-kairos-fleet`); v0.1.2+ recommended, since its shipped cluster template could not create workers before that. AuroraBoot — do not use fleet v0.1.0-beta.1, it crash-loops on a real management cluster |
| k0s | ~v1.36.1+k0s |
| k3s | ~v1.36.1+k3s1 |
| Kairos | v4.1.2 (standard and Hadron images) |
| cert-manager | v1.15+ |

Management-cluster support is the full Cluster API v1.13 band (v1.30-v1.36); v1.36 is the version validated in CI/envtest and recommended for new installs, not a hard floor. Workload Kubernetes version is whatever k0s/k3s ships in the Kairos image you deploy: `KairosConfig.spec.kubernetesVersion` / `KairosControlPlane.spec.version` are informational only and do not select or override it (see [API Reference](docs/API_REFERENCE.md)).

## Documentation

- [Install guide](docs/INSTALL.md) — installation paths (clusterctl, released flat artifact, and developer install from source).
- [High-Availability control planes](docs/HIGH_AVAILABILITY.md) — VIP configuration, worked samples, and etcd day-2 behavior.
- [Upgrade guide](docs/UPGRADING.md) — upgrade procedures and breaking-change migration steps.
- [API Reference](docs/API_REFERENCE.md) — CRD reference.
- [Testing](docs/TESTING.md) — how to run tests.

### Quickstarts

- [CAPD (Docker)](docs/QUICKSTART_CAPD.md)
- [CAPV (vSphere)](docs/QUICKSTART_CAPV.md)
- [CAPK (KubeVirt)](docs/QUICKSTART_CAPK.md)
- [Metal3 (bare metal)](docs/QUICKSTART_CAPM3.md)
- [Fleet (AuroraBoot-claimed nodes)](docs/QUICKSTART_FLEET.md)

## Development

```bash
git clone https://github.com/kairos-io/cluster-api-provider-kairos.git
cd cluster-api-provider-kairos
make test               # unit tests
make test-envtest       # integration tests (envtest)
make test-kubevirt      # end-to-end (requires Docker + kubevirt-env setup)
```

See [docs/INSTALL.md](docs/INSTALL.md) for the developer install path (`make deploy` from source).

## License

Apache-2.0

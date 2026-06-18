# Kairos CAPI Provider

**Cluster API providers for Kairos OS.**

**Repository:** [github.com/kairos-io/cluster-api-provider-kairos](https://github.com/kairos-io/cluster-api-provider-kairos)

## Overview

This project provides two Cluster API (CAPI) providers for managing Kubernetes clusters on Kairos:

1. **Bootstrap Provider** (`bootstrap.cluster.x-k8s.io`) — generates Kairos cloud-config bootstrap data.
2. **Control Plane Provider** (`controlplane.cluster.x-k8s.io`) — manages Kairos-based Kubernetes control-plane machines.

## Status

**Latest release**: [`v0.1.0-alpha.2`](https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-alpha.2) — pre-release; API surface may change before v0.1.0.

Supports single-node k0s and k3s clusters with CAPD, CAPV, CAPK, and CAPM3 (Metal3 bare metal). The alpha-2 e2e matrix is GREEN on all four CAPV/CAPM3 × k0s/k3s combinations with Kairos Hadron. `spec.replicas > 1` is currently webhook-rejected; HA control planes are on the roadmap (KD-5b / KD-25).

Read the [v0.1.0-alpha.2 release notes](docs/release-notes/v0.1.0-alpha.2.md) before installing — there are breaking changes, security hardening requirements, and known limitations that affect all operators upgrading from alpha.1. Additional infrastructure providers (Tinkerbell, hyperscalers) are on the roadmap.

## Install (released version)

> Requires [cert-manager](https://cert-manager.io) v1.15+ and Cluster API core v1.9+ installed on the management cluster. See the [install guide](docs/INSTALL.md) for the full prerequisite list.

```bash
kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0-alpha.2/kairos-capi-provider.yaml
```

The provider is distributed as a flat manifest installable via `kubectl apply -f`. [clusterctl](https://cluster-api.sigs.k8s.io/clusterctl/overview) integration is planned for a future release.

## Credentials

Provide node credentials via `userPasswordSecretRef` (recommended) or `sshPublicKey` / `githubUser`. The validating webhook rejects any `KairosConfig` that specifies no credential. Inline `userPassword` is accepted but discouraged — the value is stored in the resource spec and readable by anyone with access to KairosConfig objects.

## Target Versions

| Component | Supported |
| --- | --- |
| Kubernetes (management) | v1.30+ |
| Kubernetes (workload) | bundled in your Kairos image (k3s/k0s), typically v1.30+ |
| Cluster API core | v1.9+ required (v1beta2 wire contract); lab-validated against v1.12.x |
| CAPD | v1.8.x+ (dev only) |
| CAPV | v1.11.x (lab-validated) |
| CAPK | KubeVirt v1.8.2 / CAPK v0.1.x |
| CAPM3 | v1.13+; BMO/Ironic v0.13+ (lab-validated) |
| Kairos | v3.6.0+; Hadron validated on all four CAPV/CAPM3 × k0s/k3s |
| Distributions | k0s, k3s |
| cert-manager | v1.15+ |

## Documentation

- [Install guide](docs/INSTALL.md) — installation paths (released artifact + developer install from source).
- [Upgrade guide](docs/UPGRADING.md) — upgrade procedures and breaking-change migration steps.
- [API Reference](docs/API_REFERENCE.md) — CRD reference.
- [Testing](docs/TESTING.md) — how to run tests.

### Quickstarts

- [CAPD (Docker)](docs/QUICKSTART_CAPD.md)
- [CAPV (vSphere)](docs/QUICKSTART_CAPV.md)
- [CAPK (KubeVirt)](docs/QUICKSTART_CAPK.md)
- [Metal3 (bare metal)](docs/QUICKSTART_CAPM3.md)

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

# Kairos CAPI Provider

**Cluster API providers for Kairos OS.**

**Repository:** [github.com/kairos-io/cluster-api-provider-kairos](https://github.com/kairos-io/cluster-api-provider-kairos)

## Overview

This project provides two Cluster API (CAPI) providers for managing Kubernetes clusters on Kairos:

1. **Bootstrap Provider** (`bootstrap.cluster.x-k8s.io`) — generates Kairos cloud-config bootstrap data.
2. **Control Plane Provider** (`controlplane.cluster.x-k8s.io`) — manages Kairos-based Kubernetes control-plane machines.

## Status

**Latest release**: [`v0.1.0-alpha.1`](https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-alpha.1) — initial alpha. Pre-release; API surface may change before v0.1.0.

Supports single-node k0s and k3s clusters with CAPD, CAPV, and CAPK. `spec.replicas > 1` is currently webhook-rejected; HA control planes are on the roadmap (KD-5b / KD-25). Additional infrastructure providers (Metal3, Tinkerbell, hyperscalers) are also on the roadmap.

- v0.1.0-alpha.2 ships KD-3b — SSH no longer in the controller's hot path. SSH credentials in `KairosConfig` are only consumed by the planned `SSHFallback` opt-in mechanism (post-alpha-2). See [docs/UPGRADING.md](docs/UPGRADING.md) for the alpha-1 → alpha-2 impact.

Read the [v0.1.0-alpha.1 release notes](docs/release-notes/v0.1.0-alpha.1.md) before using — there are important security caveats and known limitations.

## Install (released version)

> Requires [cert-manager](https://cert-manager.io) and Cluster API core installed on the management cluster. See the [release notes](docs/release-notes/v0.1.0-alpha.1.md#install) for the full prerequisite list.

```bash
kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0-alpha.1/kairos-capi-provider.yaml
```

The provider is distributed as a flat manifest installable via `kubectl apply -f`. [clusterctl](https://cluster-api.sigs.k8s.io/clusterctl/overview) integration is planned for a future release.

## Credentials

Provide node credentials via `userPasswordSecretRef` (recommended) or `sshPublicKey` / `githubUser`. The validating webhook rejects any `KairosConfig` that specifies no credential. Inline `userPassword` is accepted but discouraged — the value is stored in the resource spec and readable by anyone with access to KairosConfig objects.

## Target versions

| Component | Supported |
| --- | --- |
| Kubernetes (workload) | bundled in your Kairos image (k3s/k0s, typically v1.30+) |
| Kubernetes (management) | v1.30+ |
| Cluster API | v1.8.x (v1.11.x tracking is on the roadmap, KD-13) |
| Kairos | v3.6.0+ |
| Distributions | k0s, k3s |

## Documentation

- [Install guide](docs/INSTALL.md) — installation paths (released artifact + developer install from source).
- [API Reference](docs/API_REFERENCE.md) — CRD reference.
- [Testing](docs/TESTING.md) — how to run tests.

### Quickstarts

- [CAPD (Docker)](docs/QUICKSTART_CAPD.md)
- [CAPV (vSphere)](docs/QUICKSTART_CAPV.md)
- [CAPK (KubeVirt)](docs/QUICKSTART_CAPK.md)

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

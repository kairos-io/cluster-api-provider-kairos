# Kairos CAPI Provider

**Cluster API providers for Kairos OS. This project is under active development.**

## Overview

This project provides two Cluster API (CAPI) providers for managing Kubernetes clusters on Kairos:

1. **Bootstrap Provider** (`bootstrap.cluster.x-k8s.io`) - Generates Kairos cloud-config bootstrap data
2. **Control Plane Provider** (`controlplane.cluster.x-k8s.io`) - Manages Kairos-based Kubernetes control plane machines

## Status

**Early development** - MVP implementation in progress. Supports single-node k0s and k3s clusters with CAPD, CAPV, and CAPK.

## Target Versions

- **Kubernetes**: v1.34+
- **Cluster API**: v1.11+ (v1beta2 contracts)
- **Kairos**: v3.6.0+
- **Distributions**: k0s, k3s

## Documentation

- [Install guide](docs/INSTALL.md) - Development install using make
- [Design](docs/DESIGN.md) - Architecture and design
- [API Reference](docs/API_REFERENCE.md) - CRD reference
- [Testing](docs/TESTING.md) - How to run tests

### Quickstarts

- [CAPD (Docker)](docs/QUICKSTART_CAPD.md)
- [CAPV (vSphere)](docs/QUICKSTART_CAPV.md)
- [CAPK (KubeVirt)](docs/QUICKSTART_CAPK.md)

## License

Apache-2.0

# Kairos CAPI Provider

**Cluster API providers for Kairos OS.**

**Repository:** [github.com/kairos-io/cluster-api-provider-kairos](https://github.com/kairos-io/cluster-api-provider-kairos)

## Overview

This project provides two Cluster API (CAPI) providers for managing Kubernetes clusters on Kairos:

1. **Bootstrap Provider** (`bootstrap.cluster.x-k8s.io`) — generates Kairos cloud-config bootstrap data.
2. **Control Plane Provider** (`controlplane.cluster.x-k8s.io`) — manages Kairos-based Kubernetes control-plane machines.

## Status

**Latest release**: [`v0.1.0-beta.2`](https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-beta.2) — pre-1.0; API surface may still change before v1.0.

Supports single-node and highly-available k0s and k3s clusters with CAPD, CAPV, CAPK, and CAPM3 (Metal3 bare metal). HA control planes (`spec.replicas: 3` or `5`) are supported on CAPK, CAPV, and CAPM3 for both k0s and k3s. CAPD is dev-only; HA is not exercised on CAPD. `KairosControlPlane.spec.replicas` accepts `1` (single-node), `3`, or `5`; even counts and values above `5` are webhook-rejected. See [High-Availability control planes](#high-availability-control-planes) below. k0s is the fully-supported HA distribution; k3s HA has a known day-2 limitation (KD-5d).

Read the [v0.1.0-beta.2 release notes](docs/release-notes/v0.1.0-beta.2.md) before installing. This release adds a second, `clusterctl`-native install path — if you run `clusterctl` on your management cluster, the Breaking Changes section is required reading: the `clusterctl` and flat-manifest paths are mutually exclusive and cannot be swapped in place. Additional infrastructure providers (Tinkerbell, hyperscalers) are on the roadmap.

## Install (released version)

> Requires [cert-manager](https://cert-manager.io) v1.15+ and Cluster API core v1.13.4+ installed on the management cluster. See the [install guide](docs/INSTALL.md) for the full prerequisite list.

The provider ships two ways. Pick one per management cluster: they are mutually exclusive, see below.

### Path 1 - clusterctl (recommended)

Per ADR 0007, this provider packages as two `clusterctl` providers built from one image: a bootstrap provider and a control-plane provider. It is not yet in the upstream `clusterctl` provider list, so add it explicitly to your `clusterctl` configuration first:

```yaml
# ~/.cluster-api/clusterctl.yaml (or pass --config <path> to clusterctl)
providers:
  - name: "kairos"
    url: "https://github.com/kairos-io/cluster-api-provider-kairos/releases/latest/download/bootstrap-components.yaml"
    type: "BootstrapProvider"
  - name: "kairos"
    url: "https://github.com/kairos-io/cluster-api-provider-kairos/releases/latest/download/control-plane-components.yaml"
    type: "ControlPlaneProvider"
```

```bash
clusterctl init --bootstrap kairos --control-plane kairos
```

`clusterctl init` installs cert-manager and Cluster API core automatically if they are not already present. This installs the bootstrap provider (label `bootstrap-kairos`) in namespace `capi-kairos-bootstrap-system` and the control-plane provider (label `control-plane-kairos`) in namespace `capi-kairos-control-plane-system`. See the [install guide](docs/INSTALL.md) for the full procedure, including combining this with `--infrastructure <provider>` in one call.

### Path 2 - flat manifest (`kubectl apply`)

```bash
kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0-beta.2/kairos-capi-provider.yaml
```

This applies an all-in-one manifest: both providers run as one Deployment in namespace `kairos-capi-system`, labeled `cluster.x-k8s.io/provider: kairos`. It is not a `clusterctl` artifact; it is kept as a `kubectl apply` convenience alongside the `clusterctl` path through the beta series.

### The two paths are mutually exclusive on one management cluster

They use different namespaces and different `cluster.x-k8s.io/provider` label values. Applying one path after the other does not upgrade or replace it: it runs a second, independent set of controllers and webhooks watching the same CRDs, which is unsupported. Pick one path per management cluster. See [docs/UPGRADING.md](docs/UPGRADING.md) if you need to move an existing installation from one path to the other.

## Credentials

Provide node credentials via `userPasswordSecretRef` (recommended) or `sshPublicKey` / `githubUser`. The validating webhook rejects any `KairosConfig` that specifies no credential. Inline `userPassword` is accepted but discouraged — the value is stored in the resource spec and readable by anyone with access to KairosConfig objects.

## High-Availability control planes

`KairosControlPlane.spec.replicas` accepts `1`, `3`, or `5`. `1` is single-node. `3` or `5` configure a multi-node control plane with etcd quorum (the webhook rejects even counts and values above `5`, since they add quorum cost without additional fault tolerance).

HA control planes need a stable endpoint that survives the loss of any one node. The mechanism depends on the infrastructure provider:

- **CAPV and CAPM3**: configure a kube-vip virtual IP via `spec.ha.vip`. `Cluster.spec.controlPlaneEndpoint.host` must equal `spec.ha.vip.address` — CAPI core copies the InfraCluster's endpoint into `Cluster.spec.controlPlaneEndpoint`, and every node and kubeconfig targets that value.
- **CAPK**: do not set `spec.ha.vip`. CAPK provisions its own LoadBalancer Service and reflects its IP into the control-plane endpoint; a kube-vip VIP alongside it would produce a conflicting ARP announcement.
- **CAPD**: dev-only; HA is not exercised.

```yaml
spec:
  replicas: 3
  ha:
    vip:
      address: "192.168.1.50"   # must equal Cluster.spec.controlPlaneEndpoint.host
      interface: "ens192"       # NIC name present on the control-plane nodes
      mode: ARP                 # ARP (default, flat L2 subnet) or BGP (routed fabric)
```

Where `spec.ha.vip` applies, the controller renders a kube-vip DaemonSet into each control-plane node's bootstrap cloud-config; one Pod holds leader election and advertises the VIP, failing over to a surviving node if the leader is lost.

Worked samples:

- [`config/samples/capv/kairos_cluster_k0s_ha.yaml`](config/samples/capv/kairos_cluster_k0s_ha.yaml) / [`kairos_cluster_k3s_ha.yaml`](config/samples/capv/kairos_cluster_k3s_ha.yaml)
- [`config/samples/capk/kubevirt_cluster_k0s_ha.yaml`](config/samples/capk/kubevirt_cluster_k0s_ha.yaml) / [`kubevirt_cluster_k3s_ha.yaml`](config/samples/capk/kubevirt_cluster_k3s_ha.yaml)
- [`config/samples/capm3/kairos_cluster_k0s_ha.yaml`](config/samples/capm3/kairos_cluster_k0s_ha.yaml) / [`kairos_cluster_k3s_ha.yaml`](config/samples/capm3/kairos_cluster_k3s_ha.yaml)

See the [CAPV HA quickstart walkthrough](docs/QUICKSTART_CAPV.md#high-availability-3-node-k0s-control-plane) for the full procedure.

### Day-2: etcd health and quorum-safe replacement

Each control-plane node reports its own etcd member health as the `EtcdHealthy` condition on `KairosControlPlane`: `True` when every voting member is healthy, `False(Info)` when quorum holds but a member is degraded, `False(Warning)` at or below the `(N/2)+1` quorum minimum.

Rollouts and scale-downs are quorum-safe — the controller refuses a control-plane Machine delete that would drop etcd below `(N/2)+1` healthy voting members.

On k0s, the departing node runs `k0s etcd leave` before the Machine is deleted; a CAPI pre-terminate hook blocks termination until it acks, so no member is left orphaned. **k3s has no supported clean member-remove (KD-5d):** replacing a k3s control-plane node leaves an orphaned etcd member requiring manual `etcdctl member remove` (the controller emits `EtcdMemberRemoveUnsupportedForK3s`). k0s is the fully-supported HA distribution.

A k0s node that never acks its leave within ~5 minutes is deleted anyway (the delete was already proven quorum-safe); watch for `EtcdMemberLeaveTimedOut` and remove the member manually if it fires.

**KD-51:** etcd health/leave signals are node-self-reported over vanilla RBAC, so a compromised control-plane node can forge them. Not a privilege escalation (a compromised node already has cluster-admin-equivalent access) and it cannot force an unsafe deletion — quorum-safety is decided independently of any node signal. A forged signal can only self-downgrade the clean-leave/health guarantee.

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
| k0s | ~v1.36.1+k0s |
| k3s | ~v1.36.1+k3s1 |
| Kairos | v4.1.2 (standard and Hadron images) |
| cert-manager | v1.15+ |

Management-cluster support is the full Cluster API v1.13 band (v1.30-v1.36); v1.36 is the version validated in CI/envtest and recommended for new installs, not a hard floor. Workload Kubernetes version is whatever k0s/k3s ships in the Kairos image you deploy: `KairosConfig.spec.kubernetesVersion` / `KairosControlPlane.spec.version` are informational only and do not select or override it (see [API Reference](docs/API_REFERENCE.md)).

## Documentation

- [Install guide](docs/INSTALL.md) — installation paths (clusterctl, released flat artifact, and developer install from source).
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

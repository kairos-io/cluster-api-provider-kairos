# Kairos CAPI Provider

**Cluster API providers for Kairos OS.**

**Repository:** [github.com/kairos-io/cluster-api-provider-kairos](https://github.com/kairos-io/cluster-api-provider-kairos)

## Overview

This project provides two Cluster API (CAPI) providers for managing Kubernetes clusters on Kairos:

1. **Bootstrap Provider** (`bootstrap.cluster.x-k8s.io`) — generates Kairos cloud-config bootstrap data.
2. **Control Plane Provider** (`controlplane.cluster.x-k8s.io`) — manages Kairos-based Kubernetes control-plane machines.

## Status

**Latest release**: [`v0.1.0-alpha.2`](https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-alpha.2) — pre-release; API surface may change before v0.1.0. Highly-available control planes (below) ship in the next release, `v0.1.0-alpha.3` (provisional, unreleased at time of writing) — see [its release notes](docs/release-notes/v0.1.0-alpha.3.md) for status.

Supports single-node and highly-available k0s and k3s clusters with CAPD, CAPV, CAPK, and CAPM3 (Metal3 bare metal). The alpha-2 e2e matrix is GREEN on all four CAPV/CAPM3 × k0s/k3s combinations with Kairos Hadron. `KairosControlPlane.spec.replicas` accepts `1` (single-node), `3`, or `5` (HA); even counts and values above `5` are webhook-rejected. See [High-Availability control planes](#high-availability-control-planes) below. k0s is the fully-supported HA distribution; k3s HA has a known day-2 limitation (KD-5d).

Read the [v0.1.0-alpha.2 release notes](docs/release-notes/v0.1.0-alpha.2.md) before installing — there are breaking changes, security hardening requirements, and known limitations that affect all operators upgrading from alpha.1. Additional infrastructure providers (Tinkerbell, hyperscalers) are on the roadmap.

## Install (released version)

> Requires [cert-manager](https://cert-manager.io) v1.15+ and Cluster API core v1.9+ installed on the management cluster. See the [install guide](docs/INSTALL.md) for the full prerequisite list.

```bash
kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0-alpha.2/kairos-capi-provider.yaml
```

The provider is distributed as a flat manifest installable via `kubectl apply -f`. [clusterctl](https://cluster-api.sigs.k8s.io/clusterctl/overview) integration is planned for a future release.

## Credentials

Provide node credentials via `userPasswordSecretRef` (recommended) or `sshPublicKey` / `githubUser`. The validating webhook rejects any `KairosConfig` that specifies no credential. Inline `userPassword` is accepted but discouraged — the value is stored in the resource spec and readable by anyone with access to KairosConfig objects.

## High-Availability control planes

`KairosControlPlane.spec.replicas` accepts `1`, `3`, or `5`. `1` is single-node. `3` or `5` configure a multi-node control plane with etcd quorum (the webhook rejects even counts and values above `5`, since they add quorum cost without additional fault tolerance).

HA control planes need a stable endpoint that survives the loss of any one node. For CAPV, CAPM3, and CAPD, configure a kube-vip virtual IP:

```yaml
spec:
  replicas: 3
  ha:
    vip:
      address: "192.168.1.50"   # must equal Cluster.spec.controlPlaneEndpoint.host
      interface: "ens192"       # NIC name present on the control-plane nodes
      mode: ARP                 # ARP (default, flat L2 subnet) or BGP (routed fabric)
```

The controller renders a kube-vip DaemonSet into each control-plane node's bootstrap cloud-config; one Pod holds leader election and advertises the VIP, and it fails over to a surviving node if the leader is lost. `Cluster.spec.controlPlaneEndpoint.host` must equal `spec.ha.vip.address` — CAPI core copies the InfraCluster's endpoint into `Cluster.spec.controlPlaneEndpoint`, and every node and kubeconfig targets that value.

**CAPK is the exception:** do not set `spec.ha.vip` for CAPK clusters. CAPK provisions its own LoadBalancer Service and reflects its IP into the control-plane endpoint; a kube-vip VIP alongside it produces a conflicting ARP announcement.

See the worked sample: [`config/samples/capv/kairos_cluster_k0s_ha.yaml`](config/samples/capv/kairos_cluster_k0s_ha.yaml) and the [CAPV HA quickstart walkthrough](docs/QUICKSTART_CAPV.md#high-availability-3-node-k0s-control-plane).

### Day-2: etcd health and quorum-safe replacement

Each control-plane node reports its own etcd member health; the controller surfaces this as the `EtcdHealthy` condition on `KairosControlPlane` (`True` when every desired voting member is healthy, `False(Info)` when quorum holds but a member is degraded, `False(Warning)` at or below the `(N/2)+1` quorum minimum).

Rollouts and scale-downs are quorum-safe: the controller refuses to delete a control-plane Machine if doing so would drop etcd below `(N/2)+1` healthy voting members.

On a k0s control-plane removal, the node runs `k0s etcd leave` cleanly before the Machine is deleted — a CAPI pre-terminate hook pauses termination until the node acks the leave, so no etcd member is left orphaned. **k3s has no supported clean member-remove for its embedded etcd (KD-5d):** replacing a k3s control-plane node leaves an orphaned etcd member that requires manual `etcdctl member remove`; the controller emits an `EtcdMemberRemoveUnsupportedForK3s` warning event when this happens. **k0s is the fully-supported HA distribution; k3s HA works for bring-up but needs manual cleanup on every control-plane replacement.**

A stuck or unreachable k0s node that never acknowledges its leave request within roughly 5 minutes is deleted anyway (the quorum-safety check already proved the delete is safe); its etcd member may remain registered. Watch for the `EtcdMemberLeaveTimedOut` warning event and remove the member manually if it fires.

**Known limitation (KD-51):** node-reported etcd health and leave acknowledgements are written by control-plane nodes using vanilla RBAC on objects shared across the control plane, so a compromised control-plane node can forge them. This is not a privilege escalation — a compromised control-plane node already holds cluster-admin-equivalent access — and it cannot force an unsafe deletion, since the quorum-safety decision is made independently before any node signal is consulted. A forged signal can only self-downgrade the clean-leave/health guarantee (e.g., cause an early or missed clean-leave). Per-member-scoped signals are tracked as future hardening.

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

# High-Availability Control Planes

Last verified against: provider v0.1.0, CAPI v1.13.4.

This page covers `KairosControlPlane` HA configuration (`spec.replicas` and
`spec.ha.vip`) and the day-2 etcd health and quorum-safe replacement
behavior. For installation, see [docs/INSTALL.md](INSTALL.md); for supported
component versions, see [README.md — Target Versions](../README.md#target-versions).

`KairosControlPlane.spec.replicas` accepts `1`, `3`, or `5`. `1` is single-node. `3` or `5` configure a multi-node control plane with etcd quorum (the webhook rejects even counts and values above `5`, since they add quorum cost without additional fault tolerance).

HA control planes need a stable endpoint that survives the loss of any one node. The mechanism depends on the infrastructure provider:

- **CAPV and CAPM3**: configure a kube-vip virtual IP via `spec.ha.vip`. `Cluster.spec.controlPlaneEndpoint.host` must equal `spec.ha.vip.address` — CAPI core copies the InfraCluster's endpoint into `Cluster.spec.controlPlaneEndpoint`, and every node and kubeconfig targets that value.
- **CAPK**: do not set `spec.ha.vip`. CAPK provisions its own LoadBalancer Service and reflects its IP into the control-plane endpoint; a kube-vip VIP alongside it would produce a conflicting ARP announcement. CAPK k0s HA has one further requirement the provider cannot supply — the Kairos image must carry a k0s start gate, or the control plane deadlocks below quorum. See [QUICKSTART_CAPK.md — k0s HA: the image start gate](QUICKSTART_CAPK.md#k0s-ha-the-image-start-gate).
- **Fleet (Kairos fleet / AuroraBoot)**: the control-plane endpoint is operator-supplied on `KairosFleetCluster.spec.controlPlaneEndpoint` (a kube-vip VIP, load balancer, or DNS name you manage) — the provider allocates and discovers nothing. **HA (`replicas: 3`/`5`) is not yet exercised on fleet: single control-plane only today.** No fleet HA sample is shipped (ADR 0008). See [docs/QUICKSTART_FLEET.md](QUICKSTART_FLEET.md).
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

- [`config/samples/capv/kairos_cluster_k0s_ha.yaml`](../config/samples/capv/kairos_cluster_k0s_ha.yaml) / [`kairos_cluster_k3s_ha.yaml`](../config/samples/capv/kairos_cluster_k3s_ha.yaml)
- [`config/samples/capk/kubevirt_cluster_k0s_ha.yaml`](../config/samples/capk/kubevirt_cluster_k0s_ha.yaml) / [`kubevirt_cluster_k3s_ha.yaml`](../config/samples/capk/kubevirt_cluster_k3s_ha.yaml)
- [`config/samples/capm3/kairos_cluster_k0s_ha.yaml`](../config/samples/capm3/kairos_cluster_k0s_ha.yaml) / [`kairos_cluster_k3s_ha.yaml`](../config/samples/capm3/kairos_cluster_k3s_ha.yaml)

See the [CAPV HA quickstart walkthrough](QUICKSTART_CAPV.md#high-availability-3-node-k0s-control-plane) for the full procedure.

## Day-2: etcd health and quorum-safe replacement

Each control-plane node reports its own etcd member health as the `EtcdHealthy` condition on `KairosControlPlane`: `True` when every voting member is healthy, `False(Info)` when quorum holds but a member is degraded, `False(Warning)` at or below the `(N/2)+1` quorum minimum.

Rollouts and scale-downs are quorum-safe — the controller refuses a control-plane Machine delete that would drop etcd below `(N/2)+1` healthy voting members.

On k0s, the departing node runs `k0s etcd leave` before the Machine is deleted; a CAPI pre-terminate hook blocks termination until it acks, so no member is left orphaned. **k3s has no supported clean member-remove (KD-5d):** replacing a k3s control-plane node leaves an orphaned etcd member requiring manual `etcdctl member remove` (the controller emits `EtcdMemberRemoveUnsupportedForK3s`). k0s is the fully-supported HA distribution.

A k0s node that never acks its leave within ~5 minutes is deleted anyway (the delete was already proven quorum-safe); watch for `EtcdMemberLeaveTimedOut` and remove the member manually if it fires.

**KD-51:** etcd health/leave signals are node-self-reported over vanilla RBAC, so a compromised control-plane node can forge them. Not a privilege escalation (a compromised node already has cluster-admin-equivalent access) and it cannot force an unsafe deletion — quorum-safety is decided independently of any node signal. A forged signal can only self-downgrade the clean-leave/health guarantee.

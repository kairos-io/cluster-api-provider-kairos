# High-Availability Control Planes

Last verified against: provider v0.1.3, CAPI v1.13.4.

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

The kube-vip Pod talks to its own node's API server on `127.0.0.1:6443`, so a member holds the VIP only while its own API server works. A custom API server port, or a bind address that excludes loopback, leaves the VIP down.

Worked samples:

- [`config/samples/capv/kairos_cluster_k0s_ha.yaml`](../config/samples/capv/kairos_cluster_k0s_ha.yaml) / [`kairos_cluster_k3s_ha.yaml`](../config/samples/capv/kairos_cluster_k3s_ha.yaml)
- [`config/samples/capk/kubevirt_cluster_k0s_ha.yaml`](../config/samples/capk/kubevirt_cluster_k0s_ha.yaml) / [`kubevirt_cluster_k3s_ha.yaml`](../config/samples/capk/kubevirt_cluster_k3s_ha.yaml)
- [`config/samples/capm3/kairos_cluster_k0s_ha.yaml`](../config/samples/capm3/kairos_cluster_k0s_ha.yaml) / [`kairos_cluster_k3s_ha.yaml`](../config/samples/capm3/kairos_cluster_k3s_ha.yaml)

See the [CAPV HA quickstart walkthrough](QUICKSTART_CAPV.md#high-availability-3-node-k0s-control-plane) for the full procedure.

## Known issue: k0s HA rollouts on v0.1.4

Affects k0s HA control planes on every infrastructure provider except CAPK
(CAPV, CAPM3, the Kairos fleet provider, and the generic path), in v0.1.4 and
earlier. k3s and CAPK are not affected.

In these releases only the first control-plane node puts the control-plane
endpoint in its API server certificate. The nodes that join after it do not.
Creating the cluster works. But when the first node is removed, for example during a rollout after a `spec.version` change,
the VIP moves to a joiner, and every client that checks the certificate
against the endpoint fails. That includes the management cluster. The rollout
then stops at the first node, with a `KairosControlPlane` message such as:

```text
failed to reconcile etcd member leave for <cluster>-cp-0: ... tls: failed to verify certificate: x509: certificate is valid for ..., not <endpoint>
```

The fix renders the endpoint into every control-plane member's certificate,
and ships in the next release. Until then, avoid changes that replace the first
control-plane node of an affected cluster.

A second issue affects the same k0s clusters, CAPK included. The token that new
control-plane members join with is created once, by the first control-plane
node. It points at that node's own address and expires 24 hours after it was
created. A member created after the first node has been removed, or more than
24 hours after the cluster was created, therefore cannot join. Its Machine
stays `Provisioned` without a Node, and the `KairosControlPlane` keeps waiting
for it to join. This affects rollouts, remediation and scale-up. A fix that
has the control plane issue a fresh join token for each new member is being
prepared for the next release. A member stuck this way does not recover on its
own: after upgrading to that release, delete its Machine so that a replacement
is created with a fresh token. The stuck Machine has no Node yet, so deleting
it does not touch etcd.

## Scale-up: joiners are created one at a time

Bringing up a 3 or 5-replica control plane provisions the init node first, then
each joiner in turn — never two at once. This is deliberate. Every joiner runs
`etcd member add` as it boots, so two adds against a one-member cluster raise
the configured voter count to three while a single member is still serving:
quorum `(N/2)+1` is unmet, etcd stops serving, and on k0s the join API, which
needs the apiserver to validate a join token, hangs. Neither joiner completes
and the cluster does not recover on its own.

The gate is strict for k0s: the next joiner is not created until the previous
one has registered a Node and reported a healthy voting etcd member. For k3s it
is advisory — k3s joins as a learner, which does not count toward quorum and so
cannot strand the cluster — and only an explicitly unhealthy member holds the
next joiner back.

Two practical consequences:

- Scale-up takes longer than provisioning the same machines in parallel would.
  That is the cost of not being able to lose the cluster to a race.
- A joiner that never comes up stalls the rest of the scale-up rather than
  taking the cluster down with it. The `KairosControlPlane` says which machine
  it is waiting on and why, so check its conditions before assuming the
  rollout is wedged.

The same gate applies to the surge machine created during a rolling update.
A rolling update replaces one member at a time: the surge machine has to
register a Node before an outdated member is removed, and the rollout waits
while any control-plane machine is still being deleted. Progress is reported on
the `MachinesUpToDate` condition.

## Day-2: etcd health and quorum-safe replacement

Each control-plane node reports its own etcd member health as the `EtcdHealthy` condition on `KairosControlPlane`: `True` when every voting member is healthy, `False(Info)` when quorum holds but a member is degraded, `False(Warning)` at or below the `(N/2)+1` quorum minimum.

Rollouts and scale-downs are quorum-safe — the controller refuses a control-plane Machine delete that would drop etcd below `(N/2)+1` healthy voting members.

On k0s, the departing node runs `k0s etcd leave` before the Machine is deleted; a CAPI pre-terminate hook blocks termination until it acks, so no member is left orphaned. **k3s has no supported clean member-remove (KD-5d):** replacing a k3s control-plane node leaves an orphaned etcd member requiring manual `etcdctl member remove` (the controller emits `EtcdMemberRemoveUnsupportedForK3s`). k0s is the fully-supported HA distribution.

A k0s node that never acks its leave within ~5 minutes is deleted anyway (the delete was already proven quorum-safe); watch for `EtcdMemberLeaveTimedOut` and remove the member manually if it fires.

**KD-51:** etcd health/leave signals are node-self-reported over vanilla RBAC, so a compromised control-plane node can forge them. Not a privilege escalation (a compromised node already has cluster-admin-equivalent access) and it cannot force an unsafe deletion — quorum-safety is decided independently of any node signal. A forged signal can only self-downgrade the clean-leave/health guarantee.

## Day-2: workload cluster identity and the freeze

An HA control plane pins the identity of the workload cluster it manages, and freezes control-plane membership changes if a workload API server ever answers as a different cluster (ADR 0015). The identity is the UID of the workload `kube-system` Namespace, stored on the `KairosControlPlane` in the `controlplane.cluster.x-k8s.io/workload-cluster-uid` annotation. The full rules (when it is pinned, what is checked, the annotations) are in [API_REFERENCE.md — Workload cluster identity](API_REFERENCE.md#workload-cluster-identity).

Why it exists: on k0s, a joining member whose join failed half-way can start a new etcd cluster with the certificate authority it received while joining. The new cluster's API server is trusted by the same CA, kube-vip there can take the VIP, and a controller that cannot tell the two clusters apart then removes the original members as "gone". The freeze turns that into a stop that waits for a person.

While frozen:

- `Ready`, `Available` and `EtcdHealthy` are `False` (Error) with reason `WorkloadClusterIdentityChanged`, and a Warning event `WorkloadClusterIdentityChanged` names the pinned UID, the UID read, and where it was read from;
- the controller creates and deletes no control-plane Machine and removes no etcd-leave hook, so a k0s Machine deleted by you or a MachineHealthCheck stays at its pre-terminate hook, its VM and etcd data intact;
- the controller writes nothing to the workload cluster (no etcd-leave request, no join token);
- deleting the whole cluster still works.

The freeze is sticky on purpose: it never clears by itself, because the two clusters can keep answering in turn. A wrong edit to the pin annotation also freezes the control plane.

### Runbook: `WorkloadClusterIdentityChanged`

1. **Read the freeze.** The mismatch annotation says what was seen, where, and against which pin:
   ```bash
   kubectl get kairoscontrolplane <name> -n <namespace> -o jsonpath='{.metadata.annotations}'
   kubectl describe kairoscontrolplane <name> -n <namespace>   # the Warning event
   ```
2. **Read the identity through every control-plane member, not only through the endpoint.** Two clusters elect two VIP holders, so the endpoint alone proves nothing. With the workload kubeconfig, address each member's API server by the IP its Machine reports (`kubectl get machines -n <namespace> -l cluster.x-k8s.io/control-plane -o yaml`, `status.addresses`); TLS is still verified against the cluster CA:
   ```bash
   clusterctl get kubeconfig <cluster> -n <namespace> > workload.kubeconfig
   for ip in <member-ip> ...; do
     printf '%s ' "$ip"
     kubectl --kubeconfig workload.kubeconfig --server "https://$ip:6443" \
       get namespace kube-system -o jsonpath='{.metadata.uid}{"\n"}'
   done
   ```
   Where member addresses are not in the API server certificate (CAPK reports pod IPs), run the same read on each node: `k0s kubectl get namespace kube-system -o jsonpath='{.metadata.uid}'` (k3s: `k3s kubectl`). On k0s, also run `k0s etcd member-list` on each node: a member that started its own cluster lists only itself.
3. **Decide which cluster to keep.**
   - **One or a few members (usually the newest joiner) answer with a different UID; the rest answer with the pinned one.** Those members serve a cluster of their own. Power off their VMs through the infrastructure provider; deleting the Machine is not enough while frozen, because the hook keeps it. Check again that every remaining member answers with the pinned UID, then clear the freeze by removing only the mismatch annotation (step 4). The k0s member those joiners left in the original cluster's etcd may then need removing by hand: compare `k0s etcd member-list` on a surviving member with the live Machines, and run `k0s etcd leave --peer-address <ip>` there for the stale entry.
   - **Every member answers with the same new UID.** The pinned cluster no longer exists; its state is already lost. Re-pin to the new UID so the control plane can be managed again (step 4), then restore workloads from backup if you have one.
   - **Every member answers with the pinned UID.** The cause is gone (for example, the stray VM was already powered off). Remove only the mismatch annotation (step 4).
4. **Clear the freeze.** Pick the command that matches step 3:
   ```bash
   # keep the pin; the next check freezes again if any path still disagrees
   kubectl annotate kairoscontrolplane <name> -n <namespace> \
     controlplane.cluster.x-k8s.io/workload-cluster-identity-mismatch-

   # re-pin to the UID every member now answers with
   kubectl annotate kairoscontrolplane <name> -n <namespace> --overwrite \
     controlplane.cluster.x-k8s.io/workload-cluster-uid=<verified-uid>
   ```
   Changing or removing the pin annotation clears the freeze by itself; the controller removes the mismatch annotation and emits a Normal event `WorkloadClusterIdentityFreezeCleared`. Removing the pin instead of setting it makes the controller pin again, which it does only if every path agrees. Membership changes resume on the next reconcile.

### Before upgrading an existing HA cluster

Left alone, the controller pins an existing cluster on its first reconcile after the upgrade, from whatever answers then. It refuses to pin, and freezes, if the endpoint and the members disagree, but it cannot detect a split that every path reports the same way. Close that gap by setting the pin yourself before you upgrade:

1. Run step 2 of the runbook above and check that every control-plane member answers with the same UID.
2. Set that UID as the pin. A pin that is already set is never replaced by first-use pinning, so the controller checks against your value from its first reconcile:
   ```bash
   kubectl annotate kairoscontrolplane <name> -n <namespace> \
     controlplane.cluster.x-k8s.io/workload-cluster-uid=<verified-uid>
   ```

**CAPK:** Machines usually report pod IPs, which are not in the API server certificate, so the controller's reads through member addresses do not answer and its first-use pin rests on the control-plane endpoint alone. Pre-seeding the pin matters most there. A split that appears later is still caught by the per-connection checks and the once-a-minute recheck.

### When the controller cannot pin

Once an HA control plane has a registered member, the controller takes no membership action until the identity is pinned. If no path answers the identity read, or `KubeconfigReady` stays `False`, that hold has no time limit: no joiner is admitted, no control-plane Machine is deleted, and a Machine being deleted stays at its etcd-leave hook, because the bounded etcd-leave timeout is suspended too. The controller logs `Holding control-plane membership changes until the workload cluster identity is pinned` with the reason. The way out is to verify the UID through every member (step 2 of the runbook) and set the pin annotation by hand, as above.

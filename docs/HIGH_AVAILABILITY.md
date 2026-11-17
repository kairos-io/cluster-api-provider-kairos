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
- **`KairosFleetMachine` infrastructure**: the `KairosControlPlane` takes its endpoint from `Cluster.spec.controlPlaneEndpoint`, which Cluster API core copies from the `KairosFleetCluster`; until it is set the control plane reports `WaitingForInfrastructureControlPlaneEndpoint`. **HA (`replicas: 3`/`5`) is not exercised on this infrastructure: single control plane only today.** No HA sample is shipped for it. See [docs/QUICKSTART_FLEET.md](QUICKSTART_FLEET.md).
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

## Fixed in v0.1.5: k0s HA rollouts on clusters created with v0.1.4 or earlier

Affected k0s HA control planes on every infrastructure provider except CAPK
(CAPV, CAPM3, `KairosFleetMachine`, and the generic path), in v0.1.4 and
earlier. k3s and CAPK were not affected.

In those releases only the first control-plane node put the control-plane
endpoint in its API server certificate. The nodes that joined after it did
not. Creating the cluster worked. But when the first node was removed, for
example during a rollout after a `spec.version` change, the VIP moved to a
joiner, and every client that checks the certificate against the endpoint
failed, including the management cluster. The rollout then stopped at the
first node, with a `KairosControlPlane` message such as:

```text
failed to reconcile etcd member leave for <cluster>-cp-0: ... tls: failed to verify certificate: x509: certificate is valid for ..., not <endpoint>
```

v0.1.5 renders the endpoint into the API server certificate SANs of every
k0s control-plane member (#163). The SANs are part of a member's
bootstrap data, and members keep the bootstrap data they were created with,
so on a cluster created with v0.1.4 or earlier the members that joined before
the upgrade still lack the endpoint. Replace those joiners first, so that
their replacements are created with the fix, and replace the first
control-plane node last. See the
[v0.1.5 release notes](release-notes/v0.1.5.md).

A second issue affected the same k0s clusters, CAPK included. Up to v0.1.4,
the token that new control-plane members join with was created once, by the
first control-plane node. It pointed at that node's own address and expired 24
hours after it was created, so a member created after the first node had been
removed, or more than 24 hours after the cluster was created, could not join.
Its Machine stayed `Provisioned` without a Node, and the `KairosControlPlane`
kept waiting for it to join, which stalled rollouts, remediation and scale-up.
Since v0.1.5 (#166), the control plane issues a fresh join token for each new
member, pointed at a live member. A member already stuck this way before the
upgrade does not recover on its own: delete its Machine, and the replacement
gets a token.

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

## Control-plane Machine names

Each control-plane Machine is named `<kcp-name>-<5 random characters>`, for
example `my-cluster-cp-x7k2p`, as the upstream kubeadm control plane names
them. Its `KairosConfig` and infrastructure Machine have the same name. The
name is always a valid hostname under the `spec.hostname` rule: dots in the
`KairosControlPlane` name become dashes, and a name longer than 57 characters
is shortened, so the Machine name fits in 63 characters.

A name is never given to a second Machine. By default the Machine name is also
the node's hostname, its Node name, and its etcd member name. If a replacement got the
name of a member that had just left, it could find state that member left
behind, and on k0s that can split etcd into two clusters.

Do not rely on the order of the names. Use the creation time, as the
controller does: the oldest control-plane Machine is the one the cluster was
initialised on, and a scale-down removes the newest.

If a Machine cannot be created, for example because the infrastructure
template is missing, the controller deletes the `KairosConfig` and
infrastructure Machine it made for that attempt and tries again under a new
name. While this repeats, short-lived `KairosConfig` objects appear and
disappear, and the controller log gives the reason.

Clusters created with releases up to v0.2.0 keep their `-0`, `-1` and `-2`
Machines until those Machines are replaced. New Machines get random names,
and a control plane can have both kinds of name at once.

## Day-2: etcd health and quorum-safe replacement

Each control-plane node reports its own etcd member health as the `EtcdHealthy` condition on `KairosControlPlane`: `True` when every voting member is healthy, `False(Info)` when quorum holds but a member is degraded, `False(Warning)` at or below the `(N/2)+1` quorum minimum.

Rollouts and scale-downs are quorum-safe — the controller refuses a control-plane Machine delete that would drop etcd below `(N/2)+1` healthy voting members.

On k0s, the departing node runs `k0s etcd leave` before the Machine is deleted; a CAPI pre-terminate hook blocks termination until it acks, so no member is left orphaned. **k3s has no supported clean member-remove (KD-5d):** replacing a k3s control-plane node leaves an orphaned etcd member requiring manual `etcdctl member remove` (the controller emits `EtcdMemberRemoveUnsupportedForK3s`). k0s is the fully-supported HA distribution.

A k0s node that never acks its leave within ~5 minutes has its leave handed to k0s (`spec.leave` on its `EtcdMember`); the delete proceeds once k0s reports the member gone, or after a further ~10 minutes with a warning. Watch for `EtcdMemberLeaveTimedOut` and remove the member manually if it fires. A node missing from the `<cluster>-etcd-status` Secret does not end a leave: the nodes write their entry once, at boot, and nothing removes it, so the leave ends only on the node's ack, when k0s reports the member not joined, or with `EtcdMemberLeaveTimedOut`.

**A k0s Machine without a Node still leaves etcd (ADR 0015).** A joiner joins etcd before its Node registers, and can be deleted before it ever gets a NodeRef (for example, when its providerID was never set). The controller no longer releases such a Machine at once: it looks up the k0s `EtcdMember` under the node name recorded on the Machine (see below), and runs the same leave as for any other member. It releases the Machine straight away only when the pinned workload cluster has no `EtcdMember` of that name (a VM that never booted lands here), or k0s already reports it not joined. If no workload API server answers, or none can be verified against the pinned identity, the Machine is held for at most ~15 minutes before the delete proceeds with `EtcdMemberLeaveTimedOut`; this bound now also applies to Machines with a Node before the first leave request. A member that was added to etcd but never started has no `EtcdMember` and is not visible to the controller (KD-78). On control-plane configs, `spec.hostname` must be a lowercase DNS-1123 label with no `{{`. The webhook enforces this for new or changed configs, and the `KairosControlPlane` refuses a referenced `KairosConfigTemplate` that breaks it (`InvalidControlPlaneHostname`). If the name used for the lookup is not such a label, the controller cannot tell which member is the node's, so it never takes a missing `EtcdMember` as proof and holds the Machine for the same ~15 minutes, then releases it with `EtcdMemberLeaveTimedOut`.

**The node name is recorded on the Machine.** Every control-plane Machine the `KairosControlPlane` creates carries the annotation `controlplane.cluster.x-k8s.io/rendered-node-name`, set in the same create: the hostname its node is given (`spec.hostname` of its `KairosConfig`, else the Machine name). The controller never overwrites or removes it, and `machineTemplate.metadata` does not copy this key onto Machines. For a k0s Machine without a NodeRef, the leave is keyed by this recorded name, not by the `KairosConfig`. Editing, deleting or recreating the leaving Machine's `KairosConfig`, before or during the leave, therefore does not change which member leaves (a foreground delete of the Machine can remove its `KairosConfig` before the Machine reaches the pre-terminate hook). If the `KairosConfig` now renders a different name, the controller emits the Warning event `EtcdLeaveNodeNameContradictsKairosConfig` and uses the recorded name anyway. A recorded name that is not a lowercase DNS-1123 label can only come from someone editing the annotation; on the first pass it is held and then released as described above. Do not edit the annotation.

The leave is not run when any other control-plane Machine of the Cluster, whoever owns it, may go by the same name: a registered Machine by its Node name, a Machine without a Node by its recorded name or by the hostname its `KairosConfig` renders (that Machine's node may have been bootstrapped after its `KairosConfig` was edited; a Machine with another bootstrap provider goes by its Machine name). Nor is it run, or handed to k0s, when any other control-plane Machine of the Cluster reports the `EtcdMember`'s peer address (for a Machine with a Node, only the hand-over to k0s checks this). The controller then sends nothing and releases the Machine with `EtcdMemberLeaveTimedOut`. A hostname edited on a registered Machine's `KairosConfig` does not count. If the control-plane Machines cannot be listed, or a name another Machine may go by cannot be read, the controller writes nothing to the workload cluster and holds the Machine for the usual ~15 minutes, then releases it with `EtcdMemberLeaveTimedOut`, without handing the leave to k0s. This hold is logged and has no event of its own.

The name is checked again on every pass after the leave was requested, before anything is read from or written to the workload cluster. If it no longer passes (the annotation, or for an older Machine without one its `KairosConfig` hostname, was changed to a name that is not a lowercase DNS-1123 label, or to a name another control-plane Machine of the Cluster may go by), the controller sends nothing more to the workload cluster and emits the Warning event `EtcdLeaveNodeNameUnusable` on each pass. Restoring the annotation lets the leave carry on. Otherwise, after the usual ~15 minutes from the leave request, the hook is removed with `EtcdMemberLeaveTimedOut` and the leave is never handed to k0s.

On upgrade, a control-plane Machine that is not being deleted and has no annotation gets one on the first reconcile (one patch per Machine): its Node name if it has registered, otherwise the hostname its `KairosConfig` renders, and only when that is a lowercase DNS-1123 label. Otherwise it keeps the previous lookup by `KairosConfig`, with the previous ~15-minute hold. A Machine already being deleted at upgrade keeps the previous lookup. After a downgrade the annotation is ignored.

Known gap: until `spec.hostname` becomes immutable on control-plane `KairosConfig` objects, editing it after the Machine is created but before its node has bootstrapped makes the recorded name differ from the name the node actually takes. The leave then finds no `EtcdMember` under the recorded name and releases the Machine without a leave, which can leave its etcd member behind (the orphan cleanup below can remove it once the control plane is steady again). Do not edit `spec.hostname` on an existing control-plane `KairosConfig`.

### Orphan etcd members (k0s)

Like the upstream kubeadm control plane, the controller removes etcd members that no control-plane Machine accounts for, for example a member orphaned by an older controller version. It asks k0s to remove the member through its `EtcdMember`, one at a time, and only:

- through a workload API server verified against the pinned identity, and never while frozen;
- in a steady state: replicas as desired, no rollout, and no control-plane Machine of the cluster, whoever owns it, being deleted or without a Node; every member must report healthy and voting;
- only when every control-plane Machine is paired with an `EtcdMember` of its own, by Node name or by a peer address it reports. If one is not (a member's name has drifted from its Node name, and on CAPK its bridge peer address matches no Machine), nothing is removed and `EtcdOrphanMemberRemovalBlocked` says "member naming does not match";
- for a member whose name is none of a control-plane Machine's Node name, recorded node name (`rendered-node-name`) or `KairosConfig` hostname, and whose peer address no control-plane Machine reports (on CAPK the peer address is on the bridge network, so the name decides);
- when the healthy voting members left are still a majority, of the members left and of the desired replicas. Otherwise it emits `EtcdOrphanMemberRemovalBlocked` and writes nothing.

While a removal is in progress it is recorded in the `controlplane.cluster.x-k8s.io/etcd-orphan-member-leave` annotation, and no control-plane Machine is created or deleted, even if the control plane is scaled to one replica in the meantime. A record dated more than 5 minutes in the future is discarded. Events: `EtcdOrphanMemberLeaveRequested`, then `EtcdOrphanMemberRemoved`, or `EtcdOrphanMemberRemovalTimedOut` after ~10 minutes (remove it by hand: `k0s etcd leave --peer-address <ip>` on a surviving member), or `EtcdOrphanMemberChanged` if the `EtcdMember` came to describe another member. Unstarted members (the empty-name entry in `k0s etcd member-list`) are not visible and still need removing by hand.

**KD-51:** etcd health/leave signals are node-self-reported over vanilla RBAC, so a compromised control-plane node can forge them. Not a privilege escalation (a compromised node already has cluster-admin-equivalent access) and it cannot force an unsafe deletion — quorum-safety is decided independently of any node signal. A forged signal can only self-downgrade the clean-leave/health guarantee.

## A refused k0s joiner (ADR 0015)

A k0s control-plane joiner checks that it really joined a cluster. If the live `k0scontroller` was started with its join token but `k0s etcd member-list` shows only the joiner itself, the node has bootstrapped a cluster of its own (it shares the CA, so TLS cannot tell). It then stops k0s, never sets its providerID, and refuses every later start.

**Recognise it:**
- The Machine never gets a NodeRef, and the rollout or scale-up stalls on it.
- On the node, `/var/lib/k0s/.kairos-join-refused` exists, `systemctl status k0scontroller` shows the start refused, and the journal has `kairos-join-guard: REFUSED`.
- A joiner whose confirmation passed has `/var/lib/k0s/.kairos-join-confirmed` instead.

**Fix:** delete the Machine. Do not remove the marker or start k0s by hand on that node; the replacement joins as a fresh VM. If the failed join left an unstarted etcd member behind, the replacement may stall with `Peer URLs already exists`: remove that member on a surviving control-plane node with `k0s etcd leave --peer-address <ip>` (the empty-name entry in `k0s etcd member-list`).

**Do not** delete or replace all of the older control-plane members while a joiner is still unconfirmed (no `.kairos-join-confirmed`). Until it is confirmed you cannot tell whether it joined the real cluster.

## Day-2: workload cluster identity and the freeze

An HA control plane pins the identity of the workload cluster it manages, and freezes control-plane membership changes if a workload API server ever answers as a different cluster (ADR 0015). The identity is the UID of the workload `kube-system` Namespace, stored on the `KairosControlPlane` in the `controlplane.cluster.x-k8s.io/workload-cluster-uid` annotation. The full rules (when it is pinned, what is checked, the annotations) are in [API_REFERENCE.md — Workload cluster identity](API_REFERENCE.md#workload-cluster-identity).

Why it exists: on k0s, a joining member whose join failed half-way can start a new etcd cluster with the certificate authority it received while joining. The new cluster's API server is trusted by the same CA, kube-vip there can take the VIP, and a controller that cannot tell the two clusters apart then removes the original members as "gone". The freeze turns that into a stop that waits for a person.

While frozen:

- `Ready`, `Available` and `EtcdHealthy` are `False` (Error) with reason `WorkloadClusterIdentityChanged`, and a Warning event `WorkloadClusterIdentityChanged` names the pinned UID, the UID read, and where it was read from;
- the controller creates and deletes no control-plane Machine and removes no etcd-leave hook, so a k0s Machine deleted by you or a MachineHealthCheck stays at its pre-terminate hook, its VM and etcd data intact;
- the controller writes nothing to the workload cluster (no etcd-leave request, no join token, no orphan etcd-member removal);
- anything that waits on the Cluster being `Ready` or `Available` (GitOps health checks) holds too, which is intended;
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
   - **One or a few members (usually the newest joiner) answer with a different UID; the rest answer with the pinned one.** Those members serve a cluster of their own. Power off their VMs through the infrastructure provider; deleting the Machine is not enough while frozen, because the hook keeps it. Check again that every remaining member answers with the pinned UID, then clear the freeze by removing only the mismatch annotation (step 4). Once the freeze is cleared and the control plane is steady, the controller removes a started k0s member those joiners left in the original cluster's etcd (see [Orphan etcd members](#orphan-etcd-members-k0s)). A member that never started, or one the controller reports `EtcdOrphanMemberRemovalBlocked` or `EtcdOrphanMemberRemovalTimedOut` for, needs removing by hand: compare `k0s etcd member-list` on a surviving member with the live Machines, and run `k0s etcd leave --peer-address <ip>` there for the stale entry.
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

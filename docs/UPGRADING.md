# Upgrading Kairos CAPI Provider

> This file is a stub. The canonical upgrade procedures land in a later PR
> (`docs/kd-35-kd-36-install-upgrade`). For now this captures the
> alpha-1 → alpha-2 KD-3b impact and the alpha-2 → alpha-3 KD-12 impact.

## v0.1.0-alpha.2 → v0.1.0-alpha.3

### KD-12: KCP no longer writes `Cluster.Spec.ControlPlaneEndpoint`

The alpha-3 release removes the two blocks in `updateClusterStatus` that wrote
`Cluster.Spec.ControlPlaneEndpoint` from Machine IP addresses (CAPV/CAPD) or
from the LoadBalancer Service Ingress IP (CAPK). `Cluster.Spec.ControlPlaneEndpoint`
is now written exclusively by CAPI core, which copies it from the infra-cluster's
own `Spec.ControlPlaneEndpoint`. This is the behaviour the CAPI v1beta2 contract
requires.

| Cluster state at upgrade | Behaviour after KD-12 | Operator action |
|---|---|---|
| Healthy cluster, `Cluster.Spec.ControlPlaneEndpoint` already populated | No change. The endpoint is preserved; the KCP controller stops overwriting it. CAPI core is now the sole writer, which is byte-identical to what KCP was writing. | None. |
| Mid-provisioning cluster, infra provider has already populated `<Infra>Cluster.Spec.ControlPlaneEndpoint` | CAPI core copies it on its next reconcile; everything proceeds. | None. |
| CAPV / CAPD / CAPK cluster whose `<Infra>Cluster.Spec.ControlPlaneEndpoint` (CAPV/CAPD) or `KubevirtCluster.Spec.ControlPlaneServiceTemplate` (CAPK) is unset — including any cluster that relied on KCP auto-discovering the endpoint from Machine IPs | `Cluster.Spec.ControlPlaneEndpoint` stays empty. KCP stalls with `KairosControlPlane.Status.Conditions[Available].Reason=WaitingForInfrastructureControlPlaneEndpoint`. | Set `VSphereCluster.Spec.ControlPlaneEndpoint` (CAPV), `DockerCluster.Spec.ControlPlaneEndpoint` (CAPD), or supply a LoadBalancer IP via the sample's `Cluster.spec.controlPlaneEndpoint` placeholder (CAPK) per the upstream provider's contract. For an in-flight cluster: `kubectl edit cluster <name>` and set `spec.controlPlaneEndpoint.host` and `spec.controlPlaneEndpoint.port` directly. CAPI core does not overwrite a populated value. |

No CRD changes in this release for KD-12. The new condition Reason
(`WaitingForInfrastructureControlPlaneEndpoint`) is additive — clients
that treat unknown Reasons as opaque strings are unaffected.

---

## v0.1.0-alpha.1 → v0.1.0-alpha.2

### KD-3b: SSH eliminated from controller normal operation

The alpha-2 release removes synchronous SSH from the controller's hot path.
The bootstrap controller now writes a `push_kubeconfig()` shell function into
every control-plane node's cloud-config. After k0s/k3s starts, the **node**
POSTs its workload kubeconfig back to a Secret in the management cluster using
a short-lived Bearer token. The control-plane controller waits for that Secret
via a label-filtered watch.

| State at upgrade | What happens | User action |
|---|---|---|
| CAPK control-plane healthy, kubeconfig Secret present | No change. New Secrets created during a rollout get the new `cluster.x-k8s.io/cluster-name` label and `controllers.cluster.x-k8s.io/kubeconfig-source: node-push` annotation. Existing Secrets do not, but the controller's existing-Secret check uses Get-by-name and still works. | None. |
| CAPK control-plane mid-bootstrap (Secret not yet pushed) | Controller renders a NEW bootstrap Secret on next reconcile. The node, when it comes up, uses the new template's push payload. | None unless a rollout is needed for unrelated reasons. |
| CAPV control-plane healthy, kubeconfig Secret present (was SSH-fetched in alpha-1) | Controller's existing-Secret check still passes. Cluster keeps working. | None. |
| CAPV control-plane mid-bootstrap | **Old KairosConfig has no push block.** Controller's SSH path is GONE. The node will never push; the controller will never SSH. The cluster will sit indefinitely on `KubeconfigReadyCondition=False(WaitingForNodePush)`. | **Required**: delete the KairosConfig (or trigger a KCP rollout) so the bootstrap controller renders a new userdata with the push block. CAPV will create a new VM with new userdata. |
| Air-gapped CAPV control-plane (no network reachability from VM to management cluster API server) | Cluster stuck on `KubeconfigReadyCondition=False(WaitingForNodePush)` indefinitely; condition severity transitions Info → Warning after 10 minutes. | Enable `Spec.SSHFallback`: (1) create a `kubernetes.io/ssh-auth` Secret with an `ssh-privatekey` key in the cluster's namespace; (2) create an Opaque Secret with a `known_hosts` key containing the node's SSH host-key lines; (3) set `spec.sshFallback.enabled: true`, `spec.sshFallback.identitySecretRef.name`, and `spec.sshFallback.knownHostsSecretRef.name` on the `KairosControlPlane`; (4) leave `spec.sshFallback.activateAfter` at the default `15m` unless you have a reason to change it. The fallback fires after `activateAfter` elapses from `KubeconfigReadyCondition` first becoming `False(WaitingForNodePush)`. See [Air-gapped fallback (SSHFallback)](QUICKSTART_CAPV.md#air-gapped-fallback-sshfallback) for worked examples. Or open a network path from the VM to `<management-cluster-api-server-host>:6443`. |
| Bootstrap Secret rendered before alpha-2 (no post-bootstrap providerID service block in the cloud-config) | Controller no longer patches `Node.Spec.ProviderID` (PR-8 deleted `ensureProviderIDOnNodes`). The old Secret also has no in-node patch script. `Node.Spec.ProviderID` stays empty → CAPI Machine controller cannot match the NodeRef → Machine stays `NodeNotFound`. | **Required**: trigger a KCP rollout (or delete the KairosConfig) so the bootstrap controller re-renders the Secret with the alpha-2 template, which includes the post-bootstrap providerID service. |

### KD-3b part 2: Node.Spec.ProviderID is now set exclusively in-VM

PR-8 deletes `ensureProviderIDOnNodes` — the management-controller helper
that patched `Node.Spec.ProviderID` from outside the VM. That function was
the last piece of controller-side synchronous workload-cluster I/O (KD-10).

The in-VM cloud-config now owns `Node.Spec.ProviderID` exclusively:

- **k3s (CAPV and CAPK)**: kubelet receives `--kubelet-arg=provider-id=<value>`
  at startup via a `config.yaml.d/90-provider-id.yaml` drop-in written by a
  systemd `ExecStartPre` unit. A DMI-based discovery script runs when the
  providerID was not known at cloud-config render time (first-boot race).
- **k0s (CAPV and CAPK)**: a post-bootstrap systemd service patches
  `Node.Spec.ProviderID` using the local `/var/lib/k0s/pki/admin.conf`
  after k0s starts, retrying 30 times at 5-second intervals. When ProviderID
  is empty at render time the patch is deferred to the next reconcile cycle,
  which re-renders the Secret with the correct value.

**For most operators this is a no-op.** If `Node.Spec.ProviderID` was already
set (alpha-1 or alpha-2 healthy clusters), the in-VM scripts either write the
same value idempotently (k3s) or skip the patch because the Node field is
already populated (k0s, which treats a non-empty providerID as immutable).

**If you maintain a fork or derivative that re-added `ensureProviderIDOnNodes`
between PR-7 and PR-8:** that helper is gone from the upstream source; the
code will not compile. Remove the derivative — the in-VM scripts cover the
same contract without management-cluster Node access.

To diagnose a stuck `Node.Spec.ProviderID` after PR-8, inspect the VM journal
rather than controller logs:

```bash
# On the workload node (k0s):
journalctl -u kairos-k0s-post-bootstrap --no-pager

# On the workload node (k3s):
journalctl -u kairos-k3s-post-bootstrap --no-pager
```

### Network reachability requirement

CAPV (and future non-CAPK infrastructure) VMs must reach the management
cluster's API server URL. Verify from a workload node before deploying:

```bash
curl -k https://<mgmt-api-server-host>:6443/api
```

For air-gapped or strictly-segmented network environments, enable
`Spec.SSHFallback` on the `KairosControlPlane`. See
[Air-gapped fallback (SSHFallback)](QUICKSTART_CAPV.md#air-gapped-fallback-sshfallback)
for the full operator procedure.

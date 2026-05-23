# Upgrading Kairos CAPI Provider

> This file is a stub. The canonical upgrade procedures land in a later PR
> (`docs/kd-35-kd-36-install-upgrade`). For now this captures the
> alpha-1 → alpha-2 KD-3b impact only.

## v0.1.0-alpha.1 → v0.1.0-alpha.2

### KD-3b: SSH eliminated from controller normal operation

The alpha-2 release removes synchronous SSH from the controller's hot path.
The bootstrap controller now writes a `push_kubeconfig()` function into every
control-plane node's cloud-config. After k0s/k3s starts, the **node** POSTs
its workload kubeconfig back to a Secret in the management cluster using a
short-lived Bearer token. The controller waits for that Secret via a label-
filtered watch.

| State at upgrade | What happens | User action |
|---|---|---|
| CAPK control-plane healthy, kubeconfig Secret present | No change. New Secrets created during a rollout get the new `cluster.x-k8s.io/cluster-name` label and `controllers.cluster.x-k8s.io/kubeconfig-source: node-push` annotation. Existing Secrets do not, but the controller's existing-Secret check uses Get-by-name and still works. | None. |
| CAPK control-plane mid-bootstrap (Secret not yet pushed) | Controller renders a NEW bootstrap Secret on next reconcile. The node, when it comes up, uses the new template's push payload. | None unless a rollout is needed for unrelated reasons. |
| CAPV control-plane healthy, kubeconfig Secret present (was SSH-fetched in alpha-1) | Controller's existing-Secret check still passes. Cluster keeps working. | None. |
| CAPV control-plane mid-bootstrap | **Old KairosConfig has no push block.** Controller's SSH path is GONE. The node will never push; the controller will never SSH. The cluster will sit indefinitely on `KubeconfigReadyCondition=False(WaitingForNodePush)`. | **Required**: delete the KairosConfig (or trigger a KCP rollout) so the bootstrap controller renders a new userdata with the push block. CAPV will create a new VM with new userdata. |
| Air-gapped CAPV control-plane (no network reachability from VM to management cluster API server) | Cluster stuck on `KubeconfigReadyCondition=False(WaitingForNodePush)` indefinitely; condition severity transitions Info → Warning after 10 minutes. | Wait for `SSHFallback` (post-alpha-2 PR). Or open a network path from the VM to `<management-cluster-api-server-host>:6443`. |

### Network reachability requirement

CAPV (and future non-CAPK infrastructure) VMs must reach the management
cluster's API server URL (`mgr.GetConfig().Host`). Verify with `curl -k
https://<api-server>/api` from a workload node before deploying. For
air-gapped or strictly-segmented network environments, see the planned
`SSHFallback` opt-in mechanism (post-alpha-2).

# Quick Start Guide - Fleet (Kairos fleet / AuroraBoot)

Last verified against: Kairos fleet provider v0.1.4+
(`cluster-api-provider-kairos-fleet`; do not use `v0.1.0-beta.1`, it
crash-loops on a real management cluster), this repository's fleet-support
code (ADR 0008, shipped in v0.1.0 — see the
[v0.1.0 release notes](release-notes/v0.1.0.md)), Cluster API v1.13.4
(v1beta2 contract), Kairos v4.1.2, k3s. This walkthrough has not been run
end to end against a live AuroraBoot instance in this repository's CI — it
is assembled from the fleet provider's own documentation and from reading
both controllers' source. If a step does not match your observed behavior,
treat this page as the thing that is wrong and file an issue.

This guide walks you through provisioning a single control-plane machine plus
a worker `MachineDeployment` on Kairos using Cluster API with the **Kairos
fleet** infrastructure provider (`cluster-api-provider-kairos-fleet`,
clusterctl name `kairos-fleet`). Fleet does not create machines on demand: it
claims an already-enrolled, unclaimed Kairos node from a named AuroraBoot
group, hands it a bootstrap cloud-config, and reboots it. "No capacity in a
group" is an expected, transient state — enroll more nodes or free one up —
not a failure.

**Scope of this guide:**

- Single control-plane machine (`replicas: 1`) plus a 1-replica worker
  `MachineDeployment` (`kairos_cluster_{k0s,k3s}_with_workers.yaml`). This is
  the validated, exercised topology on fleet. A standalone single node with
  no workers (`kairos_cluster_k0s_single_node.yaml`) is also covered below.
  HA (`replicas: 3`/`5`) is mechanically reachable but **not exercised** — no
  fleet HA sample is shipped (ADR 0008). See
  [docs/HIGH_AVAILABILITY.md](HIGH_AVAILABILITY.md).
- k3s is the reference / most-exercised distribution. k0s is also supported
  and newer; see the k0s samples and the notes below. A default single-node
  k0s control plane is joinable and schedulable (`--enable-worker`), so the
  shipped worker `MachineDeployment` can join it; set
  `spec.k0sSingleNode: true` on the `KairosConfigTemplate` instead for a
  standalone node that never gains workers — see
  [kairos_cluster_k0s_single_node.yaml](../config/samples/fleet/kairos_cluster_k0s_single_node.yaml).
- The control-plane endpoint is **operator-supplied**: fleet does not
  allocate or discover a VIP, load balancer, or DNS name. You must know the
  node's reachable endpoint before it is claimed — the same sharp edge as
  CAPM3's DHCP-reservation requirement.

---

## How fleet provisioning works

Understanding the provisioning model prevents the most common failure modes:

1. You enroll Kairos nodes with AuroraBoot ahead of time, into named groups
   (for example `control-plane` and `workers`). Enrollment and grouping
   happen in AuroraBoot, outside Cluster API.
2. You apply a `Cluster` + `KairosFleetCluster` + `KairosControlPlane` +
   `KairosFleetMachineTemplate` + `KairosConfigTemplate` set (plus a worker
   `MachineDeployment` if you want workers).
3. The `KairosFleetMachine` controller claims one unclaimed node from the
   target group, using the `KairosFleetMachine`'s UID, read at claim time,
   as a stable claim key so a retried reconcile finds the same node instead
   of claiming a second one. The key is recorded in the `claim-key`
   annotation and presented when the node is released.
4. It fetches the bootstrap cloud-config that the Kairos bootstrap provider
   rendered for that Machine and hands it to AuroraBoot as an apply-cloud-config
   command. AuroraBoot stages the config to the node's `/oem` overlay. There
   is **no install stage** — the node is already an installed Kairos system.
5. The controller issues a reboot, then polls until the node comes back
   `Online` with a heartbeat newer than the reboot request.
6. Once rejoined, the controller sets `status.addresses`,
   `spec.providerID = kairos-fleet://<node-id>`, and marks the
   `KairosFleetMachine` `Provisioned`.

This is a poor fit for on-demand elastic capacity: capacity is whatever is
enrolled and unclaimed in a group at claim time. It is a good fit for bare
metal and pre-provisioned edge nodes that phone home to AuroraBoot.

---

## Prerequisites

1. **Management cluster**: a Kubernetes cluster running Cluster API core
   v1.13.4+ (v1beta2 contract). See the version table in
   [README.md — Target Versions](../README.md#target-versions).

2. **Three providers installed**: the Kairos bootstrap provider, the Kairos
   control-plane provider (both from this repo), and the Kairos fleet
   infrastructure provider (`cluster-api-provider-kairos-fleet`):

   ```bash
   clusterctl init \
     --bootstrap kairos \
     --control-plane kairos \
     --infrastructure kairos-fleet
   ```

   `kairos-fleet` is not in clusterctl's built-in provider list; you must
   register it in `clusterctl.yaml` first. See
   [INSTALL.md — Registering the Kairos fleet infrastructure provider](INSTALL.md#registering-the-kairos-fleet-infrastructure-provider)
   for the exact entry and full install procedure.

3. **An AuroraBoot instance** reachable from the management cluster, with:
   - an admin bearer token,
   - at least one enrolled, unclaimed node in a control-plane group (this
     guide uses the default name `control-plane`),
   - at least one enrolled, unclaimed node in a worker group (this guide uses
     the default name `workers`),
   - nodes enrolled with a `phonehome.allowed_commands` policy that permits
     `apply-cloud-config` and `reboot`, the only two node commands the fleet
     provider sends (it never sends `reset`). AuroraBoot's install-agent
     default, `upgrade,upgrade-recovery,reboot,unregister`, lacks
     `apply-cloud-config`, and a list you set replaces the default rather
     than adding to it. See the fleet provider's
     [AuroraBoot node enrolment prerequisite](https://github.com/kairos-io/cluster-api-provider-kairos-fleet/blob/main/docs/QUICKSTART.md#auroraboot-node-enrolment-prerequisite)
     for the recipe.

4. **A control-plane endpoint** (host and port) that the workload cluster's
   API server will be reachable on. Fleet does not allocate one: use a
   kube-vip VIP, a load balancer, or a DNS name you manage, pointed at the
   control-plane node once it comes up.

5. **Network reachability from the claimed nodes to the management cluster's
   API server.** Like CAPV/CAPM3, fleet nodes PATCH their kubeconfig into a
   staging Secret the control plane owns in the management cluster, rather
   than being SSHed into. Set `KAIROS_MANAGEMENT_API_OVERRIDE` on the bootstrap
   provider to a URL the nodes can reach; without it they are given the
   in-cluster Service URL. See
   [INSTALL.md — Set the URL the nodes push to](INSTALL.md#set-the-url-the-nodes-push-to)
   and [Network reachability requirement for non-CAPK infrastructure](INSTALL.md#network-reachability-requirement-for-non-capk-infrastructure).
   For air-gapped nodes, the opt-in `SSHFallback` mechanism on
   `KairosControlPlane` is the same escape hatch documented for CAPM3 (see
   [QUICKSTART_CAPM3.md — Air-gapped fallback](QUICKSTART_CAPM3.md#air-gapped-fallback-sshfallback)).

6. **k3s is the reference path.** k0s fleet support is newer; prefer k3s
   unless you have a specific reason to choose k0s (see
   [kairos_cluster_k0s_with_workers.yaml](../config/samples/fleet/kairos_cluster_k0s_with_workers.yaml)).

---

## Creating a cluster

### Step 1: Create the AuroraBoot admin-token Secret

`KairosFleetCluster` references a Secret holding the AuroraBoot admin bearer
token. Create it in the namespace the cluster will live in, before applying
the cluster manifest. Do not put the token in a manifest that gets committed
to version control:

```bash
kubectl create secret generic auroraboot-admin-token \
  --namespace default \
  --from-literal=token="$AURORABOOT_ADMIN_TOKEN"
```

`$AURORABOOT_ADMIN_TOKEN` should come from your own secret store or shell
environment.

### Step 2: Create the user-password Secret

```bash
kubectl create secret generic kairos-user-password \
  --from-literal=password=$(openssl rand -base64 32)
```

The sample manifests reference this Secret via `userPasswordSecretRef`. Do
not set `userPassword` inline.

### Step 3: Choose a sample manifest

- k3s control-plane + worker `MachineDeployment` (reference path):
  [`config/samples/fleet/kairos_cluster_k3s_with_workers.yaml`](../config/samples/fleet/kairos_cluster_k3s_with_workers.yaml)
- k0s control-plane + worker `MachineDeployment`:
  [`config/samples/fleet/kairos_cluster_k0s_with_workers.yaml`](../config/samples/fleet/kairos_cluster_k0s_with_workers.yaml)
- k0s standalone single node, no workers (`spec.k0sSingleNode: true`):
  [`config/samples/fleet/kairos_cluster_k0s_single_node.yaml`](../config/samples/fleet/kairos_cluster_k0s_single_node.yaml)

### Step 4: Customize the manifest

Open the chosen sample and fill in every value marked `TODO`.

#### KairosFleetCluster

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: KairosFleetCluster
spec:
  # Operator-supplied; the provider does not allocate or discover it.
  controlPlaneEndpoint:
    host: "TODO-CONTROL-PLANE-ENDPOINT-HOST"
    port: 6443
  auroraboot:
    url: "https://auroraboot.example.com:8080"
    adminTokenSecretRef:
      name: auroraboot-admin-token
```

Set `spec.controlPlaneEndpoint` **only** on `KairosFleetCluster`, never also
on `Cluster.spec.controlPlaneEndpoint` — CAPI core copies the value down
automatically once `KairosFleetCluster` reports it. Setting it in both
places risks the two drifting.

`spec.auroraboot.url` is the base URL of the AuroraBoot fleet API.
`adminTokenSecretRef.name` must match the Secret created in step 1.

#### KairosFleetMachineTemplate group selection

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: KairosFleetMachineTemplate
spec:
  template:
    spec:
      group: control-plane   # or "workers" for the worker template
```

`spec.group` names one AuroraBoot group directly; there is no label or
capability selector. The group must have at least one enrolled, unclaimed
node when the `KairosFleetMachine` reconciles, or it stays
`WaitingForCapacity`.

#### KairosControlPlane and KairosConfigTemplate

```yaml
# In KairosControlPlane:
spec:
  replicas: 1
  version: "v1.34.3+k3s3"
  distribution: k3s     # explicit value always wins over the template's

# In KairosConfigTemplate:
spec:
  template:
    spec:
      distribution: k3s
      kubernetesVersion: "v1.34.3+k3s3"
      # No install: block — AuroraBoot nodes are already installed.
```

### Step 5: Apply the manifest

```bash
kubectl apply -f config/samples/fleet/kairos_cluster_k3s_with_workers.yaml
```

or for k0s with a worker:

```bash
kubectl apply -f config/samples/fleet/kairos_cluster_k0s_with_workers.yaml
```

or for a standalone k0s node with no workers:

```bash
kubectl apply -f config/samples/fleet/kairos_cluster_k0s_single_node.yaml
```

---

## Watch provisioning

The `KairosFleetMachine`'s `Ready` condition progresses through a sequence
of reasons as the controller claims and configures each node:

```bash
kubectl get kairosfleetmachines -n default -w
```

| Reason | Meaning |
| --- | --- |
| `WaitingForBootstrapData` | Waiting for the Kairos bootstrap provider to publish the cloud-config Secret. |
| `WaitingForClusterInfrastructure` | The `KairosFleetCluster` does not exist yet, or reading its admin-token Secret failed in a way that can clear on its own. |
| `WaitingForAuroraBootSecret` | The admin-token Secret the `KairosFleetCluster` names does not exist yet. Creating it (step 1) is enough. |
| `AuroraBootConnectionIncomplete` | The connection cannot work as written: the `KairosFleetCluster` has no `auroraboot.url`, or the admin-token Secret has no `token` key. Waiting does not fix it; edit the `KairosFleetCluster` or the Secret. The condition message names which. |
| `GroupNotFound` | `spec.group` does not match any AuroraBoot group by name or ID. Not terminal: create the group or fix the name and the machine picks it up on the next reconcile. |
| `WaitingForCapacity` | The target group has no unclaimed nodes. Enroll more nodes in AuroraBoot or free one up. |
| `NodeClaimed` | A node has been claimed; applying its bootstrap cloud-config next. |
| `ApplyingCloudConfig` | The cloud-config has been handed to AuroraBoot and is being written to the node, or the controller is waiting for that write to complete. |
| `CloudConfigFailed` | The apply-cloud-config command reported `Failed` or `Expired`, commonly because the node's AuroraBoot phonehome policy does not permit the command (see the `allowed_commands` prerequisite above). Not terminal: the controller clears the applied-config marker and queues a fresh command about once a minute, so the machine moves on once the node accepts it. |
| `Rebooting` | The controller has requested a reboot so the node applies the staged config. |
| `RebootFailed` | The reboot command reported `Failed` or `Expired` while the node was still `Online`. `reboot` has its own entry in the node's `allowed_commands` policy, so a node that accepted the apply can still refuse it. Not terminal: the controller clears the reboot markers and issues a fresh reboot about once a minute. |
| `WaitingForNodeRejoin` | Waiting for the node to come back `Online` with a heartbeat newer than the reboot request. |
| `NodeMissing` | The claimed AuroraBoot node no longer exists. Terminal: `status.failureReason` and `status.failureMessage` are set; the machine does not retry itself. |
| `Provisioned` | The node is claimed, configured, and `Online`. `spec.providerID` and `status.addresses` are set. |

A machine stuck at `WaitingForCapacity` needs more enrolled nodes in that
group:

```bash
kubectl get kairosfleetmachine -n default -o jsonpath='{.items[0].spec.group}'
```

A machine that reaches `CloudConfigFailed` or `RebootFailed` is not terminal
and needs no manual action beyond fixing the node (see the
`allowed_commands` prerequisite above); the controller retries on its own. A
machine that reaches `NodeMissing` has failed terminally
(`status.failureReason` / `status.failureMessage` are set); inspect the
AuroraBoot node's command history and delete and re-create the Machine.

Also watch the higher-level objects, the same as any other infrastructure
provider:

```bash
kubectl get cluster fleet-demo -w
kubectl get kairoscontrolplane fleet-demo-control-plane
kubectl get machines -n default
```

---

## Retrieve the kubeconfig and confirm the node joined correctly

```bash
kubectl get secret fleet-demo-kubeconfig \
  -o jsonpath='{.data.value}' | base64 -d > fleet-demo-kubeconfig.yaml
kubectl --kubeconfig=fleet-demo-kubeconfig.yaml get nodes
```

**Node-push behavior:** the control-plane node posts its kubeconfig to a
Secret in the management cluster at bootstrap time, the same as CAPV/CAPM3.
If the node has no route to the management cluster's API server, enable
`SSHFallback` on the `KairosControlPlane` (see Prerequisites above). The
controller validates and canonicalizes whatever it receives before
publishing it — see
[API_REFERENCE.md — KubeconfigReady condition](API_REFERENCE.md#kubeconfigready-condition)
for the full set of `KubeconfigReady` reasons if the Secret does not appear
or a kubeconfig you extract looks unexpected.

### providerID cross-provider contract — verify the match

The pass criterion for a healthy fleet Machine is that the `KairosFleetMachine`
and the workload `Node` report the **identical** `kairos-fleet://<node-id>`
value:

```bash
kubectl get kairosfleetmachine -n default -o jsonpath='{.items[0].spec.providerID}'
kubectl --kubeconfig=fleet-demo-kubeconfig.yaml get nodes -o jsonpath='{.items[0].spec.providerID}'
```

Both commands must print the same value. If they do not match, the workload
`Node` never registers against the right Machine and the Machine stays
unhealthy. The fleet controller sets `spec.providerID` on the
`KairosFleetMachine`; the node side of the match is a cross-provider
contract implemented in this repo's bootstrap cloud-config rendering (see
`internal/bootstrap/template.go`), which derives the identical
`kairos-fleet://<node-id>` string from the Kairos phone-home agent's
persisted credentials and injects it before the kubelet registers. Neither
provider patches `Node.spec.providerID` directly. If the values disagree,
confirm the node image runs the Kairos phone-home agent and has already
phoned home (so the credentials file exists) before k3s/k0s starts, and that
the `KairosConfigTemplate` distribution matches your Kairos image.

---

## Tear down

```bash
kubectl delete cluster fleet-demo -n default
```

Deleting the `Cluster` deletes its Machines, which deletes their
`KairosFleetMachine`s; each releases its claimed node back to its AuroraBoot
group. The node is **not wiped** — v0.1 of the fleet provider always
releases, never resets. The next claim re-applies fresh bootstrap
configuration onto whatever state the node was left in. A `deletePolicy`
field to opt into a wipe-on-release reset is a planned follow-up on the
fleet provider's own roadmap, not something this repo controls.

Delete the `Cluster`, not the `KairosFleetCluster`, and keep the admin-token
Secret until the `Cluster` is gone: each machine releases its node through
that connection. If the `KairosFleetCluster` or the Secret is already gone,
or the connection has no URL or token, the machine finishes deleting without
a release and its node stays claimed in AuroraBoot. Release such a node in
AuroraBoot.

---

## Troubleshooting

| Symptom | Cause | Action |
| --- | --- | --- |
| `KairosFleetMachine` stuck at `WaitingForClusterInfrastructure` | The `KairosFleetCluster` does not exist yet, or reading its admin-token Secret failed in a way that can clear on its own | Check that the `KairosFleetCluster` exists in the same namespace as the machine and that the apiserver is answering. |
| `KairosFleetMachine` stuck at `WaitingForAuroraBootSecret` | The admin-token Secret named by `spec.auroraboot.adminTokenSecretRef` does not exist | Create it in the cluster's namespace (Step 1). |
| `KairosFleetMachine` stuck at `AuroraBootConnectionIncomplete` | `spec.auroraboot.url` is empty, or the Secret has no `token` key | Edit the `KairosFleetCluster` or the Secret; the condition message says which. |
| `KairosFleetMachine` stuck at `GroupNotFound` | `spec.group` matches no AuroraBoot group by name or ID | Fix the group name or create the group; the machine picks it up on the next reconcile. |
| `KairosFleetMachine` shows `CloudConfigFailed` | The node's `phonehome.allowed_commands` policy does not permit `apply-cloud-config` | Add it to the node's policy (see the prerequisite above); the controller retries about once a minute. |
| `KairosFleetMachine` shows `RebootFailed` | The node refused or failed the reboot, for example `reboot` is missing from `allowed_commands` | Fix the node's policy; the controller issues a fresh reboot about once a minute. |
| Machines never get past claim or group lookup and the controller logs request errors (for example `resolving group ...`) | `spec.auroraboot.url` is wrong or AuroraBoot is unreachable. No condition reason names this; the reconcile errors and is retried with backoff | Check that the URL is reachable from the fleet controller pod, and read the fleet controller logs. |
| `KairosFleetMachine` stuck at `WaitingForCapacity` | The target group has no unclaimed nodes | Confirm the group name matches an AuroraBoot group with enrolled, unclaimed nodes. |
| `KairosControlPlane` stuck `Available=False(WaitingForInfrastructureControlPlaneEndpoint)` | `KairosFleetCluster.spec.controlPlaneEndpoint.host` is unset or empty | Fleet does not allocate an endpoint; set it explicitly (Step 4 above). |
| Node comes back after reboot but the machine never leaves `WaitingForNodeRejoin` | The controller requires the node's heartbeat to be newer than the recorded reboot time | Check that the AuroraBoot node's `lastHeartbeat` is advancing; a node that never phones home after reboot (agent not running, network unreachable) never satisfies the gate. |
| Workload `Node` has no `providerID`, or it does not match `kairos-fleet://...` | The bootstrap cloud-config's fleet providerID self-discovery path did not run, or ran against the wrong distribution | Confirm the `KairosConfigTemplate` distribution matches your Kairos image, and that the node image runs the Kairos phone-home agent with persisted credentials (see "providerID cross-provider contract" above). |
| `KubeconfigReadyCondition` stays `False(WaitingForNodePush)` | No network route from the node to the management cluster API server | Run the TLS-verified check from [INSTALL.md](INSTALL.md#management-api-tls-verification-and-the-ca-source) — `curl --cacert /etc/kairos-capi/management-ca.crt https://<mgmt-api-server-host>:6443/api` from the node (`curl -k` will not catch a certificate mismatch); if it fails, open the network path or enable `SSHFallback`. |

---

## Next Steps

- Fleet HA control planes are not yet supported end to end — see
  [docs/HIGH_AVAILABILITY.md](HIGH_AVAILABILITY.md) for the current status.
- Add custom Kubernetes manifests via `spec.template.spec.manifests` in
  `KairosConfigTemplate`.
- Scale the worker `MachineDeployment` by editing `spec.replicas`, provided
  enough capacity is enrolled and unclaimed in the target AuroraBoot group.

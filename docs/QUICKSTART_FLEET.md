# Quick Start Guide - KairosFleetMachine infrastructure

Last verified against: this repository's v0.2.1 code (read, not run),
Cluster API v1.13.4 (v1beta2 contract), Kairos v4.1.2, k3s and k0s. The
steps below are not exercised end to end by this repository's CI. The
recorded real-hardware validation of a control plane plus a worker
`MachineDeployment` is in [TESTING.md](TESTING.md#supported-configurations-v010).
If a step does not match what you observe, treat this page as wrong and
file an issue.

This guide covers what the Kairos bootstrap and control-plane providers in
this repository do when the infrastructure objects are `KairosFleetMachine`
and `KairosFleetMachineTemplate` (API group `infrastructure.cluster.x-k8s.io`,
version `v1alpha1`). Those kinds, and the `KairosFleetCluster` that goes with
them, belong to the Kairos fleet infrastructure provider. Fill them in as
that provider requires; this guide does not describe their fields.

**Scope of this guide:**

- A single control-plane machine (`replicas: 1`) plus a 1-replica worker
  `MachineDeployment` (`kairos_cluster_{k0s,k3s}_with_workers.yaml`) is the
  validated topology. A standalone single node with no workers
  (`kairos_cluster_k0s_single_node.yaml`) is also covered.
- HA (`replicas: 3` or `5`) is not exercised with these infrastructure kinds,
  and no fleet HA sample is shipped. See
  [HIGH_AVAILABILITY.md](HIGH_AVAILABILITY.md).
- k3s and k0s are both rendered. A default single-node k0s control plane
  renders `--enable-worker`, so it is joinable and schedulable and the
  worker `MachineDeployment` can join it. Setting
  `spec.k0sSingleNode: true` on the `KairosConfigTemplate` renders
  `--single` instead: a standalone node that refuses all joins. See
  [kairos_cluster_k0s_single_node.yaml](../config/samples/fleet/kairos_cluster_k0s_single_node.yaml)
  and the `k0sSingleNode` row in [API_REFERENCE.md](API_REFERENCE.md).

---

## What this repository does for KairosFleetMachine

- **Control-plane Machines.** The control-plane provider reads the
  `KairosFleetMachineTemplate` named in
  `KairosControlPlane.spec.machineTemplate.infrastructureRef` and creates one
  `KairosFleetMachine` per Machine, copying `spec.template.spec` verbatim into
  the new object's `spec`. The API version is taken from the template
  reference and defaults to `v1alpha1`. If the reference omits the
  namespace, the `KairosControlPlane`'s own namespace is used. The
  control-plane provider's RBAC grants it access to `kairosfleetmachines`
  and `kairosfleetmachinetemplates`.
- **No providerID in the bootstrap Secret.** The bootstrap provider never
  embeds a `providerID` for these machines and does not wait for one, unlike
  other infrastructure providers. The node
  derives `kairos-fleet://<node-id>` itself, from the node id in the Kairos
  phone-home agent's persisted credentials
  (`/usr/local/.kairos/phonehome-credentials.yaml`), and the value must be a
  UUID or it is skipped. How it is applied depends on distribution and role:
  - k3s, both roles: `kairos-fleet-discover-provider-id.sh` writes a k3s
    `config.yaml.d` kubelet drop-in before k3s starts.
  - k0s control plane: the node patches its own `Node` with
    `k0s kubectl` after bootstrap, because only a control-plane node has
    `admin.conf`.
  - k0s worker: `kairos-k0s-fleet-provider-id.sh` writes a
    `KubeletConfiguration` drop-in that the kubelet reads through
    `--config-dir` before it registers.
- **Kubeconfig node-push.** Control-plane nodes on these machines push their
  kubeconfig to a staging Secret in the management cluster, like CAPV and
  CAPM3. Without this the
  `KairosControlPlane` never publishes a kubeconfig.
- **No install: block.** The rendered cloud-config is applied to a node that
  already runs Kairos, so do not add an `install:` block to either
  `KairosConfigTemplate`.

---

## Prerequisites

1. **Management cluster**: a Kubernetes cluster running Cluster API core
   v1.13.4+ (v1beta2 contract). See the version table in
   [README.md - Target Versions](../README.md#target-versions).

2. **The Kairos bootstrap and control-plane providers installed**, as
   described in [INSTALL.md](INSTALL.md). The infrastructure provider that
   serves `KairosFleetCluster`, `KairosFleetMachine` and
   `KairosFleetMachineTemplate` must also be installed, following its own
   instructions.

3. **The infrastructure objects filled in as the fleet infrastructure
   provider requires.** The samples mark the values you must supply with
   `TODO`. The control-plane endpoint is set on the `KairosFleetCluster` and
   copied into `Cluster.spec.controlPlaneEndpoint` by Cluster API core. Until
   that field is populated the `KairosControlPlane` reports
   `WaitingForInfrastructureControlPlaneEndpoint`.

4. **Network reachability from the nodes to the management cluster's API
   server.** Control-plane nodes push their kubeconfig there rather than being
   SSHed into. Set `KAIROS_MANAGEMENT_API_OVERRIDE` on the bootstrap
   provider's Deployment to a URL the nodes can reach; without it the nodes
   are given the in-cluster Service URL and cannot reach it. See
   [INSTALL.md - Set the URL the nodes push to](INSTALL.md#set-the-url-the-nodes-push-to)
   and
   [Network reachability requirement for non-CAPK infrastructure](INSTALL.md#network-reachability-requirement-for-non-capk-infrastructure).

5. **Optional: `SSHFallback`** for nodes that cannot reach the management
   cluster. It is off by default and is configured on the
   `KairosControlPlane`; see
   [QUICKSTART_CAPM3.md - Air-gapped fallback](QUICKSTART_CAPM3.md#air-gapped-fallback-sshfallback).
   The controller dials the address in `status.addresses` of the first
   control-plane Machine it owns that has one, preferring `InternalIP`, then
   `ExternalIP`, then any other type. If the Machine has no address there is
   nothing to dial and the fallback does not run.

6. **A worker join token** for the worker `KairosConfigTemplate`. A worker
   without one fails to render: k0s workers need `workerTokenSecretRef`
   (or one of its aliases), and k3s workers need `k3sTokenSecretRef` (or one
   of its aliases). Create the Secret out of band; the samples show the name
   and key they expect.

---

## Creating a cluster

### Step 1: Create the user-password Secret

```bash
kubectl create secret generic kairos-user-password \
  --from-literal=password=$(openssl rand -base64 32)
```

The sample manifests reference this Secret through `userPasswordSecretRef`.
Do not set `userPassword` inline. Each sample also contains a placeholder
Secret of the same name; replace its `password:` value or delete that
document before applying.

### Step 2: Choose a sample manifest

- k3s control plane plus worker `MachineDeployment`:
  [`config/samples/fleet/kairos_cluster_k3s_with_workers.yaml`](../config/samples/fleet/kairos_cluster_k3s_with_workers.yaml)
- k0s control plane plus worker `MachineDeployment`:
  [`config/samples/fleet/kairos_cluster_k0s_with_workers.yaml`](../config/samples/fleet/kairos_cluster_k0s_with_workers.yaml)
- k0s standalone single node, no workers (`spec.k0sSingleNode: true`):
  [`config/samples/fleet/kairos_cluster_k0s_single_node.yaml`](../config/samples/fleet/kairos_cluster_k0s_single_node.yaml)

### Step 3: Customize the manifest

Open the chosen sample and fill in every value marked `TODO`, including the
fleet infrastructure objects. Then check the settings that belong to this
repository:

```yaml
# In KairosControlPlane:
spec:
  replicas: 1
  version: "v1.34.3+k3s3"
  distribution: k3s     # explicit value wins over the template's

# In KairosConfigTemplate:
spec:
  template:
    spec:
      distribution: k3s
      kubernetesVersion: "v1.34.3+k3s3"
      # No install: block: the node already runs Kairos.
```

- `KairosControlPlane.spec.distribution` is not defaulted by the CRD. When it
  is empty the controller inherits the `KairosConfigTemplate`'s distribution,
  and falls back to `k0s` if the template sets none. An explicit value always
  wins and the controller emits a `DistributionOverride` warning event when
  it differs from the template. Set it explicitly and keep it equal to the
  template and to what your Kairos image ships.
- `version` and `kubernetesVersion` are informational for k0s and k3s. The
  Kubernetes version is whatever the Kairos image ships (KD-24;
  [API_REFERENCE.md](API_REFERENCE.md)).
- Do not set `spec.ha.vip` for a single control plane.

### Step 4: Apply the manifest

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

Watch the Cluster API objects. The sample names the cluster `fleet-demo`:

```bash
kubectl get cluster fleet-demo -w
kubectl get kairoscontrolplane fleet-demo-control-plane
kubectl get machines -n default
```

Read the conditions on the `KairosControlPlane` for the state of this
repository's side:

```bash
kubectl describe kairoscontrolplane fleet-demo-control-plane
```

The states of the `KairosFleetMachine` objects themselves are reported by the
fleet infrastructure provider and are not described here.

---

## Retrieve the kubeconfig and confirm the node joined correctly

```bash
kubectl get secret fleet-demo-kubeconfig \
  -o jsonpath='{.data.value}' | base64 -d > fleet-demo-kubeconfig.yaml
kubectl --kubeconfig=fleet-demo-kubeconfig.yaml get nodes
```

The control-plane node posts its kubeconfig to a staging Secret in the
management cluster at bootstrap time. The controller validates and
canonicalizes whatever it receives before publishing `<cluster>-kubeconfig`,
and reports progress on the `KubeconfigReady` condition. See
[API_REFERENCE.md - KubeconfigReady condition](API_REFERENCE.md#kubeconfigready-condition)
for the full set of reasons if the Secret does not appear or a kubeconfig you
extract looks unexpected. If the node has no route to the management API
server, either fix the route (prerequisite 4) or enable `SSHFallback`
(prerequisite 5).

### providerID: verify the match

A healthy Machine has the same `kairos-fleet://<node-id>` value on its
infrastructure object and on the workload `Node`. This repository produces
the node side of that value (see "What this repository does" above); compare
it against the Machine:

```bash
kubectl get machine -n default -o jsonpath='{.items[*].spec.providerID}'
kubectl --kubeconfig=fleet-demo-kubeconfig.yaml get nodes \
  -o jsonpath='{.items[*].spec.providerID}'
```

The two lists must contain the same values. If a node's value is missing or
different, the node never registers against the right Machine and the Machine
stays unhealthy. The node-side script logs to the node's journal and, when the
phone-home credentials file is missing or its `node_id` is not a UUID, it
leaves the providerID unset rather than failing: k3s then uses its default
and k0s skips the patch. Confirm the node image runs the Kairos phone-home
agent and has already written its credentials before k3s or k0s starts, and
that the distribution matches the image.

---

## Tear down

```bash
kubectl delete cluster fleet-demo -n default
```

Deleting the `Cluster` deletes its Machines and their `KairosFleetMachine`
objects. What happens to the underlying node afterwards is decided by the
fleet infrastructure provider, not by this repository.

---

## Troubleshooting

| Symptom | Cause | Action |
| --- | --- | --- |
| `KairosControlPlane` is `Available=False` with reason `WaitingForInfrastructureControlPlaneEndpoint` | `Cluster.spec.controlPlaneEndpoint` is empty: the infrastructure provider has not published an endpoint for Cluster API core to copy down | Set the control-plane endpoint on the `KairosFleetCluster` as the fleet infrastructure provider requires (the `TODO` value in the sample). The samples leave `Cluster.spec.controlPlaneEndpoint` unset. |
| `KubeconfigReady` stays `False` with reason `WaitingForNodePush` | The node cannot reach the management cluster's API server, or `KAIROS_MANAGEMENT_API_OVERRIDE` is unset and the node was given the in-cluster Service URL | Check the override and the TLS-verified path described in [INSTALL.md](INSTALL.md#management-api-tls-verification-and-the-ca-source): `curl --cacert /etc/kairos-capi/management-ca.crt https://<mgmt-api-server-host>:6443/api` from the node. If the path cannot be opened, enable `SSHFallback`. |
| Workload `Node` has no `providerID`, or it does not match the Machine's `kairos-fleet://...` value | The node-side discovery did not run or found no usable phone-home credentials (missing file, or `node_id` not a UUID), or the distribution does not match the image | Read the node's journal for `fleet:` log lines. Confirm the phone-home agent has written `/usr/local/.kairos/phonehome-credentials.yaml` before k3s or k0s starts, and that the `KairosConfigTemplate` distribution matches the image. |
| A `DistributionOverride` warning event appears on the `KairosControlPlane`, or k3s or k0s never starts on the node | `KairosControlPlane.spec.distribution` differs from the `KairosConfigTemplate`'s, or from the distribution the Kairos image ships; `spec.distribution` wins, so the control-plane cloud-config is rendered for it | Make `spec.distribution` and the template's `distribution` match each other and the image. |
| A worker Machine's bootstrap Secret is never created, and the bootstrap controller logs `failed to generate cloud-config` with a message about a missing worker token or join token | The worker `KairosConfigTemplate` has no join token | Add `workerTokenSecretRef` (k0s) or `k3sTokenSecretRef` (k3s) pointing at a Secret that exists. |

---

## Next Steps

- HA control planes are not exercised with these infrastructure kinds; see
  [HIGH_AVAILABILITY.md](HIGH_AVAILABILITY.md) for the current status.
- Add custom Kubernetes manifests via `spec.template.spec.manifests` in
  `KairosConfigTemplate`.

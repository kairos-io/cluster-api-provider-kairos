# Quick Start Guide - CAPV (vSphere)

Last verified against: Kairos v3.6.0+, CAPI v1.8.x, CAPV v1.11.x, provider v0.1.0-alpha.2.

This guide walks you through creating a single-node k0s or k3s cluster on Kairos using Cluster API with the vSphere provider (CAPV).

**Note on `controlPlaneEndpoint`**: both the k0s and k3s flavors require `Cluster.spec.controlPlaneEndpoint` (or `VSphereCluster.spec.controlPlaneEndpoint`) to be set to a stable LoadBalancer IP or VIP before applying the manifest. The provider does not auto-discover the endpoint. Standard CAPV practice is to pre-allocate an IP via your load-balancer solution and reference it in the sample before applying. If you apply without a valid endpoint, `KairosControlPlane` will stall with `Available=False(WaitingForInfrastructureControlPlaneEndpoint)` until the endpoint is set.

## Prerequisites

1. **vSphere Environment**:
   - vCenter Server access (FQDN or IP, no URL prefix).
   - Datacenter, Datastore, Network configured.
   - A Kairos VM template uploaded to vSphere with Kairos OS.
   - Resource Pool (optional).

2. **Management Cluster**: A Kubernetes cluster with network access to vSphere.

3. **Cluster API**: CAPI v1.8.x installed. v1.11.x is on the roadmap (KD-13).

4. **CAPV**: Cluster API Provider vSphere installed and configured.

5. **Kairos CAPI Provider**: Installed (see [Install guide](INSTALL.md)).

### Installing Prerequisites

#### 1. Install Cluster API and CAPV

Install CAPI and CAPV using your preferred method. See the [Cluster API book](https://cluster-api.sigs.k8s.io/) for details.

#### 2. Configure vSphere Credentials

The Secret **must** be in the `capv-system` namespace (where the CAPV controller runs), not in the cluster namespace.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: vsphere-credentials-secret
  namespace: capv-system
type: Opaque
stringData:
  username: administrator@vsphere.local
  password: <your-password>
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereClusterIdentity
metadata:
  name: vsphere-credentials
spec:
  secretName: vsphere-credentials-secret
  allowedNamespaces:
    selector:
      matchLabels:
        vsphere-identity: allowed
```

Apply and label the target namespace:

```bash
kubectl apply -f - <<'EOF'
# paste the YAML above
EOF
kubectl label namespace default vsphere-identity=allowed
```

#### 3. Install Kairos CAPI Provider

**Recommended (released artifact):**

```bash
kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0-alpha.2/kairos-capi-provider.yaml
```

**Developer install (from source):**

```bash
make docker-build
make deploy
```

See [INSTALL.md](INSTALL.md) for the full developer install process.

## Creating a Cluster

### Step 1: Create the user-password Secret

```bash
kubectl create secret generic kairos-user-password \
  --from-literal=password=$(openssl rand -base64 32)
```

The sample manifests reference this Secret via `userPasswordSecretRef`. Do not use `userPassword` inline.

### Step 2: Choose a sample manifest

- k0s single node: `config/samples/capv/kairos_cluster_k0s_single_node.yaml`
- k3s single node: `config/samples/capv/kairos_cluster_k3s_single_node.yaml`

### Step 3: Customize the manifest

Open the chosen sample and fill in the following `TODO` values.

#### VSphereCluster

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereCluster
spec:
  # Correct: "vcenter.example.com" or "172.16.56.10"
  # Wrong:   "https://vcenter.example.com"
  server: "TODO-YOUR-VCENTER-HOSTNAME"
  identityRef:
    kind: VSphereClusterIdentity
    name: vsphere-credentials
```

#### VSphereMachineTemplate

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereMachineTemplate
spec:
  template:
    spec:
      datacenter: "Datacenter"
      datastore: "Datastore"
      folder: "Folder"
      network:
        devices:
          - networkName: "VM Network"
      resourcePool: "ResourcePool"
      numCPUs: 2
      memoryMiB: 4096
      diskGiB: 50
      template: "kairos-template"
      cloneMode: "fullClone"
```

#### KairosConfigTemplate

```yaml
apiVersion: bootstrap.cluster.x-k8s.io/v1beta2
kind: KairosConfigTemplate
spec:
  template:
    spec:
      role: control-plane
      distribution: k0s       # or k3s
      kubernetesVersion: "v1.34.1+k0s.1"
      userPasswordSecretRef:
        name: kairos-user-password
      userGroups:
        - admin
      # Optional: SSH access
      # githubUser: "your-github-username"
      # sshPublicKey: "ssh-rsa AAAA..."
```

### Step 4: Apply the manifest

k0s:

```bash
kubectl apply -f config/samples/capv/kairos_cluster_k0s_single_node.yaml
```

k3s:

```bash
kubectl apply -f config/samples/capv/kairos_cluster_k3s_single_node.yaml
```

### Step 5: Monitor Cluster Creation

```bash
kubectl get cluster kairos-cluster -w
kubectl get kairoscontrolplane kairos-control-plane
kubectl get machines
kubectl get vspheremachines
kubectl get vspherevms
```

### Step 6: Retrieve the Kubeconfig

```bash
kubectl get secret kairos-cluster-kubeconfig \
  -o jsonpath='{.data.value}' | base64 -d > kairos-kubeconfig.yaml
kubectl --kubeconfig=kairos-kubeconfig.yaml get nodes
kubectl --kubeconfig=kairos-kubeconfig.yaml get pods -n kube-system
```

**Note (alpha-2+):** As of v0.1.0-alpha.2, the control-plane controller no
longer SSHes into nodes to retrieve the kubeconfig. The node pushes its
kubeconfig to a Secret in the management cluster at bootstrap time. The
workload VM must have network reachability to the management cluster's API
server (`<mgmt-api-server-host>:6443`). See
[docs/INSTALL.md](INSTALL.md#network-reachability-requirement-for-non-capk-infrastructure)
for verification steps. If the VM has no network path to the API server,
enable the opt-in
[Air-gapped fallback (SSHFallback)](#air-gapped-fallback-sshfallback)
on the `KairosControlPlane`.

## Field Reference

### Required Fields to Customize

| Field | Location | Description |
|-------|----------|-------------|
| `server` | VSphereCluster.spec | vCenter server FQDN or IP (hostname/IP only, not a URL). |
| `datacenter` | VSphereMachineTemplate.spec | Datacenter name. |
| `datastore` | VSphereMachineTemplate.spec | Datastore name. |
| `networkName` | VSphereMachineTemplate.spec.network.devices | VM Network name. |
| `template` | VSphereMachineTemplate.spec | Kairos VM template name in vSphere. |

### Optional Fields

| Field | Location | Description |
|-------|----------|-------------|
| `thumbprint` | VSphereCluster.spec | SSL thumbprint for vCenter certificate validation. |
| `folder` | VSphereMachineTemplate.spec | VM folder path. |
| `resourcePool` | VSphereMachineTemplate.spec | Resource pool path. |
| `numCPUs` | VSphereMachineTemplate.spec | CPU count. |
| `memoryMiB` | VSphereMachineTemplate.spec | Memory in MiB. |
| `diskGiB` | VSphereMachineTemplate.spec | Disk size in GiB. |
| `cloneMode` | VSphereMachineTemplate.spec | `"fullClone"` or `"linkedClone"`. |

## Troubleshooting

### VSphere Connection Issues

```bash
kubectl describe vspherecluster kairos-cluster
kubectl get vsphereclusteridentity vsphere-credentials -o yaml
kubectl get secret vsphere-credentials-secret -n capv-system -o yaml
kubectl logs -n capv-system deployment/capv-controller-manager
```

Common causes:

1. Secret not in `capv-system` namespace — move it: `kubectl get secret vsphere-credentials-secret -n default -o yaml | kubectl apply -n capv-system -f -`
2. `server` field includes protocol or path — use hostname/IP only: `"172.16.56.10"` not `"https://172.16.56.10/sdk"`.
3. Namespace not labeled — run: `kubectl label namespace default vsphere-identity=allowed`.

### VM Creation Fails

```bash
kubectl describe vspheremachine <machine-name>
kubectl describe vspherevm <vm-name>
```

Verify template exists in vSphere and network/datastore names match exactly.

### Bootstrap Issues

```bash
kubectl describe kairosconfig <config-name>
kubectl logs -n kairos-capi-system deployment/kairos-capi-controller-manager
```

If `KairosConfig.status.failureMessage` is set, the issue is transient — it clears automatically when resolved. A missing `kairos-user-password` Secret is the most common first-run cause.

## Security Considerations

- **vSphere credentials**: Use `VSphereClusterIdentity` with a Secret in `capv-system`. Do not use inline credentials in `VSphereCluster.spec`.
- **User password**: Provide via `userPasswordSecretRef`. The webhook rejects KairosConfig objects with no credential. Do not set `userPassword` inline outside of throwaway testing.
- **SSH access**: Add `githubUser` or `sshPublicKey` if you need SSH access to nodes. Password-based SSH should not be the primary access mechanism.
- **Worker tokens**: Use `k3sTokenSecretRef` / `workerTokenSecretRef` rather than inline tokens.

## Next Steps

- Configure additional worker nodes via `MachineDeployment`.
- Add custom Kubernetes manifests via `spec.manifests` in `KairosConfigTemplate`.
- Multi-node control planes are tracked for a future release (KD-5b / KD-25).

## Air-gapped fallback (SSHFallback)

**When to use:** the workload VM has no network route back to the management
cluster API server (`<mgmt-api-server-host>:6443`). The node-push path —
where the VM POSTs its kubeconfig to a management cluster Secret — is
unreachable. Without this fallback, `KubeconfigReadyCondition` stays
`False(WaitingForNodePush)` indefinitely.

**When NOT to use:** the default node-push path works in most networks. Do
not enable SSHFallback unless you have confirmed that the VM cannot reach
the management API server. The fallback is an escape hatch, not a
replacement for node-push.

**Security requirement:** host-key verification is mandatory. The controller
verifies the workload node's SSH host key against a `known_hosts` Secret
before any data is exchanged. There is no trust-on-first-use mode.
`activateAfter` must be at least 15 minutes (greater than the
`KubeconfigReadyCondition` Info→Warning threshold of 10 minutes).

### Step 1: Create the SSH identity Secret

The Secret holds the private key the controller uses to authenticate to the
workload node. The corresponding public key must already be installed on the
node via `KairosConfig.spec.sshPublicKey` or `KairosConfig.spec.githubUser`.

```bash
kubectl create secret generic kairos-ssh-identity \
  --type=kubernetes.io/ssh-auth \
  --from-file=ssh-privatekey=/path/to/your/private_key \
  -n <cluster-namespace>
```

### Step 2: Create the known-hosts Secret

Obtain the workload node's SSH host key (for example with `ssh-keyscan`
against the VM's IP while you still have network access, or from the Kairos
OS image's pre-generated host key material). The `known_hosts` format is
standard OpenSSH: one `<host> <key-type> <base64-key>` line per entry.

```bash
kubectl create secret generic kairos-ssh-known-hosts \
  --from-file=known_hosts=/path/to/known_hosts_file \
  -n <cluster-namespace>
```

### Step 3: Enable SSHFallback on the KairosControlPlane

Add the following stanza to your `KairosControlPlane` spec. All values shown
are defaults except the Secret names, which you must set:

```yaml
spec:
  sshFallback:
    enabled: true
    user: kairos          # default; must match the node's SSH user
    port: 22              # default
    activateAfter: 15m    # default; must exceed kubeconfigReadyTimeout (10m)
    identitySecretRef:
      name: kairos-ssh-identity     # Secret from Step 1
    knownHostsSecretRef:
      name: kairos-ssh-known-hosts  # Secret from Step 2
```

Both Secrets must be in the same namespace as the `KairosControlPlane`. The
webhook rejects cross-namespace references.

Apply the updated manifest:

```bash
kubectl apply -f your-kairoscontrolplane.yaml
```

### Step 4: Verify activation

The fallback fires `activateAfter` after `KubeconfigReadyCondition` first
becomes `False(WaitingForNodePush)`. Watch the condition Reason with:

```bash
kubectl describe kairoscontrolplane <name> -n <cluster-namespace>
```

The `KubeconfigReadyCondition` Reason tells you which path is active:

| Reason | Meaning |
|--------|---------|
| `KubeconfigReady` | Node-push succeeded (normal path; SSHFallback did not fire). |
| `KubeconfigReadyViaSSHFallback` | SSH fallback supplied the kubeconfig. |
| `SSHFallbackDialing` | SSH fallback is in progress; wait up to 30 seconds. |
| `SSHFallbackFailed` | SSH attempt failed. Check Events for the categorized error (host-key mismatch, auth failure, file not found). |
| `SSHFallbackMisconfigured` | A referenced Secret is missing, empty, or unparseable. Fix the Secret; the controller retries automatically. |

For detailed error information when the Reason is `SSHFallbackFailed` or
`SSHFallbackMisconfigured`:

```bash
kubectl get events -n <cluster-namespace> \
  --field-selector involvedObject.name=<kairoscontrolplane-name> \
  --sort-by='.lastTimestamp'
```

## Cleanup

```bash
kubectl delete -f config/samples/capv/kairos_cluster_k0s_single_node.yaml
# Note: This deletes VMs in vSphere.
```

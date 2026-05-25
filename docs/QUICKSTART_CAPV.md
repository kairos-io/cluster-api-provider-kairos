# Quick Start Guide - CAPV (vSphere)

Last verified against: Kairos v3.6.0+, CAPI v1.8.x, CAPV v1.11.x, provider v0.1.0-alpha.2.

This guide walks you through creating a single-node k0s or k3s cluster on Kairos using Cluster API with the vSphere provider (CAPV).

**Note on k3s vs k0s**: the k3s flavor requires `Cluster.spec.controlPlaneEndpoint` to be set to a stable LoadBalancer IP or VIP before applying the manifest, because k3s agents join via the control-plane endpoint. The k0s flavor does not require a pre-set endpoint — the controller discovers the node IP after the VM is provisioned.

## Prerequisites

1. **vSphere Environment**:
   - vCenter Server access (FQDN or IP, no URL prefix).
   - Datacenter, Datastore, Network configured.
   - A Kairos VM template uploaded to vSphere with Kairos OS.
   - Resource Pool (optional).

   Example template preparation (vSphere UI):
   1. Create a new VM with the hardware profile you want your nodes to use (for example: 2 vCPU, 4 GiB RAM, 50 GiB disk, VMXNET3 NIC on your target network).
   2. Attach the Kairos ISO to the VM CD-ROM.
   3. Do **not** power on the VM.
   4. Right-click the VM and convert it to a template.
   5. Use that template name in `VSphereMachineTemplate.spec.template` (for example `kairos-opensuse-leap-v3.6.0`).

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
kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0-alpha.1/kairos-capi-provider.yaml
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

**Note:** The kubeconfig retrieval currently uses SSH to reach the control plane node (KD-3b tracks removing SSH from the normal flow in a future release). Ensure the Kairos user credentials allow SSH access until that work lands (PR-7 in the alpha-2 sequence will eliminate SSH from the control plane path).

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
- SSH will be removed from the normal control-plane flow in a future PR (KD-3b).

## Cleanup

```bash
kubectl delete -f config/samples/capv/kairos_cluster_k0s_single_node.yaml
# Note: This deletes VMs in vSphere.
```

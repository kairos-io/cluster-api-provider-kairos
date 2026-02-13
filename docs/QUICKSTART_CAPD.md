# Quick Start Guide - CAPD (Docker)

This guide walks you through creating a single-node k0s cluster on Kairos using Cluster API with the Docker provider (CAPD).

## Prerequisites

1. **Management Cluster**: A Kubernetes cluster (kind, minikube, or any Kubernetes cluster)
2. **Cluster API**: CAPI v1.11+ installed
3. **CAPD**: Cluster API Provider Docker installed
4. **Kairos CAPI Provider**: Installed via make (see [Install guide](INSTALL.md))

### Installing Prerequisites

#### 1. Install Cluster API and CAPD

Install CAPI and CAPD using your preferred method. See the [Cluster API book](https://cluster-api.sigs.k8s.io/) for details.

#### 2. Install Kairos CAPI Provider

```bash
make docker-build
make deploy
```

See [INSTALL.md](INSTALL.md) for full install steps.

## Creating a Cluster

### Step 1: Review the Sample Manifest

The sample manifest is located at `config/samples/capd/kairos_cluster_k0s_single_node.yaml`.

Key components:
- `Cluster` - References DockerCluster and KairosControlPlane
- `DockerCluster` - Infrastructure cluster for Docker
- `KairosControlPlane` - Control plane with replicas=1 (single-node)
- `DockerMachineTemplate` - Template for Docker machines
- `KairosConfigTemplate` - Bootstrap configuration template

### Step 2: Customize the Manifest (Optional)

Edit `config/samples/capd/kairos_cluster_k0s_single_node.yaml`:

- Update `spec.version` in `KairosControlPlane` to your desired k0s version
- Add `githubUser` or `sshPublicKey` in `KairosConfigTemplate` for SSH access
- Change `userName`/`userPassword` if needed (default: kairos/kairos)

### Step 3: Apply the Manifest

```bash
kubectl apply -f config/samples/capd/kairos_cluster_k0s_single_node.yaml
```

### Step 4: Wait for Cluster to be Ready

```bash
# Watch the cluster status
kubectl get cluster kairos-cluster -w

# Check control plane status
kubectl get kairoscontrolplane kairos-control-plane

# Check machines
kubectl get machines

# Once ready, check nodes
kubectl get nodes --kubeconfig=<path-to-kubeconfig>
```

### Step 5: Verify k0s is Running

```bash
# Get kubeconfig for the cluster (from the cluster secret)
kubectl get secret kairos-cluster-kubeconfig -o jsonpath='{.data.value}' | base64 -d > kairos-kubeconfig.yaml

# Check nodes
kubectl --kubeconfig=kairos-kubeconfig.yaml get nodes

# Check k0s pods (if accessible)
kubectl --kubeconfig=kairos-kubeconfig.yaml get pods -n kube-system
```

Notes:
- Kubeconfig retrieval may use SSH to the control plane node depending on the infrastructure provider. Ensure SSH access is available for the Kairos user credentials in your KairosConfigTemplate.

## Troubleshooting

### Cluster Not Ready

```bash
# Check cluster conditions
kubectl describe cluster kairos-cluster

# Check control plane conditions
kubectl describe kairoscontrolplane kairos-control-plane

# Check machine status
kubectl describe machine <machine-name>

# Check bootstrap config
kubectl describe kairosconfig <kairosconfig-name>
kubectl get secret <dataSecretName> -o yaml
```

### Bootstrap Data Not Generated

```bash
# Check bootstrap controller logs
kubectl logs -n kairos-capi-system deployment/kairos-capi-controller-manager | grep bootstrap

# Verify KairosConfig has required fields
kubectl get kairosconfig -o yaml
```

### Worker Token Issues

For worker nodes, ensure:
- `WorkerToken` or `WorkerTokenSecretRef` is set in `KairosConfig`
- Token secret exists and contains the correct key
- Token is valid for joining the cluster

## Adding Worker Nodes

Once your control plane is ready, you can add worker nodes using a `MachineDeployment`.

### Step 1: Get Worker Token

First, you need to obtain a worker join token from your k0s control plane:

```bash
# Get kubeconfig for the cluster (from the cluster secret)
kubectl get secret kairos-cluster-kubeconfig -o jsonpath='{.data.value}' | base64 -d > kairos-kubeconfig.yaml

# Option 1: If you have access to the control plane node
# SSH into the control plane and run:
# k0s token create --role=worker

# Option 2: Use kubectl to exec into k0s controller pod
kubectl --kubeconfig=kairos-kubeconfig.yaml exec -n kube-system \
  $(kubectl --kubeconfig=kairos-kubeconfig.yaml get pods -n kube-system -l app=k0s-controller -o jsonpath='{.items[0].metadata.name}') \
  -- k0s token create --role=worker
```

### Step 2: Create Worker Token Secret

Create a Secret with the worker token:

```bash
kubectl create secret generic kairos-worker-token \
  --from-literal=token="<your-worker-token-here>" \
  -n default
```

### Step 3: Apply Worker Sample

Apply the worker sample manifest:

```bash
kubectl apply -f config/samples/capd/kairos_cluster_k0s_with_workers.yaml
```

This creates:
- A `MachineDeployment` for worker nodes
- A `KairosConfigTemplate` for worker bootstrap configuration
- A `DockerMachineTemplate` for worker infrastructure

### Step 4: Verify Worker Nodes

```bash
# Watch machines being created
kubectl get machines -w

# Once machines are ready, check nodes in the cluster
kubectl --kubeconfig=kairos-kubeconfig.yaml get nodes

# You should see worker nodes joining
```

### Worker Configuration Options

The worker `KairosConfigTemplate` supports:

- **Worker Token**: Use `workerTokenSecretRef` (recommended) or inline `workerToken`
- **SSH Access**: Configure via `githubUser` or `sshPublicKey`
- **Custom Manifests**: Add Kubernetes manifests via `spec.manifests`

## Next Steps

- Configure additional manifests via `spec.manifests` in `KairosConfig`
- Explore multi-node control plane (set `spec.replicas > 1`)
- Scale worker nodes by updating `MachineDeployment.spec.replicas`

## Cleanup

```bash
# Delete the cluster
kubectl delete -f config/samples/capd/kairos_cluster_k0s_single_node.yaml
```


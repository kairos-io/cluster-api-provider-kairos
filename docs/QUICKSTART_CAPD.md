# Quick Start Guide - CAPD (Docker)

Last verified against: Kairos v3.6.0+, CAPI v1.9+ (lab-validated v1.12.x), provider v0.1.0-alpha.2.

This guide walks you through creating a single-node k0s cluster on Kairos using Cluster API with the Docker provider (CAPD).

**Note:** The CAPD sample uses k0s. For k3s clusters, use [CAPV](QUICKSTART_CAPV.md) or [CAPK](QUICKSTART_CAPK.md).

**Note:** CAPD is a development-only infrastructure provider. Docker-based Kairos clusters do not represent a production topology.

**Note on `controlPlaneEndpoint`**: the sample manifest requires `Cluster.spec.controlPlaneEndpoint` to be set to a reachable host and port before applying. CAPD (like CAPV) does not auto-provision an endpoint; you must supply one. For local kind-based management clusters the Docker container's IP or a host port mapping is the typical choice. Without a valid endpoint, `KairosControlPlane` stalls with `Available=False(WaitingForInfrastructureControlPlaneEndpoint)`.

## Prerequisites

1. **Management Cluster**: A Kubernetes cluster (kind, minikube, or any Kubernetes cluster).
2. **Cluster API**: CAPI v1.9+ installed (v1beta2 wire contract; lab-validated against v1.12.x).
3. **CAPD**: Cluster API Provider Docker installed.
4. **Kairos CAPI Provider**: Installed (see [Install guide](INSTALL.md)).

### Installing Prerequisites

#### 1. Install Cluster API and CAPD

Install CAPI and CAPD using your preferred method. See the [Cluster API book](https://cluster-api.sigs.k8s.io/) for details.

#### 2. Install Kairos CAPI Provider

**Recommended (released artifact):**

```bash
kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0-alpha.2/kairos-capi-provider.yaml
```

**Developer install (from source):**

```bash
make docker-build
make deploy
```

See [INSTALL.md](INSTALL.md) for the full developer install process, including the kustomize-CRD note for re-installs.

## Creating a Cluster

### Step 1: Create the user-password Secret

```bash
kubectl create secret generic kairos-user-password \
  --from-literal=password=$(openssl rand -base64 32)
```

The sample manifest references this Secret via `userPasswordSecretRef`. Do not use `userPassword` inline for anything beyond throwaway local testing.

### Step 2: Review the Sample Manifest

The sample manifest is at `config/samples/capd/kairos_cluster_k0s_single_node.yaml`.

Key components:

- `Secret` — user password (referenced by `userPasswordSecretRef` in KairosConfigTemplate).
- `Cluster` — references `DockerCluster` and `KairosControlPlane`.
- `DockerCluster` — Docker infrastructure cluster.
- `KairosControlPlane` — control plane with `replicas: 1` (single-node only; HA is not yet supported).
- `DockerMachineTemplate` — template for Docker machines.
- `KairosConfigTemplate` — bootstrap configuration with `userPasswordSecretRef`.

### Step 3: Apply the Manifest

```bash
kubectl apply -f config/samples/capd/kairos_cluster_k0s_single_node.yaml
```

### Step 4: Wait for the Cluster to be Ready

```bash
# Watch cluster status
kubectl get cluster kairos-cluster -w

# Check control plane status
kubectl get kairoscontrolplane kairos-control-plane

# Check machines
kubectl get machines
```

### Step 5: Retrieve the Kubeconfig

```bash
kubectl get secret kairos-cluster-kubeconfig \
  -o jsonpath='{.data.value}' | base64 -d > kairos-kubeconfig.yaml
kubectl --kubeconfig=kairos-kubeconfig.yaml get nodes
kubectl --kubeconfig=kairos-kubeconfig.yaml get pods -n kube-system
```

## Troubleshooting

### Cluster Not Ready

```bash
kubectl describe cluster kairos-cluster
kubectl describe kairoscontrolplane kairos-control-plane
kubectl describe machine <machine-name>
kubectl describe kairosconfig <kairosconfig-name>
```

### Bootstrap Data Not Generated

```bash
kubectl logs -n kairos-capi-system deployment/kairos-capi-controller-manager
kubectl get kairosconfig -o yaml
```

If `KairosConfig.status.failureMessage` is set, the last reconcile failed. The message clears automatically when the underlying condition is resolved (for example, if a missing Secret is created). This is a transient failure, not a terminal one.

### Missing User-Password Secret

If `failureMessage` contains "secret not found" for `kairos-user-password`, create the Secret:

```bash
kubectl create secret generic kairos-user-password \
  --from-literal=password=$(openssl rand -base64 32)
```

The controller retries automatically.

### Worker Token Issues

For worker nodes, ensure `workerTokenSecretRef` references an existing Secret containing the correct key (default: `token`). Retrieve a k0s worker token from the control plane:

```bash
kubectl get secret kairos-cluster-kubeconfig \
  -o jsonpath='{.data.value}' | base64 -d > kairos-kubeconfig.yaml
kubectl --kubeconfig=kairos-kubeconfig.yaml exec -n kube-system \
  $(kubectl --kubeconfig=kairos-kubeconfig.yaml get pods -n kube-system \
    -l app=k0s-controller -o jsonpath='{.items[0].metadata.name}') \
  -- k0s token create --role=worker
```

## Adding Worker Nodes

Once your control plane is ready, apply the workers sample:

```bash
kubectl apply -f config/samples/capd/kairos_cluster_k0s_with_workers.yaml
```

This adds a `MachineDeployment` for worker nodes referencing a separate `KairosConfigTemplate` that also uses `userPasswordSecretRef`. The `kairos-worker-token` Secret referenced in that template must be created first.

## Next Steps

- Configure additional Kubernetes manifests via `spec.manifests` in `KairosConfigTemplate`.
- Scale worker nodes by updating `MachineDeployment.spec.replicas`.
- Multi-node control planes are tracked for a future release (KD-5b / KD-25).

## Cleanup

```bash
kubectl delete -f config/samples/capd/kairos_cluster_k0s_single_node.yaml
```

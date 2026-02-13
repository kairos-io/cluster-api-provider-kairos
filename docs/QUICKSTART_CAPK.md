# Quickstart: CAPK (KubeVirt)

This guide sets up a local KubeVirt environment and provisions a single-node Kairos+k0s or Kairos+k3s cluster using CAPK.

## Prerequisites

- `docker`
- `kind`
- `kubectl`
- `helm`
- `virtctl` (from KubeVirt releases)
- Go toolchain (for building `kubevirt-env`)

## Build the local helper

```bash
make kubevirt-env
```

Or manually:

```bash
go build -o bin/kubevirt-env ./cmd/kubevirt-env
```

## Create the local KubeVirt environment
```
./bin/kubevirt-env setup
```

Notes:
- `kubevirt-env setup` creates a kind cluster, installs a default StorageClass, Calico, CDI, KubeVirt, CAPI, CAPK, osbuilder, cert-manager, and Kairos CAPI.
- KubeVirt emulation is enabled by default (set `KUBEVIRT_USE_EMULATION=false` to disable).
- To pin CAPK to a specific version, set `CAPK_VERSION` (e.g., `CAPK_VERSION=v0.1.x`).
- The control-plane API is exposed via a mandatory LoadBalancer Service named `<cluster>-control-plane-lb`. Ensure a LoadBalancer implementation is available (for example, MetalLB in kind environments).
- The controller expects the kubeconfig secret to be created in the management cluster as `<cluster>-kubeconfig`.

## Build and upload a Kairos image
```
./bin/kubevirt-env build-kairos-image
./bin/kubevirt-env upload-kairos-image
```

## Create a test cluster

```bash
kubectl apply -f config/samples/capk/kubevirt_cluster_k0s_single_node.yaml
```

For k3s:

```bash
kubectl apply -f config/samples/capk/kubevirt_cluster_k3s_single_node.yaml
```

## Check status
```
./bin/kubevirt-env test-cluster-status
```

Optional checks (cluster name from sample is `kairos-cluster-kv`):

```bash
kubectl get svc kairos-cluster-kv-control-plane-lb
kubectl get secret kairos-cluster-kv-kubeconfig
```

## Optional: Run scripted flow

```bash
make kubevirt-env
make test-kubevirt
```

## Cleanup
```
./bin/kubevirt-env delete-test-cluster
./bin/kubevirt-env cleanup
```

## Troubleshooting
- If VMs do not start, confirm KubeVirt is `Available` and that `local-path` is the default StorageClass.
- If you have `/dev/kvm` available and want hardware acceleration, set `KUBEVIRT_USE_EMULATION=false` before `kubevirt-env setup`.
- If you use bridged/multus networking and the management cluster stays NotReady, ensure `spec.controlPlaneEndpoint.host` is reachable from the CAPI controllers; KubevirtMachine status may not report VM IPs in this mode.
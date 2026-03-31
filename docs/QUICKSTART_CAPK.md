# Quickstart: CAPK (KubeVirt)

This guide sets up a local KubeVirt environment and provisions a single-node Kairos+k0s or Kairos+k3s cluster using CAPK.

## Prerequisites

- `docker`
- Go toolchain (for building `kubevirt-env`)
- Network access to `github.com` (remote `kubectl apply -k` for the Kairos operator)

Pinned `kubectl`, `kind`, `clusterctl`, and `virtctl` are downloaded into `.work-kubevirt-<cluster-name>/bin` on first run.

## Build the local helper

```bash
make kubevirt-env
```

Or manually:

```bash
go build -o bin/kubevirt-env ./cmd/kubevirt-env
```

## Create the local KubeVirt environment

```bash
./bin/kubevirt-env
```

Notes:
- The root command creates a kind cluster (name from `--cluster-name` or `CLUSTER_NAME`), installs Calico (required first: default CNI is off), then local-path, CDI, KubeVirt, CAPI, CAPK, the [Kairos operator](https://kairos.io/docs/operator-docs/installation/) plus its nginx artifact sink, builds and uploads the Kairos cloud image, cert-manager, and the Kairos CAPI provider (release image).
- KubeVirt emulation is enabled by default (set `KUBEVIRT_USE_EMULATION=false` to disable).
- To pin CAPK to a specific version, set `CAPK_VERSION` (e.g., `CAPK_VERSION=v0.1.x`).
- The control-plane API is exposed via a mandatory LoadBalancer Service named `<cluster>-control-plane-lb`. Ensure a LoadBalancer implementation is available (for example, MetalLB in kind environments).
- The controller expects the kubeconfig secret to be created in the management cluster as `<cluster>-kubeconfig`.

## Create a test cluster

```bash
kubectl apply -f config/samples/capk/kubevirt_cluster_k0s_single_node.yaml
```

For k3s:

```bash
kubectl apply -f config/samples/capk/kubevirt_cluster_k3s_single_node.yaml
```

## Check status

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

Delete workload resources with `kubectl` as needed, then remove the management kind cluster and work directory:

```bash
./bin/kubevirt-env cleanup
```

## Troubleshooting
- If VMs do not start, confirm KubeVirt is `Available` and that `local-path` is the default StorageClass.
- If you have `/dev/kvm` available and want hardware acceleration, set `KUBEVIRT_USE_EMULATION=false` before running `kubevirt-env`.
- If you use bridged/multus networking and the management cluster stays NotReady, ensure `spec.controlPlaneEndpoint.host` is reachable from the CAPI controllers; KubevirtMachine status may not report VM IPs in this mode.
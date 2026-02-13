# Install Guide

This guide describes how to install the Kairos CAPI provider. All steps use `make` targets.

## Prerequisites

- A Kubernetes cluster as your management cluster (e.g. kind, minikube)
- CAPI and an infrastructure provider (CAPD, CAPV, or CAPK) already installed
- `kubectl` configured to use the management cluster

## Install

```bash
make deploy
```

This installs CRDs, RBAC, webhooks, and the controller to the `kairos-capi-system` namespace. The controller uses the image from `IMG` (default: `ghcr.io/kairos-io/kairos-capi:latest`). To use a different image:

```bash
IMG=MY_REGISTRY/kairos-capi:v1.0 make deploy
```

## Verify

```bash
kubectl get pods -n kairos-capi-system
kubectl get crd | grep kairos
```

## Run locally (optional)

To run the controller on your host instead of deploying to the cluster:

```bash
make install
make run
```

`make install` applies only the CRDs. Ensure your kubeconfig points to the management cluster. The controller will run in the foreground.

## Uninstall

```bash
make undeploy
make uninstall
```

## Next Steps

- [CAPD Quickstart](QUICKSTART_CAPD.md) - Create a cluster with Docker
- [CAPV Quickstart](QUICKSTART_CAPV.md) - Create a cluster with vSphere
- [CAPK Quickstart](QUICKSTART_CAPK.md) - Create a cluster with KubeVirt

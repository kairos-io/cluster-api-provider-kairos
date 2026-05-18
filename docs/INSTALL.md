# Install Guide

Two install paths: the released artifact (recommended for users) and a developer install from source.

## Path 1 — Released artifact (`kubectl apply`)

Use this if you want to consume a tagged release.

### Prerequisites

1. A Kubernetes cluster acting as the management cluster (kind, EKS, GKE, AKS, etc.).
2. **cert-manager v1.15+** installed:
   ```bash
   kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.15.2/cert-manager.yaml
   kubectl wait --for=condition=Available --timeout=2m -n cert-manager deploy/cert-manager-webhook
   ```
3. **Cluster API core** installed. Easiest path:
   ```bash
   clusterctl init --infrastructure docker   # or vsphere, kubevirt, …
   ```
   `clusterctl init` installs Cluster API core as a side effect of installing the infrastructure provider.
4. `kubectl` configured to use the management cluster.

### Install

```bash
kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0-alpha.1/kairos-capi-provider.yaml
```

This applies the all-in-one provider manifest: CRDs, RBAC, webhook configurations, and the controller Deployment in the `kairos-capi-system` namespace.

### Verify

```bash
kubectl get pods -n kairos-capi-system
kubectl get crds | grep kairos
```

Expected: one Deployment `kairos-capi-controller-manager` in `kairos-capi-system` with status `Available`, and four CRDs:

- `kairosconfigs.bootstrap.cluster.x-k8s.io`
- `kairosconfigtemplates.bootstrap.cluster.x-k8s.io`
- `kairoscontrolplanes.controlplane.cluster.x-k8s.io`
- `kairoscontrolplanetemplates.controlplane.cluster.x-k8s.io`

### Uninstall

```bash
kubectl delete -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0-alpha.1/kairos-capi-provider.yaml
```

> **Note**: the provider's `reconcileDelete` does not yet clean up child resources (KairosConfig, owned secrets, InfraMachines) when a managed `Cluster` is deleted. Manually clean up any clusters created with this release before uninstalling the provider.

---

## Path 2 — Developer install (from source)

Use this if you're hacking on the provider itself.

### Prerequisites

- Go 1.26+ toolchain.
- A Kubernetes cluster acting as the management cluster.
- cert-manager and Cluster API core installed (same as Path 1).
- `kubectl` configured to use the management cluster.

### Install

```bash
git clone https://github.com/kairos-io/cluster-api-provider-kairos.git
cd cluster-api-provider-kairos
make deploy
```

This installs CRDs, RBAC, webhooks, and the controller to the `kairos-capi-system` namespace. The controller uses the image from `IMG` (default: `ghcr.io/kairos-io/cluster-api-provider-kairos:latest`). To use a different image:

```bash
IMG=MY_REGISTRY/cluster-api-provider-kairos:dev make deploy
```

### Run the controller on your host (optional)

To run the controller on your host instead of deploying to the cluster:

```bash
make install     # installs only the CRDs
make run         # runs the controller in the foreground against your kubeconfig
```

### Uninstall

```bash
make undeploy
make uninstall
```

---

## Next steps

- [CAPD Quickstart](QUICKSTART_CAPD.md) — create a cluster with Docker (development only).
- [CAPV Quickstart](QUICKSTART_CAPV.md) — create a cluster with vSphere.
- [CAPK Quickstart](QUICKSTART_CAPK.md) — create a cluster with KubeVirt.

For the current release status, known issues, and security caveats, read the [release notes](release-notes/v0.1.0-alpha.1.md).

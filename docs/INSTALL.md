# Install Guide

Last verified against: CAPI v1.8.x, cert-manager v1.15+, provider v0.1.0-alpha.2.

Two install paths: the released artifact (recommended for users) and a developer install from source.

## Path 1 — Released artifact (`kubectl apply`)

Use this if you want to consume a tagged release.

### Prerequisites

1. A Kubernetes cluster acting as the management cluster (kind, EKS, GKE, AKS, etc.).
2. **cert-manager v1.15+** installed. Verify it is present before continuing:
   ```bash
   kubectl get crd certificates.cert-manager.io
   ```
   If not installed:
   ```bash
   kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.15.2/cert-manager.yaml
   kubectl wait --for=condition=Available --timeout=2m -n cert-manager deploy/cert-manager-webhook
   ```
3. **Cluster API core** installed. Easiest path:
   ```bash
   kubectl apply -f https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.8.0/cluster-api-components.yaml
   ```
   Or use `clusterctl init --infrastructure docker` (or vsphere, kubevirt, ...) if you already have clusterctl configured — `clusterctl init` installs Cluster API core as a side effect of installing the infrastructure provider.
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

**Re-install note**: if you are re-installing across a name-prefix change or a previous failed install, stale `MutatingWebhookConfiguration` and `ValidatingWebhookConfiguration` objects from the previous install may point at a webhook Service that no longer exists. Delete them before re-installing:

```bash
kubectl get mutatingwebhookconfigurations | grep kairos
kubectl get validatingwebhookconfigurations | grep kairos
# Delete any stale entries before re-applying
kubectl delete mutatingwebhookconfiguration <stale-name>
kubectl delete validatingwebhookconfiguration <stale-name>
```

---

## Path 2 — Developer install (from source)

Use this if you are hacking on the provider itself.

### Prerequisites

- Go toolchain 1.26.3 (matches `go.mod` directive `go 1.26.0` / toolchain `go1.26.0`).
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

**CRD installation note**: `make deploy` uses `kustomize build config/crd | kubectl apply -f -`, which applies kustomize-managed labels that CAPI's conversion webhook expects (`cluster.x-k8s.io/v1beta1`, `cluster.x-k8s.io/v1beta2`). If you apply raw CRD YAML via `kubectl apply -f config/crd/bases/`, those labels are absent and CAPI core may reject the CRDs. Use `make deploy` or `bin/kustomize build config/crd | kubectl apply -f -`.

### Run the controller on your host (optional)

To run the controller on your host instead of deploying to the cluster:

```bash
make install     # installs only the CRDs via kustomize
make run         # runs the controller in the foreground against your kubeconfig
```

### Uninstall

```bash
make undeploy
make uninstall
```

---

## Network reachability requirement for non-CAPK infrastructure

Starting with v0.1.0-alpha.2, the controller no longer SSHes into nodes to
retrieve the workload kubeconfig. Instead, control-plane nodes POST their
kubeconfig back to a Secret in the management cluster.

**Workload VMs (CAPV / CAPM3 / Tinkerbell) must have network reachability to
the management cluster's API server URL.** Verify from a sample workload node
before deploying:

```bash
curl -k https://<mgmt-api-server-host>:6443/api
```

For air-gapped or strictly-segmented network environments, see the planned
`SSHFallback` opt-in mechanism (post-alpha-2, PR-9).

---

## Next steps

- [CAPD Quickstart](QUICKSTART_CAPD.md) — create a cluster with Docker (development only).
- [CAPV Quickstart](QUICKSTART_CAPV.md) — create a cluster with vSphere.
- [CAPK Quickstart](QUICKSTART_CAPK.md) — create a cluster with KubeVirt.

For the current release status, known issues, and security caveats, read the [release notes](release-notes/v0.1.0-alpha.1.md).

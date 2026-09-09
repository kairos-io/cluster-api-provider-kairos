# Install Guide

Last verified against: Kairos v3.6.0+, CAPI v1.13.4, cert-manager v1.15+,
provider v0.1.0. The "Registering the Kairos fleet infrastructure
provider" section below documents `cluster-api-provider-kairos-fleet`
v0.1.0's own `clusterctl.yaml` entry; it has not been re-verified by
running `clusterctl init --infrastructure kairos-fleet` in this repository's
CI — see [docs/QUICKSTART_FLEET.md](QUICKSTART_FLEET.md) for the same
caveat. Do not use fleet provider `v0.1.0-beta.1`: it crash-loops on a real
management cluster and is not clusterctl-installable.

Three install paths: `clusterctl` (recommended), the released flat artifact (`kubectl apply`), and a developer install from source. Path 1 and Path 2 are mutually exclusive on one management cluster: see [Path 1 and Path 2 are mutually exclusive](#path-1-and-path-2-are-mutually-exclusive) below before picking one.

## Path 1 - clusterctl (recommended)

Use this for a `clusterctl`-managed management cluster, and for the closest match to upstream Cluster API tooling and documentation.

Per ADR 0007, this provider packages as two `clusterctl` providers built from one container image: a **bootstrap provider** (namespace `capi-kairos-bootstrap-system`, label `bootstrap-kairos`) and a **control-plane provider** (namespace `capi-kairos-control-plane-system`, label `control-plane-kairos`). A release publishes `bootstrap-components.yaml`, `control-plane-components.yaml`, and a shared `metadata.yaml`, all digest-pinned to the release image.

### Prerequisites

1. A Kubernetes cluster acting as the management cluster (kind, EKS, GKE, AKS, etc.).
2. `clusterctl` CLI installed, e.g. from the [Cluster API v1.13.4 release](https://github.com/kubernetes-sigs/cluster-api/releases/tag/v1.13.4).
3. `kubectl` configured to use the management cluster.

`clusterctl init` installs cert-manager and Cluster API core automatically if they are not already present, so unlike Path 2 (a plain `kubectl apply` with no `clusterctl` involved) you do not need to install either one by hand first.

### Register the provider

This provider is not yet in the upstream `clusterctl` provider list, so add it explicitly to your `clusterctl` configuration:

```yaml
# ~/.cluster-api/clusterctl.yaml (or pass --config <path> to clusterctl)
providers:
  - name: "kairos"
    url: "https://github.com/kairos-io/cluster-api-provider-kairos/releases/latest/download/bootstrap-components.yaml"
    type: "BootstrapProvider"
  - name: "kairos"
    url: "https://github.com/kairos-io/cluster-api-provider-kairos/releases/latest/download/control-plane-components.yaml"
    type: "ControlPlaneProvider"
```

`clusterctl` discovers `metadata.yaml` next to each components file in the same GitHub release; it does not need its own entry in this file.

### Registering the Kairos fleet infrastructure provider

The Kairos fleet infrastructure provider (`cluster-api-provider-kairos-fleet`,
clusterctl name `kairos-fleet`) is a separate repository and release series.
Like the bootstrap and control-plane providers above, it is not yet in the
upstream `clusterctl` provider list, so add it explicitly:

```yaml
# ~/.cluster-api/clusterctl.yaml
providers:
  - name: "kairos-fleet"
    url: "https://github.com/kairos-io/cluster-api-provider-kairos-fleet/releases/latest/infrastructure-components.yaml"
    type: "InfrastructureProvider"
```

Point `url` at a specific tag instead of `latest` to pin a version, for
example `.../releases/download/v0.1.2/infrastructure-components.yaml`.
Do not pin `v0.1.0-beta.1`: that release crash-loops on a real management
cluster and is not clusterctl-installable. Fleet claims already-enrolled
AuroraBoot nodes rather than creating machines on demand; see
[docs/QUICKSTART_FLEET.md](QUICKSTART_FLEET.md) for the full provisioning
model and ADR 0008 (maintained in the repository's internal decision
records) for how it integrates with the bootstrap and control-plane
providers in this repo.

### Install

```bash
clusterctl init --bootstrap kairos --control-plane kairos
```

To also initialize your infrastructure provider in the same call, add `--infrastructure <provider>` (`docker`, `vsphere`, `kubevirt`, `metal3`, `kairos-fleet`; see the matching quickstart in [Next steps](#next-steps) for provider-specific setup):

```bash
clusterctl init --bootstrap kairos --control-plane kairos --infrastructure docker
```

For fleet specifically:

```bash
clusterctl init --bootstrap kairos --control-plane kairos --infrastructure kairos-fleet
```

### Verify

```bash
kubectl get pods -n capi-kairos-bootstrap-system
kubectl get pods -n capi-kairos-control-plane-system
kubectl get providers -A
```

Expected: Deployment `capi-kairos-bootstrap-controller-manager` in `capi-kairos-bootstrap-system` and Deployment `capi-kairos-control-plane-controller-manager` in `capi-kairos-control-plane-system`, both `Available`, plus a `bootstrap-kairos` and a `control-plane-kairos` entry in clusterctl's provider inventory.

### Uninstall

```bash
clusterctl delete --bootstrap kairos --control-plane kairos
```

---

## Path 2 - Flat manifest (`kubectl apply`)

Use this if you want a single `kubectl apply -f` without configuring `clusterctl`, or if your management cluster is not `clusterctl`-managed.

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
3. **Cluster API core v1.13.4+** installed. Easiest path:
   ```bash
   kubectl apply -f https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.13.4/cluster-api-components.yaml
   ```
   Or use `clusterctl init --infrastructure docker` (or vsphere, kubevirt, ...) if you already have clusterctl configured — `clusterctl init` installs Cluster API core as a side effect of installing the infrastructure provider.
4. `kubectl` configured to use the management cluster.

### Install

```bash
kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.2/kairos-capi-provider.yaml
```

This applies the all-in-one provider manifest: CRDs, RBAC, webhook configurations, and the controller Deployment in the `kairos-capi-system` namespace, labeled `cluster.x-k8s.io/provider: kairos`. It is not a `clusterctl` artifact. Do not run `clusterctl init --bootstrap kairos` against a management cluster installed this way; see [Path 1 and Path 2 are mutually exclusive](#path-1-and-path-2-are-mutually-exclusive).

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
kubectl delete -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.2/kairos-capi-provider.yaml
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

## Path 1 and Path 2 are mutually exclusive

Do not install both on the same management cluster. They use different namespaces (`capi-kairos-bootstrap-system` / `capi-kairos-control-plane-system` for Path 1, `kairos-capi-system` for Path 2) and different `cluster.x-k8s.io/provider` label values (`bootstrap-kairos` / `control-plane-kairos` for Path 1, `kairos` for Path 2). Deployment selectors and ClusterRoleBinding roleRefs are immutable, so installing the second path on a cluster already running the first does not upgrade or replace it. It runs a second, independent set of controllers and webhooks watching the same CRDs, which is unsupported and can produce conflicting webhook admission decisions. If you need to move an existing installation from one path to the other, see [docs/UPGRADING.md](UPGRADING.md).

---

## Path 3 — Developer install (from source)

Use this if you are hacking on the provider itself.

### Prerequisites

- Go toolchain 1.26.3 (matches `go.mod` directive `go 1.26.0` / toolchain `go1.26.0`).
- A Kubernetes cluster acting as the management cluster.
- cert-manager and Cluster API core installed (same as Path 2).
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

Starting with v0.1.0-alpha.2 (carried forward in v0.1.0-beta.1), the controller no longer SSHes into nodes to
retrieve the workload kubeconfig. Instead, control-plane nodes POST their
kubeconfig back to a Secret in the management cluster.

**Workload nodes running on non-CAPK infrastructure (CAPV / CAPM3 / Kairos
fleet / Tinkerbell and any future provider) must have network reachability to
the management cluster's API server URL.** This includes bare-metal nodes
provisioned via Metal3 and nodes claimed via the Kairos fleet provider — they
POST their kubeconfig to a Secret in the management cluster. Verify
reachability from a sample workload node before deploying:

```bash
curl -k https://<mgmt-api-server-host>:6443/api
```

For air-gapped or strictly-segmented network environments, enable the opt-in
`SSHFallback` mechanism on the `KairosControlPlane`. This requires a
host-key-verified SSH identity Secret and a `known_hosts` Secret. See
[QUICKSTART_CAPV.md — Air-gapped fallback](QUICKSTART_CAPV.md#air-gapped-fallback-sshfallback)
for the full configuration steps.

---

## Next steps

- [CAPD Quickstart](QUICKSTART_CAPD.md) — create a cluster with Docker (development only).
- [CAPV Quickstart](QUICKSTART_CAPV.md) — create a cluster with vSphere.
- [CAPK Quickstart](QUICKSTART_CAPK.md) — create a cluster with KubeVirt.
- [CAPM3 Quickstart](QUICKSTART_CAPM3.md) — create a cluster on bare metal via Metal3.
- [Fleet Quickstart](QUICKSTART_FLEET.md) — create a cluster from AuroraBoot-claimed nodes with the Kairos fleet infrastructure provider.

For the current release status, breaking changes, and security caveats, read the [v0.1.0 release notes](release-notes/v0.1.0.md).

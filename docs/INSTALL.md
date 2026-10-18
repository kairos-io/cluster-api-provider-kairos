# Install Guide

Last verified against: provider v0.1.4, 2026-10-16. The `clusterctl` path was
run with `clusterctl` v1.13.4 against a fresh kind management cluster
(Kubernetes v1.34.0): CAPI v1.13.4, cert-manager v1.20.3, CAPV v1.17.0. The
flat-manifest path was run as an in-place upgrade on a k0s management cluster
with CAPI v1.12.2. The "Registering the Kairos fleet infrastructure
provider" section below documents `cluster-api-provider-kairos-fleet`
v0.1.3's own `clusterctl.yaml` entry; it has not been re-verified by
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
    url: "https://github.com/kairos-io/cluster-api-provider-kairos/releases/latest/bootstrap-components.yaml"
    type: "BootstrapProvider"
  - name: "kairos"
    url: "https://github.com/kairos-io/cluster-api-provider-kairos/releases/latest/control-plane-components.yaml"
    type: "ControlPlaneProvider"
```

`clusterctl` discovers `metadata.yaml` next to each components file in the same GitHub release; it does not need its own entry in this file.

Point `url` at a specific tag instead of `latest` to pin a version, for example
`https://github.com/kairos-io/cluster-api-provider-kairos/releases/v0.1.4/bootstrap-components.yaml`.

A `clusterctl` provider `url` is not a browser download link: `clusterctl`
parses it as
`https://github.com/{owner}/{repo}/releases/{latest|tag}/{components-file}` and
reads everything after `releases/` as the version. The
`releases/download/<tag>/<file>` form that `kubectl apply -f` and `curl` use
elsewhere in this guide is correct for them, but wrong here in both of its
variants: `releases/download/v0.1.4/...` fails immediately with `release not
found for version download` (`clusterctl` reads the literal string `download`
as the version), and `releases/latest/download/...` is the more dangerous case
— it appears to work, because `clusterctl`'s fast path builds its own download
URL from the file name alone, and only fails once it falls back to the GitHub
API — on a failed direct download, under `GOPROXY=off`, or on a rate-limited or
proxied network — with `failed to get file "metadata.yaml" from "v0.1.4"
release`.

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
example `.../releases/v0.1.3/infrastructure-components.yaml` — the same URL
shape described above, with no `download/` segment.
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
kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.4/kairos-capi-provider.yaml
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
kubectl delete -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.4/kairos-capi-provider.yaml
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

- Go toolchain 1.26.4 or later (the `go` directive in `go.mod`). The v0.1.4 release image is built with Go 1.26.9.
- A Kubernetes cluster acting as the management cluster.
- cert-manager and Cluster API core installed (same as Path 2).
- `kubectl` configured to use the management cluster.
- `kustomize` on your `PATH`. `make deploy` and `make undeploy` call it directly. `make kustomize` installs a pinned copy into `bin/`, which those two targets do not use.

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

**CRD installation note**: `make deploy` runs `kustomize edit set image` in `config/manager`, which rewrites `config/manager/kustomization.yaml` in your checkout, and then `kustomize build config/default | kubectl apply -f -`. `config/default` includes `config/crd`, which adds the `cluster.x-k8s.io/v1beta1: v1beta2` and `cluster.x-k8s.io/v1beta2: v1beta2` contract labels. CAPI core needs those labels to resolve this provider's object references. The generated CRDs in `config/crd/bases/` do not carry them, so applying them directly with `kubectl apply -f config/crd/bases/` leaves CAPI core unable to resolve the references; see [UPGRADING.md](UPGRADING.md). Use `make deploy`, or `kustomize build config/crd | kubectl apply -f -`.

### Run the controller on your host (optional)

To run the controller on your host instead of deploying to the cluster:

```bash
kustomize build config/crd | kubectl apply -f -   # installs only the CRDs, with the contract labels
make run                                          # runs the controller in the foreground against your kubeconfig
```

Do not use `make install` for the first step against a management cluster that runs Cluster API. It runs `kubectl apply -f config/crd/bases`, which applies the CRDs without the contract labels described in the CRD installation note above.

### Uninstall

```bash
make undeploy
make uninstall
```

---

## Optional manager flags

### `--kubeadm-extra-controlplane-kinds`

Not in v0.1.4. This flag ships with the kubeadm worker distribution, which is
planned for v0.2.0. A v0.1.4 manager given this flag refuses to start, with
`flag provided but not defined`.

Applies to the bootstrap manager only. It widens the set of control-plane kinds
that a kubeadm worker (a `KairosConfig` with `distribution: kubeadm`) may join.
See [Kubeadm workers](KUBEADM_WORKERS.md) for how the worker join uses it.

- **Default.** The bootstrap manager trusts `KubeadmControlPlane` and
  `KamajiControlPlane` in the `controlplane.cluster.x-k8s.io` group.
  `KairosControlPlane` is also on the built-in list, but only when its
  distribution is kubeadm, which is not supported in this release.
- **Format.** Comma-separated `Kind.group` entries, for example
  `FooControlPlane.controlplane.example.com,BarControlPlane.example.org`. The Kind
  is the text before the first dot; the group is the remainder. Kind and group are
  matched together. An entry that is not in `Kind.group` form makes the manager
  exit at startup with `invalid --kubeadm-extra-controlplane-kinds`.
- **Operator-level only.** No `Cluster` or `KairosConfig` field can add a kind.
- **RBAC.** The bootstrap manager reads the control-plane object to verify it. The
  shipped bootstrap RBAC grants read on `KubeadmControlPlane` and
  `KamajiControlPlane` only, because it cannot name a kind it does not know about.
  Grant the bootstrap Deployment's ServiceAccount `get` on the plural resource of
  each kind you add. Find the ServiceAccount with
  `kubectl -n <namespace> get deployment <name> -o jsonpath='{.spec.template.spec.serviceAccountName}'`,
  using the Deployment names below.
- **When to use it.** Add a kind only for a control-plane provider you operate. The
  bootstrap manager reads the kubeconfig Secret that provider publishes and
  connects, from the management cluster's network, to the server address in it.

Set the flag on the bootstrap manager's Deployment. For the flat manifest and the
developer install:

```bash
kubectl -n kairos-capi-system patch deployment kairos-capi-controller-manager \
  --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubeadm-extra-controlplane-kinds=FooControlPlane.controlplane.example.com"}]'
```

For the `clusterctl` install, patch the bootstrap Deployment instead:

```bash
kubectl -n capi-kairos-bootstrap-system patch deployment capi-kairos-bootstrap-controller-manager \
  --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubeadm-extra-controlplane-kinds=FooControlPlane.controlplane.example.com"}]'
```

The Deployment rolls out a new Pod. Re-applying the install manifest, or running
`clusterctl upgrade`, resets the arguments, so re-apply the patch afterwards. Do
not set the flag on the `clusterctl` control-plane Deployment; it has no effect
there.

---

## Network reachability requirement for non-CAPK infrastructure

Starting with v0.1.0-alpha.2, the controller no longer SSHes into nodes to
retrieve the workload kubeconfig. Instead, control-plane nodes push their
kubeconfig back to a staging Secret that the control plane creates and owns
in the management cluster; see
[API_REFERENCE.md — KubeconfigReady condition](API_REFERENCE.md#kubeconfigready-condition)
for the current mechanism.

**Workload nodes running on non-CAPK infrastructure (CAPV / CAPM3 / Kairos
fleet / Tinkerbell and any future provider) must have network reachability to
the management cluster's API server URL.** This includes bare-metal nodes
provisioned via Metal3 and nodes claimed via the Kairos fleet provider — they
push their kubeconfig to that staging Secret in the management cluster.

### Set the URL the nodes push to

By default, the bootstrap provider gives nodes the API server URL it uses
itself. Inside the management cluster that is the in-cluster Service URL,
typically `https://10.96.0.1:443`. KubeVirt VMs run as pods and can reach it,
so CAPK needs nothing more. Nodes on any other infrastructure are on another
network and cannot, so their kubeconfig is never pushed and the
`KairosControlPlane` waits forever on `KubeconfigReady`.

For those providers, set `KAIROS_MANAGEMENT_API_OVERRIDE` on the bootstrap
provider's Deployment to a URL the nodes can reach, such as a management
control-plane node address or a load balancer in front of the API server:

```bash
# clusterctl install (Path 1): the bootstrap provider has its own Deployment
kubectl -n capi-kairos-bootstrap-system set env \
  deployment/capi-kairos-bootstrap-controller-manager \
  KAIROS_MANAGEMENT_API_OVERRIDE=https://<mgmt-api-server-host>:6443

# flat manifest (Path 2): one Deployment runs both providers
kubectl -n kairos-capi-system set env \
  deployment/kairos-capi-controller-manager \
  KAIROS_MANAGEMENT_API_OVERRIDE=https://<mgmt-api-server-host>:6443
```

The provider manifests do not set this variable, and `clusterctl` has no
setting for it, so set it after `clusterctl init`. Check that it is still set
after a `clusterctl upgrade`. Re-applying the flat manifest with
`kubectl apply` keeps it.

To confirm the value, look for `Overriding management API server URL from
environment` in the bootstrap provider's log. It prints the original and the
override. The URL is rendered into each control-plane node's bootstrap data
when that data is generated, so changing the variable does not affect nodes
that have already been rendered.

If the management cluster is a kind cluster, its API server listens on
`127.0.0.1` by default. Set `networking.apiServerAddress` (and, if needed,
`networking.apiServerPort`) in the kind configuration to a host address the
nodes can reach, and use that address in the override.

Verify reachability from a sample workload node before deploying (the node-push itself
always verifies TLS against the management API CA — the provider uses no
`curl -k` / `--insecure` anywhere, so verify the same way here):

```bash
# Point --cacert at the same CA the controller renders to the node at
# /etc/kairos-capi/management-ca.crt (see "Management API TLS verification"
# below). Do NOT use curl -k here: an unverified check can pass even when the
# CA is wrong, so it would not catch a CA mismatch the node-push itself rejects.
curl --cacert /etc/kairos-capi/management-ca.crt https://<mgmt-api-server-host>:6443/api
```

For air-gapped or strictly-segmented network environments, enable the opt-in
`SSHFallback` mechanism on the `KairosControlPlane`. This requires a
host-key-verified SSH identity Secret and a `known_hosts` Secret. See
[QUICKSTART_CAPV.md — Air-gapped fallback](QUICKSTART_CAPV.md#air-gapped-fallback-sshfallback)
for the full configuration steps.

### Management API TLS verification and the CA source

The node-to-management push (kubeconfig staging, etcd-status) is
TLS-verified: nodes run `curl --cacert` against the management API server's CA,
which the controller renders into each node's cloud-config at
`/etc/kairos-capi/management-ca.crt`. There is no insecure opt-out; a wrong or
missing CA makes the push fail closed, and air-gapped environments use
`SSHFallback` instead.

Two environment variables on the controller Deployment tune this:

| Env var | Purpose |
| --- | --- |
| `KAIROS_MANAGEMENT_API_OVERRIDE` | The URL nodes dial back to. Required for non-CAPK infrastructure where the in-cluster service IP is not routable from workload VMs — set it to a LAN-reachable URL such as `https://<mgmt-cp-node-ip>:6443`. |
| `KAIROS_MANAGEMENT_API_CA_FILE` | Overrides the CA nodes verify that URL against. Accepts either a filesystem path to a PEM bundle or an inline PEM. When unset, the controller uses its own in-cluster trust root (the management API server CA). Set this when the override URL is served by a load balancer or proxy whose certificate is not issued by the cluster CA. |

By default (no override), the controller resolves the CA from its own
`rest.Config` (the management API server CA it already trusts). The controller
**fails fast at startup** if push is enabled but no CA is available — it never
falls back to an unverified push.

---

## Next steps

- [CAPD Quickstart](QUICKSTART_CAPD.md) — create a cluster with Docker (development only).
- [CAPV Quickstart](QUICKSTART_CAPV.md) — create a cluster with vSphere.
- [CAPK Quickstart](QUICKSTART_CAPK.md) — create a cluster with KubeVirt.
- [CAPM3 Quickstart](QUICKSTART_CAPM3.md) — create a cluster on bare metal via Metal3.
- [Fleet Quickstart](QUICKSTART_FLEET.md) — create a cluster from AuroraBoot-claimed nodes with the Kairos fleet infrastructure provider.
- [Kubeadm workers](KUBEADM_WORKERS.md) — join Kairos workers to a Kamaji or `KubeadmControlPlane` control plane.

For the current release status, read the [v0.1.4 release notes](release-notes/v0.1.4.md). For the breaking changes and security caveats of the first non-beta release, read the [v0.1.0 release notes](release-notes/v0.1.0.md).

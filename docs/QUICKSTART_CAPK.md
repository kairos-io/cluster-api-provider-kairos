# Quickstart: CAPK (KubeVirt)

Last verified against: Kairos v3.6.0+, CAPI v1.8.x, KubeVirt v1.8.2, CAPK v0.1.x, provider v0.1.0-alpha.2.

This guide covers two paths:

- **Lab path** (using `kubevirt-env`): an automated local setup using kind + KubeVirt. This path is lab-only; it is not suitable for production.
- **Non-lab path**: applying sample manifests directly against an existing management cluster that already has KubeVirt and CAPI installed.

Both paths use the 2-disk Kairos installer pattern, described below.

**Note on `controlPlaneEndpoint`**: the sample manifests require `Cluster.spec.controlPlaneEndpoint` to be set to the IP that your LoadBalancer implementation (MetalLB or equivalent) will assign to the control-plane Service. The provider does not auto-populate this field. If the IP is unknown at apply time, use the `TODO-REPLACE-WITH-LB-IP` placeholder in the sample, wait for MetalLB to assign the LB IP to the `<cluster>-control-plane-lb` Service, then update the field with `kubectl edit cluster <name>`. CAPI core does not overwrite a populated value. Without a valid endpoint, `KairosControlPlane` stalls with `Available=False(WaitingForInfrastructureControlPlaneEndpoint)`.

---

## The 2-disk Kairos installer pattern

Kairos image DataVolumes (DVs) are **live-installer images**. When a KubeVirt VM boots from a Kairos image DV, the VM runs a live environment that installs Kairos OS onto a separate blank disk. The cloud-config's `install.device` field points at that blank disk.

The two-disk layout the samples use:

| Disk | Role | bootOrder | DataVolume |
|------|------|-----------|------------|
| `installeriso` (cdrom/sata) | Kairos live-installer image | 2 | `kairos-rootdisk` (your Kairos image DV) |
| `rootdisk` (virtio) | Blank install target | 1 | `kairos-install-disk` (blank, 40Gi) |

The blank install-target disk boots first (bootOrder 1) but has no OS — the firmware falls through to bootOrder 2, the Kairos installer. Kairos installs to `/dev/vda` (the blank disk), then reboots. After reboot, the blank disk now holds the installed OS and boots first.

The KairosConfigTemplate must include:

```yaml
spec:
  template:
    spec:
      install:
        auto: true
        device: "/dev/vda"
        reboot: true
```

The `virtualMachineBootstrapCheck.checkStrategy: none` setting in `KubevirtMachineTemplate` is required because CAPK's default SSH-based readiness check fails in this configuration. The Kairos CAPI provider uses a kubeconfig-push mechanism rather than SSH, so bypassing the check is correct.

---

## Lab path: using `kubevirt-env`

`kubevirt-env` is a local-development CLI that creates a kind cluster and installs the full KubeVirt + CAPI + Kairos stack automatically. This is the fastest way to get started in a lab environment.

### Prerequisites

- `docker`
- Go toolchain (for building `kubevirt-env`)
- Network access to `github.com`

Pinned `kubectl`, `kind`, `clusterctl`, and `virtctl` are downloaded to `<RepoRoot>/.e2e-bin` on first run.

### Build the helper

```bash
make kubevirt-env
```

Or directly:

```bash
go build -o bin/kubevirt-env ./cmd/kubevirt-env
```

### Create the environment

```bash
./bin/kubevirt-env
```

What this does:

- Creates a kind cluster (name from `--cluster-name` or `CLUSTER_NAME` env var; default `kairos-capi`).
- Installs Calico (required: default CNI is disabled in kind).
- Installs local-path-provisioner, CDI, KubeVirt, CAPI core, CAPK.
- Installs cert-manager and the Kairos CAPI provider (released image).
- Builds and uploads your Kairos cloud image to CDI as a DataVolume.

Options:

- `KUBEVIRT_USE_EMULATION=false` — disable KVM emulation (requires `/dev/kvm` on the host).
- `CAPK_VERSION=v0.1.x` — pin a specific CAPK version.

The control-plane API is exposed via a LoadBalancer Service named `<cluster>-control-plane-lb`. MetalLB or equivalent must be installed in the kind cluster. `kubevirt-env` installs MetalLB as part of its setup.

### Apply the install-disk DataVolume

Before applying the cluster manifest, create the blank install target:

```bash
kubectl apply -f config/samples/capk/kairos-install-disk.yaml
```

Review the `storageClassName` comment in that file. The `kubevirt-env` default is `local-path`.

### Create a test cluster

k0s:

```bash
kubectl apply -f config/samples/capk/kubevirt_cluster_k0s_single_node.yaml
```

k3s:

```bash
kubectl apply -f config/samples/capk/kubevirt_cluster_k3s_single_node.yaml
```

### Watch cluster status

```bash
# Cluster name from the sample is kairos-cluster-kv
kubectl get cluster kairos-cluster-kv -w
kubectl get kairoscontrolplane kairos-control-plane-kv-capk -w
kubectl get machines
```

Successful provisioning sequence:

1. `Machine` appears with `Provisioning` phase.
2. `KairosConfig` transitions to `ready: true` and `dataSecretName` is set.
3. KubeVirt VM starts; Kairos installer runs; VM reboots.
4. `KubevirtMachine` becomes `Ready`.
5. `KairosControlPlane` sets `initialized: true`.
6. `<cluster>-kubeconfig` Secret appears in the management cluster.

### Retrieve the kubeconfig

```bash
kubectl get secret kairos-cluster-kv-kubeconfig \
  -o jsonpath='{.data.value}' | base64 -d > kairos-cluster-kv.kubeconfig
kubectl --kubeconfig=kairos-cluster-kv.kubeconfig get nodes
```

### Run the scripted end-to-end test

```bash
make kubevirt-env      # (re)build and set up the environment
make test-kubevirt     # run the full scripted flow
```

### Cleanup

```bash
./bin/kubevirt-env cleanup
```

---

## Non-lab path: direct manifest apply

For users with an existing management cluster that already has KubeVirt, CDI, CAPI, and CAPK installed.

Do not use `clusterctl init --bootstrap kairos` — clusterctl integration is deferred to a later release (KD-38).

### Prerequisites

- Kubernetes management cluster with:
  - CAPI v1.8.x installed
  - CAPK (`infrastructure.cluster.x-k8s.io`) installed
  - CDI (Containerized Data Importer) installed
  - A LoadBalancer implementation (MetalLB or equivalent)
- Kairos CAPI provider installed:
  ```bash
  kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0-alpha.2/kairos-capi-provider.yaml
  ```
- A Kairos image uploaded to CDI as a DataVolume named `kairos-rootdisk` (for k0s) or `kairos-k3s-rootdisk` (for k3s) in namespace `default`. The image must be a Kairos live-installer image — not a pre-installed disk image.

### Step 1: Create the user-password Secret

```bash
kubectl create secret generic kairos-user-password \
  --from-literal=password=$(openssl rand -base64 32)
```

### Step 2: Apply the blank install-target DataVolume

```bash
kubectl apply -f config/samples/capk/kairos-install-disk.yaml
```

Edit the file to set the correct `storageClassName` for your cluster before applying.

### Step 3: Customize the cluster manifest

Edit `config/samples/capk/kubevirt_cluster_k0s_single_node.yaml` (or the k3s variant):

- If your cluster does not have KVM on every node, uncomment `nodeSelector` and set the hostname of a KVM-capable node.
- Adjust `cpu.cores` and `resources.requests.memory` for your workload.
- Review `dnsServers` — the sample includes lab-specific DNS addresses.

### Step 4: Apply the cluster manifest

```bash
kubectl apply -f config/samples/capk/kubevirt_cluster_k0s_single_node.yaml
```

### Step 5: Watch cluster status and retrieve kubeconfig

Same commands as the lab path above.

---

## Troubleshooting

**VMs do not start**: confirm KubeVirt shows `Available` and CDI is running. Check `kubectl get vmi` and `kubectl describe vmi <name>` for VM instance events.

**Installer does not run**: verify the `installeriso` volume references the correct Kairos image DataVolume and that the DV is `Succeeded`. If the DV is still importing, the VM will not start.

**VM boots directly to installed OS on first boot**: this is expected — the blank install-target disk boots first (bootOrder 1) and the firmware falls through to the Kairos installer (bootOrder 2) because the blank disk has no bootable partition. If the VM gets stuck before the installer image is available, check the DV import status.

**`kairos-cluster-kv-kubeconfig` Secret never appears**: check `KairosControlPlane` conditions:
```bash
kubectl describe kairoscontrolplane kairos-control-plane-kv-capk
```
Also check controller logs:
```bash
kubectl logs -n kairos-capi-system deployment/kairos-capi-controller-manager
```

**LoadBalancer Service has no external IP**: ensure MetalLB (or equivalent) is installed and has an address pool configured. Without a LoadBalancer IP, the control-plane endpoint is not resolvable and `KairosControlPlane.status.initialized` will not become `true`.

**`nodeSelector` scheduling failure**: if you have the `nodeSelector` for a specific hostname and that node does not have KVM or is not schedulable, the VM pod stays `Pending`. Either remove the selector (if all nodes have KVM) or update the hostname to match a schedulable KVM node.

**KubeVirt emulation warnings**: if `/dev/kvm` is not available, `kubevirt-env` enables software emulation (`KUBEVIRT_USE_EMULATION=true`). This is substantially slower. Expect install + boot to take several minutes.

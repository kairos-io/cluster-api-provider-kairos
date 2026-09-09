# Quickstart: CAPK (KubeVirt)

Last verified against: Kairos v4.1.2, CAPI v1.13.4, KubeVirt v1.9, CAPK v0.1.x, provider v0.1.0.

This guide covers two paths:

- **Lab path** (using `kubevirt-env`): an automated local setup using kind + KubeVirt. This path is lab-only; it is not suitable for production.
- **Non-lab path**: applying sample manifests directly against an existing management cluster that already has KubeVirt and CAPI installed.

Both paths use the 2-disk Kairos installer pattern, described below.

**Note on `controlPlaneEndpoint`**: the sample manifests require `Cluster.spec.controlPlaneEndpoint` to be set to the IP that your LoadBalancer implementation (MetalLB or equivalent) will assign to the control-plane Service. The provider does not auto-populate this field. If the IP is unknown at apply time, use the `TODO-REPLACE-WITH-LB-IP` placeholder in the sample, wait for MetalLB to assign the LB IP to the `<cluster>-control-plane-lb` Service, then update the field with `kubectl edit cluster <name>`. CAPI core does not overwrite a populated value. Without a valid endpoint, `KairosControlPlane` stalls with `Available=False(WaitingForInfrastructureControlPlaneEndpoint)`.

---

## The 2-disk Kairos installer pattern

Kairos image DataVolumes (DVs) are **live-installer images**. When a KubeVirt VM boots from a Kairos image DV, the VM runs a live environment that installs Kairos OS onto a separate blank disk. The cloud-config's `install.device` field points at that blank disk.

The two-disk layout the single-node samples use:

| Disk | Role | bootOrder | DataVolume |
|------|------|-----------|------------|
| `installeriso` (cdrom/sata) | Kairos live-installer image | 2 | `kairos-rootdisk` (your Kairos image DV) |
| `rootdisk` (virtio) | Blank install target | 1 | `kairos-install-disk` (blank, 40Gi) |

The blank install-target disk boots first (bootOrder 1) but has no OS — the firmware falls through to bootOrder 2, the Kairos installer. Kairos installs to `/dev/vda` (the blank disk), then reboots. After reboot, the blank disk now holds the installed OS and boots first.

The HA samples use the same layout but create both disks per VM via `dataVolumeTemplates` rather than referencing shared DataVolumes, so `kairos-install-disk.yaml` is not applied for HA. See [High-availability control plane](#high-availability-control-plane).

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

This walkthrough uses the flat manifest (`kubectl apply -f kairos-capi-provider.yaml`, below). `clusterctl init --bootstrap kairos --control-plane kairos` is also supported as of this release; see [docs/INSTALL.md: Path 1](INSTALL.md#path-1---clusterctl-recommended). Do not run both against the same management cluster: the two install paths are mutually exclusive (see [docs/INSTALL.md](INSTALL.md#path-1-and-path-2-are-mutually-exclusive)).

### Prerequisites

- Kubernetes management cluster with:
  - CAPI v1.13.4+ installed (v1beta2 contract)
  - CAPK (`infrastructure.cluster.x-k8s.io`) installed
  - CDI (Containerized Data Importer) installed
  - A LoadBalancer implementation (MetalLB or equivalent)
- Kairos CAPI provider installed:
  ```bash
  kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0/kairos-capi-provider.yaml
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

## High-availability control plane

CAPK supports a 3- or 5-node control plane (`spec.replicas: 3`/`5`). CAPK provisions its own LoadBalancer Service for the control-plane endpoint, so HA samples do **not** set `spec.ha.vip` — setting it would produce a conflicting endpoint.

Samples:

```bash
kubectl apply -f config/samples/capk/kubevirt_cluster_k0s_ha.yaml
kubectl apply -f config/samples/capk/kubevirt_cluster_k3s_ha.yaml
```

Prerequisite specific to CAPK HA: etcd peers over each control-plane VM's own IP. KubeVirt's default `masquerade` interface gives every VM the same self-address (`10.0.2.2`), so etcd cannot peer across nodes on that interface alone. Each control-plane VM needs a second, routable NIC (a Multus-attached bridge network or equivalent) in addition to the default masquerade interface. See the header comments in the HA sample files for the exact `interfaces`/`networks` shape.

Each control-plane VM also needs its **own** disks. The HA samples declare `dataVolumeTemplates` in the `KubevirtMachineTemplate`, so every cloned VM gets a private `installeriso` and `rootdisk`; the shared `kairos-install-disk` DataVolume used by the single-node samples is not applied here. Pointing all three VMs at one shared `ReadWriteOnce` DataVolume instead means only one can attach it and the other two never start.

### k0s HA: the image start gate

A 3-node **k0s** control plane on CAPK needs a Kairos image that stops k0s from starting during the install boot. Without it the cluster deadlocks unrecoverably, and nothing surfaces the fault — the `KairosControlPlane` reports `Available=True` while etcd is below quorum.

This applies to images that install to disk (`install.auto: true`, the CAPK pattern). It does **not** affect k3s, which joins as an etcd learner, nor pre-installed images that never run an installer boot — which is why CAPV and CAPM3 are unaffected.

It is also HA-only. Single-node CAPK k0s (`spec.replicas: 1`) needs no gated image: the harm comes from the installer's join registering a *remote* voting member on another node, and a single-node cluster has no one to join.

**What goes wrong:** Kairos applies the cloud-config on every boot that carries it, the install boot included. The k0s plugin therefore starts `k0scontroller` inside the installer, which joins and is registered on the init node as a *voting* etcd member — k0s has no learner join. The installer then reboots, leaving a configured-but-dead voter. Quorum rises to 2 with a single member serving, etcd stalls, the API server dies, and the real join from the installed system can never complete because it needs that same API.

**How to recognize it:** on the joiner, `/var/lib/k0s/etcd` is empty, and its journal from the previous boot (a second machine-id under `/var/log/journal/`) shows `Started k0s` and `Joining existing cluster`. On the init node, `k0s etcd member-list` shows two members while only one is running, and nothing listens on `6443` or `9443`.

**The fix** is image-side, so the provider cannot ship it: a yip stage baked into the image at `/system/oem`, applied at the `initramfs` stage — before systemd loads any unit. A drop-in delivered through cloud-config `write_files` is too late, and a `stages:` block inside a `#cloud-config` is silently ignored.

```yaml
# /system/oem/91_k0s_start_gate.yaml
name: "CAPI k0s start gate"
stages:
  initramfs:
    - name: "Block k0s during install/live/recovery boots"
      directories:
        - path: /etc/systemd/system/k0scontroller.service.d
          permissions: 0755
      files:
        - path: /etc/systemd/system/k0scontroller.service.d/00-capi-start-gate.conf
          permissions: 0644
          content: |
            [Unit]
            ConditionKernelCommandLine=!cdroot
            ConditionPathExists=!/run/cos/recovery_mode
            ConditionPathExists=!/run/cos/live_mode
```

Add the same drop-in under `k0sworker.service.d` for worker images. A failed `Condition` makes systemd *skip* the start rather than fail it, so `Restart=` never re-triggers, and the Kairos plugin's own restart starts k0s normally once the installed system is running.

**To verify:** on a provisioned node, the install boot should show no k0s activity at all.

```bash
# from the node; the non-current machine-id is the install boot
sudo journalctl -D /var/log/journal/<other-machine-id> -u k0scontroller \
  | grep -cE 'Started k0s|Joining existing cluster'   # expect 0
```

### Day-2 limitation: replacing a k3s control-plane node

k3s HA has the same day-2 limitation as other providers: replacing a k3s control-plane node leaves an orphaned etcd member requiring manual cleanup (KD-5d). See [docs/HIGH_AVAILABILITY.md — Day-2](HIGH_AVAILABILITY.md#day-2-etcd-health-and-quorum-safe-replacement).

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

**3-node k0s control plane never converges**: if `KairosControlPlane` reports `Available=True` but the workload API server is unreachable, the Kairos image is likely missing the k0s start gate. See [k0s HA: the image start gate](#k0s-ha-the-image-start-gate) for the diagnosis and the image-side fix.

**LoadBalancer Service has no external IP**: ensure MetalLB (or equivalent) is installed and has an address pool configured. Without a LoadBalancer IP, the control-plane endpoint is not resolvable and `KairosControlPlane.status.initialized` will not become `true`.

**`nodeSelector` scheduling failure**: if you have the `nodeSelector` for a specific hostname and that node does not have KVM or is not schedulable, the VM pod stays `Pending`. Either remove the selector (if all nodes have KVM) or update the hostname to match a schedulable KVM node.

**KubeVirt emulation warnings**: if `/dev/kvm` is not available, `kubevirt-env` enables software emulation (`KUBEVIRT_USE_EMULATION=true`). This is substantially slower. Expect install + boot to take several minutes.

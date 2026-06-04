# Quick Start Guide - CAPM3 (Metal3 / bare metal)

Last verified against: Kairos v3.6.0+, CAPI v1.12.x, CAPM3 v1.13.0, BMO v0.13.0, provider v0.1.0-alpha.2. Validated on emulated bare metal (sushy-tools Redfish BMC + libvirt/KVM); the same flow applies to physical hardware with a Redfish/IPMI BMC.

This guide walks you through creating a single-node k3s or k0s cluster on Kairos using Cluster API with the Metal3 infrastructure provider (CAPM3). The k3s path is the lab-validated end-to-end path; k0s uses the identical flow and differs only in distribution and version fields.

**Scope of this release:**

- Single-node control plane only (`replicas: 1`). The webhook rejects `replicas > 1` (KD-5b).
- DHCP-only networking. Static IPAM via Metal3DataTemplate / Metal3IPPool is a future phase.
- No cloud controller manager (`cloudProviderEnabled: false`). Node providerID is set by the Kairos cloud-config at first boot — no manual patching required.

**Note on `controlPlaneEndpoint`**: both `Cluster.spec.controlPlaneEndpoint` and `Metal3Cluster.spec.controlPlaneEndpoint` must be set to the control-plane node's stable IP:6443 before applying the manifest. The provider does not auto-discover the endpoint. In DHCP-only Phase 1, this is the node's DHCP-assigned data-plane IP. To make this stable, set a DHCP reservation tied to the BareMetalHost's MAC address before applying. If you apply without a valid endpoint, `KairosControlPlane` stalls with `Available=False(WaitingForInfrastructureControlPlaneEndpoint)`.

---

## How bare-metal provisioning works with CAPM3

Understanding the provisioning model prevents the most common failure modes:

1. You register a physical server as a `BareMetalHost` (BMH) with baremetal-operator. BMO + Ironic inspect the host and mark it `available`.
2. CAPM3 claims the host when you apply a `Metal3Machine` that matches it.
3. Ironic performs a **whole-disk deploy**: it writes the raw disk image you provide directly to the node's boot disk over the provisioning network.
4. Ironic appends a config-drive partition (label `config-2`) containing the Kairos cloud-config rendered by the bootstrap provider.
5. The node UEFI-boots from the disk. The Kairos active system reads the cloud-config from the config-drive and provisions k3s/k0s.

There is no installer step. The disk image you provide must already be a fully installed Kairos system. This is different from the CAPK (KubeVirt) 2-disk installer pattern.

---

## Building the disk image

This is the most important prerequisite. Ironic deploys a raw disk image. That image must contain an already-installed Kairos OS — not an installer ISO and not an auroraboot auto-reset image.

### Why an auroraboot `disk.raw` does not work

An auroraboot `disk.raw` (recovery / auto-reset image) is designed to auto-reset-install on first boot. Its first-boot repartitioning collides with the `config-2` partition Ironic appends as the config-drive. The node boots into `cos-recovery` (an ephemeral overlay) instead of the persistent active system. k3s / k0s then fails with an `overlay not supported as upperdir` error and never provisions a working node.

### Required: a fully-installed Kairos raw disk

The image you provide must have `COS_ACTIVE`, `COS_PERSISTENT`, `COS_OEM`, and `COS_STATE` partition labels already present. The node boots straight into the active, persistent system and reads the config-drive.

### Producing the image: QEMU install-to-disk

The supported method uses a Kairos installer ISO to install onto a blank raw disk in QEMU. The result is the fully-installed disk image.

**Inputs required:**

- A Kairos installer ISO at the exact k3s or k0s version you intend to run. Example filename:
  `kairos-ubuntu-24.04-standard-amd64-generic-v3.6.0-k3sv1.33.5+k3s1.iso`
- `qemu-system-x86_64` with KVM, OVMF firmware, and `genisoimage`.

**Step 1: Write a minimal install cloud-config.**

Create `user-data`:

```yaml
#cloud-config
install:
  auto: true
  device: /dev/vda
  poweroff: true   # powers off QEMU when install completes
users:
  - name: kairos
    groups:
      - admin
    passwd: kairos   # temporary; only used during the install VM session
    ssh_authorized_keys: []
```

Create `meta-data`:

```yaml
instance-id: kairos-install
local-hostname: kairos-install
```

Pack them into a cidata ISO:

```bash
genisoimage -output cidata.iso \
  -volid cidata \
  -joliet -rock \
  user-data meta-data
```

**Step 2: Create a blank raw disk.**

Size the disk to match or be smaller than the node's physical disk, with margin:

```bash
qemu-img create -f raw kairos-installed.raw 20G
```

**Step 3: Boot QEMU headless and let the installer run.**

```bash
qemu-system-x86_64 \
  -enable-kvm \
  -cpu host \
  -machine q35 \
  -m 4096 \
  -drive file=/path/to/OVMF_CODE.fd,if=pflash,format=raw,readonly=on \
  -drive file=kairos-installed.raw,if=virtio,format=raw \
  -drive file=/path/to/kairos-installer.iso,media=cdrom \
  -drive file=cidata.iso,media=cdrom \
  -boot order=cd \
  -no-reboot \
  -nographic
```

Replace `/path/to/OVMF_CODE.fd` with your OVMF firmware path (commonly `/usr/share/OVMF/OVMF_CODE.fd` or `/usr/share/edk2/ovmf/OVMF_CODE.fd`).

`install.poweroff: true` causes the Kairos installer to power off QEMU when installation completes. `-no-reboot` is a safety net. The QEMU process exits when the install finishes; this typically takes 3–10 minutes depending on disk speed.

**Step 4: Verify the partition layout.**

```bash
sudo losetup -fP kairos-installed.raw
# Note the device name printed, e.g. /dev/loop0
sudo blkid /dev/loop0p*
# Expect labels: COS_ACTIVE, COS_PERSISTENT, COS_OEM, COS_STATE
sudo losetup -d /dev/loop0
```

If `COS_ACTIVE` and `COS_PERSISTENT` are not present, the install did not complete — do not use this image.

**Step 5: Generate checksums and serve over HTTP.**

```bash
md5sum kairos-installed.raw > kairos-installed.raw.md5
# or for sha256:
# sha256sum kairos-installed.raw > kairos-installed.raw.sha256
```

Serve both files from an HTTP server reachable from the Ironic provisioning network:

```bash
# Simple example using Python; use a production-grade server for real deployments.
python3 -m http.server 6180 --directory /path/to/images/
```

The `Metal3MachineTemplate.spec.template.spec.image.url` and `.checksum` fields point at these URLs.

### Version pinning

The k3s or k0s version is fixed at image-build time. `KairosControlPlane.spec.version` and `KairosConfigTemplate.spec.kubernetesVersion` are informational on the Metal3 whole-disk path — they do not change what runs on the node. Set both fields to match the version bundled in the image you built. If there is a mismatch, CAPI may report incorrect version information and future upgrade tooling will not work correctly.

---

## Prerequisites

1. **Bare-metal environment**:
   - Physical or emulated servers with IPMI, Redfish, or iDRAC BMC access.
   - Ironic and baremetal-operator v0.13+ deployed and accessible from the management cluster.
   - A DHCP server on the provisioning network, or pre-assigned IP addresses.
   - An HTTP server serving the disk image and its checksum, reachable from the provisioning network.

2. **Management cluster**: A Kubernetes cluster with network access to the Ironic API and to the workload nodes.

3. **Cluster API core**: v1.9+ installed (lab-validated against v1.12.x). CAPM3 v1.13 stores its CRDs at the `v1beta2` contract, so the management cluster's CAPI core must serve `v1beta2`. (This provider's typed client additionally relies on `cluster.x-k8s.io/v1beta1` still being served; CAPI continues to serve both, so no action is needed.) The manifests in this guide are authored in `v1beta2`. Verify:
   ```bash
   kubectl api-versions | grep cluster.x-k8s.io
   ```

4. **CAPM3**: Cluster API Provider Metal3 v1.13+ installed.
   ```bash
   kubectl get crd metal3clusters.infrastructure.cluster.x-k8s.io
   ```

5. **Kairos CAPI Provider**: v0.1.0-alpha.2+ installed (see [INSTALL.md](INSTALL.md)).

6. **Fully-installed Kairos disk image**: See "Building the disk image" above.

7. **BareMetalHost registered**: At least one `BareMetalHost` in `available` state. BMO must have inspected it successfully:
   ```bash
   kubectl get baremetalhosts -A
   # Look for provisioning state "available"
   ```

### Installing CAPM3

Install CAPM3 using `clusterctl` or the upstream manifests. Refer to the [Metal3 documentation](https://book.metal3.io/capm3/introduction) for the current install procedure. The Kairos CAPI provider is installed separately:

```bash
kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0-alpha.2/kairos-capi-provider.yaml
```

See [INSTALL.md](INSTALL.md) for the full provider install and verification steps.

---

## Creating a Cluster

### Step 1: Register your BareMetalHost

If you have not already registered the target server, create the BMC credentials Secret and `BareMetalHost` object. Templates for both are included (commented out) in the sample manifest. The BareMetalHost `spec.online: true` allows BMO to inspect and then provision the host.

```bash
# Edit the commented-out sections at the bottom of the sample manifest,
# then apply:
kubectl apply -f config/samples/capm3/kairos_cluster_k3s_single_node.yaml
```

Wait for the host to reach `available` state before proceeding:

```bash
kubectl get baremetalhosts -w
# Provisioning state transitions: registering → inspecting → available
```

### Step 2: Create the user-password Secret

```bash
kubectl create secret generic kairos-user-password \
  --from-literal=password=$(openssl rand -base64 32)
```

The sample manifests reference this Secret via `userPasswordSecretRef`. Do not set `userPassword` inline.

### Step 3: Choose a sample manifest

- k3s single node (lab-validated): `config/samples/capm3/kairos_cluster_k3s_single_node.yaml`
- k0s single node: `config/samples/capm3/kairos_cluster_k0s_single_node.yaml`

### Step 4: Customize the manifest

Open the chosen sample and fill in all values marked `TODO`.

#### Cluster and Metal3Cluster

```yaml
spec:
  controlPlaneEndpoint:
    host: "192.168.1.100"   # replace with the node's actual or reserved IP
    port: 6443
```

Set the same IP in both `Cluster.spec.controlPlaneEndpoint.host` and `Metal3Cluster.spec.controlPlaneEndpoint.host`.

#### Metal3MachineTemplate

```yaml
spec:
  template:
    spec:
      image:
        url: "http://192.168.222.2:6180/images/kairos-k3s-installed.raw"
        checksum: "http://192.168.222.2:6180/images/kairos-k3s-installed.raw.md5"
        checksumType: md5    # or sha256 / sha512
        diskFormat: raw
```

The `checksumType` must match the hash algorithm used to produce the checksum file.

#### KairosControlPlane and KairosConfigTemplate

```yaml
# In KairosControlPlane:
spec:
  version: "v1.34.3+k3s3"     # must match the version bundled in your disk image
  distribution: k3s

# In KairosConfigTemplate:
spec:
  template:
    spec:
      distribution: k3s
      kubernetesVersion: "v1.34.3+k3s3"   # must match spec.version above
      # Note: no install: block — Ironic already wrote the OS to disk
```

### Step 5: Apply the manifest

k3s:

```bash
kubectl apply -f config/samples/capm3/kairos_cluster_k3s_single_node.yaml
```

k0s:

```bash
kubectl apply -f config/samples/capm3/kairos_cluster_k0s_single_node.yaml
```

### Step 6: Monitor cluster creation

```bash
kubectl get cluster kairos-m3 -w
kubectl get kairoscontrolplane kairos-m3-cp
kubectl get machines -n default
kubectl get metal3machines -n default
kubectl get baremetalhosts -n default
```

Expected BareMetalHost state progression: `available` → `provisioning` → `provisioned`.

Expected Machine state: `Provisioning` → `Running`.

Provisioning bare metal typically takes 5–20 minutes depending on disk write speed and network bandwidth for the image download.

### Step 7: Retrieve the kubeconfig

```bash
kubectl get secret kairos-m3-kubeconfig \
  -o jsonpath='{.data.value}' | base64 -d > kairos-m3-kubeconfig.yaml
kubectl --kubeconfig=kairos-m3-kubeconfig.yaml get nodes
kubectl --kubeconfig=kairos-m3-kubeconfig.yaml get pods -n kube-system
```

**Node-push behavior (alpha-2+):** The control-plane node posts its kubeconfig to a Secret in the management cluster at bootstrap time. The node must have network reachability to the management cluster's API server (`<mgmt-api-server-host>:6443`). See [INSTALL.md](INSTALL.md#network-reachability-requirement-for-non-capk-infrastructure) for verification steps.

If the node cannot reach the management API server, enable the opt-in [Air-gapped fallback (SSHFallback)](#air-gapped-fallback-sshfallback) on the `KairosControlPlane`.

---

## Field Reference

### Required fields to customize

| Field | Resource | Description |
|-------|----------|-------------|
| `spec.controlPlaneEndpoint.host` | Cluster, Metal3Cluster | The control-plane node's stable IP. Set identically in both resources. |
| `spec.controlPlaneEndpoint.port` | Cluster, Metal3Cluster | API server port. Default: 6443. |
| `spec.template.spec.image.url` | Metal3MachineTemplate | HTTP URL of the fully-installed Kairos raw disk image. |
| `spec.template.spec.image.checksum` | Metal3MachineTemplate | HTTP URL of the checksum file (one-line md5sum / sha256sum output). |
| `spec.template.spec.image.checksumType` | Metal3MachineTemplate | Hash algorithm: `md5`, `sha256`, or `sha512`. Must match the checksum file. |
| `spec.version` | KairosControlPlane | k3s/k0s version bundled in the disk image. Informational; must match the image. |
| `spec.template.spec.kubernetesVersion` | KairosConfigTemplate | Same version as above; must match `KairosControlPlane.spec.version`. |
| `spec.template.spec.distribution` | KairosConfigTemplate | `k3s` or `k0s`. Must match `KairosControlPlane.spec.distribution`. |

### Metal3MachineTemplate image fields

| Field | Values | Notes |
|-------|--------|-------|
| `diskFormat` | `raw` | Ironic whole-disk deploy; only `raw` is validated for this path. |
| `checksumType` | `md5`, `sha256`, `sha512` | `md5` has the widest Ironic version compatibility; `sha256` is preferred for new deployments. |

### KairosConfigTemplate fields specific to Metal3

| Field | Notes |
|-------|-------|
| `install` | **Must be absent.** Ironic has already written the OS. Adding an `install:` block causes Kairos to attempt a second install on the provisioned disk. |
| `role` | Set to `control-plane` for the control-plane template. |
| `distribution` | Must match `KairosControlPlane.spec.distribution`. |

### cloudProviderEnabled

`Metal3Cluster.spec.cloudProviderEnabled: false` disables the external cloud controller manager. CAPM3 v1.13 with this setting finds the Kubernetes Node by the `metal3.io/uuid` label and validates `providerID=metal3://<BareMetalHost-UID>` — it does not set them. The Kairos cloud-config sets both automatically from the Ironic config-drive metadata (the `config-2` drive's `meta_data.json` `.uuid`): on **k3s** via a `config.yaml.d` kubelet drop-in applied when the node registers; on **k0s** via a post-bootstrap `kubectl patch` of the node (setting a previously-empty `providerID` is permitted by the API server). Either way, **no manual providerID patching is required** — the node sets it itself.

---

## Troubleshooting

### Node stays in provisioning; BareMetalHost stays in `provisioning` state

```bash
kubectl describe baremetalhost kairos-m3-node-0 -n default
kubectl logs -n baremetal-operator-system deployment/baremetal-operator
```

Common causes:

1. **Image server unreachable from Ironic**: verify Ironic can reach the HTTP server. The image URL is resolved from the Ironic pod, not the management cluster's API server.
2. **Checksum mismatch**: regenerate with `md5sum <image>` and verify the `.md5` file content is exactly `<hash>  <filename>` (two spaces between hash and name — standard `md5sum` output format).
3. **Wrong `checksumType`**: `checksumType: md5` with a sha256 file (or vice versa) causes Ironic to reject the image silently. Match the type to the algorithm used.
4. **BMC credentials wrong or BMC unreachable**: verify IPMI/Redfish connectivity:
   ```bash
   ipmitool -I lanplus -H <bmc-ip> -U <user> -P <pass> chassis status
   ```

### Node provisions but k3s never starts; logs show `overlay not supported as upperdir`

This means an auroraboot auto-reset image was deployed instead of a fully-installed disk. Re-read the "Building the disk image" section. The `cos-recovery` partition is an ephemeral overlay and cannot host a persistent upper directory. Re-provision with a correctly-built image.

### Node provisions but `KubeconfigReadyCondition` stays `False(WaitingForNodePush)`

The node k3s/k0s is running but the kubeconfig has not reached the management cluster. Causes:

1. **No network route from the node to the management cluster API server**: run `curl -k https://<mgmt-api-server-host>:6443/api` from the node. If it fails, either open the network path or enable [SSHFallback](#air-gapped-fallback-sshfallback).
2. **bootstrap-Secret naming (pre-alpha-2 provider)**: if `BareMetalHost.spec.userData` references a Secret that does not exist, upgrade the provider to v0.1.0-alpha.2+. The deterministic bootstrap-Secret naming fix shipped in alpha-2 and is required for unattended Metal3 provisioning.

### Bootstrap issues

```bash
kubectl describe kairosconfig <config-name>
kubectl logs -n kairos-capi-system deployment/kairos-capi-controller-manager
```

If `KairosConfig.status.failureMessage` is set, the issue is transient — it clears when resolved. A missing `kairos-user-password` Secret is the most common first-run cause.

### Metal3Cluster not becoming ready

```bash
kubectl describe metal3cluster kairos-m3
kubectl logs -n capm3-system deployment/capm3-controller-manager
```

Verify that `cloudProviderEnabled: false` is set. With `cloudProviderEnabled: true` (the default), CAPM3 expects a cloud controller manager to be running, which will never be satisfied in this configuration.

### controlPlaneEndpoint not resolving

If the node's IP changes after provisioning (DHCP lease reassignment), the `controlPlaneEndpoint` becomes stale. Prevention: set a DHCP reservation tied to the BareMetalHost MAC address. Recovery requires updating both `Cluster.spec.controlPlaneEndpoint.host` and `Metal3Cluster.spec.controlPlaneEndpoint.host` and restarting the node's API server with the correct bind address.

---

## Security Considerations

- **BMC credentials**: store in a `kubernetes.io/opaque` Secret in the same namespace as the BareMetalHost. Do not set BMC credentials inline anywhere. The sample manifest shows the correct pattern.
- **User password**: provide via `userPasswordSecretRef`. The webhook rejects `KairosConfig` objects with no credential. Do not set `userPassword` inline outside of throwaway testing.
- **Image integrity**: use `checksumType: sha256` (or sha512) in production. md5 is shown in samples for compatibility; it is not a security-grade algorithm. Ironic verifies the checksum before deploying.
- **Image server**: serve disk images over HTTPS if the provisioning network is not isolated. An unauthenticated HTTP image server on a shared network allows image substitution.
- **SSH access**: add `sshPublicKey` or `githubUser` to `KairosConfigTemplate` if you need direct node access. Password-based SSH should not be the primary access mechanism.
- **Provisioning network**: treat the Ironic provisioning network as a trusted, isolated segment. Ironic PXE-boots nodes on this network; a compromised provisioning network allows arbitrary OS injection.

---

## Next Steps

- Configure worker nodes via `MachineDeployment` with a `Metal3MachineTemplate` and a worker-role `KairosConfigTemplate`.
- Add custom Kubernetes manifests via `spec.template.spec.manifests` in `KairosConfigTemplate`.
- Multi-node control planes are tracked for a future release (KD-5b / KD-25).
- Static IPAM via Metal3DataTemplate / Metal3IPPool is a future phase.

---

## Air-gapped fallback (SSHFallback)

**When to use:** the bare-metal node has no network route to the management cluster API server (`<mgmt-api-server-host>:6443`). The node-push path — where the node POSTs its kubeconfig to a management cluster Secret — is unreachable. Without this fallback, `KubeconfigReadyCondition` stays `False(WaitingForNodePush)` indefinitely.

**When NOT to use:** the default node-push path works in most networks. Do not enable SSHFallback unless you have confirmed the node cannot reach the management API server. The fallback is an escape hatch, not a replacement for node-push.

**Security requirement:** host-key verification is mandatory. The controller verifies the workload node's SSH host key against a `known_hosts` Secret before any data is exchanged. There is no trust-on-first-use mode. `activateAfter` must be at least 15 minutes (greater than the `KubeconfigReadyCondition` Info→Warning threshold of 10 minutes).

### Step 1: Create the SSH identity Secret

```bash
kubectl create secret generic kairos-ssh-identity \
  --type=kubernetes.io/ssh-auth \
  --from-file=ssh-privatekey=/path/to/your/private_key \
  -n default
```

The corresponding public key must already be installed on the node via `KairosConfigTemplate.spec.template.spec.sshPublicKey` or `githubUser`.

### Step 2: Create the known-hosts Secret

Obtain the node's SSH host key while you still have network access (or from the Kairos image's pre-generated host key material):

```bash
ssh-keyscan <node-ip> > known_hosts_file
kubectl create secret generic kairos-ssh-known-hosts \
  --from-file=known_hosts=known_hosts_file \
  -n default
```

### Step 3: Enable SSHFallback on the KairosControlPlane

```yaml
spec:
  sshFallback:
    enabled: true
    user: kairos          # must match the node's SSH user
    port: 22
    activateAfter: 15m    # must exceed kubeconfigReadyTimeout (10m)
    identitySecretRef:
      name: kairos-ssh-identity
    knownHostsSecretRef:
      name: kairos-ssh-known-hosts
```

Both Secrets must be in the same namespace as the `KairosControlPlane`. The webhook rejects cross-namespace references.

### Step 4: Verify activation

```bash
kubectl describe kairoscontrolplane kairos-m3-cp -n default
```

The `KubeconfigReadyCondition` Reason tells you which path is active:

| Reason | Meaning |
|--------|---------|
| `KubeconfigReady` | Node-push succeeded. SSHFallback did not fire. |
| `KubeconfigReadyViaSSHFallback` | SSH fallback supplied the kubeconfig. |
| `SSHFallbackDialing` | SSH fallback is in progress; wait up to 30 seconds. |
| `SSHFallbackFailed` | SSH attempt failed. Check Events for the categorized error. |
| `SSHFallbackMisconfigured` | A referenced Secret is missing, empty, or unparseable. Fix the Secret; the controller retries automatically. |

---

## Cleanup

```bash
kubectl delete -f config/samples/capm3/kairos_cluster_k3s_single_node.yaml
```

Deleting the `Cluster` object triggers CAPM3 to deprovision the `BareMetalHost` (power off and wipe the provisioned state). The `BareMetalHost` object itself is not deleted by CAPI — it returns to `available` state for reuse. Delete the `BareMetalHost` manually if you want to deregister the server from BMO:

```bash
kubectl delete baremetalhost kairos-m3-node-0 -n default
```

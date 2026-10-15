# Kubeadm Workers

Last verified against: CAPI v1.13.4 (v1beta2 contract). Provider version: not
yet released (follows v0.1.3).

Verification status, by control plane:

- **Kamaji** (`KamajiControlPlane`): validated end to end on a live management
  cluster with KubeVirt workers and real Kairos images, in three
  configurations -- vanilla, `spec.network.advertiseAddress` with
  `spec.serverAddress`, and a bring-your-own CA with `advertiseAddress` and no
  `serverAddress`. Workers joined and became Ready in all three. Contributed by
  @patrostkowski.
- **Upstream `KubeadmControlPlane`**: unit and envtest tests only. Not yet
  validated against a live cluster. See also the namespace requirement below,
  which applies specifically to this case.

Setting `spec.distribution: kubeadm` on a `KairosConfig` or
`KairosConfigTemplate` bootstraps a Kairos **worker** node that joins a control
plane managed outside this provider:

- a Kamaji `KamajiControlPlane` (hosted control planes), or
- a Cluster API `KubeadmControlPlane`.

This is the route for running Kairos workers under a Kamaji control plane. The
background discussion is in
[kairos-io/kairos#4920](https://github.com/kairos-io/kairos/issues/4920).

## Scope of this release

| Capability | Status |
| --- | --- |
| Kairos workers created by a `MachineDeployment` | Supported. |
| Control planes of kind `KamajiControlPlane` | Supported. Validated on a live cluster; see the verification status above. |
| Control planes of kind `KubeadmControlPlane` | Designed for, not yet validated live, and it **must** have its own namespace; see [the namespace requirement](#required-keep-upstream-kubeadmcontrolplane-clusters-in-their-own-namespace). |
| Control planes of any other kind | Need an operator flag; see [Allowing other control-plane kinds](#allowing-other-control-plane-kinds). |
| A kubeadm control plane run by this provider (`KairosControlPlane` with `distribution: kubeadm`) | Not supported. Refused; see [KairosControlPlane refuses kubeadm](#kairoscontrolplane-refuses-kubeadm). |
| `MachinePool` workers | Not supported. Refused. |
| Joining a k0s or k3s `KairosControlPlane` | Not supported. Use `distribution: k0s` or `k3s` workers for those clusters. |
| CNI installation | Not provided. You install and operate the CNI. |
| Building the node image | Not provided. See [Prerequisites](#prerequisites). |

## Required: keep upstream KubeadmControlPlane clusters in their own namespace

**Do not put a cluster whose control plane is an upstream `KubeadmControlPlane`
in the same namespace as a k0s or k3s Kairos cluster.** Give it a namespace of
its own. This is a condition of using the kubeadm worker path at all while it is
a preview, not a recommendation.

The reason is how upstream `KubeadmControlPlane` obtains its PKI: it **adopts**
a pre-existing `<cluster>-ca` or `<cluster>-kubeconfig` Secret rather than
insisting on creating one. A Secret planted before the control plane reconciles
is therefore adopted and given a legitimate controller owner reference, which
the trust check described below cannot distinguish from the real thing. The
check catches a planted Secret; it cannot catch an adopted one.

What makes that reachable today is a separate, known weakness in the k0s and k3s
control-plane paths: a control-plane node pushes its kubeconfig using a
ServiceAccount whose Role carries an unscoped `secrets` `create`. A node
compromised in a namespace that also holds an upstream-KCP cluster can use it to
plant a Secret for that neighbouring cluster. Keeping the two in separate
namespaces removes the adjacency, which is the only mitigation available until
that push path is fixed.

Kamaji is not affected in the same way: it overwrites planted data and sets
itself as the controller owner, so the trust check holds. The prohibition is
specifically about upstream `KubeadmControlPlane`.

## How it works

For a `KairosConfig` with `distribution: kubeadm` and `role: worker`, the
bootstrap controller does the following for each Machine:

1. Waits until the Cluster reports `status.initialization.controlPlaneInitialized`
   and has a valid `spec.controlPlaneEndpoint`.
2. Verifies the control plane before minting anything. All of these must hold,
   otherwise no token is minted and the KairosConfig reports a failure:
   - `Cluster.spec.controlPlaneRef` points at an allowed control-plane kind
     (matched on API group and kind).
   - The `<cluster>-kubeconfig` Secret exists and is controller-owned by that
     control-plane object.
   - The kubeconfig carries `certificate-authority-data`, and at least one of
     two independent anchors holds: either its `server` equals
     `https://<host>:<port>` of `Cluster.spec.controlPlaneEndpoint`, or a
     `<cluster>-ca` Secret exists and its `tls.crt` matches the kubeconfig's CA.
     The `server` is allowed to differ because a control-plane provider may
     publish an internal address there, as the Kamaji control-plane provider
     does (`admin.svc`) when `spec.network.advertiseAddress` is set -- and
     Kamaji publishes `<cluster>-ca` too, so the second anchor holds. Requiring
     one of the two matters because the controller reaches the workload cluster
     through this kubeconfig with admin credentials, and the Secret's owner
     reference is not an anchor on its own: ownerReferences are user-settable,
     so anyone who can create Secrets in the namespace can forge that leg.
     The node joins `spec.serverAddress` or the Cluster endpoint, never the
     kubeconfig `server`.
   - If a `<cluster>-ca` Secret exists, its `tls.crt` equals the kubeconfig's
     CA. A missing `<cluster>-ca` is allowed.
   - Under Kamaji, do not expect to control `<cluster>-ca` yourself. Kamaji
     honours a bring-your-own CA only as a pre-created `<tcp>-ca` holding a
     valid `ca.crt`/`ca.key` pair; a CAPI-style `<cluster>-ca` carrying only
     `tls.crt`/`tls.key` is overwritten with Kamaji's own generated CA, and the
     join then pins that CA rather than the one you supplied. Reported by
     @patrostkowski from a live Kamaji cluster. It does not weaken the anchor
     above, because `<cluster>-ca` still carries the tenant CA either way.
3. Mints a bootstrap token in the workload cluster: a Secret named
   `bootstrap-token-<id>` in `kube-system`, valid for 15 minutes. The
   non-secret `<id>` is recorded in `KairosConfig.status.bootstrapTokenID`. The
   secret half never appears in `spec` or `status`. It is present in the
   workload-cluster Secret and in the rendered bootstrap data, and therefore on
   the node, so restrict read access to bootstrap data Secrets as you would for
   any other distribution.
4. Renders a cloud-config that writes a kubeadm `JoinConfiguration` to
   `/etc/kubernetes/kairos-kubeadm-join.yaml` (mode `0600`) with token discovery
   and a CA-hash pin, plus a oneshot unit, `kairos-kubeadm-post-bootstrap.service`.
5. On the node, the unit runs only on an installed (active or passive) boot where
   `/etc/kubernetes` and `/var/lib/kubelet` are mount points. It checks
   `kubeadm version` against `Machine.spec.version`, records an attempt marker
   under `/var/lib/kairos`, runs `kubeadm join --config ...`, and records a
   completion marker on success. A node that has joined does not rejoin. A join
   that did not complete is not retried on the node: the unit removes
   `/etc/kubernetes/bootstrap-kubelet.conf` (which holds the token) and refuses on
   later boots, so a `MachineHealthCheck` must replace the Machine.
6. CAPI links the Machine to the Node. The controller stops refreshing the token
   when the Machine has a `status.nodeRef`.

### Which clusters the bootstrap manager connects to

Step 2 is also the connect gate, not only the mint gate. The bootstrap manager
builds a workload-cluster client in step 3, for the join that asked for it, from
the kubeconfig step 2 just verified. The client is discarded with that request
and every call it makes is bounded by a 10-second timeout. The TCP connection
itself can outlive it: client-go caches transports process-wide by TLS
configuration, so a later join to the same cluster may reuse an idle connection.
Nothing polls or health-probes it, and no connection is held open for a cluster
that is not joining a worker.

So installing this provider does not, on its own, connect the manager to
anything. A cluster the provider has no kubeadm `KairosConfig` for is never
dialled, whatever its control-plane kind.

What the anchors in step 2 constrain about that address depends on which anchor
held. Under the endpoint anchor it is `Cluster.spec.controlPlaneEndpoint`
itself. Under the `<cluster>-ca` anchor the kubeconfig's `server` is deliberately
not constrained, because Kamaji publishes an internal address there; what the
anchor pins is the CA, so whatever answers must serve a certificate issued by
the CA in the `<cluster>-ca` Secret. Protect that Secret accordingly: for a
cluster relying on the second anchor it, rather than the `server` field, is what
keeps the manager's admin credentials from being steered elsewhere.

The token refresh in step 6 re-runs step 2 before it reconnects, so a
`<cluster>-kubeconfig` Secret that is replaced or re-pointed between joins is
re-verified rather than trusted on the strength of the first join.

## Prerequisites

**Control plane.** An existing Cluster whose `spec.controlPlaneRef` is a
`KubeadmControlPlane` or `KamajiControlPlane` in the same namespace, already
initialized, with `spec.controlPlaneEndpoint` set.

**Network.** The bootstrap manager must be able to reach the workload cluster's
API server to create the token Secret. The worker VMs must be able to reach
`Cluster.spec.controlPlaneEndpoint`.

**Image.** The Kairos image the infrastructure provider boots must ship `kubeadm`,
`kubelet` (with its systemd unit), and `containerd` at the workload Kubernetes
version. The version must match `Machine.spec.version` exactly, down to the
patch: `kubeadm version -o short` on the node must print the same string. The
images from [kairos-io/provider-kubernetes](https://github.com/kairos-io/provider-kubernetes)
are the reference image. Check that the Kubernetes minor you need is inside both
this provider's supported band (see the README target versions) and the minors
that image publishes.

The cloud-config rendered for kubeadm does not write
`/system/oem/12_kairos-capi-persistency.yaml`, which k0s and k3s nodes receive
(see [Persistence behavior](API_REFERENCE.md#persistence-behavior)). Your image
must already persist the paths kubeadm writes, such as `/etc/kubernetes`,
`/var/lib/kubelet`, and `/var/lib/containerd`, across reboots. The join unit
enforces part of this: it requires `/etc/kubernetes` and `/var/lib/kubelet` to be
mount points, and systemd skips the unit, so the node never joins, when either is
not.

**Machine version.** Set `spec.template.spec.version` on the `MachineDeployment`
to the exact version in the image, in plain upstream form such as `v1.36.1`. Do
not use a `+k0s.N` or `+k3sN` suffix. `KairosConfig.spec.kubernetesVersion` is
still required by the schema; keep it equal, but it is `Machine.spec.version` that
the controller and the node enforce.

**Credentials.** The same rule as other distributions applies: set at least one of
`userPassword`, `userPasswordSecretRef`, `sshPublicKey`, or `githubUser`.
Prefer `userPasswordSecretRef`.

**CNI.** Install a CNI in the workload cluster. Joined nodes report `NotReady`
until one is running. This provider installs none.

## Example

The canonical sample is
[`config/samples/kubeadm/kairos_kubeadm_workers.yaml`](../config/samples/kubeadm/kairos_kubeadm_workers.yaml).
It contains a worker `KairosConfigTemplate`, a `MachineDeployment`, and a
`MachineHealthCheck`. It does not create the Cluster or control plane.

1. Create the password Secret. The sample never contains the value:

   ```bash
   kubectl create secret generic kairos-user-password \
     --from-literal=password="$(openssl rand -base64 32)"
   ```

2. Edit the sample: set the cluster name, the version, and replace the
   `infrastructureRef` placeholders with a MachineTemplate for your infrastructure
   provider. The kubeadm bootstrap configuration is the same on every provider.

3. Apply it:

   ```bash
   kubectl apply -f config/samples/kubeadm/kairos_kubeadm_workers.yaml
   ```

4. Check progress. `BOOTSTRAPTOKENID` is empty until the controller has minted
   the first token:

   ```bash
   kubectl get kairosconfig,machines -l cluster.x-k8s.io/cluster-name=<cluster>
   kubectl get kairosconfig -o custom-columns=NAME:.metadata.name,BOOTSTRAPTOKENID:.status.bootstrapTokenID
   clusterctl get kubeconfig <cluster> > workload.kubeconfig
   kubectl --kubeconfig workload.kubeconfig get nodes
   ```

The minimal worker template is:

```yaml
apiVersion: bootstrap.cluster.x-k8s.io/v1beta2
kind: KairosConfigTemplate
metadata:
  name: kairos-kubeadm-worker
spec:
  template:
    spec:
      role: worker
      distribution: kubeadm
      kubernetesVersion: "v1.36.1"
      userPasswordSecretRef:
        name: kairos-user-password
```

## Customizing the join

`spec.kubeadm.joinConfiguration` accepts the same fields as the kubeadm
`JoinConfiguration` in Cluster API's kubeadm bootstrap provider (v1beta2). Every
field is optional. The controller sets or overwrites these:

- `nodeRegistration.name`: the Machine name, unless you set it.
- `nodeRegistration.taints`: adds `node.cluster.x-k8s.io/uninitialized:NoSchedule`,
  which Cluster API removes once it links the Machine to the Node. Taints you set
  are kept.
- `discovery.bootstrapToken`: the whole block (minted token, API server endpoint,
  CA hash). The endpoint is `spec.serverAddress` when set, otherwise
  `Cluster.spec.controlPlaneEndpoint`. Set it when workers must reach the API
  server through a different address than the management cluster does, for
  example the address a KamajiControlPlane advertises with
  `spec.network.advertiseAddress`. The address must be covered by the API
  server certificate's SANs. The CA hash pins discovery either way.
- `discovery.file`: cleared.
- A kubelet `providerID` patch, applied through `kubeadm join --patches`, when the
  infrastructure provider's providerID is known at render time.

Everything else, such as `nodeRegistration.kubeletExtraArgs`, passes through.
Do not set `joinConfiguration.controlPlane`: control-plane joins are not
supported in this release.

These KairosConfig fields have no effect for kubeadm workers: `singleNode`,
`k0sSingleNode`, `token`, `tokenSecretRef`, `workerToken`,
`workerTokenSecretRef`, `k3sToken`, `k3sTokenSecretRef`, `podCIDR`,
`serviceCIDR`, `primaryIP`, and `manifests`. The user, hostname, `install`,
`dnsServers`, and `files` fields are rendered as for other distributions.

| `does not equal the Cluster control-plane endpoint, and there is no <cluster>-ca Secret to cross-check` | The kubeconfig's `server` differs from `Cluster.spec.controlPlaneEndpoint` and there is no `<cluster>-ca` to anchor its CA. Either align the two addresses, or publish `<cluster>-ca` with the cluster's CA certificate. |

### Admission refusals

The validating webhook rejects a `KairosConfig` with `distribution: kubeadm` that
sets any of the following.

Note the object these apply to. The webhook is registered for `KairosConfig`, not
for `KairosConfigTemplate`, so a field set on a template is refused one object
later, when Cluster API core creates the `KairosConfig` from it: the
`MachineDeployment` stops producing usable Machines rather than the template
being rejected on `kubectl apply`. The three refusals that would otherwise reach
the node (`role: control-plane`, `discovery.tlsBootstrapToken` and
`joinConfiguration.controlPlane`) are additionally enforced when the
cloud-config is rendered, so a template cannot smuggle them past admission.

| Field | Why it is refused |
| --- | --- |
| `spec.role: control-plane` | Kubeadm is worker-only in this release. |
| `spec.kubeadm.joinConfiguration.discovery.bootstrapToken.token` | Tokens never appear in the spec; the controller mints and refreshes them. |
| `spec.kubeadm.joinConfiguration.discovery.tlsBootstrapToken` | It defaults to the bootstrap token but may be overridden, so a value here would become the kubelet's TLS-bootstrap credential and escape the minted token's lifetime. |
| `spec.kubeadm.joinConfiguration.discovery.bootstrapToken.unsafeSkipCAVerification: true` | It disables the CA pin the join relies on. |
| `spec.kubeadm.joinConfiguration.discovery.file` | File-based discovery is not supported; the controller configures token discovery. |
| Any `{{` inside `spec.kubeadm` | Jinja-style placeholders are not expanded on Kairos and would land verbatim in node configuration. |
| `spec.kubeadm.joinConfiguration.nodeRegistration.name` that is not a DNS-1123 subdomain | It becomes the Node name. |

The webhook runs on `KairosConfig`, not on `KairosConfigTemplate`. A template that
violates these rules is accepted, and the error appears when the MachineSet tries
to create the `KairosConfig`: check the MachineSet's conditions and events.

## Token lifetime and failed joins

A token is valid for 15 minutes; this is not configurable in this release. A node
reads its bootstrap data at first boot, so a token must be valid when the node
boots. While a Machine has no `status.nodeRef`, the controller checks the token on
each reconcile. When the token is missing from the workload cluster or past half
its lifetime, it mints a new one, rewrites the bootstrap data Secret in place, and
updates `status.bootstrapTokenID`. This helps nodes that boot late, such as
bare-metal machines with slow provisioning. It does not change a node that has
already booted.

The controller stops refreshing 24 hours after the Machine was created. It then
sets `BootstrapReady` to `False` (Warning) with reason
`BootstrapTokenRefreshCapExceeded`. It does not delete the Machine.

Configure a `MachineHealthCheck` with `spec.checks.nodeStartupTimeoutSeconds` for
the workers so a Machine whose node never registers is replaced well before the
cap. The sample includes one. Set the timeout above your slowest expected
provision-and-join time; Cluster API defaults it to 10 minutes when unset.

## Allowing other control-plane kinds

By default the bootstrap manager trusts `KubeadmControlPlane` and
`KamajiControlPlane` in the `controlplane.cluster.x-k8s.io` group. To allow
another control-plane kind, the operator sets
`--kubeadm-extra-controlplane-kinds` on the bootstrap manager. This is an
operator-level setting; there is no per-Cluster field. The operator must also grant
the bootstrap manager read access to the added kind. See
[Optional manager flags](INSTALL.md#optional-manager-flags) for the format, the
RBAC requirement, and how to set it.

## KairosControlPlane refuses kubeadm

A kubeadm control plane run by this provider is not supported in this release.

- `KairosControlPlane.spec.distribution` accepts `k0s` and `k3s` only. An explicit
  `kubeadm` is rejected at admission.
- If `spec.distribution` is empty and the referenced `KairosConfigTemplate` has
  `distribution: kubeadm`, the `KairosControlPlane` creates no Machines. Its
  `Ready` and `Available` conditions become `False` (Warning) with reason
  `UnsupportedDistribution`, and `status.failureReason` is
  `UnsupportedDistribution`. Point it at a k0s or k3s template.
- A kubeadm worker cannot join a Kairos-managed k0s or k3s control plane. A
  `KairosControlPlane` is trusted by the worker join only when its distribution is
  kubeadm, which cannot happen in this release.

## Troubleshooting

Waiting states do not change conditions. While the control plane is not yet
initialized, the endpoint or kubeconfig Secret is missing, or the workload
cluster is unreachable, the controller requeues every 10 seconds and logs
`Waiting on join material before generating cloud-config` with a reason. Check the
bootstrap manager log first when a KairosConfig has no `dataSecretName` and no
failure.

Failures set `Ready`, `BootstrapReady`, and `DataSecretAvailable` to `False`
(Warning) with reason `BootstrapDataSecretGenerationFailed`, and put the cause in
`status.failureMessage`:

```bash
kubectl get kairosconfig <name> -o jsonpath='{.status.failureMessage}'
```

| `failureMessage` contains | Cause and action |
| --- | --- |
| `is not controller-owned by the cluster's control-plane object` | The `<cluster>-kubeconfig` Secret is not owned by the control-plane object the Cluster references. Check `metadata.ownerReferences` on the Secret and `spec.controlPlaneRef` on the Cluster. |
| `is not on the kubeadm trust allowlist` | The control-plane kind is not `KubeadmControlPlane` or `KamajiControlPlane`. Use a supported kind or have the operator set `--kubeadm-extra-controlplane-kinds`. |
| `kubeconfig CA does not match` | The CA in `<cluster>-kubeconfig` differs from `<cluster>-ca` `tls.crt`. Resolve which CA is correct before retrying. |
| `carries no certificate-authority-data` | The kubeconfig has no CA to pin. Fix the control plane's kubeconfig Secret. |
| `does not support MachinePool` | Use a `MachineDeployment`. |
| `requires Machine.spec.version` | Set `spec.template.spec.version` on the MachineDeployment. |
| `not kubeadm: a kubeadm worker cannot join a k0s/k3s control plane` | The Cluster's control plane is a k0s or k3s `KairosControlPlane`. Use k0s or k3s workers. |

A failed KairosConfig is not retried on a timer. After fixing the cause, delete
the Machine so its MachineSet creates a replacement with a fresh KairosConfig.
Infrastructure providers wait for the bootstrap data, so no VM has booted from the
failed configuration.

If the VM boots but never registers a Node, read the join unit on the node:

```bash
journalctl -u kairos-kubeadm-post-bootstrap.service
```

- `kubeadm version mismatch: image has '<A>', cluster needs '<B>'`: the image's
  kubeadm differs from `Machine.spec.version`. Build or select an image at the
  exact version, or correct the MachineDeployment version.
- `a previous kubeadm join did not complete; refusing to retry on-node`: an
  earlier attempt started and did not finish. The node does not retry. Delete the
  Machine, or let the `MachineHealthCheck` replace it.
- `kubeadm join` errors about discovery or the token: confirm the node reaches
  `Cluster.spec.controlPlaneEndpoint` and booted within the token lifetime.
- `systemctl status kairos-kubeadm-post-bootstrap.service` reports a failed
  condition and no log output: the unit was skipped. Confirm the node booted from
  the installed system (not an installer or in-RAM boot) and that
  `findmnt /etc/kubernetes` and `findmnt /var/lib/kubelet` each show a mount.

Nodes that join but stay `NotReady`: the CNI is missing or unhealthy.

A Machine that stays without a Node for 24 hours gets
`BootstrapTokenRefreshCapExceeded`; replace it through the `MachineHealthCheck` or
delete it.

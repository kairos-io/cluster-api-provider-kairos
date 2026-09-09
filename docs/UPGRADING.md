# Upgrading Kairos CAPI Provider

## v0.1.1 → v0.1.2

Full release notes: [v0.1.2](release-notes/v0.1.2.md).

No CRD changes, no API changes, and no migration steps. A `clusterctl upgrade`,
or re-applying the flat manifest, is enough:

```bash
kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.2/kairos-capi-provider.yaml
```

Two behaviour changes are worth knowing about before you next scale a control
plane.

### HA joiners are now created one at a time

A 3 or 5-replica control plane provisions its joiners serially rather than all
at once, because two concurrent `etcd member add` calls against a one-member
cluster break quorum and the cluster does not recover. Scale-up therefore takes
longer, and a joiner that never comes up now holds the next one back with a
visible reason on the `KairosControlPlane` instead of taking the cluster down.
See [High-Availability control planes](HIGH_AVAILABILITY.md#scale-up-joiners-are-created-one-at-a-time).

Nothing to do on upgrade; existing clusters are unaffected.

### CAPK k0s HA needs an image start gate

This is not new in v0.1.2, but it is newly documented, and it is the reason a
CAPK k0s HA control plane can fail in a way nothing surfaces. The image must
stop k0s starting during the install boot. See
[the CAPK quickstart](QUICKSTART_CAPK.md#k0s-ha-the-image-start-gate) for the
mechanism, the failure signature, and how to verify your image has it. The fix
is image-side, so upgrading the provider does not supply it.

### If you use the Kairos fleet provider

Fleet provider v0.1.2 is recommended alongside this release: before it, the
cluster template it ships could not create worker machines at all. See its own
[release notes](https://github.com/kairos-io/cluster-api-provider-kairos-fleet/blob/main/docs/release-notes/v0.1.2.md).

---

## v0.1.0-alpha.1 → v0.1.0-alpha.2

### Credential migration: default credentials removed

Alpha-2 removes the implicit `kairos:kairos` user/password and the
`PasswordAuthentication yes` SSH setting. The validating webhook now **rejects**
any `KairosConfig` or `KairosConfigTemplate` that carries no credential.

**Before upgrading, add a credential to every `KairosConfig` and
`KairosConfigTemplate` in your management cluster.** Recommended approach:

1. Create the user-password Secret if it does not exist:
   ```bash
   kubectl create secret generic kairos-user-password \
     --from-literal=password=$(openssl rand -base64 32)
   ```

2. Edit each `KairosConfig` (or `KairosConfigTemplate`) to include at least
   one of:

   ```yaml
   spec:
     userPasswordSecretRef:
       name: kairos-user-password   # recommended
   # or:
     sshPublicKey: "ssh-rsa AAAA..."
   # or:
     githubUser: "your-github-username"
   ```

3. Apply the changes, then upgrade the provider:
   ```bash
   kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0-alpha.2/kairos-capi-provider.yaml
   ```

If you upgrade the provider first and then add credentials, any `KairosConfig`
admission that fires in the gap (for example from a Machine rollout) will be
rejected by the webhook.

---

### KD-12: KCP no longer writes `Cluster.Spec.ControlPlaneEndpoint`

Alpha-2 removes the two blocks in `updateClusterStatus` that wrote
`Cluster.Spec.ControlPlaneEndpoint` from Machine IP addresses (CAPV/CAPD) or
from the LoadBalancer Service Ingress IP (CAPK). `Cluster.Spec.ControlPlaneEndpoint`
is now written exclusively by CAPI core, which copies it from the infra-cluster's
own `Spec.ControlPlaneEndpoint`. This is the behaviour the CAPI v1beta2 contract
requires.

| Cluster state at upgrade | Behaviour after KD-12 | Operator action |
|---|---|---|
| Healthy cluster, `Cluster.Spec.ControlPlaneEndpoint` already populated | No change. The endpoint is preserved; the KCP controller stops overwriting it. CAPI core is now the sole writer, which is byte-identical to what KCP was writing. | None. |
| Mid-provisioning cluster, infra provider has already populated `<Infra>Cluster.Spec.ControlPlaneEndpoint` | CAPI core copies it on its next reconcile; everything proceeds. | None. |
| CAPV / CAPD / CAPK cluster whose `<Infra>Cluster.Spec.ControlPlaneEndpoint` (CAPV/CAPD) or `KubevirtCluster.Spec.ControlPlaneServiceTemplate` (CAPK) is unset — including any cluster that relied on KCP auto-discovering the endpoint from Machine IPs | `Cluster.Spec.ControlPlaneEndpoint` stays empty. KCP stalls with `KairosControlPlane.Status.Conditions[Available].Reason=WaitingForInfrastructureControlPlaneEndpoint`. | Set `VSphereCluster.Spec.ControlPlaneEndpoint` (CAPV), `DockerCluster.Spec.ControlPlaneEndpoint` (CAPD), or supply a LoadBalancer IP via the sample's `Cluster.spec.controlPlaneEndpoint` placeholder (CAPK) per the upstream provider's contract. For an in-flight cluster: `kubectl edit cluster <name>` and set `spec.controlPlaneEndpoint.host` and `spec.controlPlaneEndpoint.port` directly. CAPI core does not overwrite a populated value. |

No CRD changes in this release for KD-12. The new condition Reason
(`WaitingForInfrastructureControlPlaneEndpoint`) is additive — clients
that treat unknown Reasons as opaque strings are unaffected.

---

### KD-3b: SSH eliminated from controller normal operation

Alpha-2 removes synchronous SSH from the controller's hot path.
The bootstrap controller now writes a `push_kubeconfig()` shell function into
every control-plane node's cloud-config. After k0s/k3s starts, the **node**
POSTs its workload kubeconfig back to a Secret in the management cluster using
a short-lived Bearer token. The control-plane controller waits for that Secret
via a label-filtered watch.

| State at upgrade | What happens | User action |
|---|---|---|
| CAPK control-plane healthy, kubeconfig Secret present | No change. New Secrets created during a rollout get the new `cluster.x-k8s.io/cluster-name` label and `controllers.cluster.x-k8s.io/kubeconfig-source: node-push` annotation. Existing Secrets do not, but the controller's existing-Secret check uses Get-by-name and still works. | None. |
| CAPK control-plane mid-bootstrap (Secret not yet pushed) | Controller renders a NEW bootstrap Secret on next reconcile. The node, when it comes up, uses the new template's push payload. | None unless a rollout is needed for unrelated reasons. |
| CAPV control-plane healthy, kubeconfig Secret present (was SSH-fetched in alpha-1) | Controller's existing-Secret check still passes. Cluster keeps working. | None. |
| CAPV control-plane mid-bootstrap | **Old KairosConfig has no push block.** Controller's SSH path is GONE. The node will never push; the controller will never SSH. The cluster will sit indefinitely on `KubeconfigReadyCondition=False(WaitingForNodePush)`. | **Required**: delete the KairosConfig (or trigger a KCP rollout) so the bootstrap controller renders a new userdata with the push block. CAPV will create a new VM with new userdata. |
| Air-gapped CAPV control-plane (no network reachability from VM to management cluster API server) | Cluster stuck on `KubeconfigReadyCondition=False(WaitingForNodePush)` indefinitely; condition severity transitions Info → Warning after 10 minutes. | Enable `Spec.SSHFallback`: (1) create a `kubernetes.io/ssh-auth` Secret with an `ssh-privatekey` key in the cluster's namespace; (2) create an Opaque Secret with a `known_hosts` key containing the node's SSH host-key lines; (3) set `spec.sshFallback.enabled: true`, `spec.sshFallback.identitySecretRef.name`, and `spec.sshFallback.knownHostsSecretRef.name` on the `KairosControlPlane`; (4) leave `spec.sshFallback.activateAfter` at the default `15m` unless you have a reason to change it. The fallback fires after `activateAfter` elapses from `KubeconfigReadyCondition` first becoming `False(WaitingForNodePush)`. See [Air-gapped fallback (SSHFallback)](QUICKSTART_CAPV.md#air-gapped-fallback-sshfallback) for worked examples. Or open a network path from the VM to `<management-cluster-api-server-host>:6443`. |
| Bootstrap Secret rendered before alpha-2 (no post-bootstrap providerID service block in the cloud-config) | Controller no longer patches `Node.Spec.ProviderID` (PR-8 deleted `ensureProviderIDOnNodes`). The old Secret also has no in-node patch script. `Node.Spec.ProviderID` stays empty → CAPI Machine controller cannot match the NodeRef → Machine stays `NodeNotFound`. | **Required**: trigger a KCP rollout (or delete the KairosConfig) so the bootstrap controller re-renders the Secret with the alpha-2 template, which includes the post-bootstrap providerID service. |

### KD-3b part 2: Node.Spec.ProviderID is now set exclusively in-VM

PR-8 deletes `ensureProviderIDOnNodes` — the management-controller helper
that patched `Node.Spec.ProviderID` from outside the VM. That function was
the last piece of controller-side synchronous workload-cluster I/O (KD-10).

The in-VM cloud-config now owns `Node.Spec.ProviderID` exclusively:

- **k3s (CAPV and CAPK)**: kubelet receives `--kubelet-arg=provider-id=<value>`
  at startup via a `config.yaml.d/90-provider-id.yaml` drop-in written by a
  systemd `ExecStartPre` unit. A DMI-based discovery script runs when the
  providerID was not known at cloud-config render time (first-boot race).
- **k0s (CAPV and CAPK)**: a post-bootstrap systemd service patches
  `Node.Spec.ProviderID` using the local `/var/lib/k0s/pki/admin.conf`
  after k0s starts, retrying 30 times at 5-second intervals. When ProviderID
  is empty at render time the patch is deferred to the next reconcile cycle,
  which re-renders the Secret with the correct value.

**For most operators this is a no-op.** If `Node.Spec.ProviderID` was already
set (alpha-1 or alpha-2 healthy clusters), the in-VM scripts either write the
same value idempotently (k3s) or skip the patch because the Node field is
already populated (k0s, which treats a non-empty providerID as immutable).

**If you maintain a fork or derivative that re-added `ensureProviderIDOnNodes`
between PR-7 and PR-8:** that helper is gone from the upstream source; the
code will not compile. Remove the derivative — the in-VM scripts cover the
same contract without management-cluster Node access.

To diagnose a stuck `Node.Spec.ProviderID` after PR-8, inspect the VM journal
rather than controller logs:

```bash
# On the workload node (k0s):
journalctl -u kairos-k0s-post-bootstrap --no-pager

# On the workload node (k3s):
journalctl -u kairos-k3s-post-bootstrap --no-pager
```

### Network reachability requirement

CAPV (and future non-CAPK infrastructure) VMs must reach the management
cluster's API server URL. Verify from a workload node before deploying:

```bash
curl -k https://<mgmt-api-server-host>:6443/api
```

For air-gapped or strictly-segmented network environments, enable
`Spec.SSHFallback` on the `KairosControlPlane`. See
[Air-gapped fallback (SSHFallback)](QUICKSTART_CAPV.md#air-gapped-fallback-sshfallback)
for the full operator procedure.

---

## v0.1.0-alpha.2 → v0.1.0-beta.1

Full release notes: [v0.1.0-beta.1](release-notes/v0.1.0-beta.1.md).

### Management cluster CAPI core must be v1.13.3+

This provider's dependency on `sigs.k8s.io/cluster-api` moves from v1.8 to
v1.13.3, adopting the v1beta2 contract-versioned object-reference model
(`ContractVersionedObjectReference` / `MachineNodeReference`) in place of
v1beta1 pointer-based references (commit `736aeb3`, ADR 0006).

**Required operator action:** upgrade the management cluster's Cluster API
core to v1.13.3 or later before upgrading this provider:

```bash
kubectl apply -f https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.13.3/cluster-api-components.yaml
```

**Deploy this provider's CRDs via kustomize or clusterctl, not raw `kubectl
apply -f config/crd/bases/`.** The CRDs must carry the
`cluster.x-k8s.io/v1beta2: v1beta2` contract label (applied by
`config/crd/kustomization.yaml`) or CAPI core cannot resolve this provider's
object references — the Machine owner reference is never set on
`KairosConfig` and bootstrap data never renders. `make deploy`, the
published `kairos-capi-provider.yaml`, and `clusterctl` all apply this
label correctly.

### RBAC addition: `customresourcedefinitions` read (commit `b31083f`)

The controller's ClusterRole gained `get;list;watch` on
`customresourcedefinitions.apiextensions.k8s.io`, required by
`external.GetObjectFromContractVersionedRef` to resolve which API version
satisfies the v1beta2 contract for a given object reference. Without this
rule, every infra-object read is `Forbidden` and no Machines are created —
this only surfaces against a real API server (envtest's fake client does
not enforce RBAC), so it is easy to miss if you manage RBAC separately from
the shipped manifest. Included in the release manifest; if you maintain a
hand-rolled RBAC configuration, add this rule before upgrading.

### `KairosControlPlane.spec.machineTemplate.metadata` is now a pointer (commit `58bc284`)

The field changed from a value type to `*clusterv1.ObjectMeta` so an empty
value is omitted from the API payload instead of round-tripping as `{}`.
CAPI v1beta2's `ObjectMeta` schema requires `MinProperties=1`; the
value-type field previously caused every KCP reconcile against a real API
server to be rejected by schema validation. **No operator action required**
unless you have external tooling that constructs `KairosControlPlane`
objects programmatically and depends on the Go field being a value type —
the YAML shape (including an explicit `metadata: {}`) is unaffected.

### `KairosControlPlane.spec.replicas` now accepts `3` and `5` (HA)

Previously only `1` was accepted. `replicas: 3` or `5` now provisions a
multi-node control plane; even counts and values above `5` are
webhook-rejected. Existing `replicas: 1` deployments are unaffected. See
[docs/HIGH_AVAILABILITY.md](HIGH_AVAILABILITY.md)
for the new HA fields (`spec.ha.vip`, provider-specific endpoint mechanism)
and the [HA sample manifests](../config/samples/) for CAPK, CAPV, and CAPM3.

**No in-place single-node-to-HA conversion.** To move an existing
single-node cluster to HA, provision a new HA cluster rather than editing
`spec.replicas` on a running single-node `KairosControlPlane`.

---

## v0.1.0-beta.1 → v0.1.0-beta.2

Full release notes: [v0.1.0-beta.2](release-notes/v0.1.0-beta.2.md).

This release adds a second, `clusterctl`-native install path (ADR 0007) and
bumps the required CAPI core version. Neither change force-migrates an
existing install.

### If you stay on the flat manifest: no action required

`kairos-capi-provider.yaml` keeps the same namespace (`kairos-capi-system`)
and provider label (`cluster.x-k8s.io/provider: kairos`) it has always had.
Upgrade in place with the usual `kubectl apply`:

```bash
kubectl apply -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0-beta.2/kairos-capi-provider.yaml
```

### If you want to move to `clusterctl`: not an in-place upgrade

The `clusterctl` providers use different namespaces and provider labels
than the flat manifest, and `Deployment.spec.selector` /
`ClusterRoleBinding.roleRef` are immutable, so moving to `clusterctl` is an
uninstall-old / install-new operation, not a re-apply. See
[Moving from the flat install to clusterctl packaging](#moving-from-the-flat-install-to-clusterctl-packaging)
below for the full procedure and the namespace/label comparison table.

### CAPI core v1.13.4 required

The management-side CAPI dependency moves from v1.13.3 to v1.13.4, a
low-risk patch release — `controller-runtime` (v0.23.3) and `k8s.io/*`
(v0.35.4) are unchanged. Upgrade the management cluster's Cluster API core
to v1.13.4 or later before upgrading this provider:

```bash
kubectl apply -f https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.13.4/cluster-api-components.yaml
```

---

## Moving from the flat install to clusterctl packaging

Full design: ADR 0007 (maintained in the repository's internal decision records).

This provider now also ships as two `clusterctl` providers (see
[docs/INSTALL.md: Path 1](INSTALL.md#path-1---clusterctl-recommended)),
alongside the existing flat, `kubectl apply` manifest (Path 2). **If you are
staying on the flat manifest, no action is required**: `kairos-capi-provider.yaml`
keeps the same namespace (`kairos-capi-system`) and provider label
(`cluster.x-k8s.io/provider: kairos`) as before.

**If you want to move an existing flat installation to `clusterctl`**, treat
it as an uninstall-old / install-new operation, not an in-place re-apply:

| | Flat install (unchanged) | clusterctl providers (new) |
| --- | --- | --- |
| Namespace | `kairos-capi-system` | `capi-kairos-bootstrap-system` (bootstrap), `capi-kairos-control-plane-system` (control plane) |
| `cluster.x-k8s.io/provider` label | `kairos` | `bootstrap-kairos`, `control-plane-kairos` |

`Deployment.spec.selector` and `ClusterRoleBinding.roleRef` are immutable, and
the namespaces differ, so applying the clusterctl components over an existing
flat install (or the reverse) does not update it in place. It creates a
second, independent set of controllers and webhooks watching the same CRDs.
This is unsupported; see
[docs/INSTALL.md: the two paths are mutually exclusive](INSTALL.md#path-1-and-path-2-are-mutually-exclusive).

To migrate:

1. Uninstall the flat manifest:
   ```bash
   kubectl delete -f https://github.com/kairos-io/cluster-api-provider-kairos/releases/download/v0.1.0-beta.1/kairos-capi-provider.yaml
   ```
2. Confirm no stale webhook configurations remain (see the re-install note in
   [docs/INSTALL.md: Path 2 Uninstall](INSTALL.md#uninstall-1)):
   ```bash
   kubectl get mutatingwebhookconfigurations,validatingwebhookconfigurations | grep kairos
   ```
3. Install via `clusterctl` per
   [docs/INSTALL.md: Path 1](INSTALL.md#path-1---clusterctl-recommended).

Existing `KairosConfig`, `KairosConfigTemplate`, `KairosControlPlane`, and
`KairosControlPlaneTemplate` objects, and the CRDs themselves, are unaffected
by the migration: only the controller Deployments, RBAC, webhook
configurations, and their owning namespace/label change. Machines already
provisioned keep running; reconciliation resumes once the new controllers
start.

### CA injection no longer needs a post-install script

`hack/post-install-webhook-ca-injection.sh` is gone. Webhook CA injection is
handled entirely by cert-manager's standard `cert-manager.io/inject-ca-from`
annotation on each `MutatingWebhookConfiguration` /
`ValidatingWebhookConfiguration`, wired per namespace by kustomize (KD-1).
There is no post-install CA-injection step on any install path (`clusterctl`
or flat manifest). If you have automation that runs the old script, remove
that step.

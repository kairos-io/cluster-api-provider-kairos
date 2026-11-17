# Changelog

All notable changes to the Kairos Cluster API provider are documented in this
file. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

This project is pre-1.0. Alpha releases may include breaking changes; those are
called out explicitly under **Breaking changes**. Per-scenario migration steps
live in [docs/UPGRADING.md](docs/UPGRADING.md).

## [v0.2.3] - 2026-11-17

A fix release for the v0.2 series. It hardens the k0s etcd leave that runs
when a control-plane Machine is deleted, and makes a `KairosConfig`'s role,
distribution and control-plane hostname impossible to change once it exists.
The `KairosConfig` CRD gains validation rules; no field is added or removed.
Check the stored values, and remove any etcd-leave annotation from
`machineTemplate.metadata`, before upgrading. See the
[v0.2.3 release notes](docs/release-notes/v0.2.3.md) and
[docs/UPGRADING.md](docs/UPGRADING.md#v022--v023).

### Fixed

- **A `KairosConfig`'s `spec.role`, `spec.distribution` and, on a
  control-plane config, `spec.hostname` cannot change after creation, and
  `spec` cannot be removed** (#231). Validation rules in the CRD reject the
  update with `Forbidden`; creates, other updates and `KairosConfigTemplate`
  are not affected.
- **The k0s etcd leave of a Machine without a Node uses a recorded node
  name** (#225): the annotation
  `controlplane.cluster.x-k8s.io/rendered-node-name`, set when the Machine is
  created and backfilled on existing control-plane Machines that are not
  being deleted, instead of a name worked out from the `KairosConfig` on every
  pass.
- **A member's absence from the `<cluster>-etcd-status` Secret no longer ends
  its etcd leave** (#227).
- **The etcd-leave checks see every control-plane Machine of the Cluster**,
  whoever owns it, and a check that cannot run releases the leave after the
  usual ~15 minutes (#228).
- **`machineTemplate.metadata` no longer copies the etcd-leave hook or the
  leave's state annotations**, and no longer reaches Machines being deleted
  (#226).

### Changed

- New annotation on every control-plane Machine the controller creates:
  `controlplane.cluster.x-k8s.io/rendered-node-name`. It is reserved, as are
  the etcd-leave hook and the four etcd-leave state annotations, which the
  controller removes from Machines that are not being deleted.
- New Warning events on the `KairosControlPlane`:
  `EtcdLeaveNodeNameContradictsKairosConfig` and `EtcdLeaveNodeNameUnusable`.
- Deleting a k0s control-plane Machine whose etcd-status entry is missing now
  waits for the node's ack, for k0s, or for `EtcdMemberLeaveTimedOut`.
- An etcd-leave hook that a template put on k3s or single-node control-plane
  Machines must be removed by hand; until then a k3s control plane holds with
  `DistributionMismatch`.

## [v0.2.2] - 2026-11-17

A fix release for the v0.2 series. The main fix is for the control plane's
distribution: a `KairosControlPlane` whose `spec.distribution` changed after
its Machines existed could skip a departing k0s member's etcd leave and leave
a voting etcd member behind. There is no CRD schema change; only the
description of `spec.distribution` changes. Check each control plane's
distribution before upgrading. See the
[v0.2.2 release notes](docs/release-notes/v0.2.2.md) and
[docs/UPGRADING.md](docs/UPGRADING.md#v021--v022).

### Fixed

- **The control-plane distribution follows its Machines** (#223). The
  `KairosConfig` of each control-plane Machine records what its node runs.
  When Machines exist and `spec.distribution` differs from it, or they
  disagree, or none can be read, the control plane creates and deletes no
  control-plane Machines, mints or extends no join tokens and removes no
  orphaned etcd members, with `Ready` False and reason `DistributionMismatch`. An empty
  `spec.distribution` is written back from the Machines. A Machine carrying
  the etcd-leave hook always runs the etcd leave. A change between two
  non-empty values is admitted with a warning.
- **A `kubeadm` `KairosConfigTemplate` is refused even with an explicit
  `spec.distribution`** (#223), with reason `UnsupportedDistribution`.
- **`KairosControlPlaneTemplate` no longer defaults `distribution` to `k0s`**
  (#222), which overrode a `k3s` `KairosConfigTemplate` on every ClusterClass
  control plane.
- **k0s post-bootstrap no longer holds up the boot** (#155). Users and SSH
  keys are applied without waiting for the join.

### Security

- **The image is built with Go 1.27.4**, which fixes GO-2026-6859
  (CVE-2026-97054) in `net/http`. No package linked into the manager uses
  `MaxBytesReader` or `MaxBytesHandler`, which the crash needs.
- `golang.org/x/net` v0.62.0 (#209), `golang.org/x/crypto` v0.59.0 (#211) and
  `go.opentelemetry.io/otel/trace` v1.47.0 (#214), for advisories that are not
  reachable from the manager.
- The release job keeps its token off disk, drops the unused QEMU step, and
  fails when an asset is missing (#203, #213).

### Changed

- New events on the `KairosControlPlane`: `DistributionMismatch`,
  `UnsupportedDistribution`, `DistributionMismatchResolved`,
  `DistributionInherited`, `DistributionRestored`, `DistributionWriteFailed`
  and `EtcdLeaveHookContradictsKairosConfig`.
- A `KairosControlPlane` with an explicit `k0s` or `k3s` that references a
  `kubeadm` `KairosConfigTemplate` stops creating and deleting Machines.
- `KairosControlPlaneTemplate` objects created by v0.2.1 or earlier keep
  `distribution: k0s`. Set it explicitly or remove it; removing it is now
  safe.
- The k0s post-bootstrap change reaches only Machines created after the
  upgrade.

## [v0.2.1] - 2026-10-22

A fix release for the v0.2 series. The main fix is for k0s control planes with
high availability: under a specific failure sequence, a joining k0s controller
could start a new, empty cluster with the same CA instead of joining, and the
original cluster was lost while every condition stayed True. This release
closes that on the joining node and in the `KairosControlPlane`. There is no
CRD schema change. Read the upgrade notes first: new control-plane Machine
names, a validated control-plane hostname, and a workload identity pin to
pre-seed on existing HA clusters. See the
[v0.2.1 release notes](docs/release-notes/v0.2.1.md) and
[docs/UPGRADING.md](docs/UPGRADING.md#v020--v021).

### Fixed

- **A k0s control plane could be silently replaced by a new cluster** (#193,
  #194, #195, #196).
  - A joining k0s controller clears a half-written etcd database before it
    starts, and if it finds it is the only etcd member it stops k0s and never
    registers (#196).
  - The `KairosControlPlane` pins the workload cluster's identity (its
    `kube-system` namespace UID) and checks it before every membership change.
    On a change it freezes, with reason `WorkloadClusterIdentityChanged`, and
    only an operator can clear it (#193).
  - A control-plane Machine without a Node now leaves etcd before it is
    released, bounded at about 15 minutes, and etcd members that no Machine
    accounts for are removed one at a time when quorum allows (#194).
  - Control-plane Machine names are random and never reused (#195).
- **Post-bootstrap no longer blocks boot** on kubeadm and k3s nodes, and the
  providerID drop-ins are written atomically (#149).

### Security

- **The manager no longer keeps Secret bodies in memory** (#199). Secrets are
  read live and watched by metadata only. RBAC is unchanged.
- **The kubeadm worker path connects on demand** to a KubeadmControlPlane or
  Kamaji cluster, instead of keeping a connection to each, and every request is
  bounded by a 10s timeout (#200).
- **Releases are gated on a whole-tree vulnerability scan** (osv-scanner
  v2.7.0), which stops the release on an open advisory or an incomplete scan
  (#197). Pull request scans compare against the base branch (#198).
- **The image is built with Go 1.26.10**, fixing GO-2026-6809 (CVE-2026-111943)
  in `crypto/x509`.

### Changed

- New control-plane Machines are named `<kcp-name>-<5 characters>`. Find
  control-plane Machines by label, not by name.
- A new or changed control-plane `spec.hostname` must be a lowercase DNS-1123
  label without `{{`.
- While a control plane is frozen, anything that waits on the Cluster being
  `Ready` or `Available` holds. Worker joins continue.
- Existing HA control planes are pinned on the first reconcile after the
  upgrade. Pre-seed the pin annotation before upgrading; see
  [Before upgrading an existing HA cluster](docs/HIGH_AVAILABILITY.md#before-upgrading-an-existing-ha-cluster).
- The k0s join guard reaches only control-plane members created after the
  upgrade.

## [v0.2.0] - 2026-10-18

The first release of the v0.2 series. It adds kubeadm workers, hardens how the
provider handles workload-cluster kubeconfigs and how nodes talk to the
management cluster, and carries every fix from v0.1.5. There is no CRD schema
change, but some behaviour changes; see the
[v0.2.0 release notes](docs/release-notes/v0.2.0.md) and
[docs/UPGRADING.md](docs/UPGRADING.md#v015--v020).

### Added

- **kubeadm workers** (#141, #148, #191). `spec.distribution: kubeadm` on a
  `KairosConfig` or `KairosConfigTemplate` bootstraps a Kairos worker that
  joins a control plane managed outside this provider with `kubeadm join`.
  `KamajiControlPlane` and `KubeadmControlPlane` are supported out of the box;
  other kinds can be allowed with `--kubeadm-extra-controlplane-kinds`.
  `spec.kubeadm.joinConfiguration` customises the join. A kubeadm control plane
  run by this provider, `MachinePool` workers and CNI installation are not in
  scope. See [docs/KUBEADM_WORKERS.md](docs/KUBEADM_WORKERS.md).

### Security

- **Workload kubeconfigs are sanitized and pinned** (#190). Only the control
  plane writes `<cluster>-kubeconfig`; nodes patch a staging Secret,
  `<cluster>-kubeconfig-push`. Controllers accept only inline credentials and
  an inline CA, and the CA is pinned on first use in the `kubeconfig-ca-pin`
  annotation. A published kubeconfig that fails validation is emptied, and
  `KubeconfigReady` reports `WaitingForNodePush`.
- **Narrower node permissions** (#190). The node push token can only `patch`
  two named, controller-created Secrets. Existing Roles are narrowed at
  startup.
- **Node-to-management calls verify TLS** (#190), with no insecure fallback.
- `golang.org/x/crypto` v0.58.0 (#174).

### Fixed

- **A kubeadm worker that ran `kubeadm join` more than 15 minutes after its
  first render failed** (#191). One token per worker is now reused, extended
  until the node joins, and revoked afterwards.
- **`kubeadm join` failed when the providerID patch was passed with `--patches`
  next to `--config`** (#148). The patches directory is restricted at admission
  to a path under `/etc/kubernetes/`.
- The KCP's own template reference follows the same-namespace rule (#126).
- Every fix in [v0.1.5](docs/release-notes/v0.1.5.md).

### Changed

- **Node pushes need a management CA.** If the management API is reached
  through a load balancer or proxy the in-cluster CA does not cover, set
  `KAIROS_MANAGEMENT_API_CA_FILE` on the bootstrap provider. Without a usable
  CA, a control-plane `KairosConfig` reports
  `BootstrapReady=False(ManagementCAUnavailable)`. See
  [docs/UPGRADING.md](docs/UPGRADING.md#node-to-management-pushes-are-now-tls-verified).
- Nodes booted before the upgrade keep their old bootstrap data, and their next
  push is refused once (403) because their Role was narrowed.
- Control-plane workload operations wait for the CA pin, so the first reconcile
  after the upgrade may requeue once.
- A kubeadm worker's control plane whose kubeconfig uses file paths, exec or
  auth-provider plugins, or several contexts is refused.
- `metadata.yaml` lists the 0.2 series, with the same v1beta2 contract.
- Existing `<cluster>-kubeconfig` Secrets are revalidated on the first
  control-plane reconcile after the upgrade. Check the `kubeconfig-ca-pin`
  annotation on each `KairosControlPlane`.

### Known issue

- A k0s control-plane Machine that joined etcd but never got a Node reference
  is removed without leaving etcd. See the
  [v0.1.5 release notes](docs/release-notes/v0.1.5.md#known-issue) for the
  workaround.

## [v0.1.5] - 2026-10-17

A fix release for the v0.1 series, for k0s control planes with high
availability (`replicas` > 1 with a kube-vip VIP): three day-2 failures found
while replacing control-plane members. There is no CRD schema change. See the
[v0.1.5 release notes](docs/release-notes/v0.1.5.md) and
[docs/UPGRADING.md](docs/UPGRADING.md#v014--v015).

### Fixed

- **New k0s control-plane members now serve a certificate valid for the VIP**
  (#163). Only the first node carried the control-plane endpoint in its API
  SANs.
- **No new member could join after the first node was replaced, or on a cluster
  older than a day** (#166). The `KairosControlPlane` now mints one short-lived
  join token per joining member through the workload API, pointed at a live
  member. The first node no longer creates or pushes a token.
- **Removing the member that holds the VIP stalled** (#181, #182, #183).
  - The leave request and acknowledgement go through a member that is staying.
    The leave starts after drain and volume detach, and after 5 minutes without
    an acknowledgement the control plane asks k0s to remove the member.
  - kube-vip elects its leader through its own node's API server, so a member
    that lost etcd gives up the VIP.
  - The node's leave responder bounds every call, retries the acknowledgement,
    never runs a completed leave twice, and no longer acknowledges a failed
    `k0s etcd leave`.

### Security

- `golang.org/x/crypto` v0.57.0 to v0.58.0 (#186), with `x/sync`, `x/sys`,
  `x/term`, `x/text` and `x/tools`. Routine maintenance; `govulncheck` reports
  nothing reachable at v0.57.0.
- **The node-push Role narrows** (#166). Nodes no longer get access to the
  control-plane join-token Secret.

### Changed

- New annotations written by the `KairosControlPlane`:
  `controlplane.cluster.x-k8s.io/k0s-join-token-id`,
  `controlplane.cluster.x-k8s.io/k0s-join-token-removed-at`,
  `controlplane.cluster.x-k8s.io/etcd-member-leave-requested-at` and
  `controlplane.cluster.x-k8s.io/etcd-member-leave-member-id`.
- New events: `K0sJoinTokenMinted`, `K0sJoinTokenNoLiveMember`,
  `K0sJoinTokenMissing`, `K0sJoinTokenServerUnavailable` and
  `EtcdMemberLeaveDelegated`.
- The kube-vip and responder changes (#182, #183) reach only control-plane
  members created after the upgrade. kube-vip needs the API server on
  `127.0.0.1:6443`.

### Known issue

- A control-plane Machine that joined etcd but never got a Node reference is
  removed without leaving etcd. Before deleting a Machine with no
  `status.nodeRef`, check `k0s etcd member-list` on a healthy member and remove
  its member first. See the release notes for the steps.

## [v0.1.4] — 2026-10-15

A fix release for the v0.1 series: ten control-plane and bootstrap fixes and
the Go security updates published since v0.1.3. It is cut from `main` before
the kubeadm worker distribution merged, which ships in v0.2.0. There is no CRD
schema change. Updates that reference another namespace are now rejected, so
check for those before upgrading; see the
[v0.1.4 release notes](docs/release-notes/v0.1.4.md) and
[docs/UPGRADING.md](docs/UPGRADING.md).

### Fixed

- **A control-plane rollout could delete a member before its replacement
  joined** (#117). On HA, an etcd member was removed while its replacement
  was still booting. On a single-node control plane, a `spec.version` change
  replaced the only control plane with an empty cluster.
  - Single-node control planes are no longer rolled out by replacement;
    `MachinesUpToDate` reports `SingleNodeRolloutUnsupported`.
  - HA removes an outdated Machine only after every up-to-date Machine has
    joined, one membership change at a time.
- **A CAPK cluster installed with `clusterctl` never became reachable**
  (#118). The control-plane provider's role could not read
  VirtualMachineInstances, so the API load balancer never got a backend. The
  flat manifest was unaffected.
- **`Cluster.spec.paused` was ignored** by both reconcilers (#109). Deleting a
  `KairosControlPlane` while its Cluster was paused went straight into
  teardown (#120). Both now wait for the unpause.
- **`clusterctl move` left the referenced machine template and
  `KairosConfigTemplate` behind** (#110). They now carry a (non-controller)
  owner reference to the Cluster.
- **`machineTemplate.nodeDrainTimeout` was ignored** (#112). It now reaches
  the Machine, existing Machines included, and a value too large for int32
  seconds is capped rather than wrapping to "no deadline" (#115).
- **`machineTemplate.metadata` never reached the `Machine`**, only the
  infrastructure machine, so its labels never reached the Node (#114). It is
  now applied to new and existing Machines, additively.
- **`caCertHashes` and `caCertSecretRef` were documented as securing the join
  but are read by nothing.** Setting either now returns an admission warning
  (#116).

### Security

- **References to another namespace are refused** (#119). A `KairosConfig`
  Secret reference could name any namespace, and the controller copied that
  Secret into bootstrap data the author can read. A `KairosControlPlane`
  `machineTemplate.infrastructureRef` in another namespace was cloned, and
  given an owner reference that let the garbage collector delete it. Both
  webhooks and the controllers now refuse these.
- **The image is built with Go 1.26.9**, clearing thirteen standard-library
  advisories, eight of them in `net/http` and HTTP/2. The release notes list
  each one.
- **`golang.org/x/net` v0.58.0 to v0.60.0** (#143), clearing GO-2026-6603,
  GO-2026-6610, GO-2026-6611, GO-2026-6612 and GO-2026-6617, with
  `golang.org/x/crypto` to v0.57.0 alongside.

### Changed

- `github.com/go-logr/logr` v1.4.3 to v1.4.4 (#129).

### Documentation

- The `clusterctl` provider URLs in `docs/INSTALL.md` are now in the form
  `clusterctl` parses. `releases/latest/download/...` works only until
  `clusterctl` falls back to the GitHub API. The sample manifests are
  repaired so the API server accepts them (#111).

## [v0.1.3] — 2026-09-17

A fix and supply-chain release. One real bootstrap bug, found on bare metal,
plus the migration to the kairos-io org's scanning and dependency tooling. No
CRD or API change; no migration steps from v0.1.2. See the
[v0.1.3 release notes](docs/release-notes/v0.1.3.md).

### Fixed

- **A `KairosConfig` could render its bootstrap data before Cluster API had
  published the control-plane endpoint**, and a node installs from the first
  bootstrap Secret it is handed, so regenerating it later did not help.
  Observed on a 3-replica k0s control plane using `spec.ha.vip`: the Secret was
  written 23 seconds before CAPI wrote `spec.controlPlaneEndpoint`, so the node
  installed with no `/etc/k0s/k0s.yaml`, k0s came up with API SANs that omitted
  the VIP, and the node pushed `server: https://localhost:6443` into the cluster
  kubeconfig. CAPI could never reach the workload cluster and the control plane
  held every joiner indefinitely. `Reconcile` now waits for
  `Cluster.status.initialization.infrastructureProvisioned`, and a control-plane
  role additionally waits for a usable `Cluster.spec.controlPlaneEndpoint`,
  reporting `WaitingForClusterInfrastructure` (severity `Info`) meanwhile. A
  `Cluster` watch re-reconciles waiting configs as soon as the cluster is
  provisioned rather than relying on an unrelated event.

### Security

- `golang.org/x/crypto` v0.55.0 to v0.56.0 and `golang.org/x/mod` v0.38.0 to
  v0.40.0, clearing GO-2026-6354, GO-2026-6355, GO-2026-6179 and GO-2026-6180.
  Two of those were published against the `x/crypto` version this project moved
  to for v0.1.2, three days after that release.
- Both base images in the `Dockerfile` are now pinned by digest — the multi-arch
  index digest, so the `linux/arm64` half of the release build is unaffected —
  so a repointed upstream tag cannot change what ships.

### Changed

- **Scanning and dependency updates now follow the kairos-io org practice.**
  Trivy is replaced by Google's reusable OSV-Scanner workflow, which runs on
  pull requests, on pushes to `main`, and weekly, and reports into Security >
  Code scanning. Dependabot is replaced by Renovate (`renovate.json`), with the
  Go toolchain and the Cluster API / Kubernetes library set held for explicit
  approval because each is one coordinated decision rather than a set of
  independent patches. Dependabot security updates are disabled at the
  repository level.
  - Scope note: OSV-Scanner reads declared dependencies, where Trivy read the
    built image including its base layer. Base-image CVEs are now addressed by
    the digest bumps Renovate proposes rather than by a scanner.
  - `osv-scanner.toml` ignores `GO-2026-5932` only. It is not a CVE and has no
    fixed version in any release: it records that `golang.org/x/crypto/openpgp`
    is unmaintained, and applies only to code importing those packages, which
    this module does not. The file carries the verification command.
- The e2e harness pins the CDI manifests to `v1.66.1` instead of fetching
  `releases/latest/download`, which was the last input to the e2e cluster that
  could change with no commit. Unit tests now guard every manifest URL in that
  package against a silent return to a floating tag.

### Documentation

- `docs/API_REFERENCE.md` documents the new waiting behaviour and the
  `WaitingForClusterInfrastructure` reason, including what to check if a
  `KairosConfig` stays in that state.
- `docs/TESTING.md` covers dependency scanning and how updates now arrive.

## [v0.1.2] — 2026-09-09

A fix release, driven by running the providers against real infrastructure for
both k0s and k3s, single-node and highly available. No CRD or API change; no
migration steps from v0.1.1. See the
[v0.1.2 release notes](docs/release-notes/v0.1.2.md) and
[docs/UPGRADING.md](docs/UPGRADING.md).

### Added

- Support for **any CAPI-conformant infrastructure provider**. The infra-machine
  cloner and the control plane's node-IP lookup each hardcoded a list of
  provider kinds, so an unlisted provider was rejected with
  `unsupported infrastructure provider: <Kind>MachineTemplate`. Both now fall
  back to the CAPI contract — a `<Kind>MachineTemplate`'s `spec.template.spec`
  is the spec of the `<Kind>Machine` it produces, and `status.addresses` is
  mandatory on an InfraMachine — taking group and version from the template
  object itself. Verified end to end against Beskar7. CAPD, CAPV, CAPK, CAPM3
  and the Kairos fleet provider remain the first-class path.

### Fixed

- **k3s HA came up as three unrelated single-node clusters.** `k3s.args` is
  delivered as a systemd drop-in, which on an instrumented run landed three
  seconds after `k3s.service` had already started. k3s selects its datastore on
  first start and never migrates, so `--cluster-init`, `--server` and
  `--token-file` were ignored for the life of the node, silently: the init node
  reported "etcd disabled", every joiner bootstrapped its own cluster with its
  own CA, and the control plane reported Ready with no quorum anywhere. The
  flags that must be right on the first start are now also pinned in
  `config.yaml.d`, which k3s re-reads on every start.
- **k0s controller arguments were lost on a losing race.** The same drop-in
  timing cost k0s its flags on two of five measured boots, leaving a bare
  `k0s controller`: no worker role, so no kubelet, so no Node, so no
  `providerID` patch. Arguments now go in a drop-in that sorts last, and a
  post-bootstrap check compares the live argv against the effective `ExecStart`
  and restarts only when they differ.
- **A single lost race stranded a k0s HA cluster permanently.** The
  controller-join token push made one attempt and, on failure, logged "will
  retry on next boot" from a oneshot unit that exited zero with nothing to
  reboot the node. The push and the etcd-status report now retry.
- **k0s advertised an unroutable address on CAPV.** The CAPV template — the
  default for every non-KubeVirt provider — carried neither `spec.api.address`
  nor `spec.storage.etcd.peerAddress`, so on a dual-homed bare-metal node k0s
  advertised its provisioning address and baked it into the join token. Both
  are now resolved before k0s starts, by routing; when no address can be found
  the settings are removed and k0s auto-detects, rather than wedging the node.
- **Concurrent HA joiners could destroy etcd quorum.** The joinable gate only
  inspected the init machine, so every joiner was created in the same second;
  two `etcd member add` calls against a one-member cluster leave quorum unmet
  and the cluster does not recover. Joiners are now created one at a time,
  strictly for k0s and advisorily for k3s, which joins as a learner. The
  rollout surge path, which had no quorum check at all, gets the same gate.
- **A `machineTemplate.infrastructureRef` without a namespace failed to
  resolve.** It is now resolved in the `KairosControlPlane`'s own namespace, as
  the CAPI convention requires and as clusterctl-generated templates rely on.
  Previously this surfaced only as `Ready=False` with no Machine and no
  infrastructure object created.
- The CAPK HA samples pointed all three control-plane VMs at the same two named
  DataVolumes. Those PVCs are ReadWriteOnce, so the samples could not work as
  written; both now use `dataVolumeTemplates`.

### Security

- `golang.org/x/crypto` v0.53.0 to v0.55.0, fixing **CVE-2026-56854
  (CRITICAL)** — an authentication bypass in `golang.org/x/crypto/ssh` from
  unenforced source-address restrictions. The SSH fallback worker authenticates
  with that package, so this is on a code path the provider uses.
- `golang.org/x/net` v0.55.0 to v0.57.0 (CVE-2026-46600, HIGH) and
  `golang.org/x/text` v0.38.0 to v0.41.0 (CVE-2026-56852, HIGH). Scanning the
  release image for fixable CRITICAL and HIGH findings now reports zero.
- **CI now scans the controller image.** Nothing looked at the published
  artifact before, which is how the CVE above reached a shipping image. A new
  `Image scan` workflow builds the image from the release `Dockerfile` on every
  push and pull request and fails on any fixable CRITICAL or HIGH finding, and
  runs weekly on `main` as well — a CVE is normally disclosed long after the
  vulnerable dependency merged, so a code-triggered run alone would never catch
  it.

### Documentation

- The CAPK quickstart documents the **k0s image start gate** a 3-node k0s
  control plane needs, the failure it prevents, and how to verify an image has
  it. The fix is image-side, so this provider cannot ship it.
- `docs/HIGH_AVAILABILITY.md` explains serial joiner creation and what it means
  for scale-up time and for diagnosing a stalled rollout.
- `docs/API_REFERENCE.md` gains the `KairosControlPlane.spec.sshFallback` block,
  which was implemented and referenced from the CAPV quickstart but had no
  entry in the CRD reference, and corrects `role` from required to optional.
- Bug reports are directed at [kairos-io/kairos](https://github.com/kairos-io/kairos/issues).

## [v0.1.1] — 2026-08-10

A small maintenance release; no functional change to the controllers or CRDs.
No migration steps from v0.1.0. See the
[v0.1.1 release notes](docs/release-notes/v0.1.1.md).

### Changed

- The clusterctl provider identity is now `kairos-io`: the
  `cluster.x-k8s.io/provider` label on the clusterctl bootstrap and
  control-plane components changed from `bootstrap-kairos` /
  `control-plane-kairos` to `bootstrap-kairos-io` / `control-plane-kairos-io`,
  ahead of registering the provider in the upstream clusterctl built-in list as
  `kairos-io`. Applied with `includeSelectors: false`, so Deployment selectors
  are unchanged. The flat `kubectl apply` manifest keeps its `kairos` identity.
- Docs: corrected stale `v0.1.0-beta.2` install references to `v0.1.0` and the
  fleet-provider floor to `v0.1.0+`.

## [v0.1.0] — 2026-08-10

The first non-beta 0.1.0 release. Adds the **Kairos fleet (AuroraBoot)
infrastructure provider** as a first-class peer to CAPD/CAPV/CAPK/CAPM3, and
fixes two multi-node bugs found by an integrated real-hardware end-to-end
run: worker `MachineDeployment`s never scaled up on any infrastructure
provider, and a single k0s control plane refused all worker joins. See the
[v0.1.0 release notes](docs/release-notes/v0.1.0.md) for full detail.

### Breaking changes

None in this release.

### Added

- **Kairos fleet infrastructure provider support** (ADR 0008). Fleet claims
  already-enrolled Kairos nodes from a named AuroraBoot group rather than
  creating machines on demand. New: `docs/QUICKSTART_FLEET.md`, k3s and k0s
  control-plane-plus-worker samples
  (`config/samples/fleet/kairos_cluster_{k0s,k3s}_with_workers.yaml`), a
  standalone-single-node k0s sample
  (`kairos_cluster_k0s_single_node.yaml`), and `clusterctl.yaml`
  registration guidance in `docs/INSTALL.md`. Requires fleet provider
  `v0.1.0-beta.2` or later — `v0.1.0-beta.1` crash-loops on a real
  management cluster.
- **`KairosFleetMachine` recognized as a supported infrastructure kind end
  to end**: `getNodeIP` extractor support, actionable endpoint-wait
  guidance, RBAC parity (`create;get` on `kairosfleetmachines`, `get` on
  their `status` and on `kairosfleetmachinetemplates`), template cloning,
  and node-push kubeconfig support.
- **`KairosConfig`/`KairosConfigTemplate.spec.k0sSingleNode`** (`*bool`,
  default `false`). Opts a single-node k0s control plane on the generic /
  CAPV / CAPM3 / fleet render path into the standalone k0s `--single` mode
  (refuses all joins), instead of the new default `--enable-worker`
  (joinable, schedulable). k0s control-plane machines only; k3s and
  worker/join nodes ignore it. CAPK is unchanged (always `--single` for
  single-node).
- **`KairosControlPlaneStatus.Version`**, part of the CAPI control-plane
  contract. Set once at least one control-plane member is ready.

### Fixed

- **Worker `MachineDeployment`s now scale up, on every infrastructure
  provider.** `status.version` was never set, so CAPI core's
  `ControlPlaneIsStable` preflight read every Kairos control plane as
  perpetually provisioning and refused to create worker Machines. Fixed by
  the new `status.version` field above. Found by the integrated multi-node
  e2e on real hardware — single-node kind runs never exercised a worker
  `MachineDeployment`, so the gap was invisible there.
- **A single k0s control plane now accepts workers.** It rendered k0s
  `--single` unconditionally, refusing every join. The generic / CAPV /
  CAPM3 / fleet render path now defaults to `--enable-worker`; the
  standalone behavior is preserved via the opt-in `spec.k0sSingleNode`
  field above. Validated on real hardware: a fleet-backed k0s cluster with
  a joined worker, both `Node`s `Ready` and providerIDs matched.

### Security

- **No new RBAC surface beyond the enumerated fleet infra-kind grants** —
  the same shape already granted for every other infrastructure provider's
  machine/template kinds; no cluster-wide or wildcard grant added.
- **The `kairos-fleet://<node-id>` providerID contract is a
  cross-repository string match with no compile-time link**, guarded today
  by cross-referencing doc comments and unit tests on each side; an
  integrated e2e is the intended long-term guard (tracked, not yet in CI).
- **AuroraBoot admin-token handling is out of this repo's scope.** The
  token Secret referenced by
  `KairosFleetCluster.spec.auroraboot.adminTokenSecretRef` is read and
  stored by the fleet provider. Samples and quickstart instruct creating it
  out of band and never committing a real token.

### Known limitations

- Fleet HA (`replicas: 3`/`5`) is mechanically reachable but not exercised
  — no fleet HA sample is shipped (ADR 0008).
- The integrated three-provider (bootstrap + control-plane + fleet) path is
  validated manually on real hardware, not yet in this repository's CI.
- No cosign signing or SBOM yet on release artifacts (carried forward from
  v0.1.0-beta.2).
- k3s HA day-2: control-plane replacement orphans an etcd member (KD-5d).
  k0s remains the fully-supported HA distribution.

## [v0.1.0-beta.2] — 2026-08-09

Beta-2 adds **`clusterctl`-installable packaging** (ADR 0007): one image,
packaged as two `clusterctl` providers (`bootstrap-kairos`,
`control-plane-kairos`), alongside the existing flat `kubectl apply`
manifest. Baseline moves to Kubernetes v1.36 (validated/recommended across
the CAPI v1.13 management band) and CAPI core v1.13.4. See the
[v0.1.0-beta.2 release notes](docs/release-notes/v0.1.0-beta.2.md) for full
detail.

### Breaking changes

- **The `clusterctl` install path uses new namespaces and labels, and is
  mutually exclusive with the flat manifest.** `clusterctl` providers use
  `capi-kairos-bootstrap-system`/`bootstrap-kairos` and
  `capi-kairos-control-plane-system`/`control-plane-kairos`, distinct from
  the flat manifest's `kairos-capi-system`/`kairos`. Because
  `Deployment.spec.selector` and `ClusterRoleBinding.roleRef` are
  immutable, applying one path over the other does not upgrade it in place
  — it stands up a second, independent set of controllers/webhooks
  watching the same CRDs, which is unsupported. **If you stay on the flat
  manifest, no action is required.** See
  [docs/UPGRADING.md — Moving from the flat install to clusterctl packaging](docs/UPGRADING.md#moving-from-the-flat-install-to-clusterctl-packaging).

### Added

- **Two `clusterctl` providers from one image** (ADR 0007), selected via a
  new `--controllers=bootstrap|control-plane|all` flag; a role-specific
  `LeaderElectionID` and `--namespace` flag bring single-namespace-watch
  compliance. A release now publishes `bootstrap-components.yaml`,
  `control-plane-components.yaml`, and `metadata.yaml` (digest-pinned)
  alongside `kairos-capi-provider.yaml`.
- **Per-provider least-privilege RBAC.** RBAC is split into two
  per-namespace sets; five dead cluster-wide grants are removed, including
  an unused `webhookconfigurations` `patch` permission.
- **CA injection via standard cert-manager `inject-ca-from`.**
  `hack/post-install-webhook-ca-injection.sh` is retired.
- **`clusterctl` plumbing fixed.** `config/clusterctl/metadata.yaml`
  reshaped to a valid `Metadata` document; the stray `provider.yaml`
  deleted.

### Changed

- Kubernetes v1.36 validated/recommended across the CAPI v1.13 management
  band (v1.32-v1.36; corrected 2026-11-14, this said v1.30-v1.36); CAPI core
  v1.13.3 → v1.13.4; Kairos v3.6.0+ → v4.1.2;
  e2e stack moves to KubeVirt v1.9 / kindest/node v1.36.1 / k3s v1.36.1.

### Fixed

- **SSH-fallback `KubeconfigReady` condition no longer flaps** (#85). The
  condition is now written deterministically instead of racing a
  contention-sensitive worker round-trip.

### Security

- **RBAC reduction as attack-surface reduction**: five dead cluster-wide
  grants removed, including the unused webhook-config `patch` primitive.
- **Supply chain: digest-pinned, not yet signed.** Release artifacts remain
  image-digest-pinned but are not cosign-signed and ship no SBOM.

## [v0.1.0-beta.1] — 2026-07-04

Beta-1 adds **highly-available control planes**: `spec.replicas` accepts
`1` (single-node), `3`, or `5` on CAPK, CAPV, and CAPM3, for both k0s and
k3s. CAPI core dependency moves to v1.13.3, adopting the v1beta2
contract-versioned object-reference model (ADR 0006). See
[docs/UPGRADING.md](docs/UPGRADING.md#v010-alpha2--v010-beta1) for the
per-scenario upgrade impact.

### Breaking changes

- **`spec.replicas` validation changed.** Now accepts `1`, `3`, or `5`; the
  validating webhook rejects even counts (worse etcd fault tolerance per
  quorum cost than the next-lower odd count) and values above `5`. Existing
  `replicas: 1` deployments are unaffected. (KD-5b, ADR 0005)
- **CAPI core v1.13.3 / v1beta2 contract required.** The management-side
  CAPI dependency moves from v1.8 to v1.13.3, adopting
  `ContractVersionedObjectReference`/`MachineNodeReference` in place of
  v1beta1 pointer-based references (commit `736aeb3`, ADR 0006). Deploy
  this provider's CRDs via kustomize or `clusterctl`, never raw `kubectl
  apply -f config/crd/bases/` — the contract label is required for CAPI
  core to resolve object references. RBAC gains `get;list;watch` on
  `customresourcedefinitions.apiextensions.k8s.io` (commit `b31083f`).
- **`KairosControlPlane.spec.machineTemplate.metadata` is now a pointer**
  (commit `58bc284`), so an empty value is omitted from the API payload
  instead of round-tripping as `{}` — required by CAPI v1beta2's
  `ObjectMeta` `MinProperties=1` schema. No operator action needed unless
  external tooling depends on the Go field being a value type.

### Added

- **Highly-available control planes** (ADR 0005, Phases 1-4 + CAPK/CAPM3
  extension). `spec.replicas: 3` or `5` provisions a multi-node k0s or k3s
  control plane with etcd quorum. Endpoint mechanism is provider-specific:
  `spec.ha.vip` (kube-vip) on CAPV/CAPM3; CAPK's own LoadBalancer Service on
  CAPK (do not set `spec.ha.vip` there). CAPK control-plane VMs gain a
  routable secondary NIC for etcd peering, since KubeVirt's masquerade
  interface gives every VM the same self-address.
- **`EtcdHealthy` condition** on `KairosControlPlane`, derived from
  per-cluster node-reported etcd member health.
- **Quorum-safe replacement.** The controller refuses a rollout or
  scale-down delete that would drop etcd below `(N/2)+1` healthy voting
  members.
- **Clean etcd member eviction on k0s.** A CAPI pre-terminate hook pauses
  termination after drain; the departing node runs `k0s etcd leave` and
  acknowledges before the controller lets the delete finish.
- New samples: `config/samples/{capk,capv,capm3}/*_ha.yaml`. New quickstart
  sections for CAPK/CAPV/CAPM3 HA.

### Fixed

- **`KairosControlPlane.spec.distribution` now inherits from the
  referenced `KairosConfigTemplate` when unset**, instead of silently
  defaulting to `k0s` and overriding the template's distribution. A
  `DistributionOverride` warning Event fires if an explicit value disagrees
  with the template's.

### Security

- **KD-51**: HA etcd health/leave signals are node-self-reported over
  vanilla RBAC and forgeable by a compromised control-plane node. Not a
  privilege escalation (a compromised node already holds
  cluster-admin-equivalent access) and cannot force an unsafe deletion —
  the quorum-safety decision is made independently of any node signal.
- `spec.ha.vip.address` and `spec.ha.vip.interface` are validated at
  admission and shell-quoted at render time before being written into the
  kube-vip DaemonSet manifest.
- Controller RBAC gains a read-only, cluster-scoped `get;list;watch` on
  `customresourcedefinitions.apiextensions.k8s.io`.

## [v0.1.0-alpha.2] — 2026-06-18

Alpha-2 adds **Metal3 (CAPM3) bare-metal infrastructure support** and resolves
the two security notices that shipped with alpha-1: it removes **synchronous
SSH from the controller's reconcile path** (KD-3b) and removes the **insecure
`kairos:kairos` default credentials** (KD-3a). The full Kairos Hadron
end-to-end matrix — CAPV and CAPM3, each with k0s and k3s — is lab-validated.
See [docs/UPGRADING.md](docs/UPGRADING.md#v010-alpha1--v010-alpha2) for the
per-scenario upgrade impact before rolling existing clusters forward.

### Breaking changes

- **Default credentials removed.** The bootstrap provider no longer seeds the
  node user with `kairos:kairos`, and no longer injects `PasswordAuthentication yes`
  into `sshd_config`. A node now requires an explicit `spec.userPassword`,
  `spec.userPasswordSecretRef`, or SSH key. (#49, KD-3a)
- **`KairosControlPlane.spec.replicas > 1` is rejected** by the validating
  webhook. Alpha-1 silently accepted it and produced N independent single-node
  clusters rather than an HA control plane. HA join logic is still on the
  roadmap; stay on `replicas: 1`. (#48, KD-5a)
- **KCP no longer writes `Cluster.Spec.ControlPlaneEndpoint`.** The endpoint is
  now owned exclusively by CAPI core (copied from the infra cluster), per the
  v1beta2 contract. Clusters that relied on KCP auto-discovering the endpoint
  from Machine IPs must set it on the infrastructure cluster. (#60, KD-12)

### Security

- Eliminated synchronous SSH from the controller hot path. After k0s/k3s
  starts, the **node** POSTs its own workload kubeconfig back to a Secret in the
  management cluster using a short-lived, audience-scoped bearer token; the
  control-plane controller waits for that Secret via a label-filtered watch.
  CAPV is extended to the same node-push pattern as CAPK. (#54, #55, #58, KD-3b/KD-10)
- Removed `ssh.InsecureIgnoreHostKey()`. The new opt-in SSH fallback mandates
  `known_hosts` host-key verification and public-key-only authentication, runs
  off the reconcile path in a bounded worker pool, and never logs key material.
  (#59)
- Closed a shell-injection (RCE) vector in `Hostname` cloud-config rendering and
  routed every shell-context field through the `shquote` filter. (#53, KD-22/KD-28/KD-43)
- Minimized the per-cluster node ServiceAccount RBAC: dropped the dead
  `services:get` grant and scoped `virtualmachineinstances:get` to k0s
  control-plane nodes only (the sole config shape that consumes it). Replaced the
  `infrastructure.cluster.x-k8s.io` wildcard with an explicit kind enumeration
  (including Metal3). (#63, KD-46/KD-6)
- Added `spec.userPasswordSecretRef` so the node password no longer has to live
  inline in the `KairosConfig` spec. (#49, KD-3a)

### Added

- **Metal3 (CAPM3) bare-metal infrastructure support.** Single-node k0s and k3s
  control planes on bare metal via Cluster API Provider Metal3. The bootstrap
  provider recognises the Metal3 infrastructure kind and sets the
  `metal3://<uuid>` providerID from the Ironic config-drive metadata at first
  Node registration (rather than render time). Lab-validated end-to-end on
  Hadron. (#65, ADR 0004)
- Opt-in `spec.sshFallback` on `KairosControlPlane` for air-gapped CAPV/CAPM3
  environments where the VM cannot reach the management API server. Webhook
  defaults and validates the field (mandatory key + known-hosts Secret refs,
  no cross-namespace refs, `activateAfter` bounded above the kubeconfig-ready
  timeout). (#59)
- Validating + defaulting webhook for `KairosControlPlaneTemplate`, mirroring
  the `KairosControlPlane` webhook (including SSH-fallback validation). (#61)
- `spec.files` on `KairosConfig` / `KairosConfigTemplate` is now rendered into
  the node's cloud-config `write_files:` on every distribution and infrastructure
  provider (the field was accepted but silently ignored in alpha-1). Validated
  for absolute paths, no `..` traversal, octal permissions, and `user:group`
  owner. (#65)
- `ManagementEndpoint` type and `ManagementEndpointResolver` interface; the CAPK
  kubeconfig-push path is now resolver-driven and unit-testable without
  envtest. (#54, KD-33)
- Persistent-state-paths configuration injected via cloud-config so node state
  survives reboots. (#52, KD-23)

### Changed

- `Node.Spec.ProviderID` is now set exclusively in-VM: k3s via a kubelet
  `config.yaml.d` drop-in, k0s via a post-bootstrap systemd patch service plus a
  `--kubelet-extra-args=--provider-id` `ExecStartPre` drop-in. The controller no
  longer reaches into the workload cluster to patch it. (#58, #62, KD-3b/KD-3c)
- The release container image is built multi-arch (`linux/amd64` + `linux/arm64`)
  by cross-compiling per `TARGETARCH`, and the release manifest pins
  `imagePullPolicy: IfNotPresent` so the published artifact is reproducible.
  (#65, #67)

### Fixed

- `reconcileDelete` now enumerates and removes child resources (KairosConfigs,
  owned Secrets, InfraMachines) before stripping the finalizer, and latched
  `FailureReason`/`FailureMessage` recover on the success path. (#50, KD-4/KD-14)
- The bootstrap Secret is named deterministically and carries a GC-safe owner
  reference, so unattended Metal3 provisioning no longer races on duplicate
  Secrets. (#65, KD-48)
- Bootstrap cloud-config writes `write_files` entries with a named `owner: root`
  rather than numeric `0`, so musl-based Kairos (Hadron) `yip` resolves the
  owner; k0s providerID self-discovery on CAPV uses `k0s kubectl`, since Hadron
  ships no standalone `kubectl`. (#65)

### Removed

- `ensureProviderIDOnNodes` — the last piece of synchronous workload-cluster I/O
  in `Reconcile`. ProviderID is now owned entirely in-VM (see **Changed**).
  (#58, KD-10)
- The `kairos:kairos` default user and the `PasswordAuthentication yes` sshd
  injection (see **Breaking changes**). (#49, KD-3a)

## [v0.1.0-alpha.1] — 2026-05-18

Initial public alpha. Two providers ship together: the bootstrap provider
(`bootstrap.cluster.x-k8s.io/v1beta2`) and the control-plane provider
(`controlplane.cluster.x-k8s.io/v1beta2`). Single-node k0s and k3s clusters on
CAPD (Docker), CAPV (vSphere), and CAPK (KubeVirt). See the
[release notes](docs/release-notes/v0.1.0-alpha.1.md) for the full feature list
and the alpha-1 security notices.

[v0.1.1]: https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.1
[v0.1.0]: https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0
[v0.1.0-beta.2]: https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-beta.2
[v0.1.0-beta.1]: https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-beta.1
[v0.1.0-alpha.2]: https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-alpha.2
[v0.1.0-alpha.1]: https://github.com/kairos-io/cluster-api-provider-kairos/releases/tag/v0.1.0-alpha.1

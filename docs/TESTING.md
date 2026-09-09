# Testing

Last verified against: Go toolchain 1.26.3, provider v0.1.2.

See [Install guide](INSTALL.md) for development install.

## Prerequisites

- Go toolchain 1.26.3 (matches `go.mod` directive `go 1.26.0`; the toolchain line pins `go1.26.0` for reproducibility). The `go.mod` line is `go 1.26.0`; both refer to the same release series.

## Unit tests

Run unit tests (no envtest assets needed):

```bash
go test ./...
```

Coverage includes template rendering for k0s and k3s, bootstrap controller logic, and webhook validation.

## Envtest (integration)

Envtest downloads assets automatically via `setup-envtest`:

```bash
make test-envtest
```

`make test-envtest` installs `setup-envtest` if needed, downloads Kubernetes API server binaries, and runs the `envtest`-tagged tests. This is the local integration gate and covers the full reconcile + webhook path without a real cluster.

The same suite runs in CI as the `test-envtest` job, against real API server
binaries; it is a required check, not an optional one. (This note previously
said the CI job was gated off with `if: false`. That was true when KD-19 was
open; the job has since been enabled and there is no such gate in
`.github/workflows/ci.yaml`.)

## Image scanning

Every push and pull request builds the controller image from the release
`Dockerfile` and scans it, failing on any **fixable** CRITICAL or HIGH finding
in either the runtime base layer or the manager binary:

```bash
docker build -t cluster-api-provider-kairos:scan .
trivy image --severity CRITICAL,HIGH --ignore-unfixed --scanners vuln \
  --exit-code 1 cluster-api-provider-kairos:scan
```

Run exactly that locally to reproduce a CI failure. Unfixable findings are
reported but do not fail the job: there is nothing to do about them here, and
gating on them would wedge every PR until upstream shipped a fix.

The same workflow also runs weekly on `main`. That is the trigger that matters
most, and it is not redundant with the per-PR run: a CVE is usually disclosed
long after the vulnerable dependency merged, so no code-triggered run would
ever flag it. The v0.1.2 `golang.org/x/crypto` fix came from exactly that
situation, found by a manual scan rather than by CI.

## End-to-end (KubeVirt)

The full end-to-end test spins up a kind + KubeVirt environment and provisions a real Kairos cluster:

```bash
make kubevirt-env      # build and set up the environment (downloads assets on first run)
make test-kubevirt     # run the scripted end-to-end flow
```

This is the highest-confidence gate but requires Docker and a host with enough memory for nested VMs (16 GiB+ recommended). See [QUICKSTART_CAPK.md](QUICKSTART_CAPK.md) for details on the lab environment.

## Reboot survival test

After the cluster is `Available=true`, drain a node via `kubectl drain <node> --ignore-daemonsets --delete-emptydir-data`, restart the underlying VM (`virtctl restart <vm>` for CAPK; vSphere "Restart Guest OS" for CAPV), uncordon, and verify `kubectl get nodes` shows `Ready` within 5 minutes. This validates KD-23's persistence injection — k0s/k3s state, SSH host keys, and CNI config must survive the reboot.

## Supported configurations (v0.1.0)

Single-node and 3-node HA control planes are supported on CAPK, CAPV, and CAPM3, for both k0s and k3s. CAPD is dev-only (single-node); HA is not exercised on CAPD. CAPD is tested via unit/envtest rather than a live e2e run. Fleet (Kairos fleet / AuroraBoot) supports a single control plane plus a worker `MachineDeployment`; HA is not exercised on fleet.

| Infrastructure | Distribution | Single-node | HA (3-node) |
|---|---|---|---|
| CAPV | k0s | Supported | Supported |
| CAPV | k3s | Supported | Supported (KD-5d day-2 caveat) |
| CAPM3 | k0s | Supported | Supported |
| CAPM3 | k3s | Supported | Supported (KD-5d day-2 caveat) |
| CAPK | k0s | Supported | Supported |
| CAPK | k3s | Supported | Supported (KD-5d day-2 caveat) |
| CAPD | k0s | Supported (dev only) | Not exercised |
| Fleet | k0s | Supported: control plane + worker `MachineDeployment`, validated on real hardware (Hadron v4.1.2, Kubernetes v1.36.1) | Not exercised |
| Fleet | k3s | Supported: control plane + worker `MachineDeployment`, validated on real hardware (Hadron v4.1.2, Kubernetes v1.36.1) | Not exercised |

Hadron is the musl-libc-based next-generation Kairos OS; it is exercised alongside standard (glibc) Kairos images to confirm compatibility with both targets.

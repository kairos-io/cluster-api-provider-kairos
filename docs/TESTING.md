# Testing

Last verified against: Go toolchain 1.26.3, provider v0.1.0-alpha.2.

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

**Note (KD-19):** The CI envtest job is permanently gated with `if: false` — it is not run in CI at present. `make test-envtest` is the integration gate for local development until KD-19 is resolved.

## End-to-end (KubeVirt)

The full end-to-end test spins up a kind + KubeVirt environment and provisions a real Kairos cluster:

```bash
make kubevirt-env      # build and set up the environment (downloads assets on first run)
make test-kubevirt     # run the scripted end-to-end flow
```

This is the highest-confidence gate but requires Docker and a host with enough memory for nested VMs (16 GiB+ recommended). See [QUICKSTART_CAPK.md](QUICKSTART_CAPK.md) for details on the lab environment.

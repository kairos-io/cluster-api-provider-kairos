# Testing

See [Install guide](INSTALL.md) for development install.

## Prerequisites

- Go 1.25+ (see [Install guide](INSTALL.md))

## Unit tests

Run unit tests (default, no envtest assets needed):

```bash
go test ./...
```

Coverage includes template rendering for k0s and k3s and bootstrap controller logic.

## Envtest

Envtest is optional and downloads assets automatically:

```bash
make test-envtest
```

`make test-envtest` installs setup-envtest if needed, downloads assets, and runs envtest-tagged tests.

CI tests against Go 1.25.7. The 1.24/1.25 matrix was removed due to covdata/toolchain compatibility issues (golang/go#75031).

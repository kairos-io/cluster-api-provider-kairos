# Build stage — run the builder on the native build platform and cross-compile
# to the requested target arch (TARGETOS/TARGETARCH are supplied by buildx).
# CGO is disabled, so Go cross-compiles without an emulated toolchain.
#
# Pinned by digest so a build is reproducible and a repointed tag cannot change
# what ships. The digest is the multi-arch INDEX digest, not a per-platform one:
# pinning a single platform's manifest would break the linux/arm64 half of the
# release build. Keep the tag alongside it for readability, and let Renovate
# move the digest.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:eb61c3873c97416fff0116553fff65d67a3549f72e11d9758849a689cdd8b523 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Install build dependencies
RUN apk add --no-cache git make

# Copy go mod files
COPY go.mod go.mod
COPY go.sum go.sum

# Download dependencies
RUN go mod download

# Copy source code
COPY main.go main.go
COPY api/ api/
COPY internal/ internal/

# Build for the target platform (multi-arch safe).
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -a -o manager main.go

# Runtime stage — multi-arch index digest, same reasoning as the builder above.
FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]


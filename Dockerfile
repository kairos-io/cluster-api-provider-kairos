# Build stage — run the builder on the native build platform and cross-compile
# to the requested target arch (TARGETOS/TARGETARCH are supplied by buildx).
# CGO is disabled, so Go cross-compiles without an emulated toolchain.
#
# Pinned by digest so a build is reproducible and a repointed tag cannot change
# what ships. The digest is the multi-arch INDEX digest, not a per-platform one:
# pinning a single platform's manifest would break the linux/arm64 half of the
# release build. Keep the tag alongside it for readability, and let Renovate
# move the digest.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS builder

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
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]


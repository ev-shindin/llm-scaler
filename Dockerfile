# Build the manager binary
# Digest re-pinned 2026-08-14: the previous one (sha256:94758a60...) had been
# removed from quay.io and resolved to "not found", so every build without that
# layer already cached failed -- CI could not build main at all, while local
# builds kept succeeding from cache and hid it.
FROM quay.io/projectquay/golang:1.25@sha256:584a3564b3e98dcbd200ffcb22636ff95f7ddc73e692dff13e993ad3d702fdaa AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/main.go cmd/main.go
COPY internal/ internal/

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7

LABEL org.opencontainers.image.source="https://github.com/llm-d/llm-d-workload-variant-autoscaler"
LABEL org.opencontainers.image.description="Workload Variant Autoscaler (WVA) - GPU-aware autoscaler for LLM inference workloads"
LABEL org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]

# Build the manager binary, and the kubectl plugin beside it.
FROM golang:1.26 AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /workspace

# Dependencies first, so a source-only change reuses the layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# The caches outlive the build, so a one-line change recompiles one package
# rather than everything.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o manager ./cmd \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o kubectl-ravendb_cloud ./cmd/kubectl-ravendb_cloud

# Distroless keeps the runtime free of a shell and a package manager. The
# operator holds a RavenDB Cloud API key in memory, so the smaller the attack
# surface around it, the better.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
# The kubectl plugin rides along so its export can run inside the cluster, next
# to the Secret holding the key, with nothing to download: a Job, or kubectl run
# with --overrides, running /kubectl-ravendb_cloud export with
# RAVENDB_CLOUD_API_KEY taken from that Secret through a secretKeyRef. labs/04
# shows the kubectl run form.
COPY --from=builder /workspace/kubectl-ravendb_cloud .
USER 65532:65532

ENTRYPOINT ["/manager"]

# Container image for the lab's stand-in RavenDB Cloud control plane.
# Built from the repository root:  docker build -f labs/mockcloud/Dockerfile -t ravendb-mockcloud:lab .
FROM golang:1.26 AS builder
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
COPY labs/ labs/

RUN CGO_ENABLED=0 GOOS=linux go build -a -trimpath -ldflags="-s -w" -o mockcloud ./labs/mockcloud

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/mockcloud .
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/mockcloud"]

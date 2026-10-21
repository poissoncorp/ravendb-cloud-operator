#!/usr/bin/env bash
# Runs the Go toolchain inside a container, so no host Go install is required.
#
#   hack/go.sh go build ./...
#   hack/go.sh go test ./...
#   hack/go.sh go tool controller-gen crd paths=./api/...
#
# The module and build caches live in named Docker volumes, so repeated runs are
# fast.
set -euo pipefail

IMAGE="${GO_IMAGE:-golang:1.26}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOST_DIR="$ROOT"
if command -v cygpath >/dev/null 2>&1; then
  HOST_DIR="$(cygpath -w "$ROOT")"
fi

CACHES=(-v rvncloud-gomod:/go/pkg/mod -v rvncloud-gocache:/root/.cache/go-build)
# On Linux a container running as root writes root-owned files into the
# checkout, so it runs as the caller instead. The named volumes belong to root,
# so its caches go under bin/, which git and the image build both ignore.
if [ "$(uname -s)" = Linux ]; then
  CACHES=(--user "$(id -u):$(id -g)" -e HOME=/tmp
          -e GOCACHE=/workspace/bin/.cache/go-build -e GOMODCACHE=/workspace/bin/.cache/go-mod)
fi

MSYS_NO_PATHCONV=1 exec docker run --rm \
  -v "${HOST_DIR}:/workspace" \
  "${CACHES[@]}" \
  -w /workspace \
  -e GOFLAGS=-buildvcs=false \
  -e CGO_ENABLED="${CGO_ENABLED:-0}" \
  "${IMAGE}" "$@"

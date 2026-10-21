#!/usr/bin/env bash
# Runs the Go toolchain inside a container, so no host Go install is required.
#
#   hack/go.sh go build ./...
#   hack/go.sh go test ./...
#   hack/go.sh controller-gen crd paths=./api/...
#
# Module cache, build cache and installed tool binaries live in named Docker
# volumes, so repeated runs are fast and controller-gen only downloads once.
set -euo pipefail

IMAGE="${GO_IMAGE:-golang:1.26}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOST_DIR="$ROOT"
if command -v cygpath >/dev/null 2>&1; then
  HOST_DIR="$(cygpath -w "$ROOT")"
fi

# hack/cloud-drift reads a checkout of the RavenDB Cloud source, which lives
# outside this repository, so it is mounted read-only when CLOUD_SRC names one.
CLOUD=()
if [ -n "${CLOUD_SRC:-}" ] && [ -d "$CLOUD_SRC" ]; then
  CLOUD_DIR="$CLOUD_SRC"
  if command -v cygpath >/dev/null 2>&1; then
    CLOUD_DIR="$(cygpath -w "$CLOUD_SRC")"
  fi
  CLOUD=(-v "${CLOUD_DIR}:/cloud-src:ro" -e CLOUD_SRC=/cloud-src
         -e CLOUD_PIN="${CLOUD_PIN:-}" -e CLOUD_WATCH="${CLOUD_WATCH:-}" -e CLOUD_REF="${CLOUD_REF:-}")
fi

MSYS_NO_PATHCONV=1 exec docker run --rm \
  -v "${HOST_DIR}:/workspace" \
  ${CLOUD[@]+"${CLOUD[@]}"} \
  -v rvncloud-gomod:/go/pkg/mod \
  -v rvncloud-gocache:/root/.cache/go-build \
  -v rvncloud-gobin:/go/bin \
  -w /workspace \
  -e GOFLAGS=-buildvcs=false \
  -e CGO_ENABLED="${CGO_ENABLED:-0}" \
  -e PATH=/go/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  "${IMAGE}" "$@"

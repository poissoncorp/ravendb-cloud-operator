#!/usr/bin/env bash
#
# Copy the generated parts of config/ into the Helm chart.
#
# The chart must not restate anything controller-gen produces. The CRDs come
# from `make manifests`, and the RBAC rules come from the +kubebuilder:rbac
# markers on the reconcilers. A hand-maintained copy of either means adding a
# marker grants nothing to Helm users, and the failure is a permission error at
# runtime in somebody else's cluster.
#
#   hack/chart-sync.sh            copy
#   hack/chart-sync.sh --verify   fail if the copy is stale, changing nothing
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$REPO_ROOT/charts/ravendb-cloud-operator"
VERIFY=0
[ "${1:-}" = "--verify" ] && VERIFY=1

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/crds"

# CRDs go in the chart's crds/ directory, which Helm installs before templates
# and deliberately never upgrades or deletes. That is the right behaviour for a
# CRD holding somebody's data references: an accidental `helm uninstall` must
# not take the CloudClusters with it.
for f in "$REPO_ROOT"/config/crd/bases/*.yaml; do
  cp "$f" "$work/crds/$(basename "$f")"
done

# Only the rules, and indented to sit under `rules:` in the template. The
# ClusterRole wrapper in config/rbac/role.yaml is not reused, because the chart
# decides whether it is a Role or a ClusterRole from rbac.scope.
awk '
  /^rules:/ { inrules = 1; next }
  inrules && /^[a-zA-Z]/ { inrules = 0 }
  inrules { print }
' "$REPO_ROOT/config/rbac/role.yaml" | sed 's/^/  /' >"$work/manager-rules.yaml"

if [ ! -s "$work/manager-rules.yaml" ]; then
  echo "chart-sync: extracted no RBAC rules from config/rbac/role.yaml" >&2
  echo "chart-sync: the file's shape changed; fix the awk in this script" >&2
  exit 1
fi

if [ "$VERIFY" = 1 ]; then
  rc=0
  diff -ru "$CHART/crds" "$work/crds" >/dev/null 2>&1 || rc=1
  diff -u "$CHART/files/manager-rules.yaml" "$work/manager-rules.yaml" >/dev/null 2>&1 || rc=1
  if [ "$rc" != 0 ]; then
    echo "chart-sync: the chart is stale relative to config/." >&2
    echo "chart-sync: run 'make chart-sync' and commit the result." >&2
    diff -ru "$CHART/crds" "$work/crds" 2>&1 | head -20 >&2 || true
    diff -u "$CHART/files/manager-rules.yaml" "$work/manager-rules.yaml" 2>&1 | head -20 >&2 || true
    exit 1
  fi
  echo "chart-sync: chart matches config/"
  exit 0
fi

mkdir -p "$CHART/crds" "$CHART/files"
rm -f "$CHART"/crds/*.yaml
cp "$work"/crds/*.yaml "$CHART/crds/"
cp "$work/manager-rules.yaml" "$CHART/files/manager-rules.yaml"
echo "chart-sync: copied $(ls -1 "$CHART/crds" | wc -l | tr -d ' ') CRDs and the manager RBAC rules"

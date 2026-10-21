#!/usr/bin/env bash
# Install the operator from the Helm chart and watch it work.
#
#   labs/chart_install.sh
#
# Not a numbered lab, so run_all.sh does not pick it up: this teaches nobody
# anything, it closes two gaps that docs claimed and nothing demonstrated.
#
#   1. The chart had never been installed. Every cluster test in this
#      repository deploys through kustomize, so the chart's own Deployment,
#      ServiceAccount and bindings were unproven however well they rendered.
#   2. watchNamespace, which narrows the operator's permissions to a Role in
#      that namespace, is what SECURITY.md offers in place of the cluster-wide
#      Secret grant, and nothing had ever installed it that way and confirmed
#      the operator still reconciles from inside it.
#
# Needs a cluster with the operator image already loaded, which is what
# labs/00_setup.sh leaves behind.
source "$(dirname "$0")/lib.sh"

CHART="$REPO_ROOT/charts/ravendb-cloud-operator"
RELEASE="${CHART_RELEASE:-chart-check}"
RELEASE_NS="${CHART_RELEASE_NAMESPACE:-ravendb-chart-check}"
IMAGE_TAG="${CHART_IMAGE:-ravendb-cloud-operator:lab}"

command -v helm >/dev/null 2>&1 || {
  warn "helm is not on PATH; this check needs it"
  exit 2
}

# The kustomize operator watches every namespace, so leaving it running would
# put two operators on the same objects: two creates, two status writers, and
# twice the calls. It goes down for the
# duration and comes back afterwards whatever happens here.
KUSTOMIZE_DEPLOY="deploy/ravendb-cloud-operator-controller-manager"
restore() {
  helm uninstall "$RELEASE" -n "$RELEASE_NS" --wait --timeout 90s >/dev/null 2>&1 || true
  kubectl delete namespace "$RELEASE_NS" --wait=false >/dev/null 2>&1 || true
  if kubectl -n "$LAB_OPERATOR_NAMESPACE" get "$KUSTOMIZE_DEPLOY" >/dev/null 2>&1; then
    kubectl -n "$LAB_OPERATOR_NAMESPACE" scale "$KUSTOMIZE_DEPLOY" --replicas=1 >/dev/null 2>&1 || true
  fi
}
trap restore EXIT

if kubectl -n "$LAB_OPERATOR_NAMESPACE" get "$KUSTOMIZE_DEPLOY" >/dev/null 2>&1; then
  step "Standing the kustomize operator down for the duration"
  kubectl -n "$LAB_OPERATOR_NAMESPACE" scale "$KUSTOMIZE_DEPLOY" --replicas=0 >/dev/null
  kubectl -n "$LAB_OPERATOR_NAMESPACE" rollout status "$KUSTOMIZE_DEPLOY" --timeout=90s >/dev/null
  info "two operators on the same objects would double every write and every request"
fi

step "Installing the chart, scoped to one namespace"
kubectl create namespace "$RELEASE_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
helm install "$RELEASE" "$CHART" \
  --namespace "$RELEASE_NS" \
  --set image.repository="${IMAGE_TAG%:*}" \
  --set image.tag="${IMAGE_TAG##*:}" \
  --set image.pullPolicy=IfNotPresent \
  --set watchNamespace="$LAB_NAMESPACE" \
  --set cloudEndpoint="http://mockcloud.$LAB_CLOUD_NAMESPACE.svc.cluster.local" \
  --set extraEnv[0].name=RAVENDB_CLOUD_RATE_LIMIT_PER_SECOND \
  --set-string extraEnv[0].value="${LAB_RATE_LIMIT:-50}" \
  --wait --timeout 120s >/dev/null
info "release $RELEASE in $RELEASE_NS, watching $LAB_NAMESPACE"

step "What the chart rendered"
# extraEnv and imagePullSecrets were added to the chart with nothing running
# them. This is the first thing that does.
check "extraEnv reached the container"   "${LAB_RATE_LIMIT:-50}"   "$(kubectl -n "$RELEASE_NS" get "deploy/$RELEASE-ravendb-cloud-operator"       -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="RAVENDB_CLOUD_RATE_LIMIT_PER_SECOND")].value}' 2>/dev/null)"
check "the manager grant is a Role, not a ClusterRole" \
  "1" "$(kubectl -n "$LAB_NAMESPACE" get role -l "app.kubernetes.io/instance=$RELEASE" -o name 2>/dev/null | wc -l | tr -d ' ')"
check_not_empty "leader election Role in the release namespace" \
  "$(kubectl -n "$RELEASE_NS" get role -o name 2>/dev/null | head -1)"

SA="system:serviceaccount:$RELEASE_NS:$RELEASE-ravendb-cloud-operator"
step "What that ServiceAccount may actually touch"
# This is the mitigation itself, not a rendering of it. An operator installed
# this way must not be able to read a Secret outside the namespace it serves.
check "read Secrets in the watched namespace" \
  "yes" "$(kubectl auth can-i --as="$SA" get secrets -n "$LAB_NAMESPACE" 2>&1)"
check "read Secrets in the operator's own namespace" \
  "no" "$(kubectl auth can-i --as="$SA" get secrets -n "$RELEASE_NS" 2>&1)"
check "read Secrets in kube-system" \
  "no" "$(kubectl auth can-i --as="$SA" get secrets -n kube-system 2>&1)"
check "read CloudClusters cluster-wide" \
  "no" "$(kubectl auth can-i --as="$SA" list cloudclusters --all-namespaces 2>&1)"

step "And that it still reconciles from in there"
# A new account, not the one the labs left behind. Applying over an existing
# Ready object passes this in zero seconds with nothing having reconciled,
# which is a check that cannot fail rather than a check that passed.
CHECK_ACCOUNT=chart-check-account
kubectl -n "$LAB_NAMESPACE" apply -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: $CHECK_ACCOUNT
  namespace: $LAB_NAMESPACE
spec:
  apiKeySecretRef: {name: ravendb-cloud-api-key, key: apiKey}
YAML
if ! wait_for cloudaccount "$CHECK_ACCOUNT" '.status.conditions[?(@.type=="Ready")].status' True 120; then
  warn "the account did not become Ready under the chart install; recent logs:"
  kubectl -n "$RELEASE_NS" logs "deploy/$RELEASE-ravendb-cloud-operator" --tail=40 2>&1 || true
  exit 1
fi
# Which operator did that. The kustomize one is at zero replicas, so the only
# manager running in this cluster is the one Helm installed; without this the
# check above would pass just as well against a leftover.
# readyReplicas is absent rather than 0 once the last pod goes, so the default
# is the answer and not a fallback.
other_operator="$(kubectl -n "$LAB_OPERATOR_NAMESPACE" get "$KUSTOMIZE_DEPLOY"   -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
check "no other operator was running" "0" "${other_operator:-0}"
check "CloudAccount Ready"  True                 "$(condition cloudaccount "$CHECK_ACCOUNT" Ready)"
check "reported domain"     "acme.ravendb.cloud" "$(jp cloudaccount "$CHECK_ACCOUNT" .status.domainName)"

# Nothing references it, so the finalizer this operator adds must let go at
# once. A chart install that could create objects but not release them would
# leave every namespace it touched undeletable.
kubectl -n "$LAB_NAMESPACE" delete cloudaccount "$CHECK_ACCOUNT" --timeout=60s >/dev/null
check "an unused account deletes rather than wedging on its finalizer"   "gone" "$(kubectl -n "$LAB_NAMESPACE" get cloudaccount "$CHECK_ACCOUNT" >/dev/null 2>&1 && echo present || echo gone)"

step "And that uninstalling does not take the CRDs with it"
# Helm installs from crds/ and never deletes them, which is the behaviour that
# keeps `helm uninstall` from taking every CloudCluster in the cluster with it.
# It is load bearing enough to pin rather than to trust.
helm uninstall "$RELEASE" -n "$RELEASE_NS" --wait --timeout 90s >/dev/null
check_not_empty "cloudclusters CRD survives the uninstall" \
  "$(kubectl get crd cloudclusters.cloud.ravendb.net -o name 2>/dev/null)"
check_not_empty "the CloudAccount object survives it too" \
  "$(kubectl -n "$LAB_NAMESPACE" get cloudaccount ravendb-cloud -o name 2>/dev/null)"

lab_summary

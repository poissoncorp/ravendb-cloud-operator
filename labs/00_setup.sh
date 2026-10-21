#!/usr/bin/env bash
# Lab 00: stand up everything the other labs need.
#
#   labs/00_setup.sh              create a kind cluster and install everything
#   labs/00_setup.sh --no-cluster use the cluster kubectl already points at
#   labs/00_setup.sh --teardown   delete the kind cluster
#
# Run from the repository root.
source "$(dirname "$0")/lib.sh"

CREATE_CLUSTER=1
for arg in "$@"; do
  case "$arg" in
    --no-cluster) CREATE_CLUSTER=0 ;;
    --teardown)
      step "Deleting the kind cluster"
      "$REPO_ROOT/bin/kind" delete cluster --name "$KIND_CLUSTER" 2>/dev/null ||
        kind delete cluster --name "$KIND_CLUSTER"
      exit 0
      ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

kind_bin() {
  if [ -x "$REPO_ROOT/bin/kind" ]; then echo "$REPO_ROOT/bin/kind"
  elif [ -x "$REPO_ROOT/bin/kind.exe" ]; then echo "$REPO_ROOT/bin/kind.exe"
  else echo kind
  fi
}

if [ "$CREATE_CLUSTER" -eq 1 ]; then
  step "Creating the kind cluster '$KIND_CLUSTER'"
  if "$(kind_bin)" get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
    note "already exists, reusing it"
  else
    "$(kind_bin)" create cluster --name "$KIND_CLUSTER" --wait 120s
  fi
  kubectl config use-context "kind-$KIND_CLUSTER" >/dev/null
fi

info "using context: $(kubectl config current-context)"

step "Building the operator and the lab control plane"
# The lab control plane is a real HTTP server implementing the documented
# RavenDB Cloud API. It exists so these labs cost nothing and need no account.
docker build -q -t ravendb-cloud-operator:lab "$REPO_ROOT" >/dev/null
docker build -q -f "$REPO_ROOT/labs/mockcloud/Dockerfile" -t ravendb-mockcloud:lab "$REPO_ROOT" >/dev/null
info "built ravendb-cloud-operator:lab and ravendb-mockcloud:lab"

if kubectl config current-context | grep -q "^kind-"; then
  step "Loading images into the kind cluster"
  "$(kind_bin)" load docker-image ravendb-cloud-operator:lab --name "$KIND_CLUSTER"
  "$(kind_bin)" load docker-image ravendb-mockcloud:lab --name "$KIND_CLUSTER"
fi

step "Installing the CRDs"
kubectl apply -f "$REPO_ROOT/config/crd/bases" >/dev/null
# Every CRD in the group rather than a list of names, which went stale once
# already: CloudClusterRestart was installed and not shown.
kubectl get crd \
  -o custom-columns=NAME:.metadata.name,ESTABLISHED:.status.conditions[?\(@.type==\"Established\"\)].status |
  grep -E 'NAME|cloud.ravendb.net'

step "Deploying the lab control plane"
if on_cloud; then
  note "LAB_TARGET=cloud, so the stand-in is deployed but unused: the operator"
  note "will be pointed at $LAB_CLOUD_ENDPOINT instead"
fi
kubectl apply -f "$REPO_ROOT/labs/manifests/mockcloud.yaml" >/dev/null
# Restarted for the same reason as the operator below: a fixed image tag means
# an unchanged manifest produces no new pod, so a rebuilt control plane would
# not actually be the one under test.
kubectl -n "$LAB_CLOUD_NAMESPACE" rollout restart deploy/mockcloud >/dev/null
kubectl -n "$LAB_CLOUD_NAMESPACE" rollout status deploy/mockcloud --timeout=120s

step "Deploying the operator"
kubectl apply -k "$REPO_ROOT/config/default" >/dev/null
kubectl -n "$LAB_OPERATOR_NAMESPACE" set image deploy/ravendb-cloud-operator-controller-manager \
  manager=ravendb-cloud-operator:lab >/dev/null
# The manifests reference a published image; the lab uses the one just built,
# so it must not be pulled from a registry that does not have it.
kubectl -n "$LAB_OPERATOR_NAMESPACE" patch deploy ravendb-cloud-operator-controller-manager \
  --type=json -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' >/dev/null
# The lab control plane is not the real API, so production pacing there is pure
# delay: most of the suite's wall clock was the operator waiting on a limiter
# guarding a server that was not there.
#
# The pacing itself is still covered, by TestDefaultPacingStaysAboveOneSecondPerCall
# and TestSequentialCallsArePaced, so nothing is lost by lifting it here.
#
# Only here. A cloud run must not inherit the stand-in's pace. The trailing dash
# removes the variable, so a cluster set up for the stand-in first does not keep
# it.
if on_cloud; then
  kubectl -n "$LAB_OPERATOR_NAMESPACE" set env deploy/ravendb-cloud-operator-controller-manager \
    RAVENDB_CLOUD_RATE_LIMIT_PER_SECOND- >/dev/null
else
  kubectl -n "$LAB_OPERATOR_NAMESPACE" set env deploy/ravendb-cloud-operator-controller-manager \
    RAVENDB_CLOUD_RATE_LIMIT_PER_SECOND="${LAB_RATE_LIMIT:-50}" >/dev/null
fi
# Force a new pod even when nothing in the Deployment changed.
#
# The image tag is fixed and imagePullPolicy is IfNotPresent, so on a re-run
# after editing the operator every command above is a no-op: `set image` sets
# the tag it already has, kind replaces the image in the node, and the running
# pod keeps the binary it started with. `rollout status` then passes instantly
# against the old ReplicaSet and the labs test code that is no longer in the
# tree. Restarting is the only thing that picks the new build up.
kubectl -n "$LAB_OPERATOR_NAMESPACE" rollout restart deploy/ravendb-cloud-operator-controller-manager >/dev/null
kubectl -n "$LAB_OPERATOR_NAMESPACE" rollout status deploy/ravendb-cloud-operator-controller-manager --timeout=120s

step "Creating the application namespace and the credential"
kubectl create namespace "$LAB_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
if on_cloud; then
  # The same two objects, pointed at a real environment. spec.endpoint is an
  # ordinary field, so this is the whole difference: no test hook, no build tag.
  : "${LAB_CLOUD_ENDPOINT:?set LAB_CLOUD_ENDPOINT to the RavenDB Cloud API to run against}"
  : "${LAB_CLOUD_API_KEY_FILE:?set LAB_CLOUD_API_KEY_FILE to a file holding the API key}"
  [ -r "$LAB_CLOUD_API_KEY_FILE" ] || { echo "cannot read $LAB_CLOUD_API_KEY_FILE" >&2; exit 2; }
  info "endpoint: $LAB_CLOUD_ENDPOINT"
  info "allowedIPs: $LAB_ALLOWED_IPS"
  # Through stdin, so the key never becomes a file in the repository and never
  # reaches a process listing.
  kubectl -n "$LAB_NAMESPACE" create secret generic ravendb-cloud-api-key \
    --from-file=apiKey="$LAB_CLOUD_API_KEY_FILE" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl apply -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: ravendb-cloud
  namespace: $LAB_NAMESPACE
spec:
  apiKeySecretRef:
    name: ravendb-cloud-api-key
    key: apiKey
  endpoint: $LAB_CLOUD_ENDPOINT
  defaults:
    provider: AWS
    region: eu-central-1
    allowedIPs:
      - $LAB_ALLOWED_IPS
  refreshIntervalSeconds: 1800
YAML
else
  kubectl apply -f "$REPO_ROOT/labs/manifests/account.yaml" >/dev/null
fi

step "Waiting for the CloudAccount to verify its credential"
# Not just Ready: this object survives between runs, so Ready was already true
# from the last one and waiting for it would prove nothing about the credential
# applied a moment ago. observedGeneration catching up to generation is the
# thing that says the operator has acted on THIS spec. Caught pointing at a
# stand-in while reporting a real environment's domain.
if ! wait_for cloudaccount ravendb-cloud \
     '.status.observedGeneration' "$(jp cloudaccount ravendb-cloud .metadata.generation)" 120; then
  warn "the account did not observe the spec just applied; recent operator logs:"
  operator_logs 30
  exit 1
fi
if ! wait_for cloudaccount ravendb-cloud '.status.conditions[?(@.type=="Ready")].status' True 90; then
  warn "the account did not become Ready; recent operator logs:"
  operator_logs 30
  exit 1
fi

check "CloudAccount Ready" True "$(condition cloudaccount ravendb-cloud Ready)"
check "status is for the spec that was just applied" \
  "$(jp cloudaccount ravendb-cloud .metadata.generation)" \
  "$(jp cloudaccount ravendb-cloud .status.observedGeneration)"
if on_cloud; then
  # The Ready message names the account's domain and never the endpoint, so the
  # proof that this is the control plane the run asked for is in two parts: the
  # spec the operator just observed names it, and the domain it verified is not
  # the stand-in's.
  check "the account points at the endpoint this run asked for" \
    "$LAB_CLOUD_ENDPOINT" "$(jp cloudaccount ravendb-cloud .spec.endpoint)"
  check "and verified an account that is not the stand-in's" no \
    "$([ "$(jp cloudaccount ravendb-cloud .status.domainName)" = acme.ravendb.cloud ] && echo yes || echo no)"
  # The stand-in reports fixed values; a real account reports its own, and which
  # account it is is exactly what a setup step should show rather than assert.
  info "domain          : $(jp cloudaccount ravendb-cloud .status.domainName)"
  info "default channel : $(jp cloudaccount ravendb-cloud .status.defaultReleaseChannel)"
  check_not_empty "reported domain" "$(jp cloudaccount ravendb-cloud .status.domainName)"
  check_not_empty "default channel" "$(jp cloudaccount ravendb-cloud .status.defaultReleaseChannel)"
else
  check "reported domain"       "acme.ravendb.cloud"   "$(jp cloudaccount ravendb-cloud .status.domainName)"
  check "default channel"       "Stable72"             "$(jp cloudaccount ravendb-cloud .status.defaultReleaseChannel)"
fi
check_not_empty "cached region list" "$(jp cloudaccount ravendb-cloud '.status.providers[0].regions[0]')"

step "Ready"
k get cloudaccount
cat <<EOF

    The lab environment is up.

      application namespace : $LAB_NAMESPACE
      control plane         : $LAB_CLOUD_NAMESPACE (a stand-in, not the real cloud)
      operator              : $LAB_OPERATOR_NAMESPACE

    Next: labs/01_verify_credential.sh

EOF

lab_summary

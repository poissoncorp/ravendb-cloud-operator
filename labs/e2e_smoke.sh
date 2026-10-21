#!/usr/bin/env bash
#
# Stage 0: a read-only smoke test against RavenDB Cloud.
#
#   RAVENDB_CLOUD_API_KEY=... labs/e2e_smoke.sh
#
# ============================================================================
# THIS SCRIPT NEVER WRITES TO RAVENDB CLOUD.
#
# It issues four GET requests and nothing else: account info, release channels,
# regions per provider, and instance types for one region. It creates no
# cluster, changes no cluster, and terminates no cluster. There is no code path
# here that sends POST to the RavenDB Cloud API, and the --operator mode below
# refuses to run if a CloudCluster exists that could reconcile into one.
#
# Verify that claim before trusting it:
#   grep -nE "curl .*-X|POST" labs/e2e_smoke.sh
# ============================================================================
#
# What it is for: everything else in labs/ runs against labs/mockcloud, which
# is an independent reading of the OpenAPI document. That catches disagreements
# between the operator and the mock, but it cannot catch a case where both read
# the specification the same wrong way. This closes that gap for the read-only
# half of the API, for free, in about a minute.
#
# Two modes:
#   --direct    (default) curl the endpoints. Proves the API contract only.
#   --operator  apply a CloudAccount into the current Kubernetes cluster and
#               assert the operator fills status from the live API. This
#               writes to Kubernetes, never to RavenDB Cloud.
source "$(dirname "$0")/lib.sh"

MODE=direct
for arg in "$@"; do
  case "$arg" in
    --direct)   MODE=direct ;;
    --operator) MODE=operator ;;
    *) echo "usage: $0 [--direct|--operator]" >&2; exit 2 ;;
  esac
done

SMOKE_ACCOUNT=smoke-real
SMOKE_SECRET=smoke-real-api-key

# --- preconditions ----------------------------------------------------------

# Every request here is a GET, so production is a safe default.
: "${RAVENDB_CLOUD_ENDPOINT:=https://api.cloud.ravendb.net}"

# The key can also come from a file, which is the better option when somebody
# other than its owner runs the script: the secret then never passes through a
# shell history, a chat, or this repository.
if [ -z "${RAVENDB_CLOUD_API_KEY:-}" ] && [ -n "${RAVENDB_CLOUD_API_KEY_FILE:-}" ]; then
  if [ ! -r "$RAVENDB_CLOUD_API_KEY_FILE" ]; then
    echo "RAVENDB_CLOUD_API_KEY_FILE=$RAVENDB_CLOUD_API_KEY_FILE is not readable" >&2
    exit 2
  fi
  # tr strips the trailing newline an editor will have added.
  RAVENDB_CLOUD_API_KEY="$(tr -d '\r\n' < "$RAVENDB_CLOUD_API_KEY_FILE")"
fi

if [ -z "${RAVENDB_CLOUD_API_KEY:-}" ]; then
  cat >&2 <<'EOF'
No API key. Set one of:

  RAVENDB_CLOUD_API_KEY        the key itself
  RAVENDB_CLOUD_API_KEY_FILE   path to a file containing only the key

Create the key in the portal (Account -> API Keys). Keep it out of this
repository and out of any transcript, for example:

  read -rs RAVENDB_CLOUD_API_KEY && export RAVENDB_CLOUD_API_KEY

or, to let somebody else run this without seeing it:

  printf '%s' 'THE-KEY' > ~/.ravendb-cloud-test.key && chmod 600 ~/.ravendb-cloud-test.key
  export RAVENDB_CLOUD_API_KEY_FILE=~/.ravendb-cloud-test.key

Note that a RavenDB Cloud API key is scoped to the whole account, not to one
cluster, so prefer a throwaway account.
EOF
  exit 2
fi

ENDPOINT="${RAVENDB_CLOUD_ENDPOINT%/}"
step "Target"
info "endpoint : $ENDPOINT"
info "api key  : set (${#RAVENDB_CLOUD_API_KEY} characters, never printed or written to disk)"

# --- read-only helpers ------------------------------------------------------

# check_at_least_one <description> <count>. lib.sh compares for equality, and
# the list sizes here are "more than nothing" assertions: the exact number
# of regions is not this operator's business.
check_at_least_one() {
  LAB_CHECKS=$((LAB_CHECKS + 1))
  local what="$1" got="${2:-0}"
  if [ -n "$got" ] && [ "$got" -ge 1 ] 2>/dev/null; then
    pass "$what = $got"
  else
    fail "$what = '$got', expected at least 1"
  fi
}

# cloud_get <path>: sets CLOUD_BODY and CLOUD_STATUS, returns 0 only on 200.
#
# Deliberately not "body on stdout". Callers would then need $(cloud_get ...),
# which runs in a subshell, and the status assigned inside would never reach
# the caller. Setting two globals keeps both in the current shell.
#
# This is the only function in this file that talks to RavenDB Cloud, and it
# sends no verb and no body: curl defaults to GET and nothing here overrides it.
CLOUD_BODY=''
CLOUD_STATUS=''
cloud_get() {
  local path="$1" raw
  if ! raw="$(curl -sS --connect-timeout 10 --max-time 30 \
                -H "X-Api-Key: $RAVENDB_CLOUD_API_KEY" \
                -H 'Accept: application/json' \
                -H 'User-Agent: ravendb-cloud-operator/e2e-smoke' \
                -w $'\n%{http_code}' \
                "$ENDPOINT$path" 2>/dev/null)"; then
    CLOUD_BODY=''
    CLOUD_STATUS=000
    return 1
  fi
  CLOUD_STATUS="${raw##*$'\n'}"
  CLOUD_BODY="${raw%$'\n'*}"
  [ "$CLOUD_STATUS" = "200" ]
}

# json_list <key> <json> -> the entries of a string array, one per line.
# No jq in the lab toolchain, so this splits on the array and strips quotes.
json_list() {
  printf '%s' "$2" |
    tr '{}' '\n\n' |
    sed -n "s/.*\"$1\":\[\([^]]*\)\].*/\1/p" |
    tr ',' '\n' | tr -d '"' | sed '/^$/d'
}

# json_field <key> <json> -> first scalar value for that key.
json_field() {
  printf '%s' "$2" | sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" | head -1
}

# --- 1. the credential ------------------------------------------------------

step "GET /api/v1/account/info"
cloud_get /api/v1/account/info || true
ACCOUNT_JSON="$CLOUD_BODY"
if [ "$CLOUD_STATUS" != "200" ]; then
  fail "account info returned HTTP $CLOUD_STATUS"
  case "$CLOUD_STATUS" in
    401|403) warn "the key was rejected: mistyped, deactivated or past its expiry date." ;;
    000)     warn "could not reach $ENDPOINT at all." ;;
  esac
  lab_summary
  exit 1
fi
DOMAIN="$(json_field domainName "$ACCOUNT_JSON")"
check_not_empty "account domainName" "$DOMAIN"
info "domainName = $DOMAIN"

# --- 2. release channels ----------------------------------------------------

step "GET /api/v1/metadata/release-channels"
cloud_get /api/v1/metadata/release-channels || true
CHANNELS_JSON="$CLOUD_BODY"
check "HTTP status" 200 "$CLOUD_STATUS"
DEFAULT_CHANNEL="$(json_field defaultReleaseChannel "$CHANNELS_JSON")"
check_not_empty "defaultReleaseChannel" "$DEFAULT_CHANNEL"
info "default channel = $DEFAULT_CHANNEL"
printf '%s' "$CHANNELS_JSON" | tr '{' '\n' | sed -n 's/.*"name":"\([^"]*\)".*"ravenVersion":"\([^"]*\)".*/    channel \1 -> RavenDB \2/p'

# --- 3. regions, per provider ----------------------------------------------

step "GET /api/v1/metadata/regions/{provider}"
FIRST_AWS_REGION=''
for provider in Aws Azure Gcp; do
  cloud_get "/api/v1/metadata/regions/$provider" || true
  REGIONS_JSON="$CLOUD_BODY"
  if [ "$CLOUD_STATUS" != "200" ]; then
    fail "regions for $provider returned HTTP $CLOUD_STATUS"
    continue
  fi
  COUNT="$(json_list names "$REGIONS_JSON" | wc -l | tr -d '[:space:]')"
  check_at_least_one "$provider regions" "$COUNT"
  info "$provider: $(json_list names "$REGIONS_JSON" | paste -sd' ')"
  if [ "$provider" = Aws ]; then
    FIRST_AWS_REGION="$(json_list names "$REGIONS_JSON" | head -1)"
  fi
done

# --- 4. instance types for one region --------------------------------------

step "GET /api/v1/metadata/instance-types/Aws/{region}"
if [ -z "$FIRST_AWS_REGION" ]; then
  fail "no AWS region to query instance types for"
else
  info "region = $FIRST_AWS_REGION"
  cloud_get "/api/v1/metadata/instance-types/Aws/$FIRST_AWS_REGION" || true
  TYPES_JSON="$CLOUD_BODY"
  check "HTTP status" 200 "$CLOUD_STATUS"
  # One line per instance type: name, tier, nodes. This is what the
  # operator validates spec.instanceType against, and the thing the
  # lab mock only approximates.
  printf '%s' "$TYPES_JSON" | tr '{' '\n' |
    sed -n 's/.*"name":"\([^"]*\)","tier":"\([^"]*\)".*/    \1 (\2)/p' |
    sort -u | head -40
  TYPE_COUNT="$(printf '%s' "$TYPES_JSON" | grep -o '"tier"' | wc -l | tr -d '[:space:]')"
  check_at_least_one "instance types in $FIRST_AWS_REGION" "$TYPE_COUNT"
  for tier in Free Development Production; do
    if printf '%s' "$TYPES_JSON" | grep -q "\"tier\":\"$tier\""; then
      info "tier $tier is present"
    else
      note "tier $tier is not offered in $FIRST_AWS_REGION"
    fi
  done
fi

# --- 5. optional: the same thing through the operator ----------------------

if [ "$MODE" = operator ]; then
  step "The same lists, read by the operator"
  note "this writes a Secret and a CloudAccount to Kubernetes. It creates no CloudCluster,"
  note "and the CloudAccount controller only ever issues GETs."

  # Belt and braces: refuse to proceed if anything in the namespace could
  # reconcile into a write against the real endpoint.
  EXISTING="$(kubectl -n "$LAB_NAMESPACE" get cloudcluster --no-headers 2>/dev/null | wc -l | tr -d '[:space:]')"
  if [ "${EXISTING:-0}" != "0" ]; then
    warn "$EXISTING CloudCluster(s) exist in namespace $LAB_NAMESPACE."
    warn "Refusing to run --operator: delete them first so nothing can reconcile against $ENDPOINT."
    lab_summary
    exit 1
  fi

  kubectl -n "$LAB_NAMESPACE" create secret generic "$SMOKE_SECRET" \
    --from-literal=apiKey="$RAVENDB_CLOUD_API_KEY" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$LAB_NAMESPACE" apply -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: $SMOKE_ACCOUNT
spec:
  endpoint: $ENDPOINT
  apiKeySecretRef:
    name: $SMOKE_SECRET
    key: apiKey
YAML

  cleanup_smoke() {
    kubectl -n "$LAB_NAMESPACE" delete cloudaccount "$SMOKE_ACCOUNT" --ignore-not-found >/dev/null 2>&1 || true
    kubectl -n "$LAB_NAMESPACE" delete secret "$SMOKE_SECRET" --ignore-not-found >/dev/null 2>&1 || true
  }
  trap cleanup_smoke EXIT

  if wait_for cloudaccount "$SMOKE_ACCOUNT" '.status.conditions[?(@.type=="Ready")].status' True 120; then
    check "CloudAccount Ready against the live API" True \
      "$(condition cloudaccount "$SMOKE_ACCOUNT" Ready)"
    check "domainName matches the direct read" "$DOMAIN" \
      "$(jp cloudaccount "$SMOKE_ACCOUNT" .status.domainName)"
    check "defaultReleaseChannel matches" "$DEFAULT_CHANNEL" \
      "$(jp cloudaccount "$SMOKE_ACCOUNT" .status.defaultReleaseChannel)"
    check_not_empty "cached region list" \
      "$(jp cloudaccount "$SMOKE_ACCOUNT" '.status.providers[0].regions[0]')"
    info "status.providers now holds the real regions:"
    kubectl -n "$LAB_NAMESPACE" get cloudaccount "$SMOKE_ACCOUNT" \
      -o jsonpath='{range .status.providers[*]}    {.provider}: {.regions}{"\n"}{end}' 2>/dev/null || true
  else
    fail "the CloudAccount did not become Ready"
    warn "condition: $(condition_reason cloudaccount "$SMOKE_ACCOUNT" Ready) / $(condition_message cloudaccount "$SMOKE_ACCOUNT" Ready)"
    operator_logs 20
  fi

  step "Cleaning up the Kubernetes objects"
  cleanup_smoke
  trap - EXIT
  check "CloudAccount removed" "" "$(jp cloudaccount "$SMOKE_ACCOUNT" .metadata.name)"
  note "nothing was created in RavenDB Cloud, so there is nothing to clean up there"
fi

step "Done"
cat <<EOF

    Verified read-only against $ENDPOINT:
      the API key is accepted
      the lists are readable and non-empty

    Nothing was created, changed or deleted in RavenDB Cloud.

EOF

lab_summary

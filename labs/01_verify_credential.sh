#!/usr/bin/env bash
# Lab 01: verify a RavenDB Cloud credential.
#
# A CloudAccount is the one place a RavenDB Cloud API key lives. This lab shows
# what the operator does with it: it calls GET /api/v1/account/info to prove the
# key works, mirrors the region and release channel lists into status, and carries the
# defaults every CloudCluster inherits. Then it breaks the credential three
# different ways and watches each one recover, including a key rotation that
# takes effect in seconds instead of at the next resync.
#
#   labs/01_verify_credential.sh
#
# Run from the repository root, after labs/00_setup.sh.
source "$(dirname "$0")/lib.sh"

if on_cloud; then
  LAB01_ENDPOINT="$LAB_CLOUD_ENDPOINT"
  LAB01_GOOD_KEY="$(cat "$LAB_CLOUD_API_KEY_FILE")"
else
  LAB01_ENDPOINT="http://mockcloud.${LAB_CLOUD_NAMESPACE}.svc.cluster.local"
  LAB01_GOOD_KEY="lab-api-key"
fi
# Any key the control plane will not accept. Rejection is the point, so it does
# not matter which control plane is doing the rejecting.
LAB01_BAD_KEY="rotated-out-9f3a"

# --- helpers ----------------------------------------------------------------

# apply_api_key_secret <value>: write the credential Secret. Used three times,
# because rotating a key in Kubernetes is exactly this and nothing more.
apply_api_key_secret() {
  cat <<YAML | k apply -f - >/dev/null
apiVersion: v1
kind: Secret
metadata:
  name: lab01-api-key
type: Opaque
stringData:
  apiKey: $1
YAML
}

# count_log_matches <path substring> <fixed needle>: the control plane filters
# its own request log by path; this counts a literal string in what comes back.
# No jq on purpose, it is not guaranteed to exist on the machine running a lab.
count_log_matches() {
  local log
  log="$(lab_api GET "/_lab/requests?path=$1")"
  { printf '%s' "$log" | grep -o -F -e "$2" || true; } | wc -l | tr -d '[:space:]'
}

# count_calls <path substring>: how many times the control plane has been asked
# for that path, according to its request log.
count_calls() { on_mock || { printf '0'; return 0; }; count_log_matches "$1" "\"path\":\"$1"; }

# count_unauthorized <path substring>: how many of those calls it answered 401.
count_unauthorized() { on_mock || { printf '0'; return 0; }; count_log_matches "$1" '"status":401'; }

# contains <haystack> <needle> -> yes/no, so the result can go through check().
contains() {
  case "$1" in
    *"$2"*) echo yes ;;
    *)      echo no ;;
  esac
}

cleanup_lab01() {
  k delete cloudcluster lab01-orphan --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
  k delete cloudaccount lab01-account lab01-late lab01-wrong-key lab01-broken \
    --ignore-not-found >/dev/null 2>&1 || true
  k delete secret lab01-api-key lab01-late-key --ignore-not-found >/dev/null 2>&1 || true
}

# --- 1. preconditions -------------------------------------------------------

step "Checking the lab environment"
require_ready_account
info "shared account 'ravendb-cloud' is Ready, so the operator and the control plane are up"
open_cloud_portforward

# Start from a clean slate so a re-run asserts on state this run created.
cleanup_lab01

# --- 2. the credential ------------------------------------------------------

step "Applying the Secret and the CloudAccount"
note "the Secret holds the key; the CloudAccount points at it and nothing else"
apply_api_key_secret "$LAB01_GOOD_KEY"

CALLS_INFO_BEFORE="$(count_calls /api/v1/account/info)"
CALLS_META_BEFORE="$(count_calls /api/v1/metadata/)"

cat <<YAML | k apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: lab01-account
spec:
  apiKeySecretRef:
    name: lab01-api-key
    key: apiKey
  endpoint: $LAB01_ENDPOINT
  defaults:
    provider: AWS
    region: eu-central-1
    allowedIPs:
      - $LAB_ALLOWED_IPS
  # Left at the documented default on purpose. Nothing in this lab runs for an
  # hour, so the lists are fetched once and never refreshed again, which is
  # what makes the two status timestamps distinguishable further down.
  refreshIntervalSeconds: 3600
YAML

if ! wait_for cloudaccount lab01-account '.status.conditions[?(@.type=="Ready")].status' True 90; then
  warn "the account did not verify; recent operator logs:"
  operator_logs 30
fi

k get cloudaccount lab01-account

READY_REASON="$(condition_reason cloudaccount lab01-account Ready)"
READY_MESSAGE="$(condition_message cloudaccount lab01-account Ready)"

check "Ready"                 True                   "$(condition cloudaccount lab01-account Ready)"
check "Ready reason"          Verified               "$READY_REASON"
check "observedGeneration"    1                      "$(jp cloudaccount lab01-account .status.observedGeneration)"
if on_mock; then
  check "reported domain"     acme.ravendb.cloud     "$(jp cloudaccount lab01-account .status.domainName)"
else
  check_not_empty "reported domain" "$(jp cloudaccount lab01-account .status.domainName)"
fi
check_contains "Ready message" "RavenDB Cloud accepted the API key" "$READY_MESSAGE"
check_contains "Ready message names the account it verified" \
  "$(jp cloudaccount lab01-account .status.domainName)" "$READY_MESSAGE"

# --- 3. Ready means the operator actually called the API --------------------

step "Ready is not a guess: the operator called GET /api/v1/account/info"
CALLS_INFO_AFTER="$(count_calls /api/v1/account/info)"
info "account/info calls in the control plane log: $CALLS_INFO_BEFORE -> $CALLS_INFO_AFTER"
check_calls "account/info was called while the account was verifying" yes \
  "$([ "$CALLS_INFO_AFTER" -gt "$CALLS_INFO_BEFORE" ] && echo yes || echo no)"

if on_mock; then
  REQUEST_LOG="$(lab_api GET '/_lab/requests?path=/api/v1/account/info')"
  check_contains "the caller identifies itself" "ravendb-cloud-operator" "$REQUEST_LOG"
else
  skip "the User-Agent the operator sends (needs the simulator's request log)"
fi

# domainName can only have come from the response body, so it is a second,
# independent witness that the call happened and was answered.
check_not_empty "status.domainName (only the API can supply it)" \
  "$(jp cloudaccount lab01-account .status.domainName)"

note "the key itself is sent as X-Api-Key and is never copied into the object:"
ACCOUNT_YAML="$(k get cloudaccount lab01-account -o yaml)"
check "the key value leaked into the CloudAccount object" no \
  "$(contains "$ACCOUNT_YAML" "$LAB01_GOOD_KEY")"

# --- 4. the cached lists ----------------------------------------------------

step "What this account may order, mirrored into status"
# The controller fills status.providers in a fixed order (AWS, Azure, GCP), so
# these paths address it by index and the order itself is checked below.
AWS_REGIONS="$(jp cloudaccount lab01-account '.status.providers[0].regions[*]')"
AZURE_REGIONS="$(jp cloudaccount lab01-account '.status.providers[1].regions[*]')"
CHANNELS="$(jp cloudaccount lab01-account '.status.releaseChannels[*].name')"

info "providers:        $(jp cloudaccount lab01-account '.status.providers[*].provider')"
info "AWS regions:      $AWS_REGIONS"
info "Azure regions:    $AZURE_REGIONS"
info "release channels: $CHANNELS"
info "default channel:  $(jp cloudaccount lab01-account .status.defaultReleaseChannel)"

check "all three providers are listed" "AWS Azure GCP" \
  "$(jp cloudaccount lab01-account '.status.providers[*].provider')"
check_contains "AWS regions"   "eu-central-1"  "$AWS_REGIONS"
check_contains "Azure regions" "westeurope"    "$AZURE_REGIONS"
check "eu-central-1 is not an Azure region" no "$(contains "$AZURE_REGIONS" "eu-central-1")"
# Channel names are ReleaseChannelName members, because production parses the
# value back into that enum and 400s anything else. "Stable", "LTS" and
# "Development" were invented by the old simulator, and this lab asserted them,
# so a repo check was pinning a name a real create rejects.
check_contains "release channels" "Stable72" "$CHANNELS"
check_contains "release channels" "Stable62" "$CHANNELS"
on_cloud && info "this environment offers: $CHANNELS"
if on_mock; then
  check "default release channel" Stable72 "$(jp cloudaccount lab01-account .status.defaultReleaseChannel)"
else
  # A real account has its own default and its own ordering. What matters is
  # that the operator mirrored something, and that the something is one of the
  # channels it also mirrored.
  LAB01_DEFAULT="$(jp cloudaccount lab01-account .status.defaultReleaseChannel)"
  check_not_empty "default release channel" "$LAB01_DEFAULT"
  check_contains "the default is one of the mirrored channels" "$LAB01_DEFAULT" "$CHANNELS"
  info "this account defaults to $LAB01_DEFAULT; channels: $CHANNELS"
fi
if on_mock; then
  check "second channel" Stable71 "$(jp cloudaccount lab01-account '.status.releaseChannels[1].name')"
  check "that channel version" "7.1" "$(jp cloudaccount lab01-account '.status.releaseChannels[1].ravenVersion')"
else
  # Both the ordering and the versions are this environment's. What holds
  # anywhere is that every channel the operator mirrored carries a version,
  # because a channel without one cannot be asked for in a create.
  LAB01_VERSIONS="$(jp cloudaccount lab01-account '.status.releaseChannels[*].ravenVersion')"
  info "channel versions: $LAB01_VERSIONS"
  check "every mirrored channel carries a version" \
    "$(printf '%s' "$CHANNELS" | wc -w | tr -d ' ')" \
    "$(printf '%s' "$LAB01_VERSIONS" | wc -w | tr -d ' ')"
fi

CALLS_META_AFTER="$(count_calls /api/v1/metadata/)"
info "metadata calls in the control plane log: $CALLS_META_BEFORE -> $CALLS_META_AFTER"
note "one release-channels call plus one regions call per provider"
check_calls "the refresh cost at least four metadata calls" yes \
  "$([ "$((CALLS_META_AFTER - CALLS_META_BEFORE))" -ge 4 ] && echo yes || echo no)"

# --- 5. two timestamps, on purpose ------------------------------------------

step "status.lastVerifiedTime and status.refreshedAt are separate fields"
REFRESHED_AT_0="$(jp cloudaccount lab01-account .status.refreshedAt)"
VERIFIED_AT_0="$(jp cloudaccount lab01-account .status.lastVerifiedTime)"
info "lastVerifiedTime   = $VERIFIED_AT_0"
info "refreshedAt = $REFRESHED_AT_0"
check_not_empty "status.lastVerifiedTime"   "$VERIFIED_AT_0"
check_not_empty "status.refreshedAt" "$REFRESHED_AT_0"
note "both are set by the first reconcile; the rotation below moves only one"

# --- 6. account defaults ----------------------------------------------------

step "Defaults that every CloudCluster inherits"
info "provider:   $(jp cloudaccount lab01-account .spec.defaults.provider)"
info "region:     $(jp cloudaccount lab01-account .spec.defaults.region)"
info "allowedIPs: $(jp cloudaccount lab01-account '.spec.defaults.allowedIPs[*]')"
check "default provider"   AWS            "$(jp cloudaccount lab01-account .spec.defaults.provider)"
check "default region"     eu-central-1   "$(jp cloudaccount lab01-account .spec.defaults.region)"
check "default allowedIPs" "$LAB_ALLOWED_IPS" "$(jp cloudaccount lab01-account '.spec.defaults.allowedIPs[*]')"
note "the default region is checked against the cached region list at bind time"
check "the default region exists for the default provider" yes \
  "$(contains "$AWS_REGIONS" "$(jp cloudaccount lab01-account .spec.defaults.region)")"

# --- 7. failure: the Secret does not exist yet ------------------------------

step "Failure 1: the Secret has not been created yet"
note "applying a CloudAccount whose Secret is missing, then creating the Secret"
cat <<YAML | k apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: lab01-late
spec:
  apiKeySecretRef:
    name: lab01-late-key
  endpoint: $LAB01_ENDPOINT
YAML

if ! wait_for cloudaccount lab01-late '.status.conditions[?(@.type=="Ready")].status' False 60; then
  warn "expected lab01-late to go Ready=False"
fi
LATE_MESSAGE="$(condition_message cloudaccount lab01-late Ready)"
info "message: $LATE_MESSAGE"
check "Ready"        False               "$(condition cloudaccount lab01-late Ready)"
check "Ready reason" KeyMissing          "$(condition_reason cloudaccount lab01-late Ready)"
check_contains "the message names the Secret" "lab01-late-key" "$LATE_MESSAGE"
check_contains "the message says what is wrong" "not found"    "$LATE_MESSAGE"
check_contains "the message names the namespace" "$LAB_NAMESPACE" "$LATE_MESSAGE"
check "no domain was reported" "" "$(jp cloudaccount lab01-late .status.domainName)"

info "now creating the Secret it was waiting for"
LATE_T0=$SECONDS
cat <<YAML | k apply -f - >/dev/null
apiVersion: v1
kind: Secret
metadata:
  name: lab01-late-key
type: Opaque
stringData:
  apiKey: $LAB01_GOOD_KEY
YAML
if ! wait_for cloudaccount lab01-late '.status.conditions[?(@.type=="Ready")].status' True 60; then
  warn "lab01-late did not recover; recent operator logs:"
  operator_logs 20
fi
LATE_ELAPSED=$((SECONDS - LATE_T0))
info "recovered in ${LATE_ELAPSED}s, against a 30s blind requeue for a missing Secret"
check "Ready after the Secret appeared" True "$(condition cloudaccount lab01-late Ready)"
check "recovery beat the 30s requeue" yes \
  "$([ "$LATE_ELAPSED" -lt 20 ] && echo yes || echo no)"
note "20s is a threshold, not a proof: a 30s requeue could have landed here too."
note "the key rotation further down settles it properly, against a 300s requeue."

# --- 8. failure: the right Secret, the wrong key ----------------------------

step "Failure 2: the Secret exists but the key inside it does not"
cat <<YAML | k apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: lab01-wrong-key
spec:
  apiKeySecretRef:
    name: lab01-api-key
    key: token
  endpoint: $LAB01_ENDPOINT
YAML

if ! wait_for cloudaccount lab01-wrong-key '.status.conditions[?(@.type=="Ready")].status' False 60; then
  warn "expected lab01-wrong-key to go Ready=False"
fi
WRONG_MESSAGE="$(condition_message cloudaccount lab01-wrong-key Ready)"
info "message: $WRONG_MESSAGE"
check "Ready"        False               "$(condition cloudaccount lab01-wrong-key Ready)"
check "Ready reason" KeyMissing          "$(condition_reason cloudaccount lab01-wrong-key Ready)"
check_contains "the message names the Secret" "lab01-api-key" "$WRONG_MESSAGE"
check_contains "the message names the key it looked for" "token" "$WRONG_MESSAGE"
note "the same Secret verifies fine under key 'apiKey', so this is a typo, not a bad key"
check "the sibling account using the same Secret is still Ready" True \
  "$(condition cloudaccount lab01-account Ready)"
k delete cloudaccount lab01-wrong-key --ignore-not-found >/dev/null

# --- 9. failure: the key is rejected, and the rotation that fixes it --------

step "Failure 3: RavenDB Cloud rejects the key"
note "patching the Secret to a value the control plane does not know"
UNAUTH_BEFORE="$(count_unauthorized /api/v1/account/info)"
apply_api_key_secret "$LAB01_BAD_KEY"

if ! wait_for cloudaccount lab01-account '.status.conditions[?(@.type=="Ready")].status' False 60; then
  warn "expected lab01-account to go Ready=False after the key was changed"
  operator_logs 20
fi
BAD_MESSAGE="$(condition_message cloudaccount lab01-account Ready)"
info "message: $BAD_MESSAGE"
check "Ready"        False               "$(condition cloudaccount lab01-account Ready)"
check "Ready reason" KeyRejected         "$(condition_reason cloudaccount lab01-account Ready)"
check_contains "the message carries the HTTP status"   "401"          "$BAD_MESSAGE"
check_contains "the message names the status"          "Unauthorized" "$BAD_MESSAGE"
check_contains "and the Secret to fix"                 "lab01-api-key" "$BAD_MESSAGE"
check "and no wire detail a user cannot act on" no "$(contains "$BAD_MESSAGE" "/api/v1/")"
# No server words to carry: a rejected key gets a bodiless 401 from RavenDB
# Cloud, and the control plane here copies that deliberately. So the message
# says what the status means and where the key lives, rather than quoting the
# request line, which belongs in the log.
note "the message has no server words, because a rejected key gets a bodiless 401"
check "the rejected key value is not echoed into status" no "$(contains "$BAD_MESSAGE" "$LAB01_BAD_KEY")"

# This is the check that makes "Ready reflects a real HTTP exchange" airtight.
# The earlier count of account/info calls cannot say which account made them,
# but a 401 can: every other credential in the cluster is good, so a 401 on
# account/info could only have come from the account this step has broken.
UNAUTH_AFTER="$(count_unauthorized /api/v1/account/info)"
info "401 answers to account/info in the control plane log: $UNAUTH_BEFORE -> $UNAUTH_AFTER"
check_calls "the rejection is a real HTTP answer, not a local guess" yes \
  "$([ "$UNAUTH_AFTER" -gt "$UNAUTH_BEFORE" ] && echo yes || echo no)"

note "what is already in status is kept: a bad key does not erase what was learned"
check "cached AWS regions survived the failure" "$AWS_REGIONS" \
  "$(jp cloudaccount lab01-account '.status.providers[0].regions[*]')"

step "Rotating the key back: the recovery is the point"
note "a rejected key is requeued after 5m, because retrying a bad key is pointless."
note "the Secret watch is what actually brings the account back, so time this."
ROTATE_T0=$SECONDS
apply_api_key_secret "$LAB01_GOOD_KEY"
if ! wait_for cloudaccount lab01-account '.status.conditions[?(@.type=="Ready")].status' True 90; then
  warn "the account did not recover after the key was rotated back:"
  operator_logs 20
fi
ROTATE_ELAPSED=$((SECONDS - ROTATE_T0))
info "recovered in ${ROTATE_ELAPSED}s; the blind requeue for a rejected key is 300s"
check "Ready after rotating the key back" True "$(condition cloudaccount lab01-account Ready)"
check "Ready reason" Verified "$(condition_reason cloudaccount lab01-account Ready)"
check "recovery took seconds, not the 5m requeue" yes \
  "$([ "$ROTATE_ELAPSED" -lt 30 ] && echo yes || echo no)"

# --- 10. which timestamp moved ----------------------------------------------

step "Only one of the two timestamps moved"
REFRESHED_AT_1="$(jp cloudaccount lab01-account .status.refreshedAt)"
VERIFIED_AT_1="$(jp cloudaccount lab01-account .status.lastVerifiedTime)"
info "lastVerifiedTime   $VERIFIED_AT_0 -> $VERIFIED_AT_1"
info "refreshedAt $REFRESHED_AT_0 -> $REFRESHED_AT_1"
check "refreshedAt is unchanged" "$REFRESHED_AT_0" "$REFRESHED_AT_1"
check "lastVerifiedTime advanced" yes \
  "$([ "$VERIFIED_AT_1" != "$VERIFIED_AT_0" ] && echo yes || echo no)"
note "re-verifying costs one call; re-reading the lists costs four, so they"
note "are tracked separately and run on different clocks"

# --- 11. one bad credential, one failure ------------------------------------

step "A cluster on a broken account reports the account, not a cloud error"
cat <<YAML | k apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: lab01-broken
spec:
  apiKeySecretRef:
    name: lab01-broken-key
  endpoint: $LAB01_ENDPOINT
---
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab01-orphan
spec:
  accountRef:
    name: lab01-broken
  tier: Development
  # Observe never creates anything, so this object cannot spend money even if
  # the account it names were ever fixed.
  managementPolicy: Observe
YAML

if ! wait_for cloudaccount lab01-broken '.status.conditions[?(@.type=="Ready")].status' False 60; then
  warn "expected lab01-broken to go Ready=False"
fi

# The CloudCluster controller does not watch CloudAccount, it retries a missing
# prerequisite every 20s. So its first pass can land before the account has a
# condition worth quoting, and the full message arrives one requeue later. Wait
# for the message itself rather than for the reason, which is reached at once.
ORPHAN_WANT="CloudAccount \"lab01-broken\": Secret \"lab01-broken-key\" not found in namespace \"$LAB_NAMESPACE\"."
if ! wait_for cloudcluster lab01-orphan '.status.conditions[?(@.type=="Ready")].message' "$ORPHAN_WANT" 90; then
  warn "expected lab01-orphan to quote the account's own failure"
  operator_logs 20
fi
ORPHAN_MESSAGE="$(condition_message cloudcluster lab01-orphan Ready)"
info "cluster message: $ORPHAN_MESSAGE"
k get cloudcluster lab01-orphan || true
check "cluster STATUS"       WaitingForAccount "$(status_of lab01-orphan)"
check "cluster Ready"        False             "$(condition cloudcluster lab01-orphan Ready)"
check "the cluster message is the account's own, forwarded" "$ORPHAN_WANT" "$ORPHAN_MESSAGE"
note "the operator never tried to look the cluster up in the cloud:"
check "CHANGES was never set, because nothing is bound" "" "$(changes_of lab01-orphan)"
check "no productID was resolved"      "" "$(jp cloudcluster lab01-orphan .status.productID)"
check "the account itself blames the Secret" KeyMissing \
  "$(condition_reason cloudaccount lab01-broken Ready)"

# --- 12. clean up -----------------------------------------------------------

step "Cleaning up"
cleanup_lab01
check "lab01-account is gone" "" "$(jp cloudaccount lab01-account .metadata.name)"
check "lab01-orphan is gone"  "" "$(jp cloudcluster lab01-orphan .metadata.name)"

step "The shared account is untouched"
k get cloudaccount ravendb-cloud
check "ravendb-cloud is still Ready" True "$(condition cloudaccount ravendb-cloud Ready)"
if on_mock; then
  check "ravendb-cloud still reports its domain" acme.ravendb.cloud \
    "$(jp cloudaccount ravendb-cloud .status.domainName)"
else
  check_not_empty "ravendb-cloud still reports its domain" \
    "$(jp cloudaccount ravendb-cloud .status.domainName)"
fi
note "later labs need it, so this lab leaves it exactly as it found it"

lab_summary

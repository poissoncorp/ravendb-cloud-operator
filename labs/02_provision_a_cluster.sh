#!/usr/bin/env bash
# Lab 02: provision a RavenDB Cloud cluster from the smallest manifest that works.
#
# Shows what the operator resolves for you and records in status, the STATUS
# and CHANGES columns on the way from WaitingForAccount through Creating to
# Running, the requeue cadence, the crash-safety marker that lets an interrupted
# create find its own cluster again without taking it for somebody else's, and
# the same cluster spelled out in full.
#
#   labs/00_setup.sh                  once, to build the environment
#   labs/02_provision_a_cluster.sh    this lab
#
# Run from the repository root. Everything this lab creates is removed at the end.
source "$(dirname "$0")/lib.sh"

# --- lab-local helpers ------------------------------------------------------
#
# The control surface answers JSON on one line. These helpers cut it up with
# tr/grep/sed so the lab needs nothing installed beyond curl.

# Every cluster this lab puts in the control plane is named lab02-something, so
# it can find its own leftovers without touching another lab's.
lab02_mock_ids() {
  lab_api GET /_lab/clusters \
    | tr '{' '\n' \
    | { grep '"displayName":"lab02-' || true; } \
    | sed -n 's/.*"id":"\([^"]*\)".*/\1/p'
}

# lab02_mock_chunk <id> -> the raw JSON fields of that one simulated cluster
lab02_mock_chunk() {
  lab_api GET /_lab/clusters | tr '{' '\n' | { grep "\"id\":\"$1\"" || true; }
}

# lab02_named_count <display-name> -> how many simulated clusters carry that name
lab02_named_count() {
  lab_api GET /_lab/clusters | tr '{' '\n' | { grep -c "\"displayName\":\"$1\"" || true; }
}

# lab02_detail_calls <id> -> how many times the operator has read that cluster
lab02_detail_calls() {
  lab_api GET "/_lab/requests?path=details/$1" \
    | tr '{' '\n' | { grep -c '"method"' || true; }
}

lab02_vanish_all() {
  local id
  for id in $(lab02_mock_ids); do
    lab_api POST "/_lab/clusters/$id/vanish" >/dev/null || true
  done
}

lab02_events() {
  k get events --field-selector "involvedObject.name=$1" \
    -o jsonpath='{range .items[*]}{.type} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || true
}

# yesno <0|1> turns a shell test into something check() can compare.
yesno() { if [ "$1" -eq 0 ]; then echo yes; else echo no; fi; }

require_ready_account
open_cloud_portforward

# --- 0. leave no trace from an earlier run ----------------------------------

step "Clearing anything a previous run left behind"
release lab02-pending lab02-orders lab02-twin lab02-analytics
for name in lab02-pending lab02-orders lab02-twin lab02-analytics; do
  # Events outlive the objects they describe and are selected by name here, so
  # a previous run's would otherwise answer for this one.
  k delete events --field-selector "involvedObject.name=$name" >/dev/null 2>&1 || true
done
if on_mock; then
  lab02_vanish_all
  lab_api POST /_lab/requests/reset >/dev/null
  note "the request log is reset, so the call counts later in this lab are this run's"
else
  note "names carry the run id $LAB_RUN, so nothing here can collide with an earlier run"
fi

# --- 1. WaitingForAccount ---------------------------------------------------

step "WaitingForAccount: a cluster whose prerequisite is not there"
# With a Ready CloudAccount you never see this for more than a moment, so this
# throwaway object points at an account that does not exist.
k apply -f - >/dev/null <<'YAML'
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab02-pending
  namespace: apps
spec:
  accountRef:
    name: lab02-missing-account
  tier: Development
YAML

if ! wait_for_status lab02-pending WaitingForAccount 60; then
  warn "lab02-pending never reported WaitingForAccount; recent operator logs:"
  operator_logs 30
fi

PENDING_MESSAGE="$(condition_message cloudcluster lab02-pending Ready)"
info "message: $PENDING_MESSAGE"
check "lab02-pending STATUS"                   WaitingForAccount "$(status_of lab02-pending)"
check "lab02-pending Ready condition"          False             "$(condition cloudcluster lab02-pending Ready)"
check_contains "the message names the account" "lab02-missing-account" "$PENDING_MESSAGE"
# accountRef is immutable, so the message has to say how to fix a typo.
check_contains "and the way out of a typo"      "delete this object and apply it again" "$PENDING_MESSAGE"
check_contains "and that nothing was created"   "nothing was created" "$PENDING_MESSAGE"
on_mock && check "nothing was created in the cloud" 0 "$(lab02_named_count lab02-pending)"

note "no cloud call was made: the operator will not create anything without a verified credential"
k delete cloudcluster lab02-pending --ignore-not-found --timeout=60s >/dev/null || true

# --- 2. the smallest manifest that provisions -------------------------------

step "Provisioning from the smallest manifest that works"
info "accountRef, tier and storage.sizeGB. Nothing else."
if on_cloud; then
  note "plus a subdomain and a displayName, because a real account is shared and"
  note "neither may collide with what is already there"
fi
k apply -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab02-orders
  namespace: apps
spec:
  accountRef:
    name: ravendb-cloud
  tier: Development
  storage:
    sizeGB: 20
$(cloud_names o)
YAML

# The product id is recorded on the pass that sends the create. The next look
# at the cluster is a transitional requeue away, so for the next 30 seconds the
# object says Creating whatever the control plane is doing.
if ! wait_for_set cloudcluster lab02-orders .status.productID "$LAB_CHANGE_TIMEOUT"; then
  warn "lab02-orders never recorded a product id; recent operator logs:"
  operator_logs 30
fi

# One read, so the header, the row and the checks below describe one moment.
CREATING_TABLE="$(k get cloudcluster lab02-orders 2>/dev/null || true)"
info ""
printf '%s\n' "$CREATING_TABLE" | sed 's/^/    /'
info ""

orders_id="$(jp cloudcluster lab02-orders .status.productID)"
ORDERS_NAME="lab02-orders"
on_cloud && ORDERS_NAME="$(lab_display o)"

check      "lab02-orders STATUS"               Creating     "$(status_of lab02-orders)"
check      "raw cloud status, passed through"  Creating     "$(jp cloudcluster lab02-orders .status.cloudStatus)"
check      "Ready while the cloud builds"      False        "$(condition cloudcluster lab02-orders Ready)"
check_not_empty "status.productID"             "$orders_id"
check_not_empty "status.provisionAttemptedAt"  "$(jp cloudcluster lab02-orders .status.provisionAttemptedAt)"
check      "status.provisionAttemptedName, the name the create carried" "$ORDERS_NAME" \
  "$(jp cloudcluster lab02-orders .status.provisionAttemptedName)"
check      "CHANGES, not set until there is a running cluster to compare" "" "$(changes_of lab02-orders)"
# An empty cell collapses into the whitespace around it. The header has seven
# words and the row one fewer for every empty cell, so awk '{print $4}' over
# this table reads REGION as TIER. Parse these by field name, never by position.
# Counted rather than assumed, because what RavenDB Cloud reports this early
# differs between control planes.
EMPTY_CELLS=0
for path in "$READY_REASON" "$SYNCED_REASON" .status.tier .status.region .status.nodes; do
  [ -n "$(jp cloudcluster lab02-orders "$path")" ] || EMPTY_CELLS=$((EMPTY_CELLS + 1))
done
check "columns in the header" 7 \
  "$(printf '%s\n' "$CREATING_TABLE" | awk 'NR == 1 { print NF }')"
check "fields in the row, one short for each empty cell ($EMPTY_CELLS)" "$((7 - EMPTY_CELLS))" \
  "$(printf '%s\n' "$CREATING_TABLE" | awk 'NR == 2 { print NF }')"

note "the operator reads the cluster back on the same pass that created it, so what"
note "RavenDB Cloud already reports is in the table before the cluster is usable"

LAB02_MADE="lab02-orders"

# made <name> -> whether this run actually provisioned that cluster.
#
# The budget decides it, at the point of creation. Every later step that reads
# one of those clusters has to ask, or it asserts against an object that was
# never made and reports eleven empty strings as failures. Which is exactly
# what the first cloud run did.
made() { case " $LAB02_MADE " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }

# --- 3. the same cluster, spelled out ---------------------------------------

if costly "the fully specified Production contrast (three nodes, PB20, 100 GB)"; then
step "Provisioning the fully specified contrast"
k apply -f - >/dev/null <<'YAML'
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab02-analytics
  namespace: apps
spec:
  accountRef:
    name: ravendb-cloud

  displayName: lab02-analytics   # the name shown in the portal, and the identity
  tier: Production
  provider: AWS
  region: eu-west-1              # overrides the CloudAccount default
  instanceType: PB20             # omit and the operator picks the smallest for the tier
  releaseChannel: Stable62       # overrides what the cloud reports as the account default
  nodes: 3
  # Immutable, baked into the DNS names, and capped at 13 characters by
  # RavenDB Cloud. "lab02-analytics" would be rejected at apply time, which
  # is why this is not simply the object name.
  subdomain: lab02-stats

  storage:
    sizeGB: 100                  # must be on PB20's menu: 20, 40, 80, 100, 150
    type: SsdPremium
    iops: 3000
    throughputMBps: 125

  networking:
    allowedIPs:
      - 10.0.0.0/8
      - 203.0.113.7/32

  managementPolicy: Full
  deletionPolicy: Retain
  connectionSecret:
    name: lab02-analytics-connection
    includeCertificate: true
  pollIntervalSeconds: 300
YAML

LAB02_MADE="$LAB02_MADE lab02-analytics"
fi

step "Waiting for what this run provisioned to settle"
info "clusters: $LAB02_MADE"
for name in $LAB02_MADE; do
  if ! wait_for_status "$name" Running "$LAB_SETTLE_TIMEOUT"; then
    warn "$name did not reach Running; recent operator logs:"
    operator_logs 30
  fi
done

info ""
k get cloudcluster $LAB02_MADE || true
info ""

step "Running: the cloud says Active and the Secret is published"
READY_MESSAGE="$(condition_message cloudcluster lab02-orders Ready)"
check "lab02-orders STATUS"               Running "$(status_of lab02-orders)"
check "raw cloud status"                  Active  "$(jp cloudcluster lab02-orders .status.cloudStatus)"
check "Ready condition"                   True    "$(condition cloudcluster lab02-orders Ready)"
# This message is the source of the MESSAGE column under -o wide.
check_contains "the Ready message counts the nodes" "The cluster is Active with 1 node(s)" "$READY_MESSAGE"
check_contains "and names the Secret it published" "Secret \"lab02-orders-connection\" holds its URLs and client certificate" \
  "$READY_MESSAGE"
check "lab02-orders CHANGES"              Applied "$(changes_of lab02-orders)"
check "Synced condition"                  True    "$(condition cloudcluster lab02-orders Synced)"
check "Synced message" "Everything this manifest sets matches RavenDB Cloud." \
  "$(condition_message cloudcluster lab02-orders Synced)"

info ""
info "the priority=1 columns, which only -o wide prints:"
k get cloudcluster lab02-orders -o wide || true
info ""

# --- 4. what the operator filled in -----------------------------------------

step "What the operator resolved for the minimal manifest"
info ""
k get cloudcluster lab02-orders -o custom-columns='STATUS:.status.conditions[?(@.type=="Ready")].reason,INSTANCE:.status.instanceType,CHANNEL:.status.releaseChannel,PROVIDER:.status.provider,REGION:.status.region,DISK:.status.hardware.storage.sizeGB' || true
info ""

check "provider, from the CloudAccount default" AWS            "$(jp cloudcluster lab02-orders .status.provider)"
check "region, from the CloudAccount default"   eu-central-1   "$(jp cloudcluster lab02-orders .status.region)"
if on_mock; then
  check "channel, the account default the cloud reported" Stable72 "$(jp cloudcluster lab02-orders .status.releaseChannel)"
else
  check_not_empty "channel, the account default the cloud reported" "$(jp cloudcluster lab02-orders .status.releaseChannel)"
  info "this account's default channel: $(jp cloudcluster lab02-orders .status.releaseChannel)"
fi
check "instance type, smallest for Development" Dev10          "$(jp cloudcluster lab02-orders .status.instanceType)"
check "Dev10 vCPUs"                             2              "$(jp cloudcluster lab02-orders .status.hardware.cpus)"
check "Dev10 RAM in MB"                        512            "$(jp cloudcluster lab02-orders .status.hardware.ramMB)"
check "nodes that come with Dev10"              1              "$(jp cloudcluster lab02-orders .status.nodes)"
check "disk, the one thing you asked for"       20             "$(jp cloudcluster lab02-orders .status.hardware.storage.sizeGB)"
check "disk class default"                      SsdStandard    "$(jp cloudcluster lab02-orders .status.hardware.storage.type)"
check "management policy for a cluster this object created" Full "$(jp cloudcluster lab02-orders .status.managementPolicy)"
# The manifest set no subdomain, so the operator derived one from the display
# name and the cloud built the DNS from it, as <name>-dev.<account> for a
# Development cluster. RavenDB Cloud requires the field despite the OpenAPI
# document marking it nullable, and caps it at 13 characters, so a longer name
# has to be set explicitly (see lab02-analytics).
if on_mock; then
  check "subdomain, derived from the display name" \
    "https://a.lab02-orders-dev.acme.ravendb.cloud" "$(jp cloudcluster lab02-orders '.status.urls[0]')"
else
  # A real run's subdomain carries this run's suffix, so the whole URL cannot
  # be asserted here. The subdomain this run asked for is in it, and that is
  # the claim.
  check_contains "the URL is built from the subdomain this run asked for" \
    "$(lab_subdomain o)" "$(jp cloudcluster lab02-orders '.status.urls[0]')"
  info "node URL: $(jp cloudcluster lab02-orders '.status.urls[0]')"
fi
check "connection Secret"                       lab02-orders-connection \
  "$(jp cloudcluster lab02-orders .status.connectionSecretName)"

# The resolved values live in status only: the manifest you applied is the
# manifest that is stored.
check "spec.instanceType is still unset" "" "$(jp cloudcluster lab02-orders .spec.instanceType)"
check "spec.provider is still unset"     "" "$(jp cloudcluster lab02-orders .spec.provider)"
if on_cloud; then
  # A shared account gets a run-specific name, so the manifest sets one and
  # the operator must leave it exactly as written.
  check "spec.displayName is the name this run wrote" "$ORDERS_NAME" \
    "$(jp cloudcluster lab02-orders .spec.displayName)"
else
  check "spec.displayName is still unset, so the object name is the identity" "" \
    "$(jp cloudcluster lab02-orders .spec.displayName)"
fi

# metadata.generation is still 1, and it took a bug fix to be. The operator
# writes to this object once, to add its finalizer, and a write that touches
# only metadata does not bump generation on a custom resource with a status
# subresource: only a change to spec does.
#
# That write used to be an Update, which sends the object back whole, and a spec
# that has been through Go and back is not the one that was applied: the two
# struct fields serialised as {}, so the API server saw a changed spec and bumped generation on every single
# object's first reconcile. Every one was then permanently unequal to the
# manifest it came from, which is drift a GitOps tool cannot resolve. The
# finalizer is a merge patch of the difference now.
check "generation, untouched by the operator" 1 \
  "$(jp cloudcluster lab02-orders .metadata.generation)"
check "the finalizer it wrote instead" "cloud.ravendb.net/finalizer" \
  "$(jp cloudcluster lab02-orders '.metadata.finalizers[0]')"
check "observedGeneration matches what was applied" 1 \
  "$(jp cloudcluster lab02-orders .status.observedGeneration)"

# status.adopted is a plain bool with omitempty, so a cluster this object
# created never stores the field at all, and the two printers show that
# absence differently.
check "status.adopted, omitted from status while it is false" "" \
  "$(jp cloudcluster lab02-orders .status.adopted)"
check "the same field via custom-columns" "<none>" \
  "$(k get cloudcluster lab02-orders --no-headers \
      -o custom-columns='ADOPTED:.status.adopted' 2>/dev/null | tr -d '[:space:]')"

step "The create event records where the defaults sent it"
info ""
k get events --field-selector involvedObject.name=lab02-orders \
  --sort-by=.metadata.creationTimestamp \
  -o custom-columns='TYPE:.type,REASON:.reason,MESSAGE:.message' 2>/dev/null || true
info ""
ORDERS_EVENTS="$(lab02_events lab02-orders)"
check_contains "the Created event names provider and region" "in AWS/eu-central-1" "$ORDERS_EVENTS"
check_contains "the Secret has an event of its own" \
  "Normal SecretPublished: Connection Secret \"lab02-orders-connection\" created" "$ORDERS_EVENTS"
# A Warning is something a person may have to act on, so a create that went to
# plan has at most one: the lab account's default allow list is 0.0.0.0/0,
# which is fine for a lab and worth saying once about anything real.
WARNINGS="$(printf '%s\n' "$ORDERS_EVENTS" | { grep '^Warning' || true; })"
if [ "$LAB_ALLOWED_IPS" = 0.0.0.0/0 ]; then
  check_contains "the one Warning is the open allow list" "Warning OpenAllowList: allowedIPs contains 0.0.0.0/0" "$WARNINGS"
  check "and there is no other" 1 "$(printf '%s' "$WARNINGS" | { grep -c . || true; })"
else
  check "Warning events for a create that went to plan" 0 "$(printf '%s' "$WARNINGS" | { grep -c . || true; })"
fi

# --- 5. the fully specified one ---------------------------------------------

if ! made lab02-analytics; then
  skip "the assertions about lab02-analytics, which this run did not provision"
else
step "The fully specified cluster resolved to exactly what it asked for"
check "region"          eu-west-1     "$(jp cloudcluster lab02-analytics .status.region)"
check "instance type"   PB20          "$(jp cloudcluster lab02-analytics .status.instanceType)"
check "tier"            Production    "$(jp cloudcluster lab02-analytics .status.tier)"
check "release channel" Stable62      "$(jp cloudcluster lab02-analytics .status.releaseChannel)"
check "nodes"           3             "$(jp cloudcluster lab02-analytics .status.nodes)"
check "vCPUs"           2             "$(jp cloudcluster lab02-analytics .status.hardware.cpus)"
check "RAM in MB"      4096          "$(jp cloudcluster lab02-analytics .status.hardware.ramMB)"
check "disk"            100           "$(jp cloudcluster lab02-analytics .status.hardware.storage.sizeGB)"
check "disk class"      SsdPremium    "$(jp cloudcluster lab02-analytics .status.hardware.storage.type)"
check "first allowed range" 10.0.0.0/8 "$(jp cloudcluster lab02-analytics '.status.allowedIPs[0]')"
check "CHANGES"         Applied       "$(changes_of lab02-analytics)"
fi

# --- 6. crash safety --------------------------------------------------------

if needs_mock "crash recovery and requeue cadence (both stage a cluster in the control plane and read its request log)"; then
step "Crash safety: an interrupted create finds its own cluster, and knows it is its own"
# The dangerous window: the operator stamps status.provisionAttemptedAt, sends
# the create, and dies before it records the product id. On restart the object
# has no id, and the name finds a cluster. Without the marker that cluster reads
# as one somebody else made, so it would be taken over read-only, under
# Observe, and this object's own cluster would stop being managed.
#
# The lab stages exactly that. The object starts under Observe, so nothing is
# created;
# its status gets the marker a create writes; and a cluster by its name appears
# in the control plane, as if the create had landed.
k apply -f - >/dev/null <<'YAML'
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab02-twin
  namespace: apps
spec:
  accountRef:
    name: ravendb-cloud
  tier: Development
  storage:
    sizeGB: 20
  managementPolicy: Observe
  # Deliberately short so the settled polling cadence is visible inside a lab.
  pollIntervalSeconds: 5
YAML
wait_for_status lab02-twin ClusterNotFound 60 || { warn "lab02-twin never reported ClusterNotFound"; operator_logs 30; }
check "lab02-twin STATUS under Observe" ClusterNotFound "$(status_of lab02-twin)"
check_contains "and it says nothing was created" "nothing was created" \
  "$(condition_message cloudcluster lab02-twin Ready)"

ATTEMPTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
k patch cloudcluster lab02-twin --subresource=status --type=merge \
  -p "{\"status\":{\"provisionAttemptedAt\":\"$ATTEMPTED_AT\",\"provisionAttemptedName\":\"lab02-twin\"}}" >/dev/null
info "status now carries the marker a create stamps: provisionAttemptedAt $ATTEMPTED_AT"
seeded_id="$(lab_api POST /_lab/clusters \
  '{"displayName":"lab02-twin","tier":"Development","instanceType":"Dev10","region":"eu-central-1","provider":"Aws"}' \
  | sed -n 's/.*"ids":\["\([^"]*\)".*/\1/p')"
check_not_empty "the cluster the interrupted create made" "$seeded_id"
info "and the control plane holds lab02-twin as $seeded_id, which no status names yet"

info "dropping Observe: this is the first pass after the restart"
k patch cloudcluster lab02-twin --type=merge -p '{"spec":{"managementPolicy":null}}' >/dev/null
wait_for_status lab02-twin Running 120 || { warn "lab02-twin never reached Running"; operator_logs 30; }

twin_id="$(jp cloudcluster lab02-twin .status.productID)"
check "it bound the cluster its create made" "$seeded_id" "$twin_id"
check "no second cluster was created for the name" 1 "$(lab02_named_count lab02-twin)"
check "status.adopted, left false because the marker says this object created it" "" \
  "$(jp cloudcluster lab02-twin .status.adopted)"
check "so managementPolicy resolves to Full, not Observe" Full \
  "$(jp cloudcluster lab02-twin .status.managementPolicy)"
check_contains "the event says what it found" \
  "Created: Found the cluster an interrupted create made: \"lab02-twin\" ($seeded_id)" "$(lab02_events lab02-twin)"
check "CHANGES" Applied "$(changes_of lab02-twin)"
note "the same lookup without the marker is taking a cluster over, which lab 04 shows:"
note "adopted true, Observe, and an Adopted event instead of Created"

# --- 7. requeue cadence -----------------------------------------------------

step "Requeue cadence"
provision_calls="$(lab02_detail_calls "$orders_id")"
info "the operator read lab02-orders $provision_calls time(s) between the create call and Running"
# A 5 second create polled every 30 seconds costs a handful of reads. The exact
# number depends on where the create lands inside the requeue, so the assertion
# is the order of magnitude: a hot loop would be hundreds.
if [ "$provision_calls" -ge 1 ] && [ "$provision_calls" -le 6 ]; then not_a_loop=0; else not_a_loop=1; fi
check "a whole provision cost at most 6 reads, so nothing is spinning" yes "$(yesno "$not_a_loop")"

lab_api POST /_lab/requests/reset >/dev/null
info "sampling a 16 second window with both clusters settled and Running"
sleep 16

twin_calls="$(lab02_detail_calls "$twin_id")"
orders_calls="$(lab02_detail_calls "$orders_id")"
info "lab02-twin   (pollInterval 5s)  : $twin_calls call(s)"
info "lab02-orders (pollInterval 5m)  : $orders_calls call(s)"

if [ "$twin_calls" -ge 2 ]; then paced=0; else paced=1; fi
check "a settled cluster is re-read on spec.pollInterval" yes "$(yesno "$paced")"
if [ "$orders_calls" -lt "$twin_calls" ]; then slower=0; else slower=1; fi
check "the 5m default is re-read far less often than the 5s one" yes "$(yesno "$slower")"
LAB02_MADE="$LAB02_MADE lab02-twin"
fi

# --- 8. cleanup -------------------------------------------------------------

step "Cleaning up"
# deletionPolicy defaults to Retain, so deleting the Kubernetes objects leaves
# the clusters running. That is the point of the default, and it is why this lab
# has to remove them itself.
if on_mock; then
  k delete cloudcluster $LAB02_MADE --ignore-not-found --timeout=90s >/dev/null ||
    warn "deleting the CloudClusters timed out"
  check_contains "Retain left the cloud cluster running" "\"id\":\"$orders_id\"" \
    "$(lab_api GET /_lab/clusters)"
  lab02_vanish_all
  check "lab02 clusters left in the control plane" 0 "$(lab02_mock_ids | wc -l | tr -d ' ')"
else
  # Retain is still the default, and against a real account that means a bill.
  # release() flips deletionPolicy first, so the cloud side goes with the object
  # and the operator does the terminating rather than a curl to a lab surface.
  info "switching deletionPolicy to Terminate so nothing survives this run"
  release $LAB02_MADE
fi
check "lab02 CloudClusters left in $LAB_NAMESPACE" 0 \
  "$({ k get cloudcluster -o name 2>/dev/null || true; } | { grep -c 'lab02-' || true; })"

lab_summary

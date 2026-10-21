#!/usr/bin/env bash
# Lab 06: delete the object without deleting the database.
#
# What this shows:
#   * deletionPolicy Retain (the default): the CloudCluster object goes away,
#     the cloud cluster keeps running, and the connection Secret is collected
#     with its owner
#   * deletionPolicy Terminate: the cloud cluster is destroyed, after an event
#     that outlives the object that caused it
#   * the finalizer cloud.ravendb.net/finalizer: when the operator adds it,
#     what it buys, what a wedged deletion looks like, and how to force release
#     one (and what that costs)
#   * deleting a cluster the object found rather than created, where Retain
#     means the guest leaves without touching anything, and Terminate counts
#     only when creationMode TakeOverOnly said the cluster was meant
#   * a cluster terminated behind the operator's back, which is reported and
#     never quietly replaced
#
#   labs/06_delete_safely.sh
#
# Run from the repository root, after labs/00_setup.sh.
source "$(dirname "$0")/lib.sh"

RETAIN=lab06-retain
TERMINATE=lab06-term
NOACCOUNT=lab06-noaccount
NOCRED_RETAIN=lab06-nocred-retain
NOCRED_TERMINATE=lab06-nocred-terminate
ADOPT_RETAIN=lab06-adopt-retain
ADOPT_TERMINATE=lab06-adopt-terminate
ADOPT_CONFIRMED=lab06-adopt-confirmed
GONE=lab06-gone

GUEST_ACCOUNT=lab06-guest-account
GUEST_SECRET=lab06-guest-key
MISSING_ACCOUNT=lab06-no-such-account

LAB06_CLUSTERS=("$RETAIN" "$TERMINATE" "$NOACCOUNT" "$NOCRED_RETAIN" "$NOCRED_TERMINATE"
                "$ADOPT_RETAIN" "$ADOPT_TERMINATE" "$ADOPT_CONFIRMED" "$GONE")

# --- helpers ----------------------------------------------------------------

# object_state <kind> <name> -> present | gone
object_state() {
  if kubectl -n "$LAB_NAMESPACE" get "$1" "$2" >/dev/null 2>&1; then
    echo present
  else
    echo gone
  fi
}

# wait_object_gone <kind> <name> [timeout]
wait_object_gone() {
  local kind="$1" name="$2" deadline=$((SECONDS + ${3:-90}))
  info "waiting for $kind/$name to disappear (up to ${3:-90}s)"
  while [ "$SECONDS" -lt "$deadline" ]; do
    if [ "$(object_state "$kind" "$name")" = gone ]; then
      return 0
    fi
    sleep 2
  done
  warn "$kind/$name is still there"
  return 1
}

# cloud_status <product-id> -> the status the control plane reports, or empty
# if the cluster does not exist there any more. No jq: the labs run on plain
# Git Bash too, so the JSON is split on '{' and read with sed.
cloud_status() { product_status "$1"; }

# cloud_state <product-id> -> the reported status, or "gone"
cloud_state() {
  local status
  status="$(cloud_status "$1")"
  if [ -n "$status" ]; then
    printf '%s\n' "$status"
  else
    printf 'gone\n'
  fi
}

# cloud_ids_named <display-name-prefix> -> ids, one per line
cloud_ids_named() { cloud_products | awk -v p="$1" 'index($2, p) == 1 { print $1 }'; }

# seed_cloud_cluster <display-name> -> the new product id
# Only the stand-in can conjure a cluster Kubernetes never created. The parts
# of this lab built on that say so where they begin.
seed_cloud_cluster() {
  lab_api POST /_lab/clusters "{\"displayName\":\"$1\",\"count\":1}" \
    | sed -n 's/.*"ids":\["\([^"]*\)".*/\1/p' || true
}

# wait_cloud_status <product-id> <status> [timeout]
#
# There is no wait_cloud_gone, because a terminated cluster never goes: RavenDB
# Cloud keeps the record and only changes its status.
wait_cloud_status() {
  # Every poll is a live call to RavenDB Cloud on the cloud
  # target, so it polls slower there and waits far longer: terminating a real
  # cluster is minutes of work, not the instant the stand-in takes.
  local id="$1" want="$2" gap=2 limit="${3:-90}"
  if on_cloud; then
    gap=6
    # The larger of what the caller asked for and what this environment needs.
    # Every timeout in this lab was chosen against a control plane that
    # terminates instantly, so taking the caller's number would cap a real wait
    # at a stand-in's idea of long enough. Measured: a real terminate is still
    # Terminating well past the 90 seconds these call sites pass.
    limit="${3:-$LAB_CHANGE_TIMEOUT}"
    [ "$limit" -lt "$LAB_CHANGE_TIMEOUT" ] && limit="$LAB_CHANGE_TIMEOUT"
  fi
  local deadline=$((SECONDS + limit))
  info "waiting for cloud cluster $id to report $want (up to ${limit}s)"
  while [ "$SECONDS" -lt "$deadline" ]; do
    if [ "$(cloud_status "$id")" = "$want" ]; then
      return 0
    fi
    sleep "$gap"
  done
  warn "cloud cluster $id is $(cloud_status "$id"), not $want"
  return 1
}

# object_uid <kind> <name> -> the uid, which has to be read before the delete
# because that is the only handle on the events once the object is gone.
object_uid() {
  jp "$1" "$2" .metadata.uid
}

# events_for_uid <uid> -> TYPE REASON MESSAGE, one event per line.
#
# Matched on uid rather than on name. Events outlive the object they describe
# (that is the whole point of this lab) and the object names here are stable, so
# a name selector would let a previous run of this lab answer for this one.
events_for_uid() {
  [ -n "$1" ] || return 0
  kubectl -n "$LAB_NAMESPACE" get events \
    --field-selector "involvedObject.uid=$1" \
    -o custom-columns=TYPE:.type,REASON:.reason,MESSAGE:.message --no-headers 2>/dev/null || true
}

# wait_for_event <uid> <needle> [timeout] -> the one event line containing the
# needle. One line, so type, reason and message all have to belong to the same
# event instead of being satisfied by three different ones.
wait_for_event() {
  local uid="$1" needle="$2" deadline=$((SECONDS + ${3:-60})) line=''
  while [ "$SECONDS" -lt "$deadline" ]; do
    line="$(events_for_uid "$uid" | grep -F "$needle" | head -n 1 || true)"
    if [ -n "$line" ]; then
      printf '%s\n' "$line"
      return 0
    fi
    sleep 2
  done
  {
    warn "no event matching '$needle' arrived; what is recorded for $uid:"
    events_for_uid "$uid" | sed 's/^/      /'
  } >&2
  return 1
}

# policy_event_line <uid> -> the line the deletion path wrote, or empty
policy_event_line() {
  events_for_uid "$1" | grep -F 'deletionPolicy is' | head -n 1 || true
}

force_release_finalizer() {
  kubectl -n "$LAB_NAMESPACE" patch cloudcluster "$1" \
    --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
}

# lab06_purge removes everything this lab creates, on both sides. It runs once
# at the start (so a re-run starts from a clean slate) and once at the end.
lab06_purge() {
  local name id
  for name in "${LAB06_CLUSTERS[@]}"; do
    kubectl -n "$LAB_NAMESPACE" delete cloudcluster "$name" \
      --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
  for name in "${LAB06_CLUSTERS[@]}"; do
    if [ "$(object_state cloudcluster "$name")" = present ]; then
      force_release_finalizer "$name"
    fi
    kubectl -n "$LAB_NAMESPACE" delete secret "${name}-connection" \
      --ignore-not-found >/dev/null 2>&1 || true
  done
  kubectl -n "$LAB_NAMESPACE" delete cloudaccount "$GUEST_ACCOUNT" \
    --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$LAB_NAMESPACE" delete secret "$GUEST_SECRET" \
    --ignore-not-found >/dev/null 2>&1 || true

  # Whatever this lab left running goes too, including the cluster Part 1
  # retains on purpose and any orphaned by a forced finalizer release. This is
  # the lab most likely to leave one behind, because leaving one behind is what
  # Retain means and what this lab is about.
  for id in $(cloud_ids_named "${LAB_ID}-"); do
    retire_product "$id"
  done
}

lab06_cleanup() {
  step "Cleaning up"
  lab06_purge
  info "removed every lab06 object and every lab06 cluster in the control plane"
  close_cloud_portforward
}

# --- preconditions ----------------------------------------------------------

step "Preconditions"
require_ready_account
open_cloud_portforward
# open_cloud_portforward installs its own EXIT trap, so ours has to come after
# it and close the port-forward itself.
trap lab06_cleanup EXIT

lab06_purge
for name in "${LAB06_CLUSTERS[@]}"; do
  wait_object_gone cloudcluster "$name" 60 >/dev/null 2>&1 || true
done
info "starting from a clean slate"

# --- Part 1: Retain, the default -------------------------------------------

step "Part 1: provision a cluster and leave deletionPolicy alone"

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $RETAIN
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
spec:
  accountRef:
    name: ravendb-cloud
  tier: Development
  storage:
    sizeGB: 20
$(cloud_names r)
EOF
info "applied $RETAIN with no deletionPolicy in the manifest"

wait_for_status "$RETAIN" Running "$LAB_SETTLE_TIMEOUT" || { warn "it never reached Running"; operator_logs 40; }
k get cloudcluster "$RETAIN"

check "$RETAIN STATUS" Running "$(status_of "$RETAIN")"
check "deletionPolicy defaulted by the CRD" Retain "$(jp cloudcluster "$RETAIN" .spec.deletionPolicy)"

RETAIN_ID="$(jp cloudcluster "$RETAIN" .status.productID)"
check_not_empty "the bound product id" "$RETAIN_ID"
check "the control plane has it Active" Active "$(cloud_state "$RETAIN_ID")"

step "Part 1: the finalizer and the Secret owner reference, before the delete"
info "finalizers: $(jp cloudcluster "$RETAIN" '.metadata.finalizers')"
check "the finalizer on the live object" "cloud.ravendb.net/finalizer" \
  "$(jp cloudcluster "$RETAIN" '.metadata.finalizers[0]')"
check "connection Secret owner kind" "CloudCluster" \
  "$(jp secret "${RETAIN}-connection" '.metadata.ownerReferences[0].kind')"
check "connection Secret owner name" "$RETAIN" \
  "$(jp secret "${RETAIN}-connection" '.metadata.ownerReferences[0].name')"
check "connection Secret owner apiVersion" "cloud.ravendb.net/v1alpha1" \
  "$(jp secret "${RETAIN}-connection" '.metadata.ownerReferences[0].apiVersion')"
check "the owner reference is a controller reference" "true" \
  "$(jp secret "${RETAIN}-connection" '.metadata.ownerReferences[0].controller')"
check "the owner reference blocks owner deletion" "true" \
  "$(jp secret "${RETAIN}-connection" '.metadata.ownerReferences[0].blockOwnerDeletion')"

step "Part 1: kubectl delete cloudcluster $RETAIN"
# The uid has to be read now. Once the object is gone it is the only way left
# to find the events it produced.
RETAIN_UID="$(object_uid cloudcluster "$RETAIN")"
check_not_empty "the uid to look events up by later" "$RETAIN_UID"
k delete cloudcluster "$RETAIN" --wait=false >/dev/null
wait_object_gone cloudcluster "$RETAIN" 90 || operator_logs 40

check "the CloudCluster object" "gone" "$(object_state cloudcluster "$RETAIN")"
wait_object_gone secret "${RETAIN}-connection" 90 || true
check "the connection Secret" "gone" "$(object_state secret "${RETAIN}-connection")"
check "the cloud cluster after the delete" Active "$(cloud_state "$RETAIN_ID")"

RETAIN_EVENT_LINE="$(wait_for_event "$RETAIN_UID" \
  "deletionPolicy is Retain: leaving RavenDB Cloud cluster $RETAIN_ID running" 60 || true)"
info "events still in the namespace for an object that no longer exists:"
events_for_uid "$RETAIN_UID" | sed 's/^/      /'
check_contains "the deletion event" "Normal" "$RETAIN_EVENT_LINE"
check_contains "the deletion event" "Retained" "$RETAIN_EVENT_LINE"
check_contains "the deletion event" \
  "deletionPolicy is Retain: leaving RavenDB Cloud cluster $RETAIN_ID running" \
  "$RETAIN_EVENT_LINE"
note "reason Retained. It used to be Terminated, beside a message saying the cluster"
note "was left running, and this lab had to explain the contradiction away"

# --- Part 2: Terminate ------------------------------------------------------

step "Part 2: a cluster that asks to be terminated"

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $TERMINATE
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
spec:
  accountRef:
    name: ravendb-cloud
  deletionPolicy: Terminate
  tier: Development
  storage:
    sizeGB: 20
$(cloud_names t)
EOF
info "applied $TERMINATE with deletionPolicy: Terminate written on purpose"

wait_for_status "$TERMINATE" Running "$LAB_SETTLE_TIMEOUT" || { warn "it never reached Running"; operator_logs 40; }
TERMINATE_ID="$(jp cloudcluster "$TERMINATE" .status.productID)"
check_not_empty "the bound product id" "$TERMINATE_ID"
check "spec.deletionPolicy" Terminate "$(jp cloudcluster "$TERMINATE" .spec.deletionPolicy)"
check "the control plane has it Active" Active "$(cloud_state "$TERMINATE_ID")"

step "Part 2: kubectl delete cloudcluster $TERMINATE"
TERMINATE_UID="$(object_uid cloudcluster "$TERMINATE")"
check_not_empty "the uid to look events up by later" "$TERMINATE_UID"
k delete cloudcluster "$TERMINATE" --wait=false >/dev/null
wait_object_gone cloudcluster "$TERMINATE" 90 || operator_logs 40
check "the CloudCluster object" "gone" "$(object_state cloudcluster "$TERMINATE")"

TERMINATE_EVENT_LINE="$(wait_for_event "$TERMINATE_UID" \
  "deletionPolicy is Terminate: terminating RavenDB Cloud cluster $TERMINATE_ID" 60 || true)"
info "the record left behind in the namespace:"
events_for_uid "$TERMINATE_UID" | sed 's/^/      /'
check_contains "the termination event" "Normal" "$TERMINATE_EVENT_LINE"
check_contains "the termination event" "Terminating" "$TERMINATE_EVENT_LINE"
check_contains "the termination event" \
  "deletionPolicy is Terminate: terminating RavenDB Cloud cluster $TERMINATE_ID" \
  "$TERMINATE_EVENT_LINE"

# A terminated cluster does not disappear from RavenDB Cloud. The status
# becomes Terminated and the record stays, which is worth seeing here rather
# than discovering later: the display name stays taken, so applying this same
# manifest again is refused, until the terminated cluster is hidden in the
# portal. The operator skips terminated clusters when it looks a name up, so
# it never binds to one either.
wait_cloud_status "$TERMINATE_ID" Terminated 90 || warn "it never reached Terminated"
check "the cloud cluster after the delete" Terminated "$(cloud_state "$TERMINATE_ID")"
check "and it is still listed, under the same display name" 1 \
  "$(cloud_products | awk -v id="$TERMINATE_ID" '$1 == id { n++ } END { print n + 0 }')"

# --- Part 3: the finalizer, and a deletion that cannot finish ---------------

if needs_mock "parts 3 to 5, which seed five clusters Kubernetes never created"; then
step "Part 3: a deletionPolicy with nothing to act on"
# The finalizer goes on at the start of the first pass, before the account is
# resolved, so even an object whose account does not exist has one. It still
# deletes at once: nothing was created, so there is nothing to terminate.

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $NOACCOUNT
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
spec:
  accountRef:
    name: $MISSING_ACCOUNT
  deletionPolicy: Terminate
  tier: Development
EOF
info "applied $NOACCOUNT, pointed at CloudAccount $MISSING_ACCOUNT, which does not exist"

wait_for_status "$NOACCOUNT" WaitingForAccount 90 \
  || { warn "it never reported WaitingForAccount"; operator_logs 20; }
check "STATUS with no account to resolve" WaitingForAccount "$(status_of "$NOACCOUNT")"
check "spec.deletionPolicy" Terminate "$(jp cloudcluster "$NOACCOUNT" .spec.deletionPolicy)"
check "the finalizer, before the account resolves" "cloud.ravendb.net/finalizer" \
  "$(jp cloudcluster "$NOACCOUNT" '.metadata.finalizers[0]')"

k delete cloudcluster "$NOACCOUNT" --wait=false >/dev/null
wait_object_gone cloudcluster "$NOACCOUNT" 30 || warn "still there"
check "delete with nothing created" "gone" "$(object_state cloudcluster "$NOACCOUNT")"
note "the finalizer held the object for one pass, which found no create to"
note "answer for and let it go. Terminate had nothing to act on."

step "Part 3: a second credential, so nothing here touches the shared one"

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Secret
metadata:
  name: $GUEST_SECRET
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
type: Opaque
stringData:
  apiKey: lab-api-key
---
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: $GUEST_ACCOUNT
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
spec:
  apiKeySecretRef:
    name: $GUEST_SECRET
    key: apiKey
  endpoint: http://mockcloud.ravendb-cloud-lab.svc.cluster.local
  defaults:
    provider: AWS
    region: eu-central-1
    allowedIPs:
      - $LAB_ALLOWED_IPS
  refreshIntervalSeconds: 60
EOF

wait_for cloudaccount "$GUEST_ACCOUNT" '.status.conditions[?(@.type=="Ready")].status' True 90 \
  || warn "the second account did not verify"
check "$GUEST_ACCOUNT Ready" True "$(condition cloudaccount "$GUEST_ACCOUNT" Ready)"

NOCRED_RETAIN_ID="$(seed_cloud_cluster "$(lab_display legacy-a)")"
NOCRED_TERMINATE_ID="$(seed_cloud_cluster "$(lab_display legacy-b)")"
check_not_empty "seeded cluster for $NOCRED_RETAIN" "$NOCRED_RETAIN_ID"
check_not_empty "seeded cluster for $NOCRED_TERMINATE" "$NOCRED_TERMINATE_ID"

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $NOCRED_RETAIN
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
spec:
  accountRef:
    name: $GUEST_ACCOUNT
  displayName: $(lab_display legacy-a)
  tier: Production
---
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $NOCRED_TERMINATE
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
spec:
  accountRef:
    name: $GUEST_ACCOUNT
  displayName: $(lab_display legacy-b)
  tier: Production
  # Terminate ends a cluster this object found only when it said it meant to.
  creationMode: TakeOverOnly
  deletionPolicy: Terminate
EOF

wait_for_status "$NOCRED_RETAIN" Running 120 || warn "$NOCRED_RETAIN never reached Running"
wait_for_status "$NOCRED_TERMINATE" Running 120 || warn "$NOCRED_TERMINATE never reached Running"

step "Part 3: the finalizer on a live object"
info "$NOCRED_TERMINATE finalizers: $(jp cloudcluster "$NOCRED_TERMINATE" '.metadata.finalizers')"
check "the finalizer is present" "cloud.ravendb.net/finalizer" \
  "$(jp cloudcluster "$NOCRED_TERMINATE" '.metadata.finalizers[0]')"
SECOND_FINALIZER="$(jp cloudcluster "$NOCRED_TERMINATE" '.metadata.finalizers[1]')"
check "finalizers beyond the first" "none" "${SECOND_FINALIZER:-none}"
LIVE_DELETION_TIMESTAMP="$(jp cloudcluster "$NOCRED_TERMINATE" '.metadata.deletionTimestamp')"
check "deletionTimestamp on a live object" "none" "${LIVE_DELETION_TIMESTAMP:-none}"

step "Part 3: break the credential, then delete both objects"
k delete secret "$GUEST_SECRET" >/dev/null
wait_for cloudaccount "$GUEST_ACCOUNT" '.status.conditions[?(@.type=="Ready")].status' False 120 \
  || warn "the account did not go Ready=False"
check "$GUEST_ACCOUNT Ready" False "$(condition cloudaccount "$GUEST_ACCOUNT" Ready)"
check "$GUEST_ACCOUNT reason" KeyMissing "$(condition_reason cloudaccount "$GUEST_ACCOUNT" Ready)"

NOCRED_RETAIN_UID="$(object_uid cloudcluster "$NOCRED_RETAIN")"
k delete cloudcluster "$NOCRED_RETAIN" --wait=false >/dev/null
wait_object_gone cloudcluster "$NOCRED_RETAIN" 90 || operator_logs 40
check "Retain deletes even with no usable credential" "gone" \
  "$(object_state cloudcluster "$NOCRED_RETAIN")"
check "its cloud cluster is untouched" Active "$(cloud_state "$NOCRED_RETAIN_ID")"
note "there is nothing to do in the cloud for Retain, so a broken credential"
note "cannot block the object from going away"

# This shortcut returns before reconcileDelete runs, so it skips the Retained
# event that Part 1 relied on. Waiting for the Adopted event first proves the
# recorder has flushed this object's stream, so the absence below is an absence
# and not a race.
wait_for_event "$NOCRED_RETAIN_UID" \
  "($NOCRED_RETAIN_ID) already running" 60 >/dev/null || true
NOCRED_RETAIN_EVENT_LINE="$(policy_event_line "$NOCRED_RETAIN_UID")"
check "deletion event on the broken-credential Retain path" "none" \
  "${NOCRED_RETAIN_EVENT_LINE:-none}"
warn "that shortcut costs you the event: nothing in the namespace names the"
warn "cluster ($NOCRED_RETAIN_ID) that this delete left running."

k delete cloudcluster "$NOCRED_TERMINATE" --wait=false >/dev/null
wait_for_status "$NOCRED_TERMINATE" Deleting 120 \
  || warn "expected the deletion to wedge, and say so"

check "the object is still here" "present" "$(object_state cloudcluster "$NOCRED_TERMINATE")"
check_not_empty "deletionTimestamp" "$(jp cloudcluster "$NOCRED_TERMINATE" '.metadata.deletionTimestamp')"
check "the finalizer is still holding it" "cloud.ravendb.net/finalizer" \
  "$(jp cloudcluster "$NOCRED_TERMINATE" '.metadata.finalizers[0]')"
k get cloudcluster "$NOCRED_TERMINATE" || true
WEDGED_MSG="$(condition_message cloudcluster "$NOCRED_TERMINATE" Ready)"
info "message: $WEDGED_MSG"
check "STATUS while wedged" Deleting "$(status_of "$NOCRED_TERMINATE")"
check_contains "the message says what it waits for" \
  "cannot be terminated until CloudAccount \"$GUEST_ACCOUNT\" and the API key Secret it names are usable again"   "$WEDGED_MSG"
check_contains "a Warning marks that it is stuck" "Deleting" \
  "$(wait_for_event "$(object_uid cloudcluster "$NOCRED_TERMINATE")" "cannot be terminated until" 60 || true)"
check "the cloud cluster is still running" Active "$(cloud_state "$NOCRED_TERMINATE_ID")"
warn "Terminate plus no credential is a deletion that cannot finish. The right"
warn "fix is to restore the credential. Forcing the finalizer off is the wrong"
warn "one, and the next check shows exactly what it costs."

step "Part 3: force releasing the finalizer, and the orphan it leaves"
info "kubectl -n $LAB_NAMESPACE patch cloudcluster $NOCRED_TERMINATE \\"
info "  --type=merge -p '{\"metadata\":{\"finalizers\":null}}'"
force_release_finalizer "$NOCRED_TERMINATE"
wait_object_gone cloudcluster "$NOCRED_TERMINATE" 60 || warn "still there"
check "the object after the forced release" "gone" \
  "$(object_state cloudcluster "$NOCRED_TERMINATE")"
check "the cloud cluster it was told to terminate" Active \
  "$(cloud_state "$NOCRED_TERMINATE_ID")"
warn "that cluster is now orphaned: still billed, and nothing in Kubernetes manages"
warn "it any more. Its id was $NOCRED_TERMINATE_ID; applying a manifest with its name"
warn "takes it over again, which is the way back"


# --- Part 4: deleting a cluster the object found ---------------------------

step "Part 4: deleting a cluster the object found, with the default policy"

ADOPT_RETAIN_ID="$(seed_cloud_cluster "$(lab_display legacy-c)")"
check_not_empty "seeded cluster for $ADOPT_RETAIN" "$ADOPT_RETAIN_ID"

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $ADOPT_RETAIN
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
spec:
  accountRef:
    name: ravendb-cloud
  displayName: $(lab_display legacy-c)
  tier: Production
EOF

wait_for_status "$ADOPT_RETAIN" Running 120 || { warn "never reached Running"; operator_logs 40; }
check "status.adopted" "true" "$(jp cloudcluster "$ADOPT_RETAIN" .status.adopted)"
check "managementPolicy defaulted for a found cluster" Observe \
  "$(jp cloudcluster "$ADOPT_RETAIN" .status.managementPolicy)"
check "deletionPolicy defaulted, found or created" Retain \
  "$(jp cloudcluster "$ADOPT_RETAIN" .spec.deletionPolicy)"
check "connection Secret published for the guest" "CloudCluster" \
  "$(jp secret "${ADOPT_RETAIN}-connection" '.metadata.ownerReferences[0].kind')"

ADOPT_RETAIN_UID="$(object_uid cloudcluster "$ADOPT_RETAIN")"
k delete cloudcluster "$ADOPT_RETAIN" --wait=false >/dev/null
wait_object_gone cloudcluster "$ADOPT_RETAIN" 90 || operator_logs 40
check "the CloudCluster object" "gone" "$(object_state cloudcluster "$ADOPT_RETAIN")"
wait_object_gone secret "${ADOPT_RETAIN}-connection" 90 || true
check "the connection Secret" "gone" "$(object_state secret "${ADOPT_RETAIN}-connection")"
check "the cloud cluster it found" Active "$(cloud_state "$ADOPT_RETAIN_ID")"
ADOPT_RETAIN_EVENT_LINE="$(wait_for_event "$ADOPT_RETAIN_UID" \
  "deletionPolicy is Retain: leaving RavenDB Cloud cluster $ADOPT_RETAIN_ID running" 60 || true)"
info "the only thing the operator did on the way out:"
printf '%s\n' "$ADOPT_RETAIN_EVENT_LINE" | sed 's/^/      /'
check_contains "the deletion event" "Normal" "$ADOPT_RETAIN_EVENT_LINE"
check_contains "the deletion event" \
  "deletionPolicy is Retain: leaving RavenDB Cloud cluster $ADOPT_RETAIN_ID running" \
  "$ADOPT_RETAIN_EVENT_LINE"

step "Part 4: Terminate on a cluster taken over by name alone"

ADOPT_TERMINATE_ID="$(seed_cloud_cluster "$(lab_display legacy-d)")"
check_not_empty "seeded cluster for $ADOPT_TERMINATE" "$ADOPT_TERMINATE_ID"

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $ADOPT_TERMINATE
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
spec:
  accountRef:
    name: ravendb-cloud
  displayName: $(lab_display legacy-d)
  tier: Production
  deletionPolicy: Terminate
EOF

wait_for_status "$ADOPT_TERMINATE" Running 120 || { warn "never reached Running"; operator_logs 40; }
check "status.adopted" "true" "$(jp cloudcluster "$ADOPT_TERMINATE" .status.adopted)"
check "creationMode defaulted" TakeOverOrCreate "$(jp cloudcluster "$ADOPT_TERMINATE" .spec.creationMode)"

ADOPT_TERMINATE_UID="$(object_uid cloudcluster "$ADOPT_TERMINATE")"
k delete cloudcluster "$ADOPT_TERMINATE" --wait=false >/dev/null
wait_object_gone cloudcluster "$ADOPT_TERMINATE" 90 || operator_logs 40
ADOPT_TERMINATE_EVENT_LINE="$(wait_for_event "$ADOPT_TERMINATE_UID" \
  "so it is left running" 60 || true)"
info "what the operator says instead of terminating:"
printf '%s\n' "$ADOPT_TERMINATE_EVENT_LINE" | sed 's/^/      /'
check_contains "the deletion event" "Warning" "$ADOPT_TERMINATE_EVENT_LINE"
check_contains "the deletion event" \
  "deletionPolicy is Terminate, but this object took over RavenDB Cloud cluster $ADOPT_TERMINATE_ID under creationMode TakeOverOrCreate" \
  "$ADOPT_TERMINATE_EVENT_LINE"
check "the cloud cluster it found" Active "$(cloud_state "$ADOPT_TERMINATE_ID")"
note "a name can match a cluster nobody meant this object to own, so a name"
note "alone is not enough to destroy one"

step "Part 4: Terminate on a cluster taken over on purpose"

ADOPT_CONFIRMED_ID="$(seed_cloud_cluster "$(lab_display legacy-e)")"
check_not_empty "seeded cluster for $ADOPT_CONFIRMED" "$ADOPT_CONFIRMED_ID"

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $ADOPT_CONFIRMED
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
spec:
  accountRef:
    name: ravendb-cloud
  displayName: $(lab_display legacy-e)
  tier: Production
  creationMode: TakeOverOnly
  deletionPolicy: Terminate
EOF

wait_for_status "$ADOPT_CONFIRMED" Running 120 || { warn "never reached Running"; operator_logs 40; }
check "status.adopted" "true" "$(jp cloudcluster "$ADOPT_CONFIRMED" .status.adopted)"
check "managementPolicy is still Observe" Observe \
  "$(jp cloudcluster "$ADOPT_CONFIRMED" .status.managementPolicy)"

ADOPT_CONFIRMED_UID="$(object_uid cloudcluster "$ADOPT_CONFIRMED")"
k delete cloudcluster "$ADOPT_CONFIRMED" --wait=false >/dev/null
wait_object_gone cloudcluster "$ADOPT_CONFIRMED" 90 || operator_logs 40
ADOPT_CONFIRMED_EVENT_LINE="$(wait_for_event "$ADOPT_CONFIRMED_UID" \
  "deletionPolicy is Terminate: terminating RavenDB Cloud cluster $ADOPT_CONFIRMED_ID" 60 || true)"
info "the record left behind for a cluster this operator never created:"
printf '%s\n' "$ADOPT_CONFIRMED_EVENT_LINE" | sed 's/^/      /'
check_contains "the termination event" \
  "deletionPolicy is Terminate: terminating RavenDB Cloud cluster $ADOPT_CONFIRMED_ID" \
  "$ADOPT_CONFIRMED_EVENT_LINE"
wait_cloud_status "$ADOPT_CONFIRMED_ID" Terminated 90 || warn "it never reached Terminated"
check "the found cloud cluster after the delete" Terminated "$(cloud_state "$ADOPT_CONFIRMED_ID")"
warn "This object resolved managementPolicy to Observe, so it would not have reshaped"
warn "anything. deletionPolicy reaches into RavenDB Cloud whatever that policy says,"
warn "and with creationMode TakeOverOnly it did."

# --- Part 5: deleted behind the operator's back ------------------------------

step "Part 5: a cluster terminated outside the operator"
# The portal, another key, a colleague's script: a cluster can go without this
# object deciding anything. RavenDB Cloud keeps the record and reports it
# Terminated, and the stand-in's /_lab surface can do exactly that to one.
GONE_NAME="$(lab_display gone)"
GONE_ID="$(seed_cloud_cluster "$GONE_NAME")"
check_not_empty "seeded cluster for $GONE" "$GONE_ID"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $GONE
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
spec:
  accountRef:
    name: ravendb-cloud
  displayName: $GONE_NAME
  tier: Production
  managementPolicy: Full
  pollIntervalSeconds: 5   # lab only, so the next look is seconds away
EOF
wait_for_status "$GONE" Running 120 || { warn "never reached Running"; operator_logs 40; }
GONE_UID="$(object_uid cloudcluster "$GONE")"

lab_api POST "/_lab/clusters/$GONE_ID/patch" '{"status":"Terminated"}' >/dev/null
info "the control plane now reports $GONE_ID Terminated, and nothing in Kubernetes asked for it"
wait_for_status "$GONE" DeletedInCloud 60 || { warn "it never noticed"; operator_logs 40; }
k get cloudcluster "$GONE" || true
GONE_MSG="$(condition_message cloudcluster "$GONE" Ready)"
info "message: $GONE_MSG"
check "STATUS" DeletedInCloud "$(status_of "$GONE")"
check_contains "it says what happened" "RavenDB Cloud cluster $GONE_ID was terminated outside this operator" "$GONE_MSG"
check_contains "what to do" "Delete this object" "$GONE_MSG"
check_contains "and why applying it again is not enough" \
  "after hiding the terminated one in the RavenDB Cloud portal, since it keeps the name \"$GONE_NAME\" until then" "$GONE_MSG"
GONE_EVENT="$(wait_for_event "$GONE_UID" "was terminated outside this operator" 30 || true)"
check_contains "a Warning marks it" "Warning" "$GONE_EVENT"
check_contains "under the reason STATUS shows" "DeletedInCloud" "$GONE_EVENT"
# managementPolicy Full may create a cluster, and does not create this one: a
# cluster that went away is reported, never quietly replaced, because a second
# cluster is a second bill and an empty database where the old one was. Two
# more polls, so the claim is about the passes after the one that noticed.
POLLS_SEEN=0; LAST_SYNC="$(jp cloudcluster "$GONE" .status.lastSyncTime)"; DEADLINE=$((SECONDS + 60))
while [ "$POLLS_SEEN" -lt 2 ] && [ "$SECONDS" -lt "$DEADLINE" ]; do
  sleep 2
  NOW_SYNC="$(jp cloudcluster "$GONE" .status.lastSyncTime)"
  if [ -n "$NOW_SYNC" ] && [ "$NOW_SYNC" != "$LAST_SYNC" ]; then POLLS_SEEN=$((POLLS_SEEN + 1)); LAST_SYNC="$NOW_SYNC"; fi
done
check "the operator kept looking" 2 "$POLLS_SEEN"
check "STATUS, a poll or two later" DeletedInCloud "$(status_of "$GONE")"
check "still bound to the cluster that went" "$GONE_ID" "$(jp cloudcluster "$GONE" .status.productID)"
check "clusters named $GONE_NAME, a poll or two later" 1 \
  "$(cloud_products | awk -v want="$GONE_NAME" '$2 == want { n++ } END { print n + 0 }')"

k delete cloudcluster "$GONE" --wait=false >/dev/null
wait_object_gone cloudcluster "$GONE" 60 || operator_logs 40
check "deleting it is all it takes" "gone" "$(object_state cloudcluster "$GONE")"

fi

step "What is left"
k get cloudcluster 2>/dev/null || true

lab_summary

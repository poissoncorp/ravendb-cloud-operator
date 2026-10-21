#!/usr/bin/env bash
# Lab 06: delete the object without deleting the database.
#
# What this shows:
#   * deletionPolicy Retain (the default): the CloudCluster object goes away,
#     the cloud cluster keeps running, and the connection Secret is collected
#     with its owner
#   * deletionPolicy Terminate: the cloud cluster is terminated, then hidden,
#     which frees its name, so the same manifest applied again gets a new one
#   * Terminate on a cluster the object found rather than created: it ends that
#     one too, and the takeover warning says so before anybody deletes
#   * a cluster terminated behind the operator's back, which is reported and
#     never quietly replaced
#   * deleting a CloudAccount: held while a CloudCluster names it, and a
#     teardown that deletes the key Secret, the account and a Terminate cluster
#     together still finishes
#   * appendix: a deletion that cannot finish, and what removing the finalizer
#     by hand costs
#
#   labs/06_delete_safely.sh
#
# Run from the repository root, after labs/00_setup.sh.
source "$(dirname "$0")/lib.sh"

RETAIN=lab06-retain
TERMINATE=lab06-term
ADOPT_RETAIN=lab06-adopt-retain
ADOPT_TERMINATE=lab06-adopt-terminate
GONE=lab06-gone
HOLDER=lab06-holder
SECOND=lab06-second
TOGETHER=lab06-both
NOACCOUNT=lab06-noaccount
NOCRED_RETAIN=lab06-nocred-retain
NOCRED_TERMINATE=lab06-nocred-terminate

ACCOUNT=lab06-account
KEY_SECRET=lab06-api-key
GUEST_ACCOUNT=lab06-guest-account
GUEST_SECRET=lab06-guest-key
MISSING_ACCOUNT=lab06-no-such-account

LAB06_CLUSTERS=("$RETAIN" "$TERMINATE" "$ADOPT_RETAIN" "$ADOPT_TERMINATE" "$GONE" "$HOLDER"
                "$SECOND" "$TOGETHER" "$NOACCOUNT" "$NOCRED_RETAIN" "$NOCRED_TERMINATE")

if on_cloud; then
  LAB06_ENDPOINT="$LAB_CLOUD_ENDPOINT"
  LAB06_GOOD_KEY="$(cat "$LAB_CLOUD_API_KEY_FILE")"
else
  LAB06_ENDPOINT="http://mockcloud.${LAB_CLOUD_NAMESPACE}.svc.cluster.local"
  LAB06_GOOD_KEY="lab-api-key"
fi

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

# cloud_state <product-id> -> the reported status, or "gone"
cloud_state() {
  local status
  status="$(product_status "$1")"
  if [ -n "$status" ]; then
    printf '%s\n' "$status"
  else
    printf 'gone\n'
  fi
}

# cloud_ids_named <display-name-prefix> -> ids, one per line
cloud_ids_named() { cloud_products | awk -v p="$1" 'index($2, p) == 1 { print $1 }'; }

# seed_cloud_cluster <display-name> -> the new product id
# Only the simulator can conjure a cluster Kubernetes never created. The parts
# of this lab built on that say so where they begin.
seed_cloud_cluster() {
  lab_api POST /_lab/clusters "{\"displayName\":\"$1\",\"count\":1}" \
    | sed -n 's/.*"ids":\["\([^"]*\)".*/\1/p' || true
}

# object_uid <kind> <name> -> the uid, which has to be read before the delete
# because that is the only handle on the events once the object is gone.
object_uid() {
  jp "$1" "$2" .metadata.uid
}

# events_for_uid <uid> -> TYPE REASON MESSAGE, one event per line.
#
# Matched on uid rather than on name: events outlive the object they describe,
# and the object names here are stable, so a name selector would let a
# previous run of this lab answer for this one.
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

force_release_finalizer() {
  kubectl -n "$LAB_NAMESPACE" patch cloudcluster "$1" \
    --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
}

# apply_key_secret <name> <value>
apply_key_secret() {
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Secret
metadata:
  name: $1
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
type: Opaque
stringData:
  apiKey: $2
EOF
}

# apply_account <name> <secret>
apply_account() {
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: $1
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
spec:
  apiKeySecretRef:
    name: $2
    key: apiKey
  endpoint: $LAB06_ENDPOINT
  defaults:
    provider: AWS
    region: eu-central-1
    allowedIPs:
      - $LAB_ALLOWED_IPS
EOF
}

# apply_terminate_cluster: the Part 2 manifest, applied twice.
apply_terminate_cluster() {
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
  kubectl -n "$LAB_NAMESPACE" delete cloudaccount "$GUEST_ACCOUNT" "$ACCOUNT" \
    --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
  kubectl -n "$LAB_NAMESPACE" delete secret "$GUEST_SECRET" "$KEY_SECRET" \
    --ignore-not-found >/dev/null 2>&1 || true

  # Whatever this lab left running goes too, including the cluster Part 1
  # retains on purpose and the one the appendix orphans.
  for id in $(cloud_ids_named "${LAB_ID}-"); do
    retire_product "$id"
  done
  retire_product "${TOGETHER_ID:-}"
}

lab06_cleanup() {
  step "Cleaning up"
  lab06_purge
  info "removed every lab06 object and every lab06 cluster it left running"
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

check "deletionPolicy defaulted by the CRD" Retain "$(jp cloudcluster "$RETAIN" .spec.deletionPolicy)"
RETAIN_ID="$(jp cloudcluster "$RETAIN" .status.productID)"
check_not_empty "the bound product id" "$RETAIN_ID"
check "the finalizer on the live object" "cloud.ravendb.net/finalizer" \
  "$(jp cloudcluster "$RETAIN" '.metadata.finalizers[0]')"
check "the connection Secret is owned by it" "$RETAIN" \
  "$(jp secret "${RETAIN}-connection" '.metadata.ownerReferences[0].name')"

step "Part 1: kubectl delete cloudcluster $RETAIN"
# The uid has to be read now. Once the object is gone it is the only way left
# to find the events it produced.
RETAIN_UID="$(object_uid cloudcluster "$RETAIN")"
k delete cloudcluster "$RETAIN" --wait=false >/dev/null
wait_object_gone cloudcluster "$RETAIN" 90 || operator_logs 40

check "the CloudCluster object" "gone" "$(object_state cloudcluster "$RETAIN")"
wait_object_gone secret "${RETAIN}-connection" 90 || true
check "the connection Secret, collected with its owner" "gone" "$(object_state secret "${RETAIN}-connection")"
check "the cloud cluster after the delete" Active "$(cloud_state "$RETAIN_ID")"

RETAIN_EVENT_LINE="$(wait_for_event "$RETAIN_UID" \
  "deletionPolicy is Retain: leaving RavenDB Cloud cluster $RETAIN_ID running" 60 || true)"
info "the event left behind by an object that no longer exists:"
printf '%s\n' "$RETAIN_EVENT_LINE" | sed 's/^/      /'
check_contains "the deletion event" "Normal" "$RETAIN_EVENT_LINE"
check_contains "the deletion event" "Retained" "$RETAIN_EVENT_LINE"

# --- Part 2: Terminate ------------------------------------------------------

step "Part 2: a cluster that asks to be terminated"
apply_terminate_cluster
info "applied $TERMINATE with deletionPolicy: Terminate written on purpose"

wait_for_status "$TERMINATE" Running "$LAB_SETTLE_TIMEOUT" || { warn "it never reached Running"; operator_logs 40; }
TERMINATE_ID="$(jp cloudcluster "$TERMINATE" .status.productID)"
check_not_empty "the bound product id" "$TERMINATE_ID"
TERMINATE_NAME="$(jp cloudcluster "$TERMINATE" .status.displayName)"

step "Part 2: kubectl delete cloudcluster $TERMINATE"
TERMINATE_UID="$(object_uid cloudcluster "$TERMINATE")"
k delete cloudcluster "$TERMINATE" --wait=false >/dev/null
wait_object_gone cloudcluster "$TERMINATE" 90 || operator_logs 40
check "the CloudCluster object" "gone" "$(object_state cloudcluster "$TERMINATE")"

TERMINATE_EVENT_LINE="$(wait_for_event "$TERMINATE_UID" \
  "deletionPolicy is Terminate: terminating RavenDB Cloud cluster $TERMINATE_ID" 60 || true)"
info "the record left behind in the namespace:"
printf '%s\n' "$TERMINATE_EVENT_LINE" | sed 's/^/      /'
check_contains "the termination event" "Terminating" "$TERMINATE_EVENT_LINE"
check "RavenDB Cloud is terminating it" yes "$(cloud_ended "$TERMINATE_ID")"

# The operator asks for the cluster to be hidden in the portal once it is
# terminated. A terminated cluster keeps its display name until then, and a
# hidden one gives it up, so the same manifest can be applied again.
wait_cloud_unlisted "$TERMINATE_ID" 90 || warn "it is still listed"
check "terminated and hidden, so it left the product list" no "$(cloud_listed "$TERMINATE_ID")"
on_cloud && check "RavenDB Cloud still answers for it by id" Terminated "$(product_status "$TERMINATE_ID")"

if costly "applying the same manifest again, which creates a second Development cluster"; then
step "Part 2: the same manifest, applied again"
apply_terminate_cluster
wait_for_status "$TERMINATE" Running "$LAB_SETTLE_TIMEOUT" || { warn "it never reached Running"; operator_logs 40; }
AGAIN_ID="$(jp cloudcluster "$TERMINATE" .status.productID)"
k get cloudcluster "$TERMINATE" || true
check "a new cluster under the same name" "$TERMINATE_NAME" "$(jp cloudcluster "$TERMINATE" .status.displayName)"
check "with a new id" yes "$([ -n "$AGAIN_ID" ] && [ "$AGAIN_ID" != "$TERMINATE_ID" ] && echo yes || echo no)"
check "created, not taken over" "" "$(jp cloudcluster "$TERMINATE" .status.adopted)"
k delete cloudcluster "$TERMINATE" --timeout=300s >/dev/null || true
check "and Terminate ends that one too" yes "$(cloud_ended "$AGAIN_ID")"
fi

# --- Part 3: a cluster the object found -------------------------------------

if needs_mock "parts 3 and 4 and the appendix, which seed clusters Kubernetes never created"; then
step "Part 3: deleting a cluster the object found, with the default policy"

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
check "deletionPolicy defaulted, found or created" Retain \
  "$(jp cloudcluster "$ADOPT_RETAIN" .spec.deletionPolicy)"

ADOPT_RETAIN_UID="$(object_uid cloudcluster "$ADOPT_RETAIN")"
k delete cloudcluster "$ADOPT_RETAIN" --wait=false >/dev/null
wait_object_gone cloudcluster "$ADOPT_RETAIN" 90 || operator_logs 40
check "the CloudCluster object" "gone" "$(object_state cloudcluster "$ADOPT_RETAIN")"
check "the cloud cluster it found" Active "$(cloud_state "$ADOPT_RETAIN_ID")"
check_contains "the deletion event" \
  "deletionPolicy is Retain: leaving RavenDB Cloud cluster $ADOPT_RETAIN_ID running" \
  "$(wait_for_event "$ADOPT_RETAIN_UID" "deletionPolicy is Retain" 60 || true)"

step "Part 3: Terminate on a cluster the object found"

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
ADOPT_TERMINATE_UID="$(object_uid cloudcluster "$ADOPT_TERMINATE")"
check "status.adopted" "true" "$(jp cloudcluster "$ADOPT_TERMINATE" .status.adopted)"
check "creationMode defaulted" TakeOverOrCreate "$(jp cloudcluster "$ADOPT_TERMINATE" .spec.creationMode)"
ADOPTED_LINE="$(wait_for_event "$ADOPT_TERMINATE_UID" "already running and took it over" 60 || true)"
info "the takeover warning, before anybody deletes anything:"
printf '%s\n' "$ADOPTED_LINE" | sed 's/^/      /'
check_contains "it warns that deleting would terminate" \
  "set spec.deletionPolicy to Retain before deleting this object" "$ADOPTED_LINE"

k delete cloudcluster "$ADOPT_TERMINATE" --wait=false >/dev/null
wait_object_gone cloudcluster "$ADOPT_TERMINATE" 90 || operator_logs 40
check_contains "the termination event" \
  "deletionPolicy is Terminate: terminating RavenDB Cloud cluster $ADOPT_TERMINATE_ID" \
  "$(wait_for_event "$ADOPT_TERMINATE_UID" "deletionPolicy is Terminate: terminating" 60 || true)"
check "the cluster it found is terminating" yes "$(cloud_ended "$ADOPT_TERMINATE_ID")"
note "found or created makes no difference to Terminate: only status records which,"
note "and a restore from git or a backup does not bring status back"

# --- Part 4: deleted behind the operator's back ------------------------------

step "Part 4: a cluster terminated outside the operator"
# The portal, another key, a colleague's script: a cluster can go without this
# object deciding anything. RavenDB Cloud keeps the record and reports it
# Terminated, and the simulator's lab surface can do exactly that to one.
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
info "the simulator now reports $GONE_ID Terminated, and nothing in Kubernetes asked for it"
wait_for_status "$GONE" DeletedInCloud 60 || { warn "it never noticed"; operator_logs 40; }
k get cloudcluster "$GONE" || true
GONE_MSG="$(condition_message cloudcluster "$GONE" Ready)"
info "message: $GONE_MSG"
check "STATUS" DeletedInCloud "$(status_of "$GONE")"
check_contains "it says what happened" "RavenDB Cloud cluster $GONE_ID was terminated outside this operator" "$GONE_MSG"
check_contains "and what to do" "Delete this object" "$GONE_MSG"
check_contains "a Warning marks it" "Warning" \
  "$(wait_for_event "$GONE_UID" "was terminated outside this operator" 30 || true)"
# managementPolicy Full may create a cluster, and does not create this one: a
# cluster that went away is reported, never quietly replaced. Two more polls,
# so the claim is about the passes after the one that noticed.
POLLS_SEEN=0; LAST_SYNC="$(jp cloudcluster "$GONE" .status.lastSyncTime)"; DEADLINE=$((SECONDS + 60))
while [ "$POLLS_SEEN" -lt 2 ] && [ "$SECONDS" -lt "$DEADLINE" ]; do
  sleep 2
  NOW_SYNC="$(jp cloudcluster "$GONE" .status.lastSyncTime)"
  if [ -n "$NOW_SYNC" ] && [ "$NOW_SYNC" != "$LAST_SYNC" ]; then POLLS_SEEN=$((POLLS_SEEN + 1)); LAST_SYNC="$NOW_SYNC"; fi
done
check "the operator kept looking" 2 "$POLLS_SEEN"
check "nothing was created in its place" 1 \
  "$(cloud_products | awk -v want="$GONE_NAME" '$2 == want { n++ } END { print n + 0 }')"

k delete cloudcluster "$GONE" --wait=false >/dev/null
wait_object_gone cloudcluster "$GONE" 60 || operator_logs 40
check "deleting it is all it takes" "gone" "$(object_state cloudcluster "$GONE")"
fi

# --- Part 5: deleting an account --------------------------------------------

step "Part 5: an account nothing depends on deletes at once"
if on_cloud; then
  kubectl -n "$LAB_NAMESPACE" create secret generic "$KEY_SECRET" \
    --from-file=apiKey="$LAB_CLOUD_API_KEY_FILE" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
else
  apply_key_secret "$KEY_SECRET" "$LAB06_GOOD_KEY"
fi
apply_account "$ACCOUNT" "$KEY_SECRET"
wait_for cloudaccount "$ACCOUNT" '.status.conditions[?(@.type=="Ready")].status' True 120 ||
  { warn "$ACCOUNT never verified"; operator_logs 30; }
check "the finalizer it took at once" "cloud.ravendb.net/finalizer" \
  "$(jp cloudaccount "$ACCOUNT" '.metadata.finalizers[0]')"
k delete cloudaccount "$ACCOUNT" --timeout=60s >/dev/null 2>&1 || true
check "an unused account deletes immediately" no "$(exists cloudaccount "$ACCOUNT")"

step "Part 5: the same delete, with clusters that name the account"
apply_account "$ACCOUNT" "$KEY_SECRET"
wait_for cloudaccount "$ACCOUNT" '.status.conditions[?(@.type=="Ready")].status' True 120 >/dev/null || true
# Under Observe, so nothing is created anywhere: the reference alone holds the
# account, bound or not.
for name in "$HOLDER" "$SECOND"; do
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $name
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
spec:
  accountRef:
    name: $ACCOUNT
  managementPolicy: Observe
  tier: Development
EOF
done
wait_for_status "$HOLDER" ClusterNotFound 90 || { warn "$HOLDER never settled"; operator_logs 30; }

k delete cloudaccount "$ACCOUNT" --wait=false >/dev/null 2>&1 || true
info "kubectl delete cloudaccount $ACCOUNT: accepted, and the object stays"
wait_for cloudaccount "$ACCOUNT" '.status.conditions[?(@.type=="Ready")].reason' InUse 90 ||
  { warn "$ACCOUNT never reported InUse"; operator_logs 30; }
kubectl -n "$LAB_NAMESPACE" get cloudaccount "$ACCOUNT" || true
INUSE_MSG="$(condition_message cloudaccount "$ACCOUNT" Ready)"
info "message: $INUSE_MSG"
check "the account is still here" yes "$(exists cloudaccount "$ACCOUNT")"
check "STATUS" InUse "$(condition_reason cloudaccount "$ACCOUNT" Ready)"
check "Ready stays True, since the key still works" True "$(condition cloudaccount "$ACCOUNT" Ready)"
check_contains "the message counts them" "2 CloudCluster" "$INUSE_MSG"
check_contains "and names them" "$HOLDER, $SECOND" "$INUSE_MSG"
check_contains "and says what to do" "Delete them first" "$INUSE_MSG"

k delete cloudcluster "$SECOND" --ignore-not-found --timeout=120s >/dev/null || true
wait_for cloudaccount "$ACCOUNT" '.status.conditions[?(@.type=="Ready")].message' \
  "1 CloudCluster(s) still reference this account ($HOLDER). Delete them first: without this credential they cannot reconcile, and one with deletionPolicy Terminate could not terminate its cluster either." 60 ||
  warn "the count did not follow the delete"
check_contains "the count follows each delete" "1 CloudCluster" "$(condition_message cloudaccount "$ACCOUNT" Ready)"
k delete cloudcluster "$HOLDER" --ignore-not-found --timeout=120s >/dev/null || true
wait_gone cloudaccount "$ACCOUNT" 120 || { warn "$ACCOUNT did not release its finalizer"; operator_logs 30; }
check "the account goes with its last user" no "$(exists cloudaccount "$ACCOUNT")"

if costly "deleting a key Secret, an account and a cluster it created in one go, which needs a cluster to terminate"; then
step "Part 5: a teardown that deletes the key, the account and a Terminate cluster at once"
apply_account "$ACCOUNT" "$KEY_SECRET"
wait_for cloudaccount "$ACCOUNT" '.status.conditions[?(@.type=="Ready")].status' True 120 >/dev/null || true
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $TOGETHER
  namespace: $LAB_NAMESPACE
  labels:
    lab: lab06
spec:
  accountRef:
    name: $ACCOUNT
  displayName: $(lab_display t)
  deletionPolicy: Terminate
  tier: Development
  storage:
    sizeGB: 20
$(if on_cloud; then printf '  subdomain: %s\n' "$(lab_subdomain t)"; fi)
EOF
wait_for_status "$TOGETHER" Running "$LAB_SETTLE_TIMEOUT" || { warn "$TOGETHER never reached Running"; operator_logs 30; }
TOGETHER_ID="$(jp cloudcluster "$TOGETHER" .status.productID)"
check_not_empty "$TOGETHER is bound to a cluster" "$TOGETHER_ID"

info "deleting the Secret first, then the account and the cluster, as a namespace delete may"
k delete secret "$KEY_SECRET" --wait=false >/dev/null 2>&1 || true
k delete cloudaccount "$ACCOUNT" --wait=false >/dev/null 2>&1 || true
k delete cloudcluster "$TOGETHER" --wait=false >/dev/null 2>&1 || true

if ! wait_gone cloudcluster "$TOGETHER" "$LAB_CHANGE_TIMEOUT"; then
  warn "$TOGETHER never finished deleting; what it says:"
  info "Ready: $(condition_reason cloudcluster "$TOGETHER" Ready): $(condition_message cloudcluster "$TOGETHER" Ready)"
fi
check "the cluster finished deleting" no "$(exists cloudcluster "$TOGETHER")"
wait_gone cloudaccount "$ACCOUNT" 120 || warn "$ACCOUNT is still waiting"
check "and so did the account" no "$(exists cloudaccount "$ACCOUNT")"
check "RavenDB Cloud is terminating the cluster" yes "$(cloud_ended "$TOGETHER_ID")"
note "the key Secret went first, and the operator terminated with the key it already held"
on_mock && retire_product "$TOGETHER_ID"
TOGETHER_ID=''
fi

# --- Appendix: a deletion that cannot finish ---------------------------------

if on_mock; then
step "Appendix: nothing to act on"
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
wait_for_status "$NOACCOUNT" WaitingForAccount 90 || { warn "it never reported WaitingForAccount"; operator_logs 20; }
check "the finalizer, before the account resolves" "cloud.ravendb.net/finalizer" \
  "$(jp cloudcluster "$NOACCOUNT" '.metadata.finalizers[0]')"
k delete cloudcluster "$NOACCOUNT" --wait=false >/dev/null
wait_object_gone cloudcluster "$NOACCOUNT" 30 || warn "still there"
check "Terminate with nothing created deletes at once" "gone" "$(object_state cloudcluster "$NOACCOUNT")"

step "Appendix: a key RavenDB Cloud no longer accepts"
apply_key_secret "$GUEST_SECRET" "$LAB06_GOOD_KEY"
apply_account "$GUEST_ACCOUNT" "$GUEST_SECRET"
wait_for cloudaccount "$GUEST_ACCOUNT" '.status.conditions[?(@.type=="Ready")].status' True 90 ||
  warn "the second account did not verify"

NOCRED_RETAIN_ID="$(seed_cloud_cluster "$(lab_display legacy-a)")"
NOCRED_TERMINATE_ID="$(seed_cloud_cluster "$(lab_display legacy-b)")"
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
  deletionPolicy: Terminate
EOF
wait_for_status "$NOCRED_RETAIN" Running 120 || warn "$NOCRED_RETAIN never reached Running"
wait_for_status "$NOCRED_TERMINATE" Running 120 || warn "$NOCRED_TERMINATE never reached Running"

info "putting a revoked key in the account's Secret"
apply_key_secret "$GUEST_SECRET" "revoked-0d1e"
wait_for cloudaccount "$GUEST_ACCOUNT" '.status.conditions[?(@.type=="Ready")].reason' KeyRejected 120 ||
  warn "the account did not report KeyRejected"
check "$GUEST_ACCOUNT STATUS" KeyRejected "$(condition_reason cloudaccount "$GUEST_ACCOUNT" Ready)"

NOCRED_RETAIN_UID="$(object_uid cloudcluster "$NOCRED_RETAIN")"
k delete cloudcluster "$NOCRED_RETAIN" --wait=false >/dev/null
wait_object_gone cloudcluster "$NOCRED_RETAIN" 90 || operator_logs 40
check "Retain deletes whatever the key" "gone" "$(object_state cloudcluster "$NOCRED_RETAIN")"
check "its cloud cluster is untouched" Active "$(cloud_state "$NOCRED_RETAIN_ID")"
check_contains "and the event still names the cluster it left running" \
  "leaving RavenDB Cloud cluster $NOCRED_RETAIN_ID running" \
  "$(wait_for_event "$NOCRED_RETAIN_UID" "deletionPolicy is Retain" 60 || true)"

NOCRED_TERMINATE_UID="$(object_uid cloudcluster "$NOCRED_TERMINATE")"
k delete cloudcluster "$NOCRED_TERMINATE" --wait=false >/dev/null
wait_for_status "$NOCRED_TERMINATE" Deleting 120 || warn "expected the deletion to stop, and say why"
k get cloudcluster "$NOCRED_TERMINATE" || true
STUCK_MSG="$(condition_message cloudcluster "$NOCRED_TERMINATE" Ready)"
info "message: $STUCK_MSG"
check "the object is still here" "present" "$(object_state cloudcluster "$NOCRED_TERMINATE")"
check "held by the finalizer" "cloud.ravendb.net/finalizer" \
  "$(jp cloudcluster "$NOCRED_TERMINATE" '.metadata.finalizers[0]')"
check_contains "the message says what failed" "Could not terminate RavenDB Cloud cluster $NOCRED_TERMINATE_ID" "$STUCK_MSG"
check_contains "and that the object waits for it" "The object stays until this succeeds" "$STUCK_MSG"
check_contains "a Warning marks that it is stuck" "Warning" \
  "$(wait_for_event "$NOCRED_TERMINATE_UID" "Could not terminate" 60 || true)"
check "the cloud cluster is still running" Active "$(cloud_state "$NOCRED_TERMINATE_ID")"
note "the fix is a key that works, in the Secret; the next pass terminates."
note "removing the finalizer instead is the wrong fix, and this is what it costs:"

step "Appendix: removing the finalizer by hand"
info "kubectl -n $LAB_NAMESPACE patch cloudcluster $NOCRED_TERMINATE \\"
info "  --type=merge -p '{\"metadata\":{\"finalizers\":null}}'"
force_release_finalizer "$NOCRED_TERMINATE"
wait_object_gone cloudcluster "$NOCRED_TERMINATE" 60 || warn "still there"
check "the object after the forced release" "gone" "$(object_state cloudcluster "$NOCRED_TERMINATE")"
check "the cloud cluster it was told to terminate" Active "$(cloud_state "$NOCRED_TERMINATE_ID")"
warn "that cluster is orphaned: still billed, and nothing in Kubernetes names it."
warn "Its id was $NOCRED_TERMINATE_ID; a manifest with its name takes it over again."
fi

step "What is left"
k get cloudcluster 2>/dev/null || true

lab_summary

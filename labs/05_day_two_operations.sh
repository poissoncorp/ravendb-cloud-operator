#!/usr/bin/env bash
# Lab 05: resize, rescale and reshape a running RavenDB Cloud cluster.
#
# Covers the only three day-2 changes the RavenDB Cloud API offers on a live
# cluster: instance type, storage size, node count. It shows that the operator
# applies at most one shape change per reconcile pass, that a second storage
# change inside RavenDB Cloud's six hour window waits rather than fails, that
# node scaling moves one node at a time and removes the highest node tag, that
# shrinking a disk is reported as drift and never attempted, and that a disk
# size outside the instance type's menu is refused by the operator before it
# costs a request.
#
#   labs/05_day_two_operations.sh
#
# Run from the repository root, after labs/00_setup.sh.
source "$(dirname "$0")/lib.sh"

CLUSTER=lab05-prod
BAD=lab05-offmenu

# The shape every step below operates on. The lessons are the same on any
# shape: one change per pass, a grow, an instance type change and a grow in one
# apply, a shrink refused, an off-menu size refused, a restart. Only node scaling
# needs Production, because every Development shape is exactly one node.
#
# Against the simulator, and against a real account with LAB_BUDGET=full, that is
# a three node Production cluster. Against a real account by default it is the
# smallest Development shape pair whose menus let every step happen: Dev20 sells
# {10, 20, 30, 40, 50}, Dev30 sells {10, 20, 30, 50, 75}, so 30 is on both and
# the instance type can change before the disk does without stranding it on a
# size the new type does not sell. Read from the real service's own menus.
if on_mock || [ "$LAB_BUDGET" = full ]; then
  TIER=Production;  SMALL=PB10;  BIG=PB20;  NODES=3
  DISK1=20; DISK3=40; DISK4=80; OFFMENU=100; SMALL_MENU="10, 20, 30, 50, 75"
  BASE_TAGS="A B C"
  on_cloud && warn "LAB_BUDGET=full: three Production nodes, scaling to five, billed"
else
  TIER=Development; SMALL=Dev20; BIG=Dev30; NODES=1
  DISK1=20; DISK3=50; DISK4=75; OFFMENU=25;  SMALL_MENU="10, 20, 30, 40, 50"
  BASE_TAGS="A"
fi
scales() { [ "$TIER" = Production ]; }

# RavenDB Cloud allows one storage change per cluster every 6h05m on AWS and
# 12h05m on Azure SsdPremium, counted from when the previous change was made
# (verified against RavenDB Cloud's source), and the simulator enforces the
# same window. So the lab makes one storage change, in the combined change that
# carries its main claim, and then shows what a second one inside the window
# does. An instance type change does not count against the limit.
#
# The combined change starts from DISK1, which also keeps a Production run's
# intermediate size on both PB10's and PB20's menus, since 20 is the only size
# they share. DISK4 is on BIG's menu, for the second change.
# Every poll is a live request on a real environment, so poll gently there.
POLL=3; on_cloud && POLL=15
WAIT="$LAB_CHANGE_TIMEOUT"

# --- helpers ---------------------------------------------------------------

# apply_cluster <instanceType> <nodes> <sizeGB>
apply_cluster() {
  local instance="$1" nodes="$2" size="$3"
  cat <<EOF | k apply -f - | sed 's/^/    /'
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $CLUSTER
  namespace: $LAB_NAMESPACE
spec:
  accountRef:
    name: ravendb-cloud
  # Poll faster than the 300 second default so this lab does not sit
  # waiting. A shorter interval also applies while a change is in progress,
  # so this sets how fast each of the applies below converges.
  pollIntervalSeconds: $POLL
  tier: $TIER
  instanceType: $instance
  nodes: $nodes
  storage:
    sizeGB: $size
  networking:
    allowedIPs:
      - $LAB_ALLOWED_IPS
$(cloud_names m)
EOF
}

show_shape() {
  k get cloudcluster "$CLUSTER" -o custom-columns='NAME:.metadata.name,STATUS:.status.conditions[?(@.type=="Ready")].reason,CHANGES:.status.conditions[?(@.type=="Synced")].reason,INSTANCE:.status.instanceType,NODES:.status.nodes,DISK-GIB:.status.hardware.storage.sizeGB,TAGS:.status.nodeTags' |
    sed 's/^/    /'
}

show_conditions() {
  k get cloudcluster "$CLUSTER" \
    -o jsonpath='{range .status.conditions[*]}{"    "}{.type}{"="}{.status}{" ("}{.reason}{")"}{"\n"}{end}' 2>/dev/null || true
}

# shape_snapshot reads instance type, disk size, STATUS and CHANGES in one
# call, so all four describe the same moment.
shape_snapshot() {
  k get cloudcluster "$CLUSTER" \
    -o jsonpath='{.status.instanceType}/{.status.hardware.storage.sizeGB}/{.status.conditions[?(@.type=="Ready")].reason}/{.status.conditions[?(@.type=="Synced")].reason}' 2>/dev/null || true
}

# lab_cluster_ids <displayName> -> ids of matching simulated clusters.
# "id" is the first field of a product details object and "displayName" the
# third, and neither key occurs in any nested object, so pairing them in order
# is safe without a JSON parser.
lab_cluster_ids() {
  local want="$1" json
  json="$(lab_api GET /_lab/clusters || true)"
  printf '%s' "$json" |
    grep -oE '"(id|displayName)":"[^"]*"' |
    sed -e 's/^"id":"/id /' -e 's/^"displayName":"/name /' -e 's/"$//' |
    awk -v want="$want" '$1=="id"{id=$2} $1=="name" && $2==want {print id}' ||
    true
}

# count_calls <path substring> -> how many logged API calls match it.
count_calls() {
  local json n
  on_mock || { printf '0'; return 0; }
  json="$(lab_api GET "/_lab/requests?path=$1" || true)"
  # grep -o, not grep -c: the whole log arrives on one line.
  n="$(printf '%s' "$json" | grep -o '"method"' | wc -l || true)"
  printf '%s' "${n:-0}" | tr -d '[:space:]'
}

# shape_calls -> the reshaping endpoints the control plane was asked for, in
# the order it received them.
shape_calls() {
  local json
  on_mock || return 0
  json="$(lab_api GET '/_lab/requests?path=/api/v1/products/' || true)"
  printf '%s' "$json" |
    grep -oE '"path":"[^"]*"' |
    sed -e 's/^"path":"//' -e 's/"$//' |
    grep -E 'instance-type|/products/storage|additional-node' |
    sed -e 's#^/api/v1/products/##' |
    tr '\n' ' ' |
    sed -e 's/ *$//' ||
    true
}

cluster_events() {
  k get events --field-selector "involvedObject.name=$CLUSTER" \
    -o jsonpath='{range .items[*]}{.reason}: {.message}{"\n"}{end}' 2>/dev/null || true
}

# wait_for_next_poll <previous lastSyncTime> blocks until the operator has read
# the cloud side at least once more. Used to prove a call did NOT happen.
wait_for_next_poll() {
  local before="$1" deadline=$((SECONDS + 120)) now
  while [ "$SECONDS" -lt "$deadline" ]; do
    now="$(jp cloudcluster "$CLUSTER" .status.lastSyncTime)"
    if [ -n "$now" ] && [ "$now" != "$before" ]; then
      return 0
    fi
    sleep 2
  done
  return 1
}

remove_lab_objects() {
  local id
  k delete cloudcluster "$CLUSTER" "$BAD" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
  k delete secret "$CLUSTER-connection" "$BAD-connection" --ignore-not-found >/dev/null 2>&1 || true
  # Events outlive the object they describe and the field selector matches on
  # name, not UID. Without this a rerun could assert an event that a previous
  # run emitted, which would turn the audit-trail section into a tautology.
  k delete events --field-selector "involvedObject.name=$CLUSTER" >/dev/null 2>&1 || true
  k delete events --field-selector "involvedObject.name=$BAD" >/dev/null 2>&1 || true
  k delete cloudclusterrestart lab05-restart lab05-restart-nowhere --ignore-not-found >/dev/null 2>&1 || true
  k delete events --field-selector "involvedObject.name=lab05-restart" >/dev/null 2>&1 || true
  if on_mock; then
    for id in $(lab_cluster_ids "$CLUSTER"); do
      lab_api POST "/_lab/clusters/$id/vanish" >/dev/null 2>&1 || true
    done
    for id in $(lab_cluster_ids "$BAD"); do
      lab_api POST "/_lab/clusters/$id/vanish" >/dev/null 2>&1 || true
    done
  else
    # Retain leaves the real cluster running once the object is gone, and an
    # interrupted run reaches this through the EXIT trap, so terminate it here.
    retire_product "${PRODUCT_ID:-}"
  fi
}

# --- start -----------------------------------------------------------------

require_ready_account
on_mock && open_cloud_portforward

# open_cloud_portforward installs its own EXIT trap. Replace it with one that
# tidies up first, so an interrupted run leaves nothing behind either.
lab05_cleanup() {
  remove_lab_objects
  close_cloud_portforward
}
trap lab05_cleanup EXIT

step "Clearing anything a previous run left behind"
remove_lab_objects
note "any lab05-* CloudCluster and its simulated cloud cluster are gone"

# --- 1. a production cluster to operate on ----------------------------------

step "Provisioning a $TIER cluster: $SMALL, $NODES node(s), $DISK1 GB"
info "$DISK1 is on $SMALL's disk menu {$SMALL_MENU}"
apply_cluster "$SMALL" "$NODES" "$DISK1"

wait_for_status "$CLUSTER" Running "$LAB_SETTLE_TIMEOUT" || fail "the cluster never reached Running"
show_shape

PRODUCT_ID="$(jp cloudcluster "$CLUSTER" .status.productID)"
check_not_empty "status.productID"      "$PRODUCT_ID"
check "STATUS"                Running   "$(status_of "$CLUSTER")"
check "observed instanceType" "$SMALL"     "$(jp cloudcluster "$CLUSTER" .status.instanceType)"
check "observed nodes"        "$NODES"     "$(jp cloudcluster "$CLUSTER" .status.nodes)"
check "observed disk (GB)"   "$DISK1"     "$(jp cloudcluster "$CLUSTER" .status.hardware.storage.sizeGB)"
check "observed node tags"    "$BASE_TAGS" "$(jp cloudcluster "$CLUSTER" '.status.nodeTags[*]')"
check "Synced"                True      "$(condition cloudcluster "$CLUSTER" Synced)"
check "CHANGES"               Applied   "$(changes_of "$CLUSTER")"

# --- 2. two shape changes in a single apply ---------------------------------

step "Changing instanceType AND storage in one apply"
info "$SMALL -> $BIG and $DISK1 -> $DISK3 GB, submitted together"
on_mock && lab_api POST /_lab/requests/reset >/dev/null
apply_cluster "$BIG" "$NODES" "$DISK3"

# reconcileDrift checks the instance type first, applies it and returns. The
# disk is still at its old size in the status it writes on the way out, which is
# the whole point: the two changes are serialised, not stacked.
wait_for cloudcluster "$CLUSTER" .status.instanceType "$BIG" "$WAIT" || fail "the instance type never changed"
MIDPOINT="$(shape_snapshot)"
info "after the first change landed, instanceType/disk/STATUS/CHANGES = $MIDPOINT"
# Updating on the pass a change goes out, and Applying until the manifest is
# carried through: the second change is the one this snapshot catches going out.
check "one change per pass: instance type done, disk not yet, both columns saying so" \
  "$BIG/$DISK1/Updating/Applying" "$MIDPOINT"

wait_for cloudcluster "$CLUSTER" .status.hardware.storage.sizeGB "$DISK3" "$WAIT" || fail "the disk never reached $DISK3 GB"
wait_for_status "$CLUSTER" Running "$WAIT" || fail "the cluster never returned to Running"
show_shape

ORDER="$(shape_calls)"
info "reshaping calls the control plane received: $ORDER"
check_calls "the two changes arrived as separate, sequential calls" "instance-type storage" "$ORDER"
check_calls "instance-type calls" 1 "$(count_calls /api/v1/products/instance-type)"
check_calls "storage calls"       1 "$(count_calls /api/v1/products/storage)"
check "final instanceType"  "$BIG"   "$(jp cloudcluster "$CLUSTER" .status.instanceType)"
check "final disk (GB)"    "$DISK3" "$(jp cloudcluster "$CLUSTER" .status.hardware.storage.sizeGB)"

# --- 3. a second storage change inside the window ---------------------------

step "A second storage change, minutes after the first: $DISK3 -> $DISK4 GB"
info "RavenDB Cloud allows one storage change every six hours on AWS, and the"
info "combined change above was this cluster's"
apply_cluster "$BIG" "$NODES" "$DISK4"

if ! wait_for_changes "$CLUSTER" Waiting "$WAIT"; then
  warn "CHANGES never said the change is waiting; recent operator logs:"
  operator_logs 30
fi
show_conditions
DEFERRED="$(condition_message cloudcluster "$CLUSTER" Synced)"
info "Synced message: $DEFERRED"
check "Synced"                     False  "$(condition cloudcluster "$CLUSTER" Synced)"
check "CHANGES"                    Waiting "$(changes_of "$CLUSTER")"
check_contains "it names the change"   "resizing storage from $DISK3 GB to $DISK4 GB" "$DEFERRED"
check_contains "it quotes RavenDB Cloud" "maximum storage modification rate" "$DEFERRED"
check_contains "it says when"          "available in" "$DEFERRED"
check_contains "and what happens next" "asks again" "$DEFERRED"
check "STATUS while it waits"      Running "$(status_of "$CLUSTER")"
check "Ready while it waits"       True   "$(condition cloudcluster "$CLUSTER" Ready)"
check "disk untouched"             "$DISK3" "$(jp cloudcluster "$CLUSTER" .status.hardware.storage.sizeGB)"
check_contains "an event marks when the wait began" "Waiting: RavenDB Cloud will take a change later" "$(cluster_events)"
note "the cluster serves throughout, so STATUS stays Running, and CHANGES says the spec is"
note "not carried through yet and why, in RavenDB Cloud's own words. The event is Normal:"
note "nothing is wrong, and the operator carries the change through on its own."

info "putting sizeGB back to $DISK3, so the rest of the lab starts from a settled spec"
apply_cluster "$BIG" "$NODES" "$DISK3"
wait_for cloudcluster "$CLUSTER" '.status.conditions[?(@.type=="Synced")].status' True "$WAIT" ||
  fail "Synced never came back"
check "CHANGES restored" Applied "$(changes_of "$CLUSTER")"

# --- 4. scale out 3 -> 5 ----------------------------------------------------

if ! scales; then
  skip "scaling out 3 -> 5 and back (every Development shape is one node; LAB_BUDGET=full provisions Production)"
else
step "Scaling out 3 -> 5 nodes"
on_mock && lab_api POST /_lab/requests/reset >/dev/null
apply_cluster PB20 5 40

# Watch for the intermediate 4-node state, but accept 5 as an answer too.
# Waiting for exactly 4 would block until the timeout on any run where the
# second pass landed between two polls, and the deterministic proof that the
# operator added one node at a time is the call count below, not this.
MIDPOINT="$(wait_for_one_of cloudcluster "$CLUSTER" .status.nodes 180 4 5 || true)"
if [ "$MIDPOINT" = 4 ]; then
  info "intermediate shape: $(shape_snapshot)"
  pass "an intermediate 4-node state was observed"
else
  note "the 4-node state came and went between polls; the call count below is the real proof"
fi

wait_for cloudcluster "$CLUSTER" .status.nodes 5 300 || fail "the cluster never reached 5 nodes"
wait_for_status "$CLUSTER" Running 300 || fail "the cluster never returned to Running"
show_shape

check "node tags after scaling out" "A B C D E" "$(jp cloudcluster "$CLUSTER" '.status.nodeTags[*]')"
check "add-node calls for 3 -> 5"   2 "$(count_calls /api/v1/products/additional-node/add)"
check "nothing else was reshaped"   "additional-node/add additional-node/add" "$(shape_calls)"

# --- 5. scale in 5 -> 3 -----------------------------------------------------

step "Scaling in 5 -> 3 nodes"
info "the operator removes the highest node tag, so shrinking is deterministic"
on_mock && lab_api POST /_lab/requests/reset >/dev/null
apply_cluster PB20 3 40

wait_for cloudcluster "$CLUSTER" .status.nodes 3 300 || fail "the cluster never came back to 3 nodes"
wait_for_status "$CLUSTER" Running 300 || fail "the cluster never returned to Running"
show_shape

check "node tags after scaling in"    "A B C" "$(jp cloudcluster "$CLUSTER" '.status.nodeTags[*]')"
check "remove-node calls for 5 -> 3"  2 "$(count_calls /api/v1/products/additional-node/remove)"

EVENTS="$(cluster_events)"
check_contains "events" "Removing node E (5 -> 4)" "$EVENTS"
check_contains "events" "Removing node D (4 -> 3)" "$EVENTS"
fi

# --- 6. what the operator refuses: a smaller disk ---------------------------

step "Asking for a smaller disk: $DISK3 -> $DISK1 GB"
on_mock && lab_api POST /_lab/requests/reset >/dev/null
apply_cluster "$BIG" "$NODES" "$DISK1"

wait_for cloudcluster "$CLUSTER" '.status.conditions[?(@.type=="Synced")].status' False "$WAIT" ||
  fail "Synced never went False"
show_conditions

DRIFT="$(condition_message cloudcluster "$CLUSTER" Synced)"
info "Synced message: $DRIFT"
check "CHANGES" NotApplied "$(changes_of "$CLUSTER")"
check_contains "drift message" "cloud disks cannot shrink" "$DRIFT"
check_contains "drift message" "storage size $DISK1 GB is smaller than the current $DISK3 GB" "$DRIFT"

# Refusing a change is not an error state: the cluster keeps serving.
check "STATUS while it differs" Running "$(status_of "$CLUSTER")"
check "Ready while it differs" True "$(condition cloudcluster "$CLUSTER" Ready)"
check "disk untouched"      "$DISK3" "$(jp cloudcluster "$CLUSTER" .status.hardware.storage.sizeGB)"

# Sit through a whole extra poll, then prove no resize was attempted in it
# either. This is the claim worth proving: not "it failed", but "it never ran".
LAST_SYNC="$(jp cloudcluster "$CLUSTER" .status.lastSyncTime)"
wait_for_next_poll "$LAST_SYNC" ||
  fail "the operator did not poll again, so the next check would prove nothing"

# A count of zero only means something if the log was recording. The detail
# polls are the positive control: they must be there, in the same window.
POLLS="$(count_calls /api/v1/products/details)"
info "detail polls in the same window: $POLLS"
check_calls "the operator did keep polling, so the request log is live" yes \
  "$([ "${POLLS:-0}" -gt 0 ] && echo yes || echo no)"
check_calls "ChangeStorage calls while refusing to shrink" 0 "$(count_calls /api/v1/products/storage)"

info "putting sizeGB back to $DISK3 so the manifest stops claiming a disk it has not got"
apply_cluster "$BIG" "$NODES" "$DISK3"
wait_for cloudcluster "$CLUSTER" '.status.conditions[?(@.type=="Synced")].status' True "$WAIT" ||
  fail "Synced never came back"
check "CHANGES restored" Applied "$(changes_of "$CLUSTER")"

# --- 7. a disk size that is not on the menu ---------------------------------

step "Asking for a disk size the instance type does not offer"
info "$SMALL sells {$SMALL_MENU} GB; this manifest asks for $OFFMENU"

# The assertion below is that the control plane is never asked at all, so clear
# the log and count only what this manifest causes. Reading history instead does
# not work: the log holds the last 2000 entries and this lab has polled enough
# by now to have evicted its own create.
on_mock && lab_api POST /_lab/requests/reset >/dev/null
SYNC_BEFORE="$(jp cloudcluster "$CLUSTER" .status.lastSyncTime)"
cat <<EOF | k apply -f - | sed 's/^/    /'
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $BAD
  namespace: $LAB_NAMESPACE
spec:
  accountRef:
    name: ravendb-cloud
  pollIntervalSeconds: $POLL
  tier: $TIER
  instanceType: $SMALL
  storage:
    sizeGB: $OFFMENU
  networking:
    allowedIPs:
      - $LAB_ALLOWED_IPS
$(cloud_names b)
EOF

wait_for_status "$BAD" InvalidSpec 120 || fail "$BAD never reported the rejection"
k get cloudcluster "$BAD" | sed 's/^/    /'

BAD_MSG="$(condition_message cloudcluster "$BAD" Ready)"
info "Ready message: $BAD_MSG"
check "Ready"        False    "$(condition cloudcluster "$BAD" Ready)"
# InvalidSpec, not Refused: this is the operator's own refusal, not a
# forwarded rejection. The message is the operator's wording and it lists the
# sizes that would have worked.
check "Ready reason" InvalidSpec "$(condition_reason cloudcluster "$BAD" Ready)"
check_contains "rejection" "spec.storage.sizeGB $OFFMENU is not offered for $SMALL" "$BAD_MSG"
check_contains "rejection" "set it to one of: $SMALL_MENU"                 "$BAD_MSG"
check "nothing was bound" "" "$(jp cloudcluster "$BAD" .status.productID)"

# The point of the step: the control plane never saw this create at all. The
# operator had already fetched the instance-type menu to validate PB10, so
# checking the disk against the same response cost nothing, and a request that
# was always going to be a 400 never went out.
#
# Zero next to zero proves nothing, so wait for the healthy cluster to be polled
# once more first. That poll is a logged request, which is what makes "no
# creates" a measurement rather than the signature of a dead port-forward.
if on_mock; then
  wait_for_next_poll "$SYNC_BEFORE" || fail "the operator stopped polling, so the count below would prove nothing"
  POLL_CALLS="$(count_calls /api/v1/products/details)"
  check_not_empty "polls logged since the log was cleared" "$POLL_CALLS"
  [ "${POLL_CALLS:-0}" -gt 0 ] || fail "no polls were logged, so the request log is not live"
  info "$POLL_CALLS details polls logged since the reset, so the log is live"
fi
check_calls "create requests the control plane saw for the off-menu manifest" 0 "$(count_calls /api/v1/products/create)"

# --- 8. an action, which is not a state -------------------------------------

RESTART_REASON='.status.conditions[?(@.type=="Complete")].reason'
on_mock && lab_api POST /_lab/requests/reset >/dev/null

step "Restarting one node"
# Everything above this point was desired state: you write what you want the
# cluster to look like and the operator converges on it. A restart is not that.
# Nothing about the cluster afterwards is different, so no field could hold the
# value "restarted", and a field that meant "restart" would re-fire on every
# resync and reboot a database node each time.
#
# So it is an object of its own, which runs once and then is the record of
# having run. The same shape a Job has, and for the same reason.
NODE_TAG="$(jp cloudcluster "$CLUSTER" '.status.nodeTags[0]')"

# A restart changes the cluster, so it obeys the same policy a reshape does.
# Observe first, to watch the restart wait for it rather than fail.
info "setting managementPolicy Observe, then asking for a restart of node $NODE_TAG"
k patch cloudcluster "$CLUSTER" --type=merge -p '{"spec":{"managementPolicy":"Observe"}}' >/dev/null
wait_for cloudcluster "$CLUSTER" .status.managementPolicy Observe 60 >/dev/null || true

cat <<YAML | k apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudClusterRestart
metadata:
  name: lab05-restart
spec:
  clusterRef:
    name: $CLUSTER
  nodeTag: $NODE_TAG
YAML

wait_for cloudclusterrestart lab05-restart "$RESTART_REASON" Waiting 60 ||
  warn "the restart did not report Waiting"
k get cloudclusterrestart lab05-restart -o wide | sed 's/^/    /' || true
WAITING_MSG="$(condition_message cloudclusterrestart lab05-restart Complete)"
check "restart STATUS under Observe"   Waiting "$(restart_status lab05-restart)"
check "Complete condition"             False   "$(condition cloudclusterrestart lab05-restart Complete)"
check_contains "it says what holds it back" "has managementPolicy Observe, so the operator may not change it" "$WAITING_MSG"
check_contains "and what lets it run"       "Set spec.managementPolicy to Full and this runs" "$WAITING_MSG"
check "nothing was asked of RavenDB Cloud yet" "" "$(jp cloudclusterrestart lab05-restart .status.issuedAt)"
check_calls "restart calls while it waits" 0 "$(count_calls /api/v1/products/restart-node)"
note "Waiting keeps the request and carries it out the moment the policy allows"

info "managementPolicy Full: the restart runs without anybody touching it"
k patch cloudcluster "$CLUSTER" --type=merge -p '{"spec":{"managementPolicy":"Full"}}' >/dev/null
if ! wait_for cloudclusterrestart lab05-restart "$RESTART_REASON" Succeeded "$WAIT"; then
  warn "the restart did not complete; recent operator logs:"
  operator_logs 30
  exit 1
fi
k get cloudclusterrestart lab05-restart | sed 's/^/    /' || true
check "restart STATUS"                 Succeeded "$(restart_status lab05-restart)"
check "Complete condition"             True      "$(condition cloudclusterrestart lab05-restart Complete)"
check_not_empty "issuedAt is stamped"            "$(jp cloudclusterrestart lab05-restart .status.issuedAt)"
check_not_empty "completedAt is stamped"         "$(jp cloudclusterrestart lab05-restart .status.completedAt)"
check_calls "restart calls" 1 "$(count_calls /api/v1/products/restart-node)"
RESTART_EVENTS="$(k get events --field-selector involvedObject.name=lab05-restart \
  -o jsonpath='{range .items[*]}{.type} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || true)"
check_contains "a Normal event when the node goes down, because it was asked for" \
  "Normal Restarting: Restarting node $NODE_TAG" "$RESTART_EVENTS"
check_contains "and a Normal event when it is back" "Normal Restarted: Node $NODE_TAG of CloudCluster \"$CLUSTER\" restarted." "$RESTART_EVENTS"
# The node went through DeployingChanges and came back, which is what the
# restart was waiting on. It asked RavenDB Cloud rather than reading the
# CloudCluster's status, which is only as fresh as that cluster's last poll.
wait_for_status "$CLUSTER" Running "$WAIT" >/dev/null || true
check "the cluster is Running again"   Running   "$(status_of "$CLUSTER")"

step "The record is a record: it cannot be re-aimed, and it cannot re-fire"
# Immutable, so there is no edit that means "do it again". The API server says
# so at kubectl apply, before anything is stored.
EDIT="$(k patch cloudclusterrestart lab05-restart --type=merge \
  -p '{"spec":{"nodeTag":"B"}}' 2>&1 || true)"
check_contains "editing a finished restart" "immutable" "$EDIT"
info "to restart again you apply another object, which is also the history"

# And the owner reference, which is what has the records collected with the
# cluster rather than left pointing at nothing.
check "owned by the cluster it acted on" "$CLUSTER" \
  "$(jp cloudclusterrestart lab05-restart '.metadata.ownerReferences[0].name')"

step "A node the cluster does not have is refused without a call"
# The tag shape is checked at admission; whether the node exists is not
# decidable from the object, so the operator checks it against status.nodeTags
# and fails the restart rather than spending a call to be told no.
cat <<YAML | k apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudClusterRestart
metadata:
  name: lab05-restart-nowhere
spec:
  clusterRef:
    name: $CLUSTER
  nodeTag: Z
YAML
if ! wait_for cloudclusterrestart lab05-restart-nowhere "$RESTART_REASON" Failed 60; then
  warn "the impossible restart did not fail"
  exit 1
fi
NOWHERE_MSG="$(condition_message cloudclusterrestart lab05-restart-nowhere Complete)"
info "message: $NOWHERE_MSG"
check "STATUS for a node that does not exist" Failed "$(restart_status lab05-restart-nowhere)"
check "Failed, as on a Job"                    True   "$(condition cloudclusterrestart lab05-restart-nowhere Failed)"
check "and not Complete, so kubectl wait never passes" False "$(condition cloudclusterrestart lab05-restart-nowhere Complete)"
check_contains "it names the nodes there are" "has no node \"Z\"; it has [$BASE_TAGS]" "$NOWHERE_MSG"
check_contains "and the way out"              "create a new one naming one of those" "$NOWHERE_MSG"
check_calls "restart calls, still only the one above" 1 "$(count_calls /api/v1/products/restart-node)"

# One upper case letter is the shape, and that much the API server enforces, so
# a lower case tag never reaches the operator at all.
SHAPE="$(cat <<YAML | k apply -f - 2>&1 || true
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudClusterRestart
metadata:
  name: lab05-restart-badshape
spec:
  clusterRef:
    name: $CLUSTER
  nodeTag: a
YAML
)"
check_contains "a lower case node tag is refused at admission" "nodeTag" "$SHAPE"

# --- 8. the audit trail -----------------------------------------------------

step "What the operator did, as Kubernetes events"
k get events --field-selector "involvedObject.name=$CLUSTER" \
  --sort-by=.metadata.creationTimestamp \
  -o custom-columns='TYPE:.type,REASON:.reason,MESSAGE:.message' | sed 's/^/    /'

EVENTS="$(cluster_events)"
check_contains "events" "Changing instance type from $SMALL to $BIG"     "$EVENTS"
check_contains "events" "Resizing storage from $DISK1 GB to $DISK3 GB" "$EVENTS"
if scales; then
  check_contains "events" "Adding a node (3 -> 4)"                   "$EVENTS"
  check_contains "events" "Adding a node (4 -> 5)"                   "$EVENTS"
  check_contains "events" "Removing node E (5 -> 4)"                 "$EVENTS"
  check_contains "events" "Removing node D (4 -> 3)"                 "$EVENTS"
fi

# --- 9. clean up ------------------------------------------------------------

step "Cleaning up"
info "deleting the restart records (the cluster deletion below would collect them anyway)"
k delete cloudclusterrestart lab05-restart lab05-restart-nowhere --ignore-not-found --timeout=60s | sed 's/^/    /'

info "deleting the CloudClusters (deletionPolicy Retain leaves the cloud side running)"
k delete cloudcluster "$CLUSTER" "$BAD" --ignore-not-found --timeout=120s | sed 's/^/    /'
check "the CloudCluster object is gone"    "" "$(jp cloudcluster "$CLUSTER" .metadata.name)"
check "the rejected object is gone too"    "" "$(jp cloudcluster "$BAD" .metadata.name)"

if on_mock; then
  info "removing the simulated cluster $PRODUCT_ID from the simulator"
  lab_api POST "/_lab/clusters/$PRODUCT_ID/vanish" >/dev/null 2>&1 || true
  check "no lab05 cluster left in the control plane" "" "$(lab_cluster_ids "$CLUSTER")"
else
  # Retain left it running when the object went, which is right for a user and
  # a bill for a lab, so it is terminated directly.
  info "terminating $PRODUCT_ID, which Retain left running"
  retire_product "$PRODUCT_ID"
  case "$(product_status "$PRODUCT_ID")" in
    Terminating|Terminated) pass "$PRODUCT_ID is no longer running" ;;
    *) fail "$PRODUCT_ID is still $(product_status "$PRODUCT_ID") after terminate" ;;
  esac
  PRODUCT_ID=''
fi

lab_summary

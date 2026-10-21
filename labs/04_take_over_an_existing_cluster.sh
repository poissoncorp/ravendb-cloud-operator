#!/usr/bin/env bash
# Lab 04: take over a RavenDB Cloud cluster that already exists.
#
# Shows that a manifest naming an existing cluster binds to it without creating
# anything and changes nothing until told to, that a field written differently
# from the cluster is reported and then applied only under managementPolicy
# Full, that the export tool writes a manifest that takes a cluster over with
# no change at all, and the four ways binding refuses: a second object for the
# same cluster, a name on a cluster of another tier, a name nothing carries
# under Observe, and a name two clusters share.
#
#   labs/04_take_over_an_existing_cluster.sh
#
# Run from the repository root, after labs/00_setup.sh. The lab removes every
# cluster it seeded or created, and every object it applied.
source "$(dirname "$0")/lib.sh"

# The cluster somebody made in the portal. Its display name is also the object
# name below, which is the whole of the minimal manifest's identity.
LEGACY="$(lab_display legacy)"
SECOND=lab04-second
WRONG_TIER=lab04-wrong-tier
NOT_THERE=lab04-not-there
AMBIGUOUS=lab04-ambiguous
SEED=lab04-seed
EXPORT_POD=lab04-export
MISSING="$(lab_display missing)"
TWINS="$(lab_display twin)"
ALL_OBJECTS="$LEGACY $SECOND $WRONG_TIER $NOT_THERE $AMBIGUOUS $SEED"

# The shape of the cluster to take over. The simulator seeds the kind of cluster
# a team would have made by hand, a three node Production PB10. A real account
# has nothing to seed into, so there the lab makes its own predecessor from the
# smallest Development shape, which is also the cheapest to reshape.
if on_mock; then
  TIER=Production; OTHER_TIER=Development; SMALL=PB10; BIG=PB20; POLL=5
else
  TIER=Development; OTHER_TIER=Production; SMALL=Dev10; BIG=Dev20; POLL=30
fi

# --- helpers ----------------------------------------------------------------

cluster_count() { cloud_products | wc -l | tr -d '[:space:]'; }
count_named()   { product_ids_named "$1" | wc -l | tr -d '[:space:]'; }

# calls_to <path substring> -> how many requests the control plane logged for
# it. grep -o and wc -l, not grep -c: the whole request log arrives on one
# line, so grep -c would answer 1 however many there were.
calls_to() {
  on_mock || { printf '0'; return 0; }
  lab_api GET "/_lab/requests?path=$1" |
    { grep -o '"method"' || true; } | wc -l | tr -d '[:space:]'
}

# reshape_calls -> every request that would have changed a cluster.
reshape_calls() {
  echo $(( $(calls_to /api/v1/products/instance-type) + $(calls_to /api/v1/products/storage) \
         + $(calls_to /api/v1/products/additional-node) + $(calls_to /api/v1/products/create) ))
}

# wait_for_next_poll <object> <previous lastSyncTime> blocks until the operator
# has read the cloud side at least once more. Used to prove a call did NOT
# happen: zero calls in a window the operator never looked in proves nothing.
wait_for_next_poll() {
  local name="$1" before="$2" deadline=$((SECONDS + 60)) now
  while [ "$SECONDS" -lt "$deadline" ]; do
    now="$(jp cloudcluster "$name" .status.lastSyncTime)"
    [ -n "$now" ] && [ "$now" != "$before" ] && return 0
    sleep 2
  done
  return 1
}

# events_named <object> -> TYPE REASON: MESSAGE, one event per line. By name,
# which is safe here because every name that outlives a run carries its run id,
# and the fixed ones have their events cleared before the lab starts.
events_named() {
  k get events --field-selector "involvedObject.name=$1" \
    -o jsonpath='{range .items[*]}{.type} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || true
}

delete_lab_objects() {
  # shellcheck disable=SC2086
  k delete cloudcluster $ALL_OBJECTS --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
  k delete pod "$EXPORT_POD" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
  local name
  for name in $ALL_OBJECTS; do
    k delete secret "${name}-connection" --ignore-not-found >/dev/null 2>&1 || true
    k delete events --field-selector "involvedObject.name=$name" >/dev/null 2>&1 || true
  done
}

# Everything this lab seeds or creates is named lab04-something, so the
# simulator's leftovers can be found by prefix and nothing of another lab's is
# touched. On a real account it terminates only what this run made.
forget_lab_clusters() {
  local id
  if on_mock; then
    for id in $(cloud_products | awk 'index($2, "lab04-") == 1 { print $1 }'); do
      retire_product "$id"
    done
  else
    retire_product "${LEGACY_ID:-}"
  fi
}

lab04_cleanup() {
  delete_lab_objects
  forget_lab_clusters
  close_cloud_portforward
}

# ---------------------------------------------------------------------------

step "Preflight"
require_ready_account
on_mock && open_cloud_portforward
trap lab04_cleanup EXIT
info "CloudAccount 'ravendb-cloud' is Ready"

# Re-running must start from the same place, so remove anything an earlier run
# of this lab left behind before measuring anything.
delete_lab_objects
on_mock && forget_lab_clusters

# --- 1. what is already out there -------------------------------------------

step "What is already out there"
if on_mock; then
  note "seeding $LEGACY through the simulator, standing in for a cluster"
  note "a colleague created in the portal months ago"
  LEGACY_ID="$(lab_api POST /_lab/clusters \
    "{\"displayName\":\"$LEGACY\",\"tier\":\"$TIER\",\"instanceType\":\"$SMALL\",\"region\":\"eu-central-1\",\"provider\":\"Aws\"}" |
    sed -n 's/.*"ids":\["\([^"]*\)".*/\1/p')" || LEGACY_ID=''
else
  # Nothing to seed into on a real account, so this lab makes its own
  # predecessor: create a cluster, then delete the object under the default
  # Retain, which leaves exactly what taking over is for, a cluster running in
  # RavenDB Cloud that no CloudCluster names.
  info "creating $LEGACY, then letting go of it, to stand in for a cluster"
  info "somebody created in the portal months ago"
  k apply -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $SEED
spec:
  accountRef:
    name: ravendb-cloud
  displayName: $LEGACY
  subdomain: $(lab_subdomain g)
  tier: $TIER
  storage:
    sizeGB: 20
YAML
  wait_for_status "$SEED" Running "$LAB_SETTLE_TIMEOUT" ||
    { warn "the predecessor never reached Running"; operator_logs 30; }
  LEGACY_ID="$(jp cloudcluster "$SEED" .status.productID)"
  k delete cloudcluster "$SEED" --ignore-not-found --timeout=300s >/dev/null || true
  info "released it: $LEGACY_ID is now a cluster nothing in Kubernetes names"
fi
if [ -z "$LEGACY_ID" ]; then
  fail "there is no cluster named $LEGACY to take over"
  exit 1
fi
check_not_empty "id of the pre-existing cluster $LEGACY" "$LEGACY_ID"
check "clusters named $LEGACY" 1 "$(count_named "$LEGACY")"
info "$LEGACY is $LEGACY_ID"

BASELINE_COUNT="$(cluster_count)"
info "the lab's share of the account holds $BASELINE_COUNT cluster(s)"

# --- 2. a name and a tier ---------------------------------------------------

step "Taking it over with a name and a tier"
# Reset the request log so "the operator never called create" is a
# measurement and not an assumption.
on_mock && lab_api POST /_lab/requests/reset >/dev/null

k apply -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $LEGACY
spec:
  accountRef:
    name: ravendb-cloud
  tier: $TIER
YAML

wait_for_status "$LEGACY" Running 150 || { warn "recent operator logs:"; operator_logs 30; }
k get cloudcluster "$LEGACY" -o wide || true

check "STATUS"                   Running    "$(status_of "$LEGACY")"
check "CHANGES"                  Applied    "$(changes_of "$LEGACY")"
check "status.productID"         "$LEGACY_ID" "$(jp cloudcluster "$LEGACY" .status.productID)"
check "status.adopted"           true       "$(jp cloudcluster "$LEGACY" .status.adopted)"
check "managementPolicy for a cluster it found" Observe \
  "$(jp cloudcluster "$LEGACY" .status.managementPolicy)"
check "status.instanceType, read from the cluster" "$SMALL" "$(jp cloudcluster "$LEGACY" .status.instanceType)"
check "Synced message" "Everything this manifest sets matches RavenDB Cloud." \
  "$(condition_message cloudcluster "$LEGACY" Synced)"
check_contains "the Adopted event says what it found and how it manages it" \
  "Warning Adopted: Found RavenDB Cloud cluster \"$LEGACY\" ($LEGACY_ID) already running and took it over; managing it with managementPolicy Observe, which changes nothing, until spec.managementPolicy is Full" \
  "$(events_named "$LEGACY")"
check_contains "and, since nothing asked for an existing cluster, how to say it was meant" \
  "set spec.creationMode to TakeOverOnly to confirm this one is meant" \
  "$(events_named "$LEGACY")"
note "only what the manifest sets is managed. This one sets a tier, and the tier matches,"
note "so CHANGES is Applied: nothing it asks for differs from the cluster"

step "Nothing was created"
check "cluster count after taking over" "$BASELINE_COUNT" "$(cluster_count)"
check "clusters named $LEGACY"          1                 "$(count_named "$LEGACY")"
check_calls "recorded POST /api/v1/products/create calls" 0 "$(calls_to /api/v1/products/create)"
note "finding the name is a read plus a bind, never a write"

step "Connectivity is published exactly as for a cluster it created"
SECRET="${LEGACY}-connection"
check "status.connectionSecretName" "$SECRET" "$(jp cloudcluster "$LEGACY" .status.connectionSecretName)"
check_contains "and Running says so" "Secret \"$SECRET\" holds its URLs and client certificate" \
  "$(condition_message cloudcluster "$LEGACY" Ready)"
# What the cluster's endpoints are. The simulator's are known in advance; a real
# environment's are whatever RavenDB Cloud built, including the per-tier "-dev"
# rewrite, so there they come from the object and are tied to this run by the
# subdomain the predecessor was created with.
if on_mock; then
  EXPECTED_URL="https://a.${LEGACY}.acme.ravendb.cloud"
  EXPECTED_TAGS="A,B,C"
else
  EXPECTED_URL="$(jp cloudcluster "$LEGACY" '.status.urls[0]')"
  EXPECTED_TAGS="$(jp cloudcluster "$LEGACY" '.status.nodeTags[*]' | tr ' ' ',')"
  check_contains "the URL is the cluster this run created" "$(lab_subdomain g)" "$EXPECTED_URL"
fi
check "secret key url"       "$EXPECTED_URL"                     "$(secret_value "$SECRET" url)"
check "secret key nodeTags"  "$EXPECTED_TAGS"                    "$(secret_value "$SECRET" nodeTags)"
check "secret key productID" "$LEGACY_ID"                        "$(secret_value "$SECRET" productID)"
check "secret key studioURL" "${EXPECTED_URL}/studio/index.html" "$(secret_value "$SECRET" studioURL)"
# Go templates walk a map in sorted key order, which makes the whole key set
# one exact comparison.
check "connection Secret keys" \
  "certificate.password,client.pem,client.pfx,client.with-password.pfx,nodeTags,productID,studioURL,tls.crt,tls.key,url,urls," \
  "$(k get secret "$SECRET" -o go-template='{{range $k, $v := .data}}{{$k}},{{end}}' 2>/dev/null || true)"
check "Secret owner is the CloudCluster" "$LEGACY" "$(jp secret "$SECRET" '.metadata.ownerReferences[0].name')"

# --- 3. a field written by hand ---------------------------------------------

step "A hand-written manifest that says $BIG where the cluster runs $SMALL"
on_mock && lab_api POST /_lab/requests/reset >/dev/null
k apply -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $LEGACY
spec:
  accountRef:
    name: ravendb-cloud
  tier: $TIER
  instanceType: $BIG
  pollIntervalSeconds: $POLL   # lab only, so each step below settles quickly
YAML

wait_for_changes "$LEGACY" NotApplied 90 || { warn "recent operator logs:"; operator_logs 30; }
k get cloudcluster "$LEGACY" || true
NOT_APPLIED="$(condition_message cloudcluster "$LEGACY" Synced)"
info "Synced message: $NOT_APPLIED"
check "CHANGES"  NotApplied "$(changes_of "$LEGACY")"
check "STATUS, because the cluster serves either way" Running "$(status_of "$LEGACY")"
check_contains "it names the difference" "instanceType differs (desired $BIG, actual $SMALL)" "$NOT_APPLIED"
check_contains "and why it was not applied" \
  "managementPolicy is Observe, the default for a cluster this object found already running, so no changes were applied" "$NOT_APPLIED"
check_contains "and what to do about it" "set spec.managementPolicy to Full to apply them" "$NOT_APPLIED"
if on_mock; then
  # Zero means something only once the operator has demonstrably looked again.
  wait_for_next_poll "$LEGACY" "$(jp cloudcluster "$LEGACY" .status.lastSyncTime)" ||
    warn "the operator did not poll again, so the count below proves less"
  check "the operator kept polling, so the log below is live" yes \
    "$([ "$(calls_to "products/details/$LEGACY_ID")" -gt 0 ] && echo yes || echo no)"
fi
check_calls "instance-type calls while Observe" 0 "$(calls_to /api/v1/products/instance-type)"
check "the cluster is still $SMALL" "$SMALL" "$(jp cloudcluster "$LEGACY" .status.instanceType)"
note "a manifest written by hand that gets a field wrong reshapes nothing: taking over"
note "defaults to Observe exactly so that a typo is a report and not a resize"

step "managementPolicy: Full, and the change goes out"
k apply -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $LEGACY
spec:
  accountRef:
    name: ravendb-cloud
  tier: $TIER
  instanceType: $BIG
  pollIntervalSeconds: $POLL
  managementPolicy: Full
YAML

wait_for cloudcluster "$LEGACY" .status.instanceType "$BIG" "$LAB_CHANGE_TIMEOUT" ||
  { warn "the instance type never changed; recent operator logs:"; operator_logs 30; }
wait_for_changes "$LEGACY" Applied "$LAB_CHANGE_TIMEOUT" || true
wait_for_status "$LEGACY" Running "$LAB_CHANGE_TIMEOUT" || true
k get cloudcluster "$LEGACY" -o wide || true
check "status.managementPolicy"  Full    "$(jp cloudcluster "$LEGACY" .status.managementPolicy)"
check "status.instanceType"      "$BIG"  "$(jp cloudcluster "$LEGACY" .status.instanceType)"
check "CHANGES"                  Applied "$(changes_of "$LEGACY")"
check "STATUS"                   Running "$(status_of "$LEGACY")"
check_calls "instance-type calls once Full" 1 "$(calls_to /api/v1/products/instance-type)"
check_contains "the change has an event" "Normal Applying: Changing instance type from $SMALL to $BIG" \
  "$(events_named "$LEGACY")"
check "status.adopted is still true: Full is a choice, not a claim of authorship" true \
  "$(jp cloudcluster "$LEGACY" .status.adopted)"

# --- 4. a second object for the same cluster --------------------------------

step "A second object for the same cluster"
k apply -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $SECOND
spec:
  accountRef:
    name: ravendb-cloud
  displayName: $LEGACY
  tier: $TIER
YAML
wait_for_status "$SECOND" NameTaken 90 || { warn "recent operator logs:"; operator_logs 30; }
SECOND_MSG="$(condition_message cloudcluster "$SECOND" Ready)"
info "message: $SECOND_MSG"
check "$SECOND STATUS"            NameTaken "$(status_of "$SECOND")"
check "$SECOND Ready"             False     "$(condition cloudcluster "$SECOND" Ready)"
check_contains "the message names the object that holds it" \
  "is already managed by CloudCluster $LAB_NAMESPACE/$LEGACY" "$SECOND_MSG"
check "$SECOND status.productID"  "<empty>" "$(v="$(jp cloudcluster "$SECOND" .status.productID)"; echo "${v:-<empty>}")"
check "$SECOND CHANGES"           "<empty>" "$(v="$(changes_of "$SECOND")"; echo "${v:-<empty>}")"
check "$SECOND wrote no Secret"   no        "$(exists secret "${SECOND}-connection")"
check_contains "a Warning marks it" "Warning NameTaken: RavenDB Cloud cluster \"$LEGACY\" ($LEGACY_ID) is already managed" \
  "$(events_named "$SECOND")"
note "two objects managing one cluster would take turns reshaping it, so the second"
note "is refused before it binds, and the first never notices"
k delete cloudcluster "$SECOND" --ignore-not-found --timeout=60s >/dev/null || true

# --- 5. let go, then get it wrong -------------------------------------------

step "Deleting the object keeps the cluster"
k delete cloudcluster "$LEGACY" --timeout=120s >/dev/null || true
check "the CloudCluster still exists" no "$(exists cloudcluster "$LEGACY")"
check "the cluster is still running" Active "$(product_status "$LEGACY_ID")"
check_contains "and the operator said so" \
  "Normal Retained: deletionPolicy is Retain: leaving RavenDB Cloud cluster $LEGACY_ID running" \
  "$(events_named "$LEGACY")"
note "deletionPolicy defaults to Retain, found or created. What the lab does next"
note "is what a rebuilt Kubernetes cluster does: apply manifests for clusters that exist"

step "A manifest that gets the tier wrong"
on_mock && lab_api POST /_lab/requests/reset >/dev/null
k apply -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $WRONG_TIER
spec:
  accountRef:
    name: ravendb-cloud
  displayName: $LEGACY
  tier: $OTHER_TIER
YAML
wait_for_status "$WRONG_TIER" NameTaken 90 || { warn "recent operator logs:"; operator_logs 30; }
WRONG_MSG="$(condition_message cloudcluster "$WRONG_TIER" Ready)"
info "message: $WRONG_MSG"
check "$WRONG_TIER STATUS" NameTaken "$(status_of "$WRONG_TIER")"
check_contains "the message says how the cluster differs" \
  "it is $TIER and spec.tier says $OTHER_TIER" "$WRONG_MSG"
check_contains "and that nothing was touched" "was not bound to it and nothing was changed" "$WRONG_MSG"
check_contains "and both ways out, as edits" "Set spec.displayName to another name to create a new cluster, or those fields to the cluster's own values to manage that one" "$WRONG_MSG"
check "$WRONG_TIER status.productID" "<empty>" "$(v="$(jp cloudcluster "$WRONG_TIER" .status.productID)"; echo "${v:-<empty>}")"
check_calls "calls that create or reshape anything" 0 "$(reshape_calls)"
note "tier cannot change on either side, so a name on a cluster of another tier belongs"
note "to a different cluster than the one this manifest describes"
k delete cloudcluster "$WRONG_TIER" --ignore-not-found --timeout=60s >/dev/null || true

# --- 6. the manifest written from the cluster -------------------------------

step "Writing the manifest from the cluster itself"
# The export tool rides in the operator image, so it runs in the cluster next
# to the Secret that holds the key, and the key never leaves Kubernetes: it goes
# from the Secret into the pod's environment and nowhere else.
IMAGE="$(kubectl -n "$LAB_OPERATOR_NAMESPACE" get deploy ravendb-cloud-operator-controller-manager \
  -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"
ENDPOINT="$(jp cloudaccount ravendb-cloud .spec.endpoint)"
KEY_SECRET="$(jp cloudaccount ravendb-cloud .spec.apiKeySecretRef.name)"
KEY_NAME="$(jp cloudaccount ravendb-cloud .spec.apiKeySecretRef.key)"
check_not_empty "the operator image" "$IMAGE"
info "image: $IMAGE"
# The pod reaches the same control plane the account does. Against RavenDB
# Cloud itself the account names no endpoint and neither does the pod.
ENV_JSON="{\"name\":\"RAVENDB_CLOUD_API_KEY\",\"valueFrom\":{\"secretKeyRef\":{\"name\":\"$KEY_SECRET\",\"key\":\"${KEY_NAME:-apiKey}\"}}}"
[ -n "$ENDPOINT" ] && ENV_JSON="$ENV_JSON,{\"name\":\"RAVENDB_CLOUD_ENDPOINT\",\"value\":\"$ENDPOINT\"}"

EXPORT_ERR="$(mktemp)"
# MSYS_NO_PATHCONV: Git Bash on Windows rewrites an argument that starts with a
# slash into a Windows path, which turns /kubectl-ravendb_cloud into a file that
# is not in the image. Everywhere else it does nothing.
EXPORTED="$(MSYS_NO_PATHCONV=1 k run "$EXPORT_POD" --rm -i --quiet --restart=Never \
  --image="$IMAGE" --image-pull-policy=IfNotPresent \
  --override-type=strategic \
  --overrides="{\"spec\":{\"containers\":[{\"name\":\"$EXPORT_POD\",\"env\":[$ENV_JSON]}]}}" \
  --command -- /kubectl-ravendb_cloud export --account ravendb-cloud -n "$LAB_NAMESPACE" "$LEGACY" 2>"$EXPORT_ERR")" || true
printf '%s\n' "$EXPORTED" | sed 's/^/    /'
# What the tool and kubectl said on stderr, kept out of the manifest so that
# what is applied below is exactly what the tool printed.
if [ -s "$EXPORT_ERR" ]; then sed 's/^/    stderr: /' "$EXPORT_ERR"; fi
rm -f "$EXPORT_ERR"

check_contains "it says where the manifest came from" \
  "# Written from RavenDB Cloud cluster \"$LEGACY\" ($LEGACY_ID)" "$EXPORTED"
check_contains "it is a CloudCluster" "kind: CloudCluster" "$EXPORTED"
check_contains "named after the cluster" "name: $LEGACY" "$EXPORTED"
check_contains "for the account it was asked about" "name: ravendb-cloud" "$EXPORTED"
check_contains "with the shape the cluster has now" "instanceType: $BIG" "$EXPORTED"
check_contains "and the tier" "tier: $TIER" "$EXPORTED"
check_contains "and managementPolicy Full, since nothing differs" "managementPolicy: Full" "$EXPORTED"
check "the API key is not in it" no \
  "$(case "$EXPORTED" in *"$(k get secret "$KEY_SECRET" -o jsonpath="{.data.${KEY_NAME:-apiKey}}" | base64 -d)"*) echo yes ;; *) echo no ;; esac)"
check "the pod is still around" no "$(exists pod "$EXPORT_POD")"

step "Applying it takes the cluster over with no change"
on_mock && lab_api POST /_lab/requests/reset >/dev/null
printf '%s\n' "$EXPORTED" | k apply -f - >/dev/null || fail "the exported manifest did not apply"
wait_for_status "$LEGACY" Running 120 || { warn "recent operator logs:"; operator_logs 30; }
wait_for_changes "$LEGACY" Applied 60 || true
check "STATUS"                   Running      "$(status_of "$LEGACY")"
check "CHANGES"                  Applied      "$(changes_of "$LEGACY")"
check "status.productID"         "$LEGACY_ID" "$(jp cloudcluster "$LEGACY" .status.productID)"
check "status.adopted"           true         "$(jp cloudcluster "$LEGACY" .status.adopted)"
check "managementPolicy, from the manifest" Full "$(jp cloudcluster "$LEGACY" .status.managementPolicy)"
check_contains "the Adopted event names the policy it runs under" \
  "already running; managing it with managementPolicy Full" "$(events_named "$LEGACY")"
if on_mock; then
  check "the operator read the cluster, so the log is live" yes \
    "$([ "$(calls_to "products/details/$LEGACY_ID")" -gt 0 ] && echo yes || echo no)"
fi
check_calls "calls that create or reshape anything" 0 "$(reshape_calls)"
note "Full from the first pass, and nothing to do: every field the manifest sets is what"
note "the cluster already is, so the first change is the one somebody edits in"

# --- 7. a name nothing carries ----------------------------------------------

step "Observe, for a name nothing carries"
BEFORE_MISSING="$(cluster_count)"
on_mock && lab_api POST /_lab/requests/reset >/dev/null
k apply -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $NOT_THERE
spec:
  accountRef:
    name: ravendb-cloud
  displayName: $MISSING
  tier: $TIER
  managementPolicy: Observe
YAML
wait_for_status "$NOT_THERE" ClusterNotFound 90 || { warn "recent operator logs:"; operator_logs 30; }
MISSING_MSG="$(condition_message cloudcluster "$NOT_THERE" Ready)"
info "message: $MISSING_MSG"
check "$NOT_THERE STATUS" ClusterNotFound "$(status_of "$NOT_THERE")"
check_contains "the message names what it looked for" "No RavenDB Cloud cluster named \"$MISSING\"" "$MISSING_MSG"
check_contains "and why it stopped there" "managementPolicy Observe never creates one" "$MISSING_MSG"
check "$NOT_THERE status.productID" "<empty>" "$(v="$(jp cloudcluster "$NOT_THERE" .status.productID)"; echo "${v:-<empty>}")"
check "clusters named $MISSING"     0 "$(count_named "$MISSING")"
check "cluster count unchanged"     "$BEFORE_MISSING" "$(cluster_count)"
check_calls "create calls"          0 "$(calls_to /api/v1/products/create)"
note "an explicit Observe is how to say \"this cluster exists; bind to it or tell me\":"
note "a typo in the name is a report, never a new cluster"

# --- 8. two clusters, one name ----------------------------------------------

if needs_mock "two clusters sharing a display name (RavenDB Cloud refuses a create whose name is taken)"; then
step "Two clusters, one name"
TWIN_IDS="$(lab_api POST /_lab/clusters "{\"displayName\":\"$TWINS\",\"tier\":\"Production\",\"count\":2}" |
  sed -n 's/.*"ids":\[\([^]]*\)\].*/\1/p' | tr -d '"')" || TWIN_IDS=''
TWIN_A="${TWIN_IDS%%,*}"; TWIN_B="${TWIN_IDS##*,}"
check_not_empty "the first $TWINS" "$TWIN_A"
check_not_empty "the second $TWINS" "$TWIN_B"
check "clusters named $TWINS" 2 "$(count_named "$TWINS")"
info "seeded $TWIN_A and $TWIN_B, both called $TWINS"

k apply -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $AMBIGUOUS
spec:
  accountRef:
    name: ravendb-cloud
  displayName: $TWINS
  tier: Production
YAML
wait_for_status "$AMBIGUOUS" NameTaken 90 || { warn "recent operator logs:"; operator_logs 30; }
AMB_MSG="$(condition_message cloudcluster "$AMBIGUOUS" Ready)"
info "message: $AMB_MSG"
check "$AMBIGUOUS STATUS" NameTaken "$(status_of "$AMBIGUOUS")"
check_contains "the message names the first candidate"  "$TWIN_A" "$AMB_MSG"
check_contains "the message names the second candidate" "$TWIN_B" "$AMB_MSG"
check_contains "and the way out" "terminate or hide in the portal the one this object should not manage" "$AMB_MSG"
check "$AMBIGUOUS status.productID" "<empty>" "$(v="$(jp cloudcluster "$AMBIGUOUS" .status.productID)"; echo "${v:-<empty>}")"
check "$AMBIGUOUS wrote no Secret" no "$(exists secret "${AMBIGUOUS}-connection")"
note "refusing is the point: one of those two may be production, and binding to an"
note "arbitrary one would hand the namespace working credentials for the wrong database"
fi

# --- 9. the table -----------------------------------------------------------

step "The objects as kubectl sees them"
# The object that bound prints a full row. The two that never bound print
# only STATUS, because every other column reads status the operator never
# wrote for them.
TABLE="$LEGACY $NOT_THERE"
on_mock && TABLE="$TABLE $AMBIGUOUS"
# shellcheck disable=SC2086
k get cloudcluster $TABLE || true

step "Cleaning up"
delete_lab_objects
check "no lab04 CloudClusters left" "<none>" \
  "$({ k get cloudcluster -o name 2>/dev/null || true; } | { grep lab04 || echo '<none>'; })"
if on_mock; then
  check "$LEGACY survived every object that named it" Active "$(product_status "$LEGACY_ID")"
  forget_lab_clusters
  check "lab04 clusters left in the control plane" 0 "$(cluster_count)"
else
  # Retain never touches the cloud side, and a CloudCluster that found its
  # cluster must not destroy what it did not create. The one this lab made as its
  # simulator predecessor is terminated directly, because nothing else would.
  check "$LEGACY_ID is still running after every object naming it was deleted" \
    Active "$(product_status "$LEGACY_ID")"
  forget_lab_clusters
  LEGACY_ID=''
fi

lab_summary

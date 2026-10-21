#!/usr/bin/env bash
# Lab 07: delete a credential that something still depends on.
#
# A CloudAccount is not just a Secret reference. It is the only way a
# CloudCluster can reach RavenDB Cloud, so removing one while a cluster still
# names it would leave that cluster unable to reconcile, unable to report, and,
# if its deletionPolicy said Terminate, unable to terminate the thing it is
# billing for.
#
# The operator holds the account open instead. This lab watches that happen:
# the delete is accepted, the object does not go away, and it says why.
#
#   labs/07_delete_a_credential_in_use.sh
#
# Run from the repository root, after labs/00_setup.sh.
#
# Parts 1 to 3 never create anything. The cluster that holds the account open
# is under Observe, and clustersUsing counts any CloudCluster naming the account
# whether or not it ever bound, so they cost the same against either control
# plane: nothing. Part 4 deletes an account and a cluster together, which needs
# a real cluster, so against a real account it runs with LAB_BUDGET=full.
source "$(dirname "$0")/lib.sh"

ACCOUNT="lab07-account"
KEY_SECRET="lab07-api-key"
HOLDER="lab07-holder"
SECOND="lab07-second"
TOGETHER="lab07-both"
# Part 4 terminates the cluster it makes, and a terminated cluster keeps its
# name until somebody hides it in the portal, so a fixed name would be refused
# on the next run. The derived subdomain has to fit in 13 characters, and this
# one is 12.
TOGETHER_NAME="$(lab_display t)"

cleanup_objects() {
  k delete cloudcluster "$HOLDER" "$SECOND" "$TOGETHER" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
  k delete cloudaccount "$ACCOUNT" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
  k delete secret "$KEY_SECRET" --ignore-not-found >/dev/null 2>&1 || true
  # Part 4 makes a real cluster. If the run dies before it is terminated,
  # nothing else would.
  retire_product "${TOGETHER_ID:-}"
}

step "Preflight"
cleanup_objects
check "the shared account is Ready" True "$(condition cloudaccount ravendb-cloud Ready)"
note "this lab uses its own account and its own credential, so the shared one is untouched"

# --- 1. an account of its own -----------------------------------------------

step "An account nothing depends on yet"
if on_cloud; then
  kubectl -n "$LAB_NAMESPACE" create secret generic "$KEY_SECRET" \
    --from-file=apiKey="$LAB_CLOUD_API_KEY_FILE" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  LAB07_ENDPOINT="$LAB_CLOUD_ENDPOINT"
else
  cat <<YAML | k apply -f - >/dev/null
apiVersion: v1
kind: Secret
metadata:
  name: $KEY_SECRET
type: Opaque
stringData:
  apiKey: lab-api-key
YAML
  LAB07_ENDPOINT="http://mockcloud.${LAB_CLOUD_NAMESPACE}.svc.cluster.local"
fi

cat <<YAML | k apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: $ACCOUNT
spec:
  apiKeySecretRef:
    name: $KEY_SECRET
    key: apiKey
  endpoint: $LAB07_ENDPOINT
  defaults:
    provider: AWS
    region: eu-central-1
    allowedIPs:
      - $LAB_ALLOWED_IPS
YAML

if ! wait_for cloudaccount "$ACCOUNT" '.status.conditions[?(@.type=="Ready")].status' True 120; then
  warn "$ACCOUNT never verified; recent operator logs:"
  operator_logs 30
fi
check "$ACCOUNT Ready"        True                          "$(condition cloudaccount "$ACCOUNT" Ready)"
check "$ACCOUNT STATUS"       Verified                      "$(condition_reason cloudaccount "$ACCOUNT" Ready)"
check "the finalizer it took" "cloud.ravendb.net/finalizer" \
  "$(jp cloudaccount "$ACCOUNT" '.metadata.finalizers[0]')"
note "the finalizer goes on at the first reconcile, before anything depends on the account:"
note "taking it later would leave a window where a delete slipped through"

step "Deleting it while nothing uses it"
k delete cloudaccount "$ACCOUNT" --timeout=60s >/dev/null 2>&1 || true
if ! wait_gone cloudaccount "$ACCOUNT" 60; then
  warn "$ACCOUNT did not go away even though nothing referenced it"
fi
check "an unused account deletes immediately" no "$(exists cloudaccount "$ACCOUNT")"
note "the finalizer is not a lock. With no users, the operator drops it on the same pass."

# --- 2. the same delete, with a user ----------------------------------------

step "Recreating it, this time with a cluster that names it"
cat <<YAML | k apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: $ACCOUNT
spec:
  apiKeySecretRef:
    name: $KEY_SECRET
    key: apiKey
  endpoint: $LAB07_ENDPOINT
  defaults:
    provider: AWS
    region: eu-central-1
    allowedIPs:
      - $LAB_ALLOWED_IPS
YAML
wait_for cloudaccount "$ACCOUNT" '.status.conditions[?(@.type=="Ready")].status' True 120 >/dev/null || true

# under Observe, so nothing is created anywhere. What holds the account open is the
# reference, not the cluster having reached RavenDB Cloud: clustersUsing lists
# every CloudCluster whose accountRef names it, bound or not.
cat <<YAML | k apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $HOLDER
spec:
  accountRef:
    name: $ACCOUNT
  managementPolicy: Observe
  tier: Development
  storage:
    sizeGB: 20
YAML

if ! wait_for_status "$HOLDER" ClusterNotFound 90; then
  warn "$HOLDER never reported ClusterNotFound; recent operator logs:"
  operator_logs 30
fi
check "$HOLDER finds nothing"      ClusterNotFound "$(status_of "$HOLDER")"
check "$HOLDER never bound"        ""     "$(jp cloudcluster "$HOLDER" .status.productID)"
note "nothing was created in RavenDB Cloud, and the account is held open all the same"

step "kubectl delete cloudaccount, with a cluster still naming it"
k delete cloudaccount "$ACCOUNT" --timeout=20s >/dev/null 2>&1 || true
info "the delete is accepted: the API server stamps deletionTimestamp and waits for the finalizer"

if ! wait_for cloudaccount "$ACCOUNT" \
     '.status.conditions[?(@.type=="Ready")].reason' InUse 90; then
  warn "$ACCOUNT never reported InUse; recent operator logs:"
  operator_logs 30
fi

check "the account is still here"      yes    "$(exists cloudaccount "$ACCOUNT")"
check_not_empty "deletionTimestamp is set" "$(jp cloudaccount "$ACCOUNT" .metadata.deletionTimestamp)"
check "the finalizer is still held"    "cloud.ravendb.net/finalizer" \
  "$(jp cloudaccount "$ACCOUNT" '.metadata.finalizers[0]')"
check "Ready stays True"                True   "$(condition cloudaccount "$ACCOUNT" Ready)"
check "STATUS"                         InUse  "$(condition_reason cloudaccount "$ACCOUNT" Ready)"
note "True, because the credential still works, and the clusters holding it open are the"
note "ones that need it: one with deletionPolicy Terminate reads this Ready before it"
note "terminates. It used to go False here, which is how part 4 below deadlocked."

INUSE_MSG="$(condition_message cloudaccount "$ACCOUNT" Ready)"
check_contains "the message names the cluster holding it" "$HOLDER" "$INUSE_MSG"
check_contains "and counts them"                          "1 CloudCluster" "$INUSE_MSG"
check_contains "and says what to do"                      "Delete them first" "$INUSE_MSG"
info "message: $INUSE_MSG"

note "this is the whole point of the finalizer: a credential cannot be removed out from"
note "under a cluster that still needs it to reconcile, report, or terminate."

step "A second user, while the account is already terminating"
cat <<YAML | k apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $SECOND
spec:
  accountRef:
    name: $ACCOUNT
  managementPolicy: Observe
  tier: Development
  storage:
    sizeGB: 20
YAML
# The account is mid-delete, so the only thing that wakes it is one of its own
# watches. A CloudCluster naming it is one, which is why the count moves without
# anybody touching the account.
if ! wait_for cloudaccount "$ACCOUNT" \
     '.status.conditions[?(@.type=="Ready")].message' \
     "$(printf '2 CloudCluster(s) still reference this account (%s, %s). Delete them first: without this credential they cannot reconcile, and one with deletionPolicy Terminate could not terminate its cluster either.' "$HOLDER" "$SECOND")" 90; then
  warn "the message did not pick up the second cluster"
  info "message now: $(condition_message cloudaccount "$ACCOUNT" Ready)"
fi
check_contains "the count follows the second cluster" "2 CloudCluster" \
  "$(condition_message cloudaccount "$ACCOUNT" Ready)"
check_contains "and it is named too" "$SECOND" \
  "$(condition_message cloudaccount "$ACCOUNT" Ready)"
note "a watch on CloudCluster is what wakes a terminating account, so the count is live"

# --- 3. releasing it --------------------------------------------------------

step "Deleting one of the two"
k delete cloudcluster "$SECOND" --ignore-not-found --timeout=120s >/dev/null || true
wait_gone cloudcluster "$SECOND" 120 || warn "$SECOND is still around"
if ! wait_for cloudaccount "$ACCOUNT" \
     '.status.conditions[?(@.type=="Ready")].reason' InUse 60; then
  warn "unexpected: the account left InUse with one user remaining"
fi
check "still held by the first"  "$(exists cloudaccount "$ACCOUNT")" yes
check_contains "back to one user" "1 CloudCluster" \
  "$(condition_message cloudaccount "$ACCOUNT" Ready)"

step "Deleting the last one"
k delete cloudcluster "$HOLDER" --ignore-not-found --timeout=120s >/dev/null || true
wait_gone cloudcluster "$HOLDER" 120 || warn "$HOLDER is still around"

if ! wait_gone cloudaccount "$ACCOUNT" 120; then
  warn "$ACCOUNT did not release its finalizer; recent operator logs:"
  operator_logs 30
fi
check "the account is collected once its last user goes" no "$(exists cloudaccount "$ACCOUNT")"
note "no timer did that: deleting the cluster is an event the account watches, so it"
note "re-ran reconcileDelete, found nobody left, and dropped the finalizer"

# --- 4. deleting both at once -----------------------------------------------

# kubectl delete -f on a file that holds an account and a cluster it created is
# the most ordinary way to tear down, and until a review of the failure paths
# found it, it never finished. The account held on for the cluster and turned Ready False
# while it did. The cluster, with deletionPolicy Terminate, needs a Ready
# account to terminate with, so each waited for the other.
if costly "deleting an account together with a cluster it provisioned, which needs a real cluster to terminate"; then
step "Deleting an account and the cluster it provisioned, in one command"
on_mock && open_cloud_portforward
cat <<YAML | k apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: $ACCOUNT
spec:
  apiKeySecretRef:
    name: $KEY_SECRET
    key: apiKey
  endpoint: $LAB07_ENDPOINT
  defaults:
    provider: AWS
    region: eu-central-1
    allowedIPs:
      - $LAB_ALLOWED_IPS
YAML
wait_for cloudaccount "$ACCOUNT" '.status.conditions[?(@.type=="Ready")].status' True 120 >/dev/null || true

cat <<YAML | k apply -f - >/dev/null
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $TOGETHER
spec:
  accountRef:
    name: $ACCOUNT
  displayName: $TOGETHER_NAME
  deletionPolicy: Terminate
  tier: Development
  storage:
    sizeGB: 20
$(if on_cloud; then printf '  subdomain: %s\n' "$(lab_subdomain t)"; fi)
YAML
if ! wait_for_status "$TOGETHER" Running "$LAB_SETTLE_TIMEOUT"; then
  warn "$TOGETHER never reached Running; recent operator logs:"
  operator_logs 30
fi
TOGETHER_ID="$(jp cloudcluster "$TOGETHER" .status.productID)"
check_not_empty "$TOGETHER is bound to a real cluster" "$TOGETHER_ID"

info "kubectl delete cloudaccount $ACCOUNT cloudcluster $TOGETHER, as one command would"
k delete cloudaccount "$ACCOUNT" --wait=false >/dev/null 2>&1 || true
k delete cloudcluster "$TOGETHER" --wait=false >/dev/null 2>&1 || true

if ! wait_gone cloudcluster "$TOGETHER" "$LAB_CHANGE_TIMEOUT"; then
  warn "$TOGETHER never finished deleting; what it says:"
  info "Ready: $(condition_reason cloudcluster "$TOGETHER" Ready): $(condition_message cloudcluster "$TOGETHER" Ready)"
  info "$ACCOUNT Ready: $(condition cloudaccount "$ACCOUNT" Ready) $(condition_reason cloudaccount "$ACCOUNT" Ready)"
fi
check "the cluster finished deleting" no "$(exists cloudcluster "$TOGETHER")"
wait_gone cloudaccount "$ACCOUNT" 120 || warn "$ACCOUNT is still waiting"
check "and so did the account"        no "$(exists cloudaccount "$ACCOUNT")"
case "$(product_status "$TOGETHER_ID")" in
  Terminating|Terminated) pass "RavenDB Cloud is terminating $TOGETHER_ID, as deletionPolicy Terminate asked" ;;
  *) fail "RavenDB Cloud says $TOGETHER_ID is $(product_status "$TOGETHER_ID")" ;;
esac
# The stand-in keeps a terminated record the way RavenDB Cloud does, and this
# lab has no further use for it.
on_mock && retire_product "$TOGETHER_ID"
TOGETHER_ID=''
fi

step "What is left"
check "lab07 CloudClusters" "<none>" \
  "$({ k get cloudcluster -o name 2>/dev/null || true; } | { grep lab07 || echo '<none>'; })"
check "lab07 CloudAccounts" "<none>" \
  "$({ k get cloudaccount -o name 2>/dev/null || true; } | { grep lab07 || echo '<none>'; })"
cleanup_objects
check "the shared account is still Ready" True "$(condition cloudaccount ravendb-cloud Ready)"

lab_summary

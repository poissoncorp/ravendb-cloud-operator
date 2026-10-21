#!/usr/bin/env bash
# Shared helpers for the lab scripts.
#
# Sourced, not executed. Every lab script starts with:
#   source "$(dirname "$0")/lib.sh"

set -euo pipefail

LAB_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# REPO_ROOT can be pre-set, which is what lets the labs be run from a copy of
# this directory while still pointing at the real repository for manifests.
REPO_ROOT="${REPO_ROOT:-$(cd "$LAB_ROOT/.." && pwd)}"

: "${LAB_NAMESPACE:=apps}"
: "${LAB_CLOUD_NAMESPACE:=ravendb-cloud-lab}"
: "${LAB_OPERATOR_NAMESPACE:=ravendb-cloud-operator-system}"
: "${KIND_CLUSTER:=ravendb-lab}"

# --- which control plane ----------------------------------------------------
#
# These labs were written against the simulator and assumed it everywhere: in how
# long a create takes, in what a cluster may be called, and in the /_lab surface
# they reach for to manufacture a situation. LAB_TARGET is what lets one script
# run against both.
#
#   mock  (default) the simulator: free, fast, and able to stage things
#                   a real service will not arrange on request
#   cloud           a real RavenDB Cloud environment, which bills for what it
#                   creates and settles on its own schedule
: "${LAB_TARGET:=mock}"

case "$LAB_TARGET" in
  mock)
    # The simulator settles a create in 25s by default and a change at once.
    : "${LAB_SETTLE_TIMEOUT:=240}"
    : "${LAB_CHANGE_TIMEOUT:=120}"
    ;;
  cloud)
    # Real provisioning boots a VM, configures it over SSH and issues an ACME
    # certificate. Minutes, not seconds, and the variance is the point: these
    # are deadlines for giving up, not expectations.
    : "${LAB_SETTLE_TIMEOUT:=1200}"
    : "${LAB_CHANGE_TIMEOUT:=900}"
    ;;
  *)
    printf 'LAB_TARGET must be mock or cloud, got %s\n' "$LAB_TARGET" >&2
    exit 2
    ;;
esac

# LAB_RUN makes a name unique across runs, and across two people sharing one
# account. Four characters, so every subdomain a lab makes stays at eight,
# well inside RavenDB Cloud's limit.
: "${LAB_RUN:=$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 4)}"
[ -n "${LAB_RUN:-}" ] || LAB_RUN="r$(printf '%03d' $((RANDOM % 1000)))"

: "${LAB_BUDGET:=frugal}"

# Who may reach a cluster these labs create.
#
# The simulator ignores this, so the lab account has always said 0.0.0.0/0 and
# nobody had to think about it. Against a real environment that same line
# provisions a RavenDB cluster reachable from the whole internet, and inheriting
# that from a simulator is not a decision anybody made.
#
# Two facts decide what it should be instead. RavenDB Cloud requires at least
# one entry, so it cannot simply be left out: its product validation answers an
# empty list with "At least one Allowed IP is required." And no lab
# here ever connects to the cluster it creates. Lab 03 comes closest, mounting
# the Secret in a Job that reads the endpoints and parses the certificate, and
# even that never opens a connection.
#
# So the right entry is one address that reaches nobody. 192.0.2.0/24 is
# TEST-NET-1, reserved by RFC 5737 for documentation and not routed on the
# public internet, which makes it the smallest true statement available: this
# cluster admits exactly one address, and nobody is at it.
#
# Override it when you actually want to connect. RavenDB Cloud takes the list at
# creation only, so that has to be decided before the cluster exists, not after.
: "${LAB_ALLOWED_IPS:=$([ "$LAB_TARGET" = cloud ] && echo 192.0.2.1/32 || echo 0.0.0.0/0)}"

# --- output ----------------------------------------------------------------

if [ -t 1 ]; then
  C_BOLD=$'\033[1m'; C_DIM=$'\033[2m'; C_GREEN=$'\033[32m'; C_RED=$'\033[31m'
  C_YELLOW=$'\033[33m'; C_CYAN=$'\033[36m'; C_OFF=$'\033[0m'
else
  C_BOLD=''; C_DIM=''; C_GREEN=''; C_RED=''; C_YELLOW=''; C_CYAN=''; C_OFF=''
fi

step()  { printf '\n%s==> %s%s\n' "$C_BOLD$C_CYAN" "$*" "$C_OFF"; }
info()  { printf '    %s\n' "$*"; }
note()  { printf '    %s%s%s\n' "$C_DIM" "$*" "$C_OFF"; }
warn()  { printf '    %s%s%s\n' "$C_YELLOW" "$*" "$C_OFF"; }
pass()  { printf '    %sPASS%s %s\n' "$C_GREEN" "$C_OFF" "$*"; }
fail()  { printf '    %sFAIL%s %s\n' "$C_RED" "$C_OFF" "$*"; LAB_FAILURES=$((LAB_FAILURES+1)); }

LAB_FAILURES=0
LAB_CHECKS=0
LAB_SKIPS=0

# skip <why> is a reported outcome, not a silent pass. A lab that skips half its
# steps against a different control plane has to say so in the tally, or a
# narrower run looks exactly like a complete one.
skip() {
  LAB_SKIPS=$((LAB_SKIPS+1))
  printf '    %sSKIP%s %s\n' "$C_YELLOW" "$C_OFF" "$*"
}

# check <description> <expected> <actual>
check() {
  LAB_CHECKS=$((LAB_CHECKS+1))
  local what="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    pass "$what = $got"
  else
    fail "$what = '$got', expected '$want'"
  fi
}

# check_contains <description> <needle> <haystack>
check_contains() {
  LAB_CHECKS=$((LAB_CHECKS+1))
  local what="$1" needle="$2" haystack="$3"
  # An empty needle is in every string, so a check with one passes whatever
  # happens and proves nothing. That is not a thing to write on purpose, and it
  # is easy to write by accident: a variable that is only set on one control
  # plane, read on the other. Two of those reached a live run before this
  # existed, and both reported PASS.
  if [ -z "$needle" ]; then
    fail "$what was given an empty string to look for, which always matches"
    return 0
  fi
  case "$haystack" in
    *"$needle"*) pass "$what mentions '$needle'" ;;
    *)           fail "$what does not mention '$needle': $haystack" ;;
  esac
}

# check_not_empty <description> <value>
check_not_empty() {
  LAB_CHECKS=$((LAB_CHECKS+1))
  local what="$1" value="$2"
  if [ -n "$value" ]; then
    pass "$what is set"
  else
    fail "$what is empty"
  fi
}

# Every lab ends with this, so a lab that quietly did nothing still reports.
lab_summary() {
  printf '\n'
  local skipped=''
  [ "$LAB_SKIPS" -gt 0 ] && skipped=" $LAB_SKIPS skipped."
  if [ "$LAB_FAILURES" -eq 0 ]; then
    printf '%s%s checks passed.%s%s\n' "$C_GREEN$C_BOLD" "$LAB_CHECKS" "$skipped" "$C_OFF"
  else
    # A bare fail() reports a problem that was never framed as a check, so the
    # denominator has to be at least the number of failures. Otherwise a lab
    # that dies early prints the nonsense "1 of 0 checks failed".
    local total="$LAB_CHECKS"
    [ "$total" -lt "$LAB_FAILURES" ] && total="$LAB_FAILURES"
    printf '%s%s of %s checks failed.%s\n' "$C_RED$C_BOLD" "$LAB_FAILURES" "$total" "$C_OFF"
  fi
  return "$([ "$LAB_FAILURES" -eq 0 ] && echo 0 || echo 1)"
}

# --- kubernetes helpers -----------------------------------------------------

k() { kubectl -n "$LAB_NAMESPACE" "$@"; }

# LAB_ID is the object-name prefix this lab uses, and LAB_INDEX its number.
# Both come from the script's own file name, so a lab never has to declare them
# and they cannot drift from the file it lives in.
LAB_INDEX="$(basename "${BASH_SOURCE[1]:-$0}" | grep -oE '^[0-9]+' || echo 0)"
LAB_INDEX="$((10#${LAB_INDEX:-0}))"
LAB_ID="lab$(printf '%02d' "$LAB_INDEX")"

# jsonpath <resource> <name> <path>
#
# --request-timeout is what makes the deadline in wait_for binding. Without it a
# kubectl that never answers parks the polling loop indefinitely, because the
# loop only tests $SECONDS between iterations. An empty answer is already the
# "not yet" case here, so a timed-out read costs one more pass and nothing else.
jp() {
  local kind="$1" name="$2" path="$3"
  kubectl -n "$LAB_NAMESPACE" --request-timeout=10s \
    get "$kind" "$name" -o jsonpath="{$path}" 2>/dev/null || true
}

# condition <kind> <name> <type> -> the condition's status (True/False/"")
condition() {
  jp "$1" "$2" ".status.conditions[?(@.type==\"$3\")].status"
}

# condition_reason <kind> <name> <type>
condition_reason() {
  jp "$1" "$2" ".status.conditions[?(@.type==\"$3\")].reason"
}

# condition_message <kind> <name> <type>
condition_message() {
  jp "$1" "$2" ".status.conditions[?(@.type==\"$3\")].message"
}

# wait_for <kind> <name> <jsonpath> <expected> [timeout-seconds]
# Polls rather than using "kubectl wait --for=jsonpath", which fails at once on
# an object that does not exist yet, and labs often wait for one to appear.
wait_for() {
  local kind="$1" name="$2" path="$3" want="$4" timeout="${5:-180}"
  local deadline=$((SECONDS + timeout)) got=''
  info "waiting for $kind/$name $path == $want (up to ${timeout}s)"
  while [ "$SECONDS" -lt "$deadline" ]; do
    got="$(jp "$kind" "$name" "$path")"
    if [ "$got" = "$want" ]; then
      info "reached after $((timeout - (deadline - SECONDS)))s"
      return 0
    fi
    sleep 2
  done
  warn "timed out with $path = '$got'"
  return 1
}

# The two words kubectl get prints for a CloudCluster. STATUS is the reason on
# the Ready condition, CHANGES the reason on Synced, so a lab waits on and
# asserts exactly what a person reads in the table.
READY_REASON='.status.conditions[?(@.type=="Ready")].reason'
SYNCED_REASON='.status.conditions[?(@.type=="Synced")].reason'

# wait_for_status <cloudcluster> <STATUS> [timeout]
wait_for_status() { wait_for cloudcluster "$1" "$READY_REASON" "$2" "${3:-180}"; }

# wait_for_changes <cloudcluster> <CHANGES> [timeout]
wait_for_changes() { wait_for cloudcluster "$1" "$SYNCED_REASON" "$2" "${3:-180}"; }

# status_of / changes_of <cloudcluster> -> the STATUS or CHANGES column
status_of()  { condition_reason cloudcluster "$1" Ready; }
changes_of() { condition_reason cloudcluster "$1" Synced; }

# restart_status <cloudclusterrestart> -> its STATUS column, the Complete reason
restart_status() { condition_reason cloudclusterrestart "$1" Complete; }

# wait_for_set <kind> <name> <jsonpath> [timeout-seconds]
# Waits until the field has any value at all, for the ones a lab cannot know in
# advance: a product id, a timestamp.
wait_for_set() {
  local kind="$1" name="$2" path="$3" timeout="${4:-180}"
  local deadline=$((SECONDS + timeout))
  info "waiting for $kind/$name $path to be set (up to ${timeout}s)"
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ -n "$(jp "$kind" "$name" "$path")" ] && return 0
    sleep 2
  done
  warn "timed out with $path still empty"
  return 1
}

# wait_for_one_of <kind> <name> <jsonpath> <timeout> <value>...
# Waits until the field matches any of the given values and echoes the one it
# saw. Use this instead of wait_for when a transient state may already have
# passed: waiting for exactly the intermediate value would otherwise block
# until the timeout even though the end state is already there.
wait_for_one_of() {
  local kind="$1" name="$2" path="$3" timeout="$4"; shift 4
  local deadline=$((SECONDS + timeout)) got='' want
  info "waiting for $kind/$name $path in [$*] (up to ${timeout}s)"
  while [ "$SECONDS" -lt "$deadline" ]; do
    got="$(jp "$kind" "$name" "$path")"
    for want in "$@"; do
      if [ "$got" = "$want" ]; then
        printf '%s' "$got"
        return 0
      fi
    done
    sleep 2
  done
  warn "timed out with $path = '$got'"
  return 1
}

# wait_exists <kind> <name> [timeout]
wait_exists() {
  local kind="$1" name="$2" timeout="${3:-90}"
  local deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    kubectl -n "$LAB_NAMESPACE" get "$kind" "$name" >/dev/null 2>&1 && return 0
    sleep 2
  done
  return 1
}

# wait_gone <kind> <name> [timeout]
wait_gone() {
  local kind="$1" name="$2" timeout="${3:-120}"
  local deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    kubectl -n "$LAB_NAMESPACE" get "$kind" "$name" >/dev/null 2>&1 || return 0
    sleep 2
  done
  return 1
}

# exists <kind> <name> -> yes | no
exists() {
  if kubectl -n "$LAB_NAMESPACE" get "$1" "$2" >/dev/null 2>&1; then echo yes; else echo no; fi
}

# --- naming and portability -------------------------------------------------

on_cloud() { [ "$LAB_TARGET" = cloud ]; }
on_mock()  { [ "$LAB_TARGET" = mock ]; }

# lab_subdomain <slot> -> a short DNS label, unique per lab, per cluster within
# a lab, and per run: l, two digits, slot and run, eight characters in all.
lab_subdomain() { printf 'l%02d%s%s' "$LAB_INDEX" "$1" "$LAB_RUN"; }

# lab_display <slot> -> a portal-visible name nothing else on the account shares.
#
# The display name is the identity: a CloudCluster takes over whatever cluster
# carries its name, and RavenDB Cloud refuses a create whose name is held by any
# product not hidden in the portal, terminated ones included. So a fixed name
# collides with the previous run rather than with somebody else.
lab_display() { printf '%s-%s-%s' "$LAB_ID" "$1" "$LAB_RUN"; }

# cloud_names <slot> -> the spec lines a real environment needs and the
# simulator does not, ready to paste at the end of a CloudCluster spec.
#
# Emitted rather than always set, so the manifest a guide shows stays the
# smallest one that works when the guide is read against the simulator.
cloud_names() {
  on_cloud || return 0
  printf '  subdomain: %s\n' "$(lab_subdomain "$1")"
  printf '  displayName: %s\n' "$(lab_display "$1")"
}

# costly <what> -> true when a step that spends real money should run.
#
# Every lesson here can be taught by one small Development cluster. A few steps
# reach for a second one, or for a Production shape, and against a real account
# that is a bill rather than a detail. The default keeps a cloud run cheap and
# says out loud what it skipped; LAB_BUDGET=full runs the lot.
costly() {
  on_mock && return 0
  [ "${LAB_BUDGET}" = full ] && return 0
  skip "$1 (LAB_BUDGET=$LAB_BUDGET; set LAB_BUDGET=full to spend for it)"
  return 1
}

# check_calls is check for a claim about how many times the operator called the
# API. Only a control plane that keeps a request log can answer that, so on a
# real one this reports a skip rather than comparing two zeroes and passing,
# which is the shape of a green check that proves nothing.
check_calls() {
  on_mock && { check "$@"; return 0; }
  skip "$1 (needs the simulator's request log)"
}

# --- asking the control plane directly --------------------------------------
#
# A lab asks these when there is no CloudCluster left to read status from, which
# is exactly the moment worth asking about: did deleting the object destroy the
# database, is that name taken, how many clusters does the account hold. The
# simulator answers from its /_lab surface; a real environment answers from the
# same endpoints the operator itself uses.

# cloud_get <path> -> the body, or non-zero having said why.
#
# A lab answered with a 429 does not get an error, it gets the 429 body, which
# parses to an empty string and reads as "no such cluster" or "that name is
# free". Both are the opposite of the truth, and both would be silent.
#
# So a 429 is retried, and any other failure is a failure rather than an
# answer.
cloud_get() {
  local path="$1" attempt=0 code out
  out="$(mktemp)"
  while [ "$attempt" -lt 5 ]; do
    attempt=$((attempt + 1))
    code="$(curl -s --max-time 25 -o "$out" -w '%{http_code}' \
      -H "X-API-Key: $(cat "$LAB_CLOUD_API_KEY_FILE")" \
      "${LAB_CLOUD_ENDPOINT}${path}" 2>/dev/null)" || code=''
    case "$code" in
      200) cat "$out"; rm -f "$out"; return 0 ;;
      429) sleep 1.3 ;;
      *)
        # Warnings go to stderr: a caller reads this through $(...) and a
        # warning on stdout would be parsed as if it were the answer.
        printf '    %sGET %s answered %s%s\n' "$C_YELLOW" "$path" "${code:-no response}" "$C_OFF" >&2
        rm -f "$out"; return 1 ;;
    esac
  done
  printf '    %sGET %s was throttled %s times running%s\n' "$C_YELLOW" "$path" "$attempt" "$C_OFF" >&2
  rm -f "$out"; return 1
}

# cloud_products -> one "id name" line per cluster on the account.
#
# The two control planes answer different shapes, which is worth knowing rather
# than hiding: the simulator's /_lab surface returns full cluster records, while
# products/list on a real environment returns only {"id","name"} per entry.
# Status is in neither list, so ask product_status for that. Terminated clusters
# stay listed forever on a real account.
cloud_products() {
  if on_mock; then
    lab_api GET /_lab/clusters \
      | tr '{' '\n' \
      | sed -n 's/.*"id":"\([^"]*\)","subdomainName":"[^"]*","displayName":"\([^"]*\)".*/\1 \2/p' || true
  else
    cloud_get /api/v1/products/list \
      | tr '{' '\n' \
      | sed -n 's/.*"id":"\([^"]*\)","name":"\([^"]*\)".*/\1 \2/p'
  fi
}

# product_status <product-id> -> what RavenDB Cloud says about that cluster, or
# empty when it has no record of it.
#
# This carries lab 06's whole point: after kubectl delete, is the cluster still
# running. products/details is what the operator polls for the same answer.
product_status() {
  if on_mock; then
    lab_api GET /_lab/clusters \
      | tr '{' '\n' \
      | grep -F "\"id\":\"$1\"," \
      | sed -n 's/.*"status":"\([^"]*\)".*/\1/p' \
      | awk 'NR == 1 { print }' || true
  else
    cloud_get "/api/v1/products/details/$1" \
      | sed -n 's/.*"status":"\([^"]*\)".*/\1/p' \
      | awk 'NR == 1 { print }'
  fi
}

# product_ids_named <display-name> -> ids carrying exactly that display name.
#
# Terminated clusters stay in the product list forever on a real account, which
# is why every name a lab creates carries a run id: without one this would count
# last week's run as a duplicate of this week's.
product_ids_named() {
  cloud_products | awk -v want="$1" '$2 == want { print $1 }'
}

# retire_product <product-id> -> make sure the control plane is not still
# running this cluster, whatever Kubernetes thinks.
#
# A lab that demonstrates Retain leaves a cluster running on purpose, and the
# CloudCluster that could have reaped it is gone by then. On the simulator that
# costs nothing and /_lab makes it vanish. On a real account it is a bill nobody
# is watching, so this asks RavenDB Cloud to terminate it directly, hidden once
# it is terminated, as the operator's own terminate asks.
#
# The terminate endpoint takes a body: a bodyless POST is answered 411 Length
# Required rather than accepted, which is not in the OpenAPI document.
retire_product() {
  [ -n "${1:-}" ] || return 0
  if on_mock; then
    lab_api POST "/_lab/clusters/$1/vanish" >/dev/null 2>&1 || true
    return 0
  fi
  curl -s -o /dev/null --max-time 25 -X POST \
    -H "X-API-Key: $(cat "$LAB_CLOUD_API_KEY_FILE")" \
    -H "Content-Type: application/json" \
    --data '{"hideInPortal":true}' \
    "${LAB_CLOUD_ENDPOINT}/api/v1/products/terminate/$1" >/dev/null 2>&1 || true
  sleep 1.3
}

# cloud_listed <product-id> -> yes while the cluster is in the account's
# product list. A cluster the operator terminates is hidden once termination
# completes, and a hidden cluster leaves the list, which is what frees its name.
cloud_listed() {
  if cloud_products | awk -v id="$1" '$1 == id { found = 1 } END { exit !found }'; then
    echo yes
  else
    echo no
  fi
}

# cloud_ended <product-id> -> yes once RavenDB Cloud is terminating the
# cluster, has terminated it, or has terminated and hidden it. The simulator's
# list is the only place a lab can ask, and a hidden cluster is not in it, so
# there "not listed" is one of the answers. A real environment answers for a
# hidden cluster by id.
cloud_ended() {
  case "$(product_status "$1")" in
    Terminating|Terminated) echo yes ;;
    '') if on_mock; then echo yes; else echo no; fi ;;
    *) echo no ;;
  esac
}

# wait_cloud_unlisted <product-id> [timeout] -> waits until the cluster has
# left the product list: terminated and hidden.
wait_cloud_unlisted() {
  local id="$1" limit="${2:-90}" gap=2
  if on_cloud; then
    gap=6
    [ "$limit" -lt "$LAB_CHANGE_TIMEOUT" ] && limit="$LAB_CHANGE_TIMEOUT"
  fi
  local deadline=$((SECONDS + limit))
  info "waiting for cloud cluster $id to be terminated and hidden (up to ${limit}s)"
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(cloud_listed "$id")" = no ] && return 0
    sleep "$gap"
  done
  warn "cloud cluster $id is still listed, as $(product_status "$id")"
  return 1
}

# needs_mock <what> -> true when the step can run here.
#
# Some of what these labs show is only arrangeable against the simulator: a
# cluster deleted behind the operator back, a portal edit, an API that fails
# once. Skipping is reported, never silent, because a step that vanishes without
# saying so turns a narrower run into an identical-looking one.
needs_mock() {
  on_mock && return 0
  skip "$1 (needs the simulator; production has no equivalent)"
  return 1
}

# release <cloudcluster>... -> delete the objects and leave no bill behind.
#
# deletionPolicy defaults to Retain, which is right for a user and wrong for a
# lab that just created a cluster nobody asked to keep. On the simulator this is
# free either way; on a real account it is the difference between tidying up and
# walking away from a running cluster.
release() {
  local name
  for name in "$@"; do
    if on_cloud; then
      k patch cloudcluster "$name" --type=merge \
        -p '{"spec":{"deletionPolicy":"Terminate"}}' >/dev/null 2>&1 || true
    fi
    k delete cloudcluster "$name" --ignore-not-found --timeout=600s >/dev/null 2>&1 || true
  done
}

# secret_value <secret> <key> -> decoded value
secret_value() {
  kubectl -n "$LAB_NAMESPACE" get secret "$1" -o jsonpath="{.data.$2}" 2>/dev/null | base64 -d 2>/dev/null || true
}

# --- simulator helpers ---------------------------------------------
#
# These reach the mock's /_lab surface, which is how a lab simulates something
# happening outside Kubernetes: a portal edit, a deletion, an API outage.

# lab_api <method> <path> [body]
#
# kubectl port-forward drops connections now and then, and a lab that silently
# turns a dropped connection into an empty response produces a wrong answer
# rather than an error. So this retries, and it returns non-zero if it never
# got through, which is what lets callers tell "no matches" apart from
# "could not ask".
lab_api() {
  local method="$1" path="$2" body="${3:-}"
  # Scope request-log reads and resets to this lab. The simulator attributes
  # each logged call to the cluster it concerns, so two labs sharing one
  # control plane still measure only their own traffic. Without this a reset in
  # one lab wiped another's measurements mid-run, which cost real debugging
  # time before the labs ever ran concurrently.
  case "$path" in
    /_lab/requests*|/_lab/clusters\?*|/_lab/clusters)
      if [ "${path#*lab=}" = "$path" ]; then
        if [ "${path#*\?}" = "$path" ]; then path="$path?lab=$LAB_ID"; else path="$path&lab=$LAB_ID"; fi
      fi
      ;;
  esac
  local args=(-s --connect-timeout 5 --max-time 20
              -X "$method" "http://localhost:${LAB_PORT:-18080}${path}")
  if [ -n "$body" ]; then
    args+=(-H 'Content-Type: application/json' -d "$body")
  fi
  local attempt out
  for attempt in 1 2 3 4; do
    if out="$(curl "${args[@]}" 2>/dev/null)"; then
      printf '%s' "$out"
      return 0
    fi
    # A long lab can outlive its port-forward. Rebuild it once rather than
    # letting every later measurement quietly read as zero.
    if [ "$attempt" = 2 ] && [ -n "$LAB_PF_PID" ]; then
      note "the port-forward went away, reopening it"
      close_cloud_portforward
      open_cloud_portforward >/dev/null 2>&1 || true
    fi
    sleep 1
  done
  warn "simulator did not answer $method $path after 4 attempts"
  return 1
}

# Opens a port-forward to the mock control plane and closes it on exit.
LAB_PF_PID=''

# try_portforward -> true once the simulator answers through a forward on
# LAB_PORT. False as soon as it is clear it never will on that port: the
# forward could not bind, or the port answers but not as the simulator, which is
# another program listening there.
try_portforward() {
  kubectl -n "$LAB_CLOUD_NAMESPACE" port-forward svc/mockcloud "${LAB_PORT}:80" >/dev/null 2>&1 &
  LAB_PF_PID=$!
  trap close_cloud_portforward EXIT
  local deadline=$((SECONDS + 30)) code
  while [ "$SECONDS" -lt "$deadline" ]; do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://localhost:${LAB_PORT}/_lab/healthz" 2>/dev/null)" || true
    case "$code" in
      200) return 0 ;;
      000|'') kill -0 "$LAB_PF_PID" 2>/dev/null || return 1 ;;
      *) return 1 ;;
    esac
    sleep 1
  done
  return 1
}

open_cloud_portforward() {
  # One port per lab, so labs can run at the same time. Derived from the file
  # name, because a fixed port meant the second lab silently attached to the
  # first one's forward or failed to bind. LAB_PORT_BASE moves the whole range.
  : "${LAB_PORT:=$((${LAB_PORT_BASE:-18080} + LAB_INDEX))}"
  # A port another program already listens on would send this lab's control
  # plane calls to a stranger, so a port that is taken is skipped, in steps of
  # ten, which keeps labs that run at the same time apart.
  local attempt
  for attempt in 1 2 3 4 5; do
    if try_portforward; then
      note "port-forward to the simulator on localhost:${LAB_PORT}"
      return 0
    fi
    close_cloud_portforward
    LAB_PORT=$((LAB_PORT + 10))
    note "that port is taken, trying localhost:${LAB_PORT}"
  done
  echo "could not reach the simulator through the port-forward" >&2
  return 1
}

close_cloud_portforward() {
  if [ -n "$LAB_PF_PID" ]; then
    kill "$LAB_PF_PID" 2>/dev/null || true
    LAB_PF_PID=''
  fi
}

# require_ready_account fails early with a useful message instead of letting a
# lab produce a confusing timeout.
require_ready_account() {
  require_current_crds
  if [ "$(condition cloudaccount ravendb-cloud Ready)" != "True" ]; then
    echo "CloudAccount 'ravendb-cloud' is not Ready. Run labs/00_setup.sh first." >&2
    exit 1
  fi
}

# require_current_crds refuses to run a lab against a schema older than the
# source tree.
#
# This is worth a check rather than a convention because of how it fails
# otherwise. Rename a CRD field, rebuild the images, restart the deployments,
# and every lab that uses the new name dies with "unknown field", which reads
# like a lab bug or an operator bug and is neither. It cost real time before it
# was understood, twice.
#
# kubectl diff exits 1 when the live object differs, and >1 on an actual error,
# so a missing CRD or an unreachable cluster is reported rather than mistaken
# for drift.
require_current_crds() {
  # "|| rc=$?": under set -e a bare failing assignment ends the script before
  # rc is read, which hid exactly the messages below.
  local out rc=0
  out="$(kubectl diff -f "$REPO_ROOT/config/crd/bases" 2>&1)" || rc=$?
  case "$rc" in
    0) return 0 ;;
    1)
      echo "The CRDs in the cluster are older than config/crd/bases." >&2
      echo "Run 'make install' to apply them, or labs/00_setup.sh for everything." >&2
      printf '%s\n' "$out" | head -20 >&2
      exit 1
      ;;
    *)
      echo "Could not compare CRDs against the cluster:" >&2
      printf '%s\n' "$out" | head -5 >&2
      exit 1
      ;;
  esac
}

# operator_logs prints recent operator logs, which is what you actually want
# when a lab check fails.
operator_logs() {
  kubectl -n "$LAB_OPERATOR_NAMESPACE" logs deploy/ravendb-cloud-operator-controller-manager --tail="${1:-40}" 2>/dev/null || true
}

#!/usr/bin/env bash
# Run every lab in order and report a single tally.
#
#   labs/run_all.sh              run labs 01..07 against the current cluster
#   labs/run_all.sh --setup      run 00_setup.sh first
#
# Each lab is self-checking and cleans up after itself, so this is also the
# regression suite for the labs themselves: if the operator changes in a way
# that breaks a documented claim, a lab fails here.
source "$(dirname "$0")/lib.sh"

if [ "${1:-}" = "--setup" ]; then
  "$LAB_ROOT/00_setup.sh" || exit 1
fi

mapfile -t SCRIPTS < <(find "$LAB_ROOT" -maxdepth 1 -name '[0-9][0-9]_*.sh' \
  ! -name '00_setup.sh' ! -name 'run_all.sh' | sort)

if [ "${#SCRIPTS[@]}" -eq 0 ]; then
  echo "no lab scripts found in $LAB_ROOT" >&2
  exit 1
fi

# PASSED, FAILED and TIMINGS are assembled below, from files the concurrent
# runs append to, because a background job cannot write to a parent array.
START=$SECONDS

# Lab 01 runs on its own, first, for two reasons. It leaves the CloudAccount
# Ready, which everything after it needs, and it measures account and metadata
# traffic, which belongs to no cluster and so cannot be attributed to one lab.
#
# The rest run concurrently. They share a cluster, an operator and a control
# plane, but not their measurements: each gets its own port-forward, and the
# simulator attributes every logged call to the cluster it concerns, so a reset
# in one lab no longer wipes another's numbers.
#
# LAB_SERIAL=1 goes back to one at a time, which is easier to read when a lab is
# failing and you want its output uninterleaved.
TIMING_FILE="$(mktemp)"
FAILED_FILE="$(mktemp)"
trap 'rm -f "$TIMING_FILE" "$FAILED_FILE"' EXIT

run_one() {
  local script="$1" name log start rc
  name="$(basename "$script")"
  log="$(mktemp)"
  start=$SECONDS
  if bash "$script" >"$log" 2>&1; then rc=0; else rc=1; fi
  # Buffered, then printed in one go, so concurrent labs do not interleave.
  printf '
%s================================================================
  %s
================================================================%s
'     "$C_BOLD" "$name" "$C_OFF"
  cat "$log"
  rm -f "$log"
  printf '%s %s
' "$name" "$((SECONDS - start))" >>"$TIMING_FILE"
  [ "$rc" -eq 0 ] || printf '%s
' "$name" >>"$FAILED_FILE"
  return 0
}

FIRST=()
REST=()
for script in "${SCRIPTS[@]}"; do
  case "$(basename "$script")" in
    01_*) FIRST+=("$script") ;;
    *)    REST+=("$script") ;;
  esac
done

for script in "${FIRST[@]}"; do
  run_one "$script"
done

# Against a real environment the labs run one at a time, so they do not compete
# with each other for the API.
if [ "${LAB_TARGET:-mock}" = cloud ] && [ "${LAB_SERIAL:-0}" != "1" ]; then
  note "LAB_TARGET=cloud, so the labs run one at a time"
  LAB_SERIAL=1
fi

if [ "${LAB_SERIAL:-0}" = "1" ] || [ "${#REST[@]}" -le 1 ]; then
  for script in "${REST[@]}"; do run_one "$script"; done
else
  for script in "${REST[@]}"; do run_one "$script" & done
  wait
fi

FAILED=()
if [ -s "$FAILED_FILE" ]; then mapfile -t FAILED < <(sort -u "$FAILED_FILE"); fi
PASSED=()
for script in "${SCRIPTS[@]}"; do
  name="$(basename "$script")"
  if [ "${#FAILED[@]}" -eq 0 ] || ! printf '%s
' "${FAILED[@]}" | grep -qx "$name"; then
    PASSED+=("$name")
  fi
done
TIMINGS=()
if [ -s "$TIMING_FILE" ]; then
  mapfile -t TIMINGS < <(sort -k2 -rn "$TIMING_FILE" | awk '{printf "  %5ss  %s\n", $2, $1}')
fi

printf '\n%s================================================================%s\n' "$C_BOLD" "$C_OFF"
printf '%sLabs: %d passed, %d failed, %ds elapsed%s\n' \
  "$C_BOLD" "${#PASSED[@]}" "${#FAILED[@]}" "$((SECONDS - START))" "$C_OFF"
# Per-lab timing, because a total on its own cannot answer "which one was
# slow", and the answer to that has already turned out to matter.
printf '%s\n' "${TIMINGS[@]}"
for name in "${FAILED[@]}"; do
  printf '  %sFAIL%s %s\n' "$C_RED" "$C_OFF" "$name"
done

[ "${#FAILED[@]}" -eq 0 ]

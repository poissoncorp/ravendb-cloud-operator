#!/usr/bin/env bash
# Lab 03: connect an application to the cluster.
#
# Shows the connection Secret, which is the reason to run the operator at all:
# all eleven keys and what each is for, the certificate proved real with
# openssl, a pod inside the cluster mounting it, the ownerReference that
# garbage collects it, why the certificate is downloaded once rather than on
# every poll, and what renaming it or switching the certificate off does.
#
#   labs/03_connect_an_application.sh
#
# Run from the repository root. labs/00_setup.sh must have run first.
# Needs openssl on PATH. The pod step needs the busybox image; the script
# loads it into the kind node from local Docker when it can, and says so and
# skips that one step when the image cannot be had.
source "$(dirname "$0")/lib.sh"

CLUSTER=lab03-app
SECRET="lab03-app-connection"
SECRET_RENAMED="lab03-app-endpoints"
SECRET_NOCERT="lab03-app-urls-only"
SQUATTER="lab03-squatter"
JOB="lab03-reader"
BUSYBOX_IMAGE="${LAB03_IMAGE:-busybox:1.36}"

# What the simulator always produces. A real environment produces its own, so
# these are re-derived from the cluster once it is Ready and the checks below
# assert the relationships rather than the strings.
NODE_URL="https://a.lab03-app.acme.ravendb.cloud"
ALL_URLS="https://a.lab03-app.acme.ravendb.cloud,https://b.lab03-app.acme.ravendb.cloud,https://c.lab03-app.acme.ravendb.cloud"
NODE_TAGS="A,B,C"
STUDIO="${NODE_URL}/studio/index.html"
CERT_CN="lab03-app.acme.ravendb.cloud.client.certificate"
PFX_PASSWORD="lab-pfx-password"

# derive_expectations reads back what this environment actually built.
CERT_SUBJECT_NEEDLE="lab03-app.acme.ravendb.cloud.client.certificate"

derive_expectations() {
  on_mock && return 0
  CERT_SUBJECT_NEEDLE="$(lab_subdomain a)"
  NODE_URL="$(jp cloudcluster "$CLUSTER" '.status.urls[0]')"
  ALL_URLS="$(jp cloudcluster "$CLUSTER" '.status.urls[*]' | tr ' ' ',')"
  NODE_TAGS="$(jp cloudcluster "$CLUSTER" '.status.nodeTags[*]' | tr ' ' ',')"
  STUDIO="${NODE_URL}/studio/index.html"
  PFX_PASSWORD="$(secret_value "$SECRET" 'certificate\\.password')"
}

EXPECTED_KEYS="certificate.password client.pem client.pfx client.with-password.pfx nodeTags productID studioURL tls.crt tls.key url urls"
EXPECTED_KEYS_NOCERT="nodeTags productID studioURL url urls"

LAB03_TMP=''

# --- helpers ---------------------------------------------------------------

lab03_cleanup() {
  # Remove the seeded cluster from the control plane before the port-forward
  # closes, otherwise a re-run leaves a second cluster with the same name.
  if on_mock; then
    for id in ${PRODUCT_ID:-} ${SQUAT_ID:-}; do
      lab_api POST "/_lab/clusters/${id}/vanish" >/dev/null 2>&1 || true
    done
  fi
  close_cloud_portforward
  if [ -n "${LAB03_TMP:-}" ]; then rm -rf "$LAB03_TMP" 2>/dev/null || true; fi
  kubectl -n "$LAB_NAMESPACE" delete job "$JOB" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
  kubectl -n "$LAB_NAMESPACE" delete cloudcluster "$SQUATTER" --ignore-not-found --timeout=90s >/dev/null 2>&1 || true
  kubectl -n "$LAB_NAMESPACE" delete cloudcluster "$CLUSTER" --ignore-not-found --timeout=150s >/dev/null 2>&1 || true
  kubectl -n "$LAB_NAMESPACE" delete secret "$SECRET" "$SECRET_RENAMED" "$SECRET_NOCERT" \
    --ignore-not-found >/dev/null 2>&1 || true
}

# secret_keys <secret> -> one key per line, C-sorted
secret_keys() {
  kubectl -n "$LAB_NAMESPACE" get secret "$1" \
    -o go-template='{{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}' 2>/dev/null |
    sed '/^$/d' | LC_ALL=C sort
}

# secret_key_line <secret> -> the keys on one line, space separated
secret_key_line() {
  local line
  line="$(secret_keys "$1" | tr '\n' ' ' | sed 's/ *$//')" || true
  printf '%s' "$line"
}

key_purpose() {
  case "$1" in
    url)                      echo "first node endpoint" ;;
    urls)                     echo "every node endpoint, comma separated" ;;
    nodeTags)                 echo "RavenDB node tags, aligned with urls" ;;
    studioURL)                echo "RavenDB Studio address" ;;
    productID)                echo "RavenDB Cloud cluster id" ;;
    tls.crt)                  echo "client certificate, PEM" ;;
    tls.key)                  echo "client private key, PEM" ;;
    client.pem)               echo "certificate and key in one PEM file" ;;
    client.pfx)               echo "PKCS#12, no password" ;;
    client.with-password.pfx) echo "PKCS#12, protected" ;;
    certificate.password)     echo "password for the protected PKCS#12" ;;
    certificate.bundle)       echo "raw ZIP, only written when unpacking failed" ;;
    *)                        echo "unexpected key" ;;
  esac
}

# count_calls <path-substring> -> how many API calls the control plane logged.
# Zero without asking when there is no request log, so a caller embedded in a
# check argument costs nothing rather than a retrying HTTP call that cannot work.
count_calls() {
  local out n
  on_mock || { printf '0'; return 0; }
  out="$(lab_api GET "/_lab/requests?path=$1")" || out='[]'
  n="$(printf '%s' "$out" | tr ',' '\n' | grep -c '"method"' || true)"
  printf '%s' "${n:-0}"
}

certificate_calls() { count_calls "security/certificate/${PRODUCT_ID}"; }
details_calls()     { count_calls "products/details/${PRODUCT_ID}"; }

# seeded_ids <displayName> -> the control plane ids carrying that display name.
# There is no jq in the lab image, so this puts one JSON object per line and
# reads the id with sed. /_lab/clusters emits "id" before "displayName", so the
# id on a matching line is that cluster's own.
seeded_ids() {
  lab_api GET /_lab/clusters 2>/dev/null |
    sed 's/},{/}\n{/g' |
    grep "\"displayName\":\"$1\"" |
    sed -n 's/^[^{]*{"id":"\([^"]*\)".*/\1/p'
}

cert_field() {
  openssl x509 -in "$LAB03_TMP/tls.crt" -noout "$1" 2>/dev/null || true
}

# Best effort: put busybox in the kind node so the kubelet needs no registry.
preload_busybox() {
  kubectl config current-context 2>/dev/null | grep -q '^kind-' || return 0
  command -v docker >/dev/null 2>&1 || return 0
  local kb=''
  if   [ -x "$REPO_ROOT/bin/kind" ];     then kb="$REPO_ROOT/bin/kind"
  elif [ -x "$REPO_ROOT/bin/kind.exe" ]; then kb="$REPO_ROOT/bin/kind.exe"
  elif command -v kind >/dev/null 2>&1;  then kb="kind"
  else return 0
  fi
  docker image inspect "$BUSYBOX_IMAGE" >/dev/null 2>&1 ||
    docker pull "$BUSYBOX_IMAGE" >/dev/null 2>&1 ||
    return 0
  "$kb" load docker-image "$BUSYBOX_IMAGE" --name "$KIND_CLUSTER" >/dev/null 2>&1 || return 0
  note "loaded $BUSYBOX_IMAGE into the kind node, so the kubelet needs no registry"
}

# --- 1. prerequisites -------------------------------------------------------

step "Checking prerequisites"
require_ready_account
if ! command -v openssl >/dev/null 2>&1; then
  echo "this lab parses the published certificate and needs openssl on PATH" >&2
  exit 1
fi
info "CloudAccount ravendb-cloud is Ready, openssl is $(openssl version 2>/dev/null | head -1)"
LAB03_TMP="$(mktemp -d)"
on_mock && open_cloud_portforward
trap lab03_cleanup EXIT

step "Clearing anything left over from an earlier run"
kubectl -n "$LAB_NAMESPACE" delete job "$JOB" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
kubectl -n "$LAB_NAMESPACE" delete cloudcluster "$SQUATTER" --ignore-not-found --timeout=90s >/dev/null 2>&1 || true
kubectl -n "$LAB_NAMESPACE" delete cloudcluster "$CLUSTER" --ignore-not-found --timeout=150s >/dev/null 2>&1 || true
kubectl -n "$LAB_NAMESPACE" delete secret "$SECRET" "$SECRET_RENAMED" "$SECRET_NOCERT" \
  --ignore-not-found >/dev/null 2>&1 || true
# A run killed before its EXIT trap leaves its seeded cluster in the control
# plane. Nothing in Kubernetes owns it, so clear it here, or the next run finds
# two clusters called lab03-app and, rightly, binds to neither. A real account
# needs no equivalent: names carry a run id, so a leftover from last time cannot
# be mistaken for this one.
if on_mock; then
  while read -r stale; do
    [ -n "$stale" ] || continue
    lab_api POST "/_lab/clusters/${stale}/vanish" >/dev/null 2>&1 || true
    note "removed a leftover control plane cluster ($stale)"
  done < <(seeded_ids "$CLUSTER"; seeded_ids "$SQUATTER")
fi
note "starting from a clean namespace"

# --- 2. a cluster to connect to --------------------------------------------

step "A cluster to connect to"
# This lab is about the Secret, not about how the cluster got there. Against the
# simulator it seeds a three-node cluster called lab03-app and applies a
# CloudCluster by that name, which takes it over: that keeps the Secret's
# multi-node shape in view and leaves taking over itself to lab 04. A real
# account has nothing to seed into and would bill for a Production shape, so
# there it creates the smallest Development cluster instead. The Secret is the
# same object either way, which is the point being made.
if on_mock; then
  SEED_ID="$(lab_api POST /_lab/clusters \
    '{"displayName":"lab03-app","tier":"Production","instanceType":"PB10","region":"eu-central-1"}' |
    sed -n 's/.*"ids":\["\([^"]*\)".*/\1/p')" || SEED_ID=''
  if [ -z "$SEED_ID" ]; then
    echo "could not seed a cluster in the simulator" >&2
    exit 1
  fi
  PRODUCT_ID="$SEED_ID"
  info "seeded PB10 cluster $PRODUCT_ID (three nodes) as 'lab03-app'"
  kubectl apply -n "$LAB_NAMESPACE" -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $CLUSTER
spec:
  accountRef:
    name: ravendb-cloud
  tier: Production
  # Faster than the 5m default, so this lab can watch several polls go by.
  pollIntervalSeconds: 5
YAML
else
  info "provisioning a Development cluster; this takes minutes, not seconds"
  kubectl apply -n "$LAB_NAMESPACE" -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $CLUSTER
spec:
  accountRef:
    name: ravendb-cloud
  pollIntervalSeconds: 30
  tier: Development
  storage:
    sizeGB: 20
$(cloud_names a)
YAML
fi

if ! wait_for_status "$CLUSTER" Running "$LAB_SETTLE_TIMEOUT"; then
  warn "the cluster did not reach Running; recent operator logs:"
  operator_logs 40
  lab_summary
  exit 1
fi
k get cloudcluster "$CLUSTER" || true

on_cloud && PRODUCT_ID="$(jp cloudcluster "$CLUSTER" .status.productID)"
check_not_empty "status.productID" "$PRODUCT_ID"
if on_mock; then
  check "the id it took over is the one that was seeded" "$SEED_ID" "$PRODUCT_ID"
  # Found, not created, so it defaults to Observe. Publishing the Secret does
  # not look at the policy: Observe changes nothing in RavenDB Cloud, and
  # writing a Secret in this namespace is not a change there.
  check "managementPolicy for a cluster it found" Observe "$(jp cloudcluster "$CLUSTER" .status.managementPolicy)"
fi

check "STATUS"                       Running        "$(status_of "$CLUSTER")"
# Running means published: the message says which Secret holds what.
check_contains "the Ready message names the Secret" \
  "Secret \"$SECRET\" holds its URLs and client certificate" "$(condition_message cloudcluster "$CLUSTER" Ready)"
check "status.connectionSecretName"  "$SECRET"      "$(jp cloudcluster "$CLUSTER" .status.connectionSecretName)"
check_not_empty "status.certificateRefreshedAt" "$(jp cloudcluster "$CLUSTER" .status.certificateRefreshedAt)"

# --- 3. the eleven keys -----------------------------------------------------

step "The eleven keys in $SECRET"
printf '    %-26s %s\n' "KEY" "WHAT IT IS"
while read -r key; do
  [ -n "$key" ] || continue
  printf '    %-26s %s\n' "$key" "$(key_purpose "$key")"
done < <(secret_keys "$SECRET")

KEY_LINE="$(secret_key_line "$SECRET")"
KEY_COUNT="$(secret_keys "$SECRET" | wc -l | tr -d ' ')" || KEY_COUNT=0
check "number of keys"       11                "$KEY_COUNT"
check "the key set"          "$EXPECTED_KEYS"  "$KEY_LINE"
check "Secret type"          Opaque            "$(jp secret "$SECRET" .type)"
check "cluster label"        "$CLUSTER"        "$(jp secret "$SECRET" '.metadata.labels.cloud\.ravendb\.net/cluster')"
# certificate.bundle is the twelfth key, written only when the archive from
# RavenDB Cloud could not be unpacked. A healthy Secret does not have it.
check "certificate.bundle is absent" "" "$(jp secret "$SECRET" '.data.certificate\.bundle')"

step "What the operator actually wrote"
derive_expectations
URL_V="$(secret_value "$SECRET" url)"
URLS_V="$(secret_value "$SECRET" urls)"
TAGS_V="$(secret_value "$SECRET" nodeTags)"
STUDIO_V="$(secret_value "$SECRET" studioURL)"
PID_V="$(secret_value "$SECRET" productID)"
PW_V="$(secret_value "$SECRET" 'certificate\.password')"
info "url        $URL_V"
info "urls       $URLS_V"
info "nodeTags   $TAGS_V"
info "studioURL  $STUDIO_V"
info "productID  $PID_V"
info "password   $PW_V"

check "url"                  "$NODE_URL"      "$URL_V"
check "urls"                 "$ALL_URLS"      "$URLS_V"
check "nodeTags"             "$NODE_TAGS"     "$TAGS_V"
check "studioURL"            "$STUDIO"        "$STUDIO_V"
check "productID"            "$PRODUCT_ID"    "$PID_V"
check_not_empty "certificate.password" "$PW_V"
on_mock && check "the simulator's fixed PFX password" "$PFX_PASSWORD" "$PW_V"

# The relationships, which hold on any environment and are what an application
# actually depends on: url is the first of urls, the tags line up with them one
# for one, and studioURL is url with the Studio path on it.
check "url is the first of urls" "${URLS_V%%,*}" "$URL_V"
check "one tag per url" "$(printf '%s' "$URLS_V" | awk -F, '{print NF}')" \
  "$(printf '%s' "$TAGS_V" | awk -F, '{print NF}')"
check "studioURL is url plus the Studio path" "${URL_V}/studio/index.html" "$STUDIO_V"

# --- 4. the material is real ------------------------------------------------

step "Parsing the published certificate with openssl"
secret_value "$SECRET" 'tls\.crt'    > "$LAB03_TMP/tls.crt"
secret_value "$SECRET" 'tls\.key'    > "$LAB03_TMP/tls.key"
secret_value "$SECRET" 'client\.pem' > "$LAB03_TMP/client.pem"

SUBJECT="$(cert_field -subject)"
ENDDATE="$(cert_field -enddate)"
SERIAL_1="$(cert_field -serial)"
info "$SUBJECT"
info "$ENDDATE"
info "$SERIAL_1"

if on_mock; then
  check_contains "certificate subject" "$CERT_CN" "$SUBJECT"
  check_contains "certificate subject" "RavenDB Cloud lab mock" "$SUBJECT"
else
  # A real certificate is issued for the cluster's own DNS name, so the check is
  # that it belongs to this cluster rather than that it says a fixed string.
  check_contains "the certificate is issued for this cluster" \
    "$(lab_subdomain a)" "$SUBJECT"
  info "issuer: $(openssl x509 -in "$LAB03_TMP/tls.crt" -noout -issuer 2>/dev/null)"
fi
check_not_empty "certificate notAfter" "$ENDDATE"
check "certificate is valid for at least another day" yes \
  "$(openssl x509 -in "$LAB03_TMP/tls.crt" -noout -checkend 86400 >/dev/null 2>&1 && echo yes || echo no)"
check "tls.key parses as an RSA private key" yes \
  "$(openssl rsa -in "$LAB03_TMP/tls.key" -noout -check >/dev/null 2>&1 && echo yes || echo no)"

PEM_BLOCKS="$(grep -c -- '-----BEGIN' "$LAB03_TMP/client.pem" || true)"
info "client.pem holds $PEM_BLOCKS PEM blocks"
check "client.pem PEM blocks" 2 "$PEM_BLOCKS"
check "client.pem carries the certificate" yes \
  "$(grep -q -- '-----BEGIN CERTIFICATE-----' "$LAB03_TMP/client.pem" && echo yes || echo no)"
check "client.pem carries the key" yes \
  "$(grep -q -- '-----BEGIN RSA PRIVATE KEY-----' "$LAB03_TMP/client.pem" && echo yes || echo no)"

PFX_B64="$(kubectl -n "$LAB_NAMESPACE" get secret "$SECRET" -o jsonpath='{.data.client\.pfx}' 2>/dev/null)" || PFX_B64=''
PFXPW_B64="$(kubectl -n "$LAB_NAMESPACE" get secret "$SECRET" -o jsonpath='{.data.client\.with-password\.pfx}' 2>/dev/null)" || PFXPW_B64=''
printf '%s' "$PFX_B64" | base64 -d > "$LAB03_TMP/client.pfx" 2>/dev/null || true
check_not_empty "client.pfx"               "$PFX_B64"
check_not_empty "client.with-password.pfx" "$PFXPW_B64"
check "the two PKCS#12 forms are different bytes" yes \
  "$([ "$PFX_B64" != "$PFXPW_B64" ] && echo yes || echo no)"
check "client.pfx starts with a DER SEQUENCE tag" "30" \
  "$(od -An -tx1 -N1 < "$LAB03_TMP/client.pfx" 2>/dev/null | tr -d ' \n')"
if openssl pkcs12 -in "$LAB03_TMP/client.pfx" -passin pass: -nokeys -noout >/dev/null 2>&1; then
  note "openssl also opened client.pfx with an empty password"
fi

# --- 5. ownership -----------------------------------------------------------

step "Who owns the Secret"
CLUSTER_UID="$(jp cloudcluster "$CLUSTER" .metadata.uid)"
info "ownerReferences[0]:"
info "  apiVersion         $(jp secret "$SECRET" '.metadata.ownerReferences[0].apiVersion')"
info "  kind               $(jp secret "$SECRET" '.metadata.ownerReferences[0].kind')"
info "  name               $(jp secret "$SECRET" '.metadata.ownerReferences[0].name')"
info "  controller         $(jp secret "$SECRET" '.metadata.ownerReferences[0].controller')"
info "  blockOwnerDeletion $(jp secret "$SECRET" '.metadata.ownerReferences[0].blockOwnerDeletion')"

check "owner kind"         CloudCluster    "$(jp secret "$SECRET" '.metadata.ownerReferences[0].kind')"
check "owner name"         "$CLUSTER"      "$(jp secret "$SECRET" '.metadata.ownerReferences[0].name')"
check "owner uid"          "$CLUSTER_UID"  "$(jp secret "$SECRET" '.metadata.ownerReferences[0].uid')"
check "owner is the controller" true       "$(jp secret "$SECRET" '.metadata.ownerReferences[0].controller')"
check "blockOwnerDeletion" true            "$(jp secret "$SECRET" '.metadata.ownerReferences[0].blockOwnerDeletion')"
note "the Secret is garbage collected with the CloudCluster; the last step proves it"
note "a second controller cannot take it over either; that is proved after the pod step"

# --- 6. the certificate is downloaded once ----------------------------------

step "The certificate is downloaded once, not on every poll"
if on_cloud; then
  # No request log to count against, but the claim has a user-visible form:
  # status.certificateRefreshedAt is stamped when the archive is fetched, so it
  # standing still across several polls is the same statement, made from
  # outside. lastSyncTime moving is what proves the polls actually happened.
  CERT_AT_BEFORE="$(jp cloudcluster "$CLUSTER" .status.certificateRefreshedAt)"
  SYNC_BEFORE="$(jp cloudcluster "$CLUSTER" .status.lastSyncTime)"
  check_not_empty "status.certificateRefreshedAt" "$CERT_AT_BEFORE"
  info "waiting for the operator to poll at least twice at pollIntervalSeconds 30"
  SYNC_NOW="$SYNC_BEFORE"; SEEN=0
  DEADLINE=$((SECONDS + 180))
  while [ "$SECONDS" -lt "$DEADLINE" ] && [ "$SEEN" -lt 2 ]; do
    sleep 5
    NEXT="$(jp cloudcluster "$CLUSTER" .status.lastSyncTime)"
    if [ -n "$NEXT" ] && [ "$NEXT" != "$SYNC_NOW" ]; then
      SYNC_NOW="$NEXT"; SEEN=$((SEEN + 1))
    fi
  done
  info "lastSyncTime moved $SEEN time(s): $SYNC_BEFORE -> $SYNC_NOW"
  check "the operator kept polling" yes "$([ "$SEEN" -ge 2 ] && echo yes || echo no)"
  check "certificateRefreshedAt did not move with them" \
    "$CERT_AT_BEFORE" "$(jp cloudcluster "$CLUSTER" .status.certificateRefreshedAt)"
  note "the archive is fetched once and carried forward; a refresh only happens when"
  note "the stored certificate is close enough to expiry to need replacing"
else
CERTS_AFTER_READY="$(certificate_calls)"
DETAILS_BASE="$(details_calls)"
info "so far for $PRODUCT_ID: $DETAILS_BASE detail polls, $CERTS_AFTER_READY certificate downloads"
check_calls "certificate downloads to reach Ready" 1 "$CERTS_AFTER_READY"

info "waiting for two more polls at pollIntervalSeconds 5"
DETAILS_NOW="$DETAILS_BASE"
DEADLINE=$((SECONDS + 120))
while [ "$SECONDS" -lt "$DEADLINE" ]; do
  DETAILS_NOW="$(details_calls)"
  if [ "$DETAILS_NOW" -ge $((DETAILS_BASE + 2)) ]; then
    break
  fi
  sleep 3
done
info "detail polls are now $DETAILS_NOW (was $DETAILS_BASE)"
check "the operator kept polling" yes \
  "$([ "$DETAILS_NOW" -ge $((DETAILS_BASE + 2)) ] && echo yes || echo no)"
check "certificate downloads after those polls" "$CERTS_AFTER_READY" "$(certificate_calls)"
fi

# --- 7. delete the Secret ---------------------------------------------------

step "Deleting the Secret to watch the operator publish it again"
CERTS_BEFORE_DELETE="$(certificate_calls)"
k delete secret "$SECRET" >/dev/null
info "deleted $SECRET"
if ! wait_exists secret "$SECRET" 90; then
  warn "the Secret was not republished; recent operator logs:"
  operator_logs 30
fi
check "the Secret is back"    yes "$(exists secret "$SECRET")"
check "and has all its keys"  "$EXPECTED_KEYS" "$(secret_key_line "$SECRET")"
check_calls "certificate downloads" "$((CERTS_BEFORE_DELETE + 1))" "$(certificate_calls)"

secret_value "$SECRET" 'tls\.crt' > "$LAB03_TMP/tls.crt"
SERIAL_2="$(cert_field -serial)"
info "serial before $SERIAL_1"
info "serial after  $SERIAL_2"
check_contains "the republished certificate has the same subject" \
  "$CERT_SUBJECT_NEEDLE" "$(cert_field -subject)"
if on_mock; then
  # The simulator mints a fresh certificate on every download, which is how you
  # can tell the operator went back to the API rather than restoring a cache.
  check "the serial changed, so this was a real download" yes \
    "$([ "$SERIAL_1" != "$SERIAL_2" ] && echo yes || echo no)"
else
  # RavenDB Cloud does not. It hands back the certificate the cluster already
  # has, so an unchanged serial is the correct answer here and a changed one
  # would mean the cluster had been reissued underneath us. Measured: the same
  # serial came back from the real service after a real re-download.
  #
  # Which makes this a place the simulator is not faithful, and the assertion
  # that read as "the operator really called the API" was reading a property of
  # the simulator. What travels is that the material is usable again.
  check "the serial is unchanged, because the cluster keeps its certificate" \
    "$SERIAL_1" "$SERIAL_2"
  check "the republished key still parses" yes \
    "$(secret_value "$SECRET" 'tls\.key' > "$LAB03_TMP/tls.key";
       openssl rsa -in "$LAB03_TMP/tls.key" -noout -check >/dev/null 2>&1 && echo yes || echo no)"
fi

# --- 8. a pod that mounts it ------------------------------------------------

step "Mounting the Secret from a pod"
preload_busybox
kubectl -n "$LAB_NAMESPACE" delete job "$JOB" --ignore-not-found >/dev/null 2>&1 || true
# The manifest is quoted so the in-pod $VARIABLES survive to the container.
# The image is the one placeholder the shell substitutes, so LAB03_IMAGE picks
# a different image for real and not only for the docker pull above.
JOB_YAML="$(cat <<'YAML'
apiVersion: batch/v1
kind: Job
metadata:
  name: lab03-reader
spec:
  backoffLimit: 1
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: reader
          image: __IMAGE__
          imagePullPolicy: IfNotPresent
          env:
            # Endpoints belong in the environment: they are not secret and a
            # RavenDB client wants the whole list.
            - name: RAVENDB_URLS
              valueFrom:
                secretKeyRef:
                  name: lab03-app-connection
                  key: urls
            - name: RAVENDB_NODE_TAGS
              valueFrom:
                secretKeyRef:
                  name: lab03-app-connection
                  key: nodeTags
          command: ["/bin/sh", "-c"]
          args:
            - |
              echo "urls      : $RAVENDB_URLS"
              echo "nodeTags  : $RAVENDB_NODE_TAGS"
              echo "mounted   : $(ls /etc/ravendb | tr '\n' ' ')"
              # busybox has no openssl, so pull the common name straight out of
              # the DER: everything printable, one token per line.
              grep -v CERTIFICATE /etc/ravendb/tls.crt | base64 -d > /tmp/cert.der
              subject=$(tr -c '[:print:]' '\n' < /tmp/cert.der \
                        | grep -o '[A-Za-z0-9][A-Za-z0-9.-]*\.client\.certificate' | head -1)
              if [ -z "$subject" ]; then
                subject=$(strings -n 8 /tmp/cert.der 2>/dev/null \
                          | grep -o '[A-Za-z0-9][A-Za-z0-9.-]*\.client\.certificate' | head -1)
              fi
              echo "subject   : $subject"
              echo "pem blocks: $(grep -c BEGIN /etc/ravendb/client.pem)"
          volumeMounts:
            - name: ravendb
              mountPath: /etc/ravendb
              readOnly: true
      volumes:
        - name: ravendb
          secret:
            secretName: lab03-app-connection
            defaultMode: 0400
            items:
              - key: tls.crt
                path: tls.crt
              - key: tls.key
                path: tls.key
              - key: client.pem
                path: client.pem
YAML
)"
printf '%s\n' "${JOB_YAML//__IMAGE__/$BUSYBOX_IMAGE}" |
  kubectl apply -n "$LAB_NAMESPACE" -f - >/dev/null

if kubectl -n "$LAB_NAMESPACE" wait --for=condition=complete "job/$JOB" --timeout=180s >/dev/null 2>&1; then
  JOB_LOG="$(k logs "job/$JOB" 2>/dev/null)" || JOB_LOG=''
  printf '%s\n' "$JOB_LOG" | sed 's/^/    /'
  check_contains "the pod read every endpoint from the Secret" "$ALL_URLS"  "$JOB_LOG"
  check_contains "the pod read the node tags"                  "$NODE_TAGS" "$JOB_LOG"
  check_contains "the pod saw the mounted files"             "client.pem tls.crt tls.key" "$JOB_LOG"
  check_contains "the pod parsed the certificate subject"    "$CERT_SUBJECT_NEEDLE"  "$JOB_LOG"
  check_contains "client.pem is two PEM blocks in the pod"   "pem blocks: 2" "$JOB_LOG"
else
  warn "the $JOB Job did not complete. Can this cluster get $BUSYBOX_IMAGE?"
  warn "every other check in this lab still ran; only the in-pod ones are skipped."
  kubectl -n "$LAB_NAMESPACE" describe "job/$JOB" 2>/dev/null | tail -15 || true
fi
kubectl -n "$LAB_NAMESPACE" delete job "$JOB" --ignore-not-found >/dev/null 2>&1 || true

# --- 9. one Secret, one controller ------------------------------------------
#
# A second cluster, so the squatter binds to something of its own and gets as
# far as the Secret. Aimed at lab03-app's own cluster instead, it would be
# refused one step earlier, for managing a cluster another object already
# manages, and never reach the Secret at all: lab 04 shows that refusal.

if needs_mock "a second cluster whose object aims at this Secret (the simulator can seed one for free)"; then
step "One Secret, one controller"
SQUAT_ID="$(lab_api POST /_lab/clusters \
  "{\"displayName\":\"$SQUATTER\",\"tier\":\"Production\",\"instanceType\":\"PB10\",\"region\":\"eu-central-1\"}" |
  sed -n 's/.*"ids":\["\([^"]*\)".*/\1/p')" || SQUAT_ID=''
check_not_empty "a second cluster, $SQUATTER" "$SQUAT_ID"
info "taking $SQUAT_ID over from a second object, aimed at $CLUSTER's Secret"
kubectl apply -n "$LAB_NAMESPACE" -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: $SQUATTER
spec:
  accountRef:
    name: ravendb-cloud
  tier: Production
  connectionSecret:
    name: $SECRET
YAML

if ! wait_for_status "$SQUATTER" NameTaken 120; then
  warn "$SQUATTER did not report the conflict; recent operator logs:"
  operator_logs 30
fi
SQUATTER_MSG="$(condition_message cloudcluster "$SQUATTER" Ready)"
info "$SQUATTER_MSG"

check "$SQUATTER Ready"                 False       "$(condition cloudcluster "$SQUATTER" Ready)"
check "$SQUATTER STATUS"                NameTaken   "$(status_of "$SQUATTER")"
check_contains "$SQUATTER message names the Secret" "Secret \"$SECRET\" already exists" "$SQUATTER_MSG"
check_contains "$SQUATTER message names the field to change" "spec.connectionSecret.name" "$SQUATTER_MSG"
check "its own cluster is bound all the same" "$SQUAT_ID" "$(jp cloudcluster "$SQUATTER" .status.productID)"
check "the Secret still belongs to $CLUSTER" "$CLUSTER" \
  "$(jp secret "$SECRET" '.metadata.ownerReferences[0].name')"
check "and was not overwritten" "$EXPECTED_KEYS" "$(secret_key_line "$SECRET")"
check "the url in it is still $CLUSTER's" "$NODE_URL" "$(secret_value "$SECRET" url)"
# The refusal comes before the download: the operator looks at who controls
# the Secret first, so the conflict cost no certificate budget.
check_calls "the conflict cost no certificate download" 0 "$(count_calls "security/certificate/${SQUAT_ID}")"

kubectl -n "$LAB_NAMESPACE" delete cloudcluster "$SQUATTER" --ignore-not-found --timeout=90s >/dev/null
wait_gone cloudcluster "$SQUATTER" 60 || warn "$SQUATTER is still around"
note "deleting it left both clusters alone: deletionPolicy defaults to Retain"
lab_api POST "/_lab/clusters/${SQUAT_ID}/vanish" >/dev/null 2>&1 || true
fi

# --- 10. point the Secret at a different name -------------------------------

step "Pointing the Secret at a different name"
CERTS_BEFORE_RENAME="$(certificate_calls)"
k patch cloudcluster "$CLUSTER" --type=merge \
  -p "{\"spec\":{\"connectionSecret\":{\"name\":\"$SECRET_RENAMED\"}}}" >/dev/null
if ! wait_for cloudcluster "$CLUSTER" .status.connectionSecretName "$SECRET_RENAMED" 90; then
  warn "the rename did not take; recent operator logs:"
  operator_logs 30
fi
wait_exists secret "$SECRET_RENAMED" 60 || true

check "status.connectionSecretName" "$SECRET_RENAMED" "$(jp cloudcluster "$CLUSTER" .status.connectionSecretName)"
check "the new Secret has the same key set" "$EXPECTED_KEYS" "$(secret_key_line "$SECRET_RENAMED")"
check "the new Secret is owned too" "$CLUSTER" "$(jp secret "$SECRET_RENAMED" '.metadata.ownerReferences[0].name')"

# The old Secret is not deleted: it is still owned, so it goes away with the
# CloudCluster, not with the rename. It is emptied, so no copy of tls.key and
# certificate.password stays at an address the spec no longer mentions.
info "keys left in $SECRET: [$(secret_key_line "$SECRET")]"
check "the old Secret is left in place" yes "$(exists secret "$SECRET")"
check "and it holds nothing"            0   "$(secret_keys "$SECRET" | wc -l | tr -d ' ')"
check "it keeps its owner reference"    "$CLUSTER"   "$(jp secret "$SECRET" '.metadata.ownerReferences[0].name')"
# A rename means a Secret with no certificate in it, so the operator downloads
# one for it. That is the only thing that makes it download again.
check_calls "the rename cost one more certificate download" \
  "$((CERTS_BEFORE_RENAME + 1))" "$(certificate_calls)"

# --- 11. turn the certificate off -------------------------------------------

# In place first, on the Secret that already holds a certificate. This is the
# case a rename hides: the step below changes the name at the same time, so it
# lands on a fresh Secret and would pass even if the operator never removed
# anything.
step "includeCertificate: false on a Secret that already has one"
CERTS_BEFORE_INPLACE="$(certificate_calls)"
k patch cloudcluster "$CLUSTER" --type=merge \
  -p '{"spec":{"connectionSecret":{"includeCertificate":false}}}' >/dev/null
if ! wait_for secret "$SECRET_RENAMED" '.data.tls\.crt' "" 90; then
  warn "the certificate was not removed; recent operator logs:"
  operator_logs 30
fi
info "keys in $SECRET_RENAMED: $(secret_key_line "$SECRET_RENAMED")"
for key in 'tls\.crt' 'tls\.key' 'client\.pem' 'client\.pfx' 'certificate\.password'; do
  check "$key is gone" "" "$(jp secret "$SECRET_RENAMED" ".data.$key")"
done
check "the endpoints survived it" "$NODE_URL" "$(secret_value "$SECRET_RENAMED" url)"
check_calls "removing it cost no download" "$CERTS_BEFORE_INPLACE" "$(certificate_calls)"
note "endpoints and certificate are separate field managers, so pruning one"
note "leaves the other alone; that is what server-side apply buys here"

step "includeCertificate: false and a new name, together"
CERTS_BEFORE_NOCERT="$(certificate_calls)"
k patch cloudcluster "$CLUSTER" --type=merge \
  -p "{\"spec\":{\"connectionSecret\":{\"name\":\"$SECRET_NOCERT\",\"includeCertificate\":false}}}" >/dev/null
if ! wait_for cloudcluster "$CLUSTER" .status.connectionSecretName "$SECRET_NOCERT" 90; then
  warn "the change did not take; recent operator logs:"
  operator_logs 30
fi
wait_exists secret "$SECRET_NOCERT" 60 || true

info "keys in $SECRET_NOCERT: $(secret_key_line "$SECRET_NOCERT")"
check "endpoints only"          "$EXPECTED_KEYS_NOCERT" "$(secret_key_line "$SECRET_NOCERT")"
check "no tls.crt"              ""  "$(jp secret "$SECRET_NOCERT" '.data.tls\.crt')"
check "url is still published"  "$NODE_URL" "$(secret_value "$SECRET_NOCERT" url)"
check_calls "no extra download"       "$CERTS_BEFORE_NOCERT" "$(certificate_calls)"
# Running still means an application has everything it needs, and here that
# is the URLs alone, so the message says what the Secret holds and why.
NOCERT_MSG="$(condition_message cloudcluster "$CLUSTER" Ready)"
info "message: $NOCERT_MSG"
check "STATUS"                  Running "$(status_of "$CLUSTER")"
check_contains "the message says the certificate is left out on purpose" \
  "includeCertificate is false, so the client certificate is not in it" "$NOCERT_MSG"

# --- 12. turn publication off altogether ------------------------------------

step "connectionSecret.enabled: false"
k patch cloudcluster "$CLUSTER" --type=merge \
  -p '{"spec":{"connectionSecret":{"enabled":false}}}' >/dev/null
if ! wait_for cloudcluster "$CLUSTER" .status.connectionSecretName "" 90; then
  warn "status still names a Secret; recent operator logs:"
  operator_logs 30
fi
DISABLED_MSG="$(condition_message cloudcluster "$CLUSTER" Ready)"
info "message: $DISABLED_MSG"
check "status.connectionSecretName"  ""              "$(jp cloudcluster "$CLUSTER" .status.connectionSecretName)"
check "STATUS, because publishing is off rather than failing" Running "$(status_of "$CLUSTER")"
check "the cluster itself is fine"   True            "$(condition cloudcluster "$CLUSTER" Ready)"
check_contains "the message says publishing is off" \
  "spec.connectionSecret.enabled is false, so Secret \"$SECRET_NOCERT\" holds none of this operator's keys" "$DISABLED_MSG"

# The object survives and its contents do not: the owner asked for the
# certificate and the key to stop being published.
info "keys left in $SECRET_NOCERT: [$(secret_key_line "$SECRET_NOCERT")]"
check "the Secret object stays"      yes  "$(exists secret "$SECRET_NOCERT")"
check "and it holds nothing"         0    "$(secret_keys "$SECRET_NOCERT" | wc -l | tr -d ' ')"
check "the owner reference stays"    "$CLUSTER"   "$(jp secret "$SECRET_NOCERT" '.metadata.ownerReferences[0].name')"
note "the object has to survive: the operator holds no delete on Secrets, and"
note "that owner reference is what garbage collects it in the next step"

# The Secrets the two earlier renames left behind were emptied at the rename,
# not now. Every Secret this cluster owns is empty at this point, which is the
# invariant worth stating: nothing the operator published is still published.
for s in "$SECRET" "$SECRET_RENAMED"; do
  check "$s holds nothing either" 0 "$(secret_keys "$s" | wc -l | tr -d ' ')"
done

# --- 13. garbage collection -------------------------------------------------

step "Deleting the CloudCluster garbage collects every Secret it owns"
info "three Secrets are owned by $CLUSTER right now:"
kubectl -n "$LAB_NAMESPACE" get secret -l "cloud.ravendb.net/cluster=$CLUSTER" \
  --no-headers -o custom-columns=NAME:.metadata.name,TYPE:.type 2>/dev/null |
  sed 's/^/    /' || true

if ! kubectl -n "$LAB_NAMESPACE" delete cloudcluster "$CLUSTER" --timeout=180s >/dev/null; then
  warn "deleting the CloudCluster did not finish; recent operator logs:"
  operator_logs 30
fi
info "deleted the CloudCluster; deletionPolicy defaults to Retain, so $PRODUCT_ID is untouched"

GC_OK=yes
for s in "$SECRET" "$SECRET_RENAMED" "$SECRET_NOCERT"; do
  if ! wait_gone secret "$s" 120; then
    GC_OK=no
    warn "$s is still there"
  fi
done
check "every owned Secret was garbage collected" yes "$GC_OK"
check "the CloudCluster is gone"                 no  "$(exists cloudcluster "$CLUSTER")"

step "Removing the cluster this lab left running"
# Retain left it running, which is the whole point of Retain, and nothing in
# Kubernetes will clean it up. The simulator has a lab surface for that; a real
# account does not, and the CloudCluster is already gone, so there the cluster
# is handed back to the operator by taking it over once more, by its name, with
# deletionPolicy Terminate, and deleting that. creationMode TakeOverOnly makes
# sure it never creates a cluster in place of one that is already gone.
if on_mock; then
  lab_api POST "/_lab/clusters/${PRODUCT_ID}/vanish" >/dev/null 2>&1 || true
  REMAINING="$(lab_api GET /_lab/clusters | tr ',' '\n' | grep -c "\"id\":\"${PRODUCT_ID}\"" || true)"
  check "the cloud cluster is gone from the control plane" 0 "$REMAINING"
else
  info "taking $PRODUCT_ID over again just to terminate it"
  kubectl apply -n "$LAB_NAMESPACE" -f - >/dev/null <<YAML
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab03-reaper
spec:
  accountRef:
    name: ravendb-cloud
  displayName: $(lab_display a)
  tier: Development
  creationMode: TakeOverOnly
  deletionPolicy: Terminate
YAML
  wait_for_status lab03-reaper Running "$LAB_CHANGE_TIMEOUT" >/dev/null || true
  check "the reaper took over the cluster this lab made" "$PRODUCT_ID" \
    "$(jp cloudcluster lab03-reaper .status.productID)"
  k delete cloudcluster lab03-reaper --ignore-not-found --timeout=600s >/dev/null || true
  check_contains "and terminated it" "Terminat" "$(product_status "$PRODUCT_ID")"
  check "nothing this lab made is left in $LAB_NAMESPACE" "<none>" \
    "$({ k get cloudcluster -o name 2>/dev/null || true; } | { grep lab03 || echo '<none>'; })"
  note "deletionPolicy Terminate is the mechanism that works on any control plane,"
  note "which is why lab 06 makes it the subject rather than the cleanup"
fi

step "Done"
cat <<'EOF'

    The connection Secret is the operator's whole output as far as an
    application is concerned: mount it, read urls and tls.crt, connect.

    Next: labs/04_take_over_an_existing_cluster.sh

EOF

lab_summary

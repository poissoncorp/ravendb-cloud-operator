# Status and monitoring

> **See it run:** [Lab 02](../../labs/02-provision-a-cluster.md) walks through STATUS and CHANGES as a cluster comes up, and [lab 05](../../labs/05-day-two-operations.md) through them as it changes.

A CloudCluster answers two questions, one column each:

- **STATUS**, the reason on the `Ready` condition: can I use the cluster?
- **CHANGES**, the reason on the `Synced` condition: did my manifest reach
  RavenDB Cloud?

```
$ kubectl -n apps get cloudcluster
NAME          STATUS    CHANGES      TIER          REGION         NODES   AGE
orders-dev    Running   Applied      Development   eu-central-1   1       6m
legacy-prod   Running   Applied      Production    eu-central-1   3       5m
legacy-dr     Running   NotApplied   Production    eu-west-1      3       1m
```

Every reason comes with a message that says what happened and what to change.
`-o wide` adds the STATUS message, and the CHANGES one is in
`kubectl describe`, or:

```bash
kubectl -n apps get cloudcluster legacy-dr -o jsonpath='{.status.conditions[?(@.type=="Synced")].message}'
```

## STATUS

| STATUS | Ready | Means | What to do |
|---|---|---|---|
| `Running` | True | Active, and the connection Secret is published | nothing |
| `Creating`, `Updating` | False | RavenDB Cloud is creating the cluster, or applying a change | wait; a create takes minutes |
| `WaitingForCertificate` | False | Active, but RavenDB Cloud has not issued the client certificate yet | wait |
| `WaitingForAccount` | False | the CloudAccount is not usable, or its key belongs to another RavenDB Cloud account | look at the account; the message says which ([accounts.md](accounts.md)) |
| `InvalidSpec` | False | the operator will not send this | change the field the message names |
| `Refused` | False | RavenDB Cloud refused to create the cluster | change what its message names; nothing was created, so every field can be edited |
| `QuotaReached` | False | the account has as many clusters as it may (its product quota) | terminate one, or have RavenDB raise the quota; retried every ten minutes |
| `NameTaken` | False | the name is in use in a way this object may not take: another CloudCluster holds the cluster, it is of another tier, provider or region, it is being terminated, `creationMode` is `CreateOnly`, two clusters share the name, or the connection Secret's name belongs to another Secret | the message says which, and what to set |
| `ClusterNotFound` | False | no cluster by that name, and this object may not create one: `creationMode: TakeOverOnly`, `managementPolicy: Observe`, or `tier: Free`, which only the portal can create | correct `spec.displayName`, or allow creating |
| `DeletedInCloud` | False | the cluster was terminated or deleted outside the operator | delete the object; to get a new cluster, apply it again |
| `AwaitingPayment` | False | RavenDB Cloud waits for a payment | settle it in the portal |
| `CreationFailed` | False | RavenDB Cloud could not create the cluster and opened a support request | nothing in the manifest fixes it; RavenDB support follows up |
| `SecretNotWritten` | False | the cluster serves, but the connection Secret could not be written | the message has the API server's error, often a policy or quota in the namespace; retried with backoff |
| `Deleting` | False | the object is being deleted | the message says what it waits for ([deleting.md](deleting.md)) |
| `CloudUnreachable` | Unknown | RavenDB Cloud did not answer | nothing; retried with backoff. If it lasts, check the operator's network |
| `KeyRejected` | Unknown | RavenDB Cloud rejected the account's API key | put a new key in the account's Secret |
| `UnexpectedResponse` | Unknown | RavenDB Cloud answered in a way this operator cannot read | upgrade the operator |

Unknown means the operator could not look. That alone changes nothing about
the cluster, and the rest of status is what the operator saw at
`status.lastSyncTime`.

## CHANGES

CHANGES appears once the object has its cluster.

| CHANGES | Synced | Means | What to do |
|---|---|---|---|
| `Applied` | True | everything the manifest sets matches the cluster | nothing |
| `Applying` | False | a change was sent and RavenDB Cloud is applying it | wait |
| `Waiting` | False | RavenDB Cloud takes the change later: after a storage change, the next one waits six hours on AWS and twelve on Azure premium disks, and a busy cluster waits for its current change | nothing; the operator sends it when allowed |
| `Refused` | False | RavenDB Cloud refused a change to the running cluster | change the spec as its message says; the operator does not ask again until the spec changes |
| `NotApplied` | False | a difference the operator may not apply (`managementPolicy: Observe`) or cannot (`releaseChannel`, `networking.allowedIPs`, or `storage.throughputMBps` alone) | set `Full`, or make the change in the portal, or put the manifest back |

`Refused` on STATUS is about creating the cluster, on CHANGES about changing
one that runs.

## Accounts and restarts

A CloudAccount's STATUS is `Verified`, `KeyMissing`, `KeyRejected`,
`CloudUnreachable`, `UnexpectedResponse`, or `InUse` while its deletion waits
for clusters that still name it ([accounts.md](accounts.md)).

A CloudClusterRestart's STATUS is `Waiting`, `Restarting`, `Succeeded` or
`Failed`. As on a Job, `Complete` is True only once the node came back, and a
failed restart sets `Failed` instead, so
`kubectl wait --for=condition=Complete` never reports a failure as done.

## GitOps tools

Every kind also carries the two conditions
[kstatus](https://github.com/kubernetes-sigs/cli-utils/tree/master/pkg/kstatus)
reads, and with it Flux and `kubectl wait`. Each is present only while True,
with the reason and message of the condition behind it:

- **`Reconciling`**: the operator is still working towards the spec. From
  STATUS `Creating`, `Updating`, `WaitingForCertificate`, `WaitingForAccount`,
  `SecretNotWritten`, `CloudUnreachable` and `Deleting`, and CHANGES
  `Applying` and `Waiting`.
- **`Stalled`**: stuck until a person changes something. From every other
  STATUS that is not `Running`, and CHANGES `Refused`.

`NotApplied` is neither: the manifest and the cluster differ on purpose, or on
a field only the portal can change. A Flux Kustomization with `wait: true`
fails on a `Stalled` object instead of waiting for its timeout. A storage
change in its waiting period keeps the object `Reconciling` for those hours.

Argo CD needs a health check for a kind it does not know. This one reads the
same conditions, in `argocd-cm`:

```yaml
data:
  resource.customizations.health.cloud.ravendb.net_CloudCluster: |
    local conds = (obj.status or {}).conditions or {}
    for _, c in ipairs(conds) do
      if c.type == "Stalled" and c.status == "True" then
        return { status = "Degraded", message = c.message }
      end
    end
    for _, c in ipairs(conds) do
      if c.type == "Reconciling" and c.status == "True" then
        return { status = "Progressing", message = c.message }
      end
    end
    for _, c in ipairs(conds) do
      if c.type == "Ready" and c.status == "True" then
        return { status = "Healthy", message = c.message }
      end
    end
    return { status = "Progressing", message = "Waiting for the operator" }
```

The same check, keyed `cloud.ravendb.net_CloudAccount`, works for accounts.
For restarts, key it `cloud.ravendb.net_CloudClusterRestart` and read
`Complete` in place of `Ready`.

## Events and logs

Events are for `kubectl describe`: Normal ones for what the operator did
(`Created`, `Adopted` when it took a cluster over, `Applying`,
`SecretPublished`, `Retained`, `Terminating`, `Restarting`, `Restarted`), and a
Warning when a problem starts. A problem warns once, not on every poll, so an
alert on Warning events pages for problems and not for work somebody asked
for. A wait that usually ends by itself, such as an account applied a moment
after its clusters, warns only once it has lasted a minute. Events expire after
an hour, so an alert should read them as they arrive.

The log has one line per condition change, `condition changed`, with
`condition`, `from` and `to`. Every call to RavenDB Cloud is logged at debug
level (`logging.level: debug` in the chart).

## Metrics

Metrics are off by default. `metrics.enabled: true` serves them on port 8443
over HTTPS, and `metrics.service.enabled: true` adds a Service in front. A
scraper authenticates with its ServiceAccount token and needs permission to
read `/metrics`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: ravendb-cloud-operator-metrics-reader}
rules:
  - nonResourceURLs: ["/metrics"]
    verbs: ["get"]
```

Bind that to your Prometheus ServiceAccount, and scrape with `scheme: https`,
the ServiceAccount's bearer token, and `insecureSkipVerify`, since the operator
serves a self-signed certificate.

Next to controller-runtime's own metrics, calls to RavenDB Cloud have
`ravendb_cloud_requests_total` and `ravendb_cloud_request_duration_seconds` by
method and status code (`none` when no answer came),
`ravendb_cloud_retries_total`, and `ravendb_cloud_request_queued_seconds`, how
long a request waited for its turn. The operator paces its calls, so when
polls slow down these say whether RavenDB Cloud is slow, refusing, or not
answering.

There is no metric per cluster. To alert on a cluster that is not `Running`, or
on a certificate close to `status.certificateExpiresAt`, export those fields
with kube-state-metrics' custom resource state metrics, or alert on Warning
events.

# Status and monitoring

A CloudCluster answers two questions, one column each. **STATUS** is the reason
on the `Ready` condition: can I use it? **CHANGES** is the reason on `Synced`:
did my manifest reach RavenDB Cloud? Both lists are closed, and every message
says what happened and what to do next, naming the spec field to change.

```
$ kubectl -n apps get cloudcluster
NAME          STATUS    CHANGES      TIER          REGION         NODES   AGE
orders-dev    Running   Applied      Development   eu-central-1   1       6m
legacy-prod   Running   Applied      Production    eu-central-1   3       5m
legacy-dr     Running   NotApplied   Production    eu-west-1      3       1m
```

`-o wide` adds POLICY, INSTANCE, CLUSTER-ID, URL and the Ready message.

| STATUS | Ready | Means |
|---|---|---|
| `Running` | True | Active, and the connection Secret is published (or publishing is off) |
| `Creating`, `Updating` | False | RavenDB Cloud is creating the cluster, or applying a change |
| `WaitingForCertificate` | False | Active; RavenDB Cloud has not issued the client certificate yet |
| `WaitingForAccount` | False | the CloudAccount is not usable; the message says why |
| `InvalidSpec` | False | the operator will not send this; the message names the field |
| `Refused` | False | RavenDB Cloud refused to create the cluster, in its own words |
| `QuotaReached` | False | the account is at its product quota; retried every ten minutes |
| `NameTaken` | False | the name belongs to another cluster or another CloudCluster, or the Secret name is taken |
| `ClusterNotFound` | False | no cluster by that name, and this object may not create one (`Observe`, or Free) |
| `DeletedInCloud` | False | the cluster was deleted outside the operator |
| `AwaitingPayment` | False | RavenDB Cloud waits for payment; resolve it in the portal |
| `CreationFailed` | False | RavenDB Cloud could not create it and opened a support request |
| `SecretNotWritten` | False | Active, but the connection Secret could not be written |
| `Deleting` | False | this object is being deleted; the message says what it waits for |
| `CloudUnreachable`, `KeyRejected`, `UnexpectedResponse` | Unknown | the operator could not look; the cluster is probably still serving |

| CHANGES | Synced | Means |
|---|---|---|
| `Applied` | True | everything the manifest sets matches RavenDB Cloud |
| `Applying` | False | a change was sent |
| `Waiting` | False | RavenDB Cloud takes it later: a storage change window, or the cluster busy with another change or a restart; asked again each poll |
| `Refused` | False | RavenDB Cloud refused it; not asked again until the spec changes |
| `NotApplied` | False | a difference the operator may not apply (`Observe`) or cannot (a field the API only takes at creation) |

A CloudAccount's STATUS is `Verified`, `KeyMissing`, `KeyRejected`,
`CloudUnreachable`, `UnexpectedResponse`, or `InUse` while its deletion waits
for the clusters still naming it. A CloudClusterRestart's is `Waiting`,
`Restarting`, `Succeeded` or `Failed`. As on a Job, `Complete` is True only once
the node came back, and a failed restart sets `Failed` instead, so
`kubectl wait --for=condition=Complete` never reports a failure as done.

Every kind also carries the two conditions [kstatus](https://github.com/kubernetes-sigs/cli-utils/tree/master/pkg/kstatus)
reads, and with it Flux and `kubectl wait`: `Reconciling` while the operator is
still working towards the spec, and `Stalled` while it is stuck until a person
changes something, such as a refused create or a rejected key. Each is present
only while True and repeats the reason and message of the condition behind it.
A Flux Kustomization with `wait: true` therefore fails on a `Stalled` object
instead of waiting for its timeout.

Argo CD needs a health check for a kind it does not know. This one reads the
same conditions:

```yaml
# argocd-cm
data:
  resource.customizations.health.cloud.ravendb.net_CloudCluster: |
    hs = { status = "Progressing", message = "" }
    for _, c in ipairs((obj.status or {}).conditions or {}) do
      if c.type == "Stalled" and c.status == "True" then
        return { status = "Degraded", message = c.message }
      end
      if c.type == "Ready" and c.status == "True" then
        hs = { status = "Healthy", message = c.message }
      end
    end
    return hs
```

The same check, keyed `cloud.ravendb.net_CloudAccount`, works for accounts.

`status.cloudStatus` carries RavenDB Cloud's own status string unchanged, for
anyone who needs more than the operator's vocabulary.

Events are for `kubectl describe`: Normal ones for what the operator did
(`Created`, `Adopted`, `Applying`, `SecretPublished`, `Retained`, `Terminating`,
`Restarting`, `Restarted`), and a Warning when a problem starts, once rather
than on every poll, so an alert on Warning events pages for problems and not
for work somebody asked for. A wait that usually ends by itself, an account or
a key Secret applied a moment later, warns only once it has lasted a minute.

The log has one line per condition change, `condition changed` with the
condition and its old and new reason as `condition`, `from` and `to`. The
per-request trail to RavenDB Cloud is at debug level (`logging.level: debug`
in the chart).

Calls to RavenDB Cloud have their own metrics next to controller-runtime's:
`ravendb_cloud_requests_total` and `ravendb_cloud_request_duration_seconds` by
method and status code (`none` when no answer came),
`ravendb_cloud_retries_total`, and `ravendb_cloud_request_queued_seconds`, how
long a request waited for its turn. When polls slow down, these say whether
RavenDB Cloud is slow, refusing, or not answering.

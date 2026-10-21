# Status, events and logs

What the operator reports and how. Read before adding a condition reason,
emitting an event, or changing what `kubectl get` shows.

## Conditions, not a phase

There is no `status.phase`. Each kind has conditions whose reasons are the
operator's vocabulary, a closed list per condition in
`internal/controller/conditions.go`:

| Kind | Condition | Column | Answers |
|---|---|---|---|
| CloudCluster | `Ready` | STATUS | can the cluster be used, and what is it doing |
| CloudCluster | `Synced` | CHANGES | did the manifest reach RavenDB Cloud |
| CloudAccount | `Ready` | STATUS | can the key be used |
| CloudClusterRestart | `Complete` | STATUS | did the restart run; `Failed` is True when it failed |

The reasons do not mirror every RavenDB Cloud status; `status.cloudStatus`
carries the raw value. Adding a reason because the cloud grew a status is
usually wrong.

## One call sets everything

Set conditions only through `ready`, `synced` and `complete`. Each looks the
reason up in its table (`meaning`), which decides:

- the condition status (True, False or Unknown);
- whether entering the reason records an event, and its type;
- its progress: settled, reconciling or stalled;
- whether a long stay earns one late Warning (`warnAfter`, used for waits that
  usually end by themselves, such as an account applied a moment later).

From those, `summarize` derives the kstatus conditions `Reconciling` and
`Stalled`, so Argo CD, Flux and `kstatus` read health without custom rules. A
reason missing from its table is logged as a bug and reads Unknown.

## Events

Warning means a problem somebody should look at; Normal means the operator did
what it was asked (`Created`, `Applying`, `Terminating`, `Restarting`). A
problem warns once when it starts, not on every poll. Events are written
through `events.k8s.io` with the reason doubling as the action (`emit`).

## Logs and metrics

One log line per condition change, with fixed text and the values as keys:
`condition changed` with `condition`, `from`, `to` and `message`. Calls to
RavenDB Cloud are at debug level, except an answer the client cannot decode,
which is logged as an error.

Metrics are off unless `--metrics-bind-address` is set, which the Helm chart
does with `metrics.enabled`. Next to controller-runtime's metrics:
`ravendb_cloud_requests_total` and `ravendb_cloud_request_duration_seconds` by
method and code (`none` when no answer came), `ravendb_cloud_retries_total`, and
`ravendb_cloud_request_queued_seconds`.

## Writing status

A pass ends with one status write, only when status changed (`writeStatus`),
under its own short deadline so a pass cut off still records why. A write
refused for a stale resourceVersion is dropped, not returned, and the pass keeps
the requeue it chose: its status is stale anyway, and returning the error would
redo the whole pass, the RavenDB Cloud read included. A pass that must persist
something before calling RavenDB Cloud, the create stamp, uses
`writeStatusNow`, which moves the pass's starting point with it. A restart
stamps `issuedAt` the same way.

`lastSyncTime` moves on every successful read, which is one status write per
poll and no more.

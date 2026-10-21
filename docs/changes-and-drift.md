# Changes and drift

How a running cluster is compared with its manifest and brought in line. Read
before changing `internal/controller/drift.go` or adding a field a user can
change on a running cluster.

## Only what the manifest says is managed

A field left out of the manifest is left as the cluster has it. `shapeDifferences`
compares only fields the manifest sets, storage field by field: a manifest with
`sizeGB` and no `type` does not change the disk class. A minimal manifest (a
name and a tier) cannot reshape anything, even under `managementPolicy: Full`.

## One difference, one home

Every field that can diverge appears in exactly one `shapeDifference`. One with
an `apply` function can be changed through the API; one with a nil `apply` is
reported on the `Synced` condition and never silently ignored.

| Field | Applied | Why not, when not |
|---|---|---|
| `instanceType` | yes | |
| `storage.sizeGB`, `type`, `iops`, `throughput` | yes, together in one change | a size the cluster cannot resize to is reported, not sent |
| `nodes` | yes, one node per pass | |
| `allowedIPs` | no | the API takes it at creation only |
| `releaseChannel` | no | no endpoint changes it |
| `provider`, `region` | no | a cluster cannot move; binding already refuses a mismatch |

Under `managementPolicy: Observe` everything is reported (`CHANGES NotApplied`)
and nothing applied.

## One change per pass

A pass applies at most one change, then polls at the transitional interval
while the cluster is `DeployingChanges`. RavenDB Cloud requires the cluster
Active before the next change, so the order is: instance type, storage, nodes.

Nodes move one at a time. Additional nodes are added one per pass, each once the
cluster is Active again; removal takes the highest additional tag, since A, B
and C are never removed. Instance types sold without additional nodes (the PS
and PN families) report a node difference instead of asking.

## When RavenDB Cloud says no

- **Busy** (`cloudapi.IsBusy`: the cluster is not Active, or its status does not
  allow the change): `Synced Waiting`, asked again each poll.
- **A time window** (one storage change per six hours on AWS): also `Waiting`,
  with RavenDB Cloud's own message.
- **Any other refusal**: `Synced Refused`, a Warning, and no timer: the refusal
  stands until the spec changes. It blocks only that change; later differences
  are still applied (`driftPass.refused`).
- **No answer**: `CloudUnreachable` on Ready, retried with backoff. A change
  whose answer was lost is not sent again; the next pass reads the cluster and
  sees whether it happened.

The cluster stays `Running` through all of these: a refused change says nothing
about whether it serves.

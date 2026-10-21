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
| `storage.sizeGB`, `type`, `iops`, `throughputMBps` | yes, together in one change | a smaller size (disks do not shrink), or one the disk cannot move to, is reported, and then nothing else about storage is sent; a change to `throughputMBps` alone is reported, because RavenDB Cloud refuses a storage request that moves nothing else |
| `nodes` | yes, one node per pass | an instance type sold without additional nodes |
| `allowedIPs` | no | the API takes it at creation only |
| `releaseChannel` | no | no endpoint changes it |
| `provider`, `region` | no | a cluster cannot move; binding already refuses a mismatch |

The storage request replaces the whole disk spec, so `storageChange` sends an
attribute the manifest leaves out as the disk has it now. The exception is IOPS
and throughput when the class changes: they belong to the old class, and
leaving them out asks for the new class's defaults.

Under `managementPolicy: Observe` everything is reported (`CHANGES NotApplied`)
and nothing applied.

## One change per pass

A pass applies at most one change, in the order instance type, storage, nodes,
then polls at the transitional interval while the cluster is
`DeployingChanges`. A cluster that is not Active ends the pass before anything
is compared.

Nodes move one at a time: added one per pass, each once the cluster is Active
again; removal takes the highest tag. Nine instance types are sold without
additional nodes (`withoutAdditionalNodes` in `cloud_limits.go`: PN10 to PN70,
PS10 and PS20), and on those a node difference is reported instead of asked.

## When RavenDB Cloud says no

`applyFirst` tries the differences in that order and stops at the first one
RavenDB Cloud takes.

- **Too soon** (`cloudapi.IsTooSoon`): one storage change per cluster every
  6h05m on AWS and 12h05m on Azure SsdPremium, counted from when the previous
  one finished. `Synced Waiting` with RavenDB Cloud's own message, asked again
  each poll, and the next difference is still tried.
- **Busy** (`cloudapi.IsBusy`): the cluster is applying another change or
  restarting. `Synced Waiting`, asked again each poll, and the pass ends there,
  since every other change would be refused the same way.
- **A rejected key** (401 or 403): `KeyRejected` on Ready, not on Synced.
- **Any other 4xx**: `Synced Refused` with a Warning, and the next difference
  is still tried in that pass. While the refusal stands, until the spec changes
  or the cluster stops differing (`refusalStands`, `stillDiffers`), a poll
  reads the cluster and sends no change.
- **No usable answer** (429, 5xx, a lost connection): `Synced Applying` saying
  it retries, and the error goes back to controller-runtime for backoff. A
  change whose answer was lost is not sent again by the client; the next pass
  reads the cluster first and sends it only if the cluster is Active and still
  differs.

Too soon and busy are told apart from a refusal that never lifts only by the
text of RavenDB Cloud's 400 (`storageRefusal` and `busyRefusal` in `rest.go`).
If RavenDB Cloud rewords one, it degrades to an ordinary refusal.

The cluster stays `Running` through all of these: a refused change says nothing
about whether it serves.

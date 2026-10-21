# Deletion

What deleting a CloudCluster does to the cluster in RavenDB Cloud. Read before
changing `internal/controller/deletion.go`, the finalizer, or `deletionPolicy`.

## The finalizer comes first

`cloud.ravendb.net/finalizer` is added at the start of the first pass, before
the account is read or RavenDB Cloud is asked anything, so no cluster is ever
created without it. It is added only to a live object: Kubernetes refuses a new
finalizer on one that already has a `deletionTimestamp`. Finalizers are written
with an optimistically locked merge patch (`lockedPatch`), so a stale copy
never drops another controller's finalizer.

## Retain and Terminate

`deletionPolicy` defaults to `Retain`: deleting the object leaves the cluster
running and records a Normal `Retained` event naming it. `Terminate` ends the
cluster, after a Normal `Terminating` event, and only for:

- a cluster this object created, or
- one it took over under `creationMode: TakeOverOnly`.

A cluster taken over under the default `TakeOverOrCreate` is left running with a
Warning `Retained` that says why (`keepsWhatItTookOver`): a name can match a
cluster nobody meant this object to own, and a name alone must not be enough to
destroy one. `managementPolicy` has no say: `Observe` with `Terminate` under
`TakeOverOnly` still terminates.

`nothingToTerminate` short-cuts the finalizer when there is nothing in RavenDB
Cloud to act on: `Retain`, a cluster already gone, one this object may not
terminate, or an object that never bound and never sent a create. An object
with a create stamp and no binding looks its attempted name up first
(`findInterruptedCreate`), so a create whose answer was lost is still found and
terminated, and a cluster another object holds or that does not match is not.

## When the account cannot be used

Terminating needs the API key. If the CloudAccount or its Secret is missing:

- with nothing to terminate, the finalizer is released and the object goes;
- with something to terminate, the object stays in `Deleting` and says which
  of the account or its Secret to restore, or to set `Retain`;
- if the Secret went in the same delete as the account and its clusters, the
  client built before still holds the key (`Clients.LastFor`), and that is used.

Removing the finalizer by hand releases the object and orphans the cluster:
running, billed, and named only in events that expire.

## What goes with the object

The connection Secret carries an owner reference to its CloudCluster, so
Kubernetes garbage collects it. The operator holds no delete permission on
Secrets. Events are not owned and outlive the object.

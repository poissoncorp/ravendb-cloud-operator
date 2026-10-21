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
cluster the object holds, after a Normal `Terminating` event, whether the
object created it or took it over. `managementPolicy` has no say: `Observe`
with `Terminate` still terminates.

It does not matter how the object came to hold the cluster because only status
records that, and a restore from git or a backup loses status. A Terminate that
worked only on a cluster the object remembered creating would leave it running
and billed after a restore. What guards against ending a cluster nobody meant to
hold is the take-over itself: a found cluster must match the manifest, and one
taken over under the default `TakeOverOrCreate` records a Warning saying to set
`Retain` before deleting the object if the cluster is not meant to be its own.

The terminate asks RavenDB Cloud to hide the product in the portal. RavenDB
Cloud does that once termination completes, which frees the display name, so a
manifest deleted with `Terminate` and applied again gets a new cluster under the
same name. A terminate answered 404 or 410 counts as done.

`nothingToTerminate` short-cuts the finalizer when there is nothing in RavenDB
Cloud to act on: `Retain`, a cluster already gone (`goneFromCloud`: a
`cloudStatus` of Terminating or Terminated, or Ready `DeletedInCloud`), or an
object that never bound and never sent a create. An object with a create stamp
and no binding looks its attempted name up first (`findInterruptedCreate`), so
a create whose answer was lost is still found and terminated, and a cluster
another object holds or that does not match is not. Two clusters by that name
leave it in `Deleting`, with the advice to hide the one that is not this
object's.

A terminate that fails keeps the object in `Deleting`, with a Warning saying
why, until it goes through. A failure another attempt could fix is retried
with backoff. A rejected key is tried again every poll, because a new key in
the Secret wakes the account and not the cluster. Any other refusal waits for
the object to change, as every terminal refusal does.

## When the account cannot be used

Terminating needs the API key, but not a Ready account. `clientForDeletion`
builds a client from the Secret whatever the account's condition says, so a key
RavenDB Cloud no longer accepts shows up as a failed terminate. A teardown such
as `kubectl delete namespace` removes the Secret, the account and the cluster in
no particular order; when the Secret is already gone, the client built from it
before (`Clients.LastFor`) still holds the key. That client lives in memory, so
it is gone once the operator restarts.

With no client at all, a missing account, or a Secret gone and the operator
restarted since:

- with nothing to terminate, the finalizer is released and the object goes;
- with something to terminate, the object stays in `Deleting` and says to
  restore whichever is missing, or to set `Retain`.

If the account's key now answers for another RavenDB Cloud account than the one
the cluster was bound in (`status.accountDomain`), `Retain` releases as usual
and `Terminate` stays in `Deleting`: in that account a terminate would end
nothing and then release the object, leaving the cluster billed with no object
to name it.

Removing the finalizer by hand releases the object and orphans the cluster:
running, billed, and named only in events that expire.

## What goes with the object

The connection Secret carries an owner reference to its CloudCluster, so
Kubernetes garbage collects it. The operator holds no delete permission on
Secrets. Events are not owned and outlive the object.

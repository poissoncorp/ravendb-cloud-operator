# Deleting

`kubectl delete cloudcluster` deletes a Kubernetes object. Whether it also
deletes a database is decided by `spec.deletionPolicy`.

- **`Retain`, the default.** The object goes, the cluster keeps running, and a
  Normal `Retained` event names it. Apply the manifest again later and the
  object finds the cluster by its name.
- **`Terminate`.** The cluster is terminated first, then the object goes. It
  applies to a cluster the object created, or one it took over with
  `creationMode: TakeOverOnly`. A cluster found under the default
  `TakeOverOrCreate` is left running, with a Warning saying so: a name alone is
  not enough to destroy a cluster somebody else may own.

`managementPolicy` has no say in this: an object under `Observe` with
`Terminate` still terminates.

## When the delete does not finish

Terminating needs the account's API key. If the CloudAccount or its Secret is
gone while a `Terminate` is pending, the object stays with STATUS `Deleting`
and a message saying which to restore. Restore it and the delete completes, or
set `deletionPolicy: Retain` to let the object go and keep the cluster.

Removing the finalizer by hand releases the object at once and leaves the
cluster running, billed, and named only in events that expire within the hour.
Do it only after writing the cluster's id down.

## What goes with the object

The connection Secret is owned by the CloudCluster and garbage collected with
it. Events stay. A terminated cluster keeps its display name in RavenDB Cloud
until it is hidden in the portal, so applying the same manifest again to get a
new cluster is refused until then.

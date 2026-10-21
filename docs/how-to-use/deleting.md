# Deleting

> **See it run:** [Lab 06](../../labs/06-delete-safely.md) deletes with each policy, with a broken credential, and with the finalizer forced off.

`kubectl delete cloudcluster` deletes a Kubernetes object. Whether it also ends
the cluster in RavenDB Cloud is decided by `spec.deletionPolicy`:

- **`Retain`, the default.** The object goes and the cluster keeps running. A
  Normal `Retained` event names it. Apply the manifest again later and the
  object finds the same cluster by its name.
- **`Terminate`.** The operator asks RavenDB Cloud to terminate the cluster,
  and the object goes once RavenDB Cloud has accepted. This ends whatever
  cluster the object holds, whether it created it or took it over, and whatever
  `managementPolicy` says. Terminating cannot be undone.

A terminated cluster takes a few minutes to wind down. When it has, RavenDB
Cloud hides it in the portal, which frees its name: RavenDB Cloud keeps a
terminated cluster's record, and the name stays taken until that record is
hidden. So applying the same manifest again gets a new, empty cluster. Applied
before the old one has finished, the object reports `NameTaken` and creates
the new one at a later poll.

## Retiring a cluster

1. Set `deletionPolicy: Terminate` on the object and apply it.
2. Delete the object. It disappears once RavenDB Cloud has accepted the
   terminate; until then STATUS reads `Deleting` with the reason.
3. Check that it is gone. The `Terminating` event names the cluster, but events
   expire after an hour. `kubectl ravendb-cloud export --account <account> -n <namespace> --all`
   lists every cluster in the account that is not terminated or being
   terminated.

To find clusters no object holds, compare that list with the clusters the
CloudClusters hold:

```bash
kubectl get cloudclusters -A -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,CLUSTER:.status.displayName,ID:.status.productID
```

## When the delete does not finish

The object carries a finalizer: a marker that makes Kubernetes keep it until
the operator has done what `deletionPolicy` asks. While it waits, STATUS is
`Deleting` and the message says what for.

- **The operator cannot get the API key.** A teardown that deletes the key
  Secret, the account and the clusters together, such as
  `kubectl delete namespace`, still terminates: the operator keeps the key it
  already read. It cannot if the operator restarted after the Secret was gone.
  Restore the Secret, or set `deletionPolicy: Retain` to let the object go and
  terminate the cluster in the portal. In a namespace that is being deleted
  nothing new can be created, so `Retain` is the only way out there.
- **The account's key now belongs to another RavenDB Cloud account.** The
  operator does not terminate through it, since that account has no such
  cluster. Put the right key back, or set `Retain`.
- **RavenDB Cloud refuses to terminate.** The message quotes it, and the
  operator asks again every poll.

Removing the finalizer by hand releases the object at once and leaves the
cluster running and billed, named only in events that expire within the hour.
Do it only after writing the cluster's id down (`status.productID`).

## What goes with the object

The connection Secret and the CloudClusterRestart records belong to the
CloudCluster and are deleted with it. Events stay until they expire.

A CloudAccount cannot be deleted while a CloudCluster in its namespace names it:
it reports `InUse` and waits. Delete the clusters first.

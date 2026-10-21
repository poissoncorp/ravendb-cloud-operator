# Lab 06: delete the object without deleting the database

> **Reference:** [Deleting](../docs/how-to-use/deleting.md) and
> [Accounts](../docs/how-to-use/accounts.md) cover the same ground as guides.

Kubernetes objects get deleted all the time: a `kubectl delete -f` in the wrong directory, a
GitOps prune, a namespace cleanup. For a Deployment that is routine; for a database it is not.
In this lab you delete clusters and accounts in every way the operator distinguishes, and see
that a database goes only when the manifest said so in writing.

## The rule

> `kubectl delete cloudcluster` deletes a Kubernetes object. What happens to the cluster in
> RavenDB Cloud is decided by `spec.deletionPolicy`:
>
> - **Retain**, the default: the cluster keeps running.
> - **Terminate**: the cluster this object holds is terminated, whether the object created it or
>   found it. Once termination completes it is hidden in the portal, which frees its name.

## Do this

```bash
labs/06_delete_safely.sh
```

## Retain, the default

The lab creates `lab06-retain` with no `deletionPolicy` in the manifest. The CRD fills in
`Retain`. Then:

```bash
kubectl -n apps delete cloudcluster lab06-retain
```

The object is gone, really gone, not stuck. Its connection Secret goes with it, because the
Secret is owned by the object. The cluster in RavenDB Cloud is still `Active`, which the script
checks with the simulator directly, and one event outlives the object to say so:

```
Normal   Retained   deletionPolicy is Retain: leaving RavenDB Cloud cluster 1-A running.
```

Apply the same manifest again and the new object finds that cluster by its name. Nothing is lost.

## Terminate, written on purpose

```yaml
spec:
  deletionPolicy: Terminate      # irreversible
```

Delete that one and the operator terminates the cluster before letting the object go:

```
Normal   Terminating   deletionPolicy is Terminate: terminating RavenDB Cloud cluster 2-A.
```

A terminated cluster stays in RavenDB Cloud's records and keeps its name until it is hidden in
the portal. The operator asks for that as part of the terminate, so once termination completes
the cluster leaves the list. Apply the same manifest again, and you get a new cluster under the
same name, with a new id.

## A cluster the object found

Terminate does not care how the object came by its cluster. The lab seeds a cluster in the
simulator, takes it over with `deletionPolicy: Terminate`, and the takeover already warns, before
anybody deletes anything:

```
Warning Adopted: Found RavenDB Cloud cluster "lab06-legacy-d-t7a1" (9-A) already running and
took it over; ... If it is not, set spec.deletionPolicy to Retain before deleting this object:
deletionPolicy is Terminate, so deleting it now terminates that cluster.
```

Delete it, and it is terminated. Why not spare found clusters? Only `status` records whether the
object created its cluster or found it, and a restore from git or a backup does not bring status
back. A rule that depended on it would quietly stop terminating the cluster you created, the
first time you rebuilt the namespace. The protection against a name that matches by accident is
the warning above, `managementPolicy: Observe` for a found cluster, and `creationMode`.

With the default `Retain`, deleting an object that found its cluster leaves the cluster exactly
as it was.

## Terminated behind the operator's back

Someone terminates a cluster in the portal. The lab does that through the simulator:

```
NAME         STATUS           ...
lab06-gone   DeletedInCloud   ...

RavenDB Cloud cluster 10-A was terminated outside this operator. Delete this object; to get a
new cluster, apply it again, ...
```

The object has `managementPolicy: Full`, which may create clusters, and it still does not create
a replacement: a second cluster would be a second bill and an empty database where the old one
was. It reports, keeps polling, and waits for you to delete it.

## Deleting an account

A `CloudAccount` holds the key its clusters need, including to terminate. So deleting one that a
`CloudCluster` still names is accepted and held:

```
NAME            STATUS   DOMAIN
lab06-account   InUse    acme.ravendb.cloud

2 CloudCluster(s) still reference this account (lab06-holder, lab06-second). Delete them first:
without this credential they cannot reconcile, and one with deletionPolicy Terminate could not
terminate its cluster either.
```

Delete the clusters and the account goes with the last of them, by itself. An account nothing
names deletes at once.

Last, the ordinary teardown: the key Secret, the account and a `Terminate` cluster deleted
together, Secret first, as a namespace delete may. The cluster is still terminated, with the key
the operator already holds, and then the account goes.

## Appendix: a deletion that cannot finish

Terminating needs a key RavenDB Cloud accepts. The lab puts a revoked key in an account's Secret
and deletes two clusters on it. The `Retain` one goes at once, since it needs nothing from RavenDB
Cloud. The `Terminate` one stays:

```
NAME                     STATUS     ...
lab06-nocred-terminate   Deleting   ...

Could not terminate RavenDB Cloud cluster 18-A: 401 Unauthorized. The object stays until this succeeds.
```

The fix is a working key in the Secret: the operator tries again every `pollIntervalSeconds`,
and the next attempt terminates and lets the object go. Or set `deletionPolicy: Retain` to let
the object go and keep the cluster.

The wrong fix is removing the finalizer by hand:

```bash
kubectl -n apps patch cloudcluster lab06-nocred-terminate --type=merge \
  -p '{"metadata":{"finalizers":null}}'
```

The object disappears at once, and the cluster it was supposed to terminate keeps running, billed,
with nothing in Kubernetes naming it. Write its id down first; applying a manifest with its name
takes it over again.

## Takeaway

`kubectl delete` removes a database only when `deletionPolicy: Terminate` is written in the
manifest, and then it removes the one the object holds, however it got it. Everything else leaves
the cluster running and says so in an event. An account stays as long as a cluster needs its key.

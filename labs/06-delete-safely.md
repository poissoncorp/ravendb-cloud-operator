# Lab 06: delete the object without deleting the database

**For:** anyone who is going to run `kubectl delete` in a namespace that contains a
production database, on purpose or by accident. This lab walks the whole deletion path: what
`Retain` does, what `Terminate` does, when the finalizer appears and what it is holding,
what happens when the operator cannot reach the cloud while a delete is pending, what
deleting a cluster the object found touches, and what the operator says when a cluster is
deleted behind its back. You end up able to predict, from the manifest and the state of the
account alone, whether a delete costs you a Kubernetes object or a database.

## Run it

```bash
labs/06_delete_safely.sh
```

The complete example is [`06_delete_safely.sh`](06_delete_safely.sh). The whole
lab turns on one optional field:

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab06-retain
spec:
  accountRef: {name: ravendb-cloud}
  # deletionPolicy is not written here. The CRD defaults it to Retain, and the
  # default is the whole safety story: kubectl delete drops this object and
  # leaves the cluster in RavenDB Cloud running.
  tier: Development
  storage: {sizeGB: 20}
```

Destroying the database takes one more line, written by a human who meant it:

```yaml
spec:
  deletionPolicy: Terminate   # irreversible
```

## Why Retain is the default

A `CloudCluster` is a pointer to something that lives outside the cluster, keeps state, and
costs money. Kubernetes objects, by contrast, get deleted constantly and often by accident:
a `kubectl delete -f .` in the wrong directory, a `kustomize` prune after a resource is
renamed, a GitOps sync that drops a manifest somebody moved to another repository, an
`argocd app delete`, a namespace deleted during a cleanup. Every one of those is routine
for a Deployment and unrecoverable for a database.

So the operator makes the cheap outcome the default and the expensive one explicit. Part 1
creates a cluster with no `deletionPolicy` in the manifest at all, and the CRD fills it in:

```bash
kubectl -n apps get rcc lab06-retain -o jsonpath='{.spec.deletionPolicy}'
# Retain
```

Then the lab deletes the object and asks the control plane directly, not the operator,
whether the cluster is still there:

```bash
curl -s localhost:18086/_lab/clusters   # the lab control surface, through the lab's port-forward
```

```
    PASS the CloudCluster object = gone
    PASS the connection Secret = gone
    PASS the cloud cluster after the delete = Active
```

The object is really gone, not stuck in `Terminating`. That distinction matters: an object
wedged with a finalizer looks deleted in `kubectl get` output at a glance, still blocks its
namespace from finishing deletion, and quietly holds a controller loop open. Part 1 asserts
the stronger thing, that `kubectl get` returns not found.

The connection Secret goes with it, and not because the operator deletes it. The Secret
carries an owner reference, which the operator writes along with the endpoints when it applies
the Secret ([`connection_secret.go`](../internal/controller/connection_secret.go)):

```bash
kubectl -n apps get secret lab06-retain-connection \
  -o jsonpath='{.metadata.ownerReferences[0]}'
# {"apiVersion":"cloud.ravendb.net/v1alpha1","blockOwnerDeletion":true,"controller":true,
#  "kind":"CloudCluster","name":"lab06-retain","uid":"7c1f...-a0b2"}
```

Kubernetes garbage collection does the rest. This is worth doing this way rather than
deleting the Secret from the deletion path: garbage collection still works if the operator
is down, mid-upgrade, or crash-looping, and there is no code path that could delete the
wrong Secret. The operator holds no delete permission on Secrets at all.

Finally, the operator says what it did, in an event that outlives the object that caused
it:

```
Normal    Created           Created RavenDB Cloud cluster "lab06-retain" (11-A) in AWS/eu-central-1.
Warning   OpenAllowList     allowedIPs contains 0.0.0.0/0, so this cluster accepts connections from any address. ...
Normal    SecretPublished   Connection Secret "lab06-retain-connection" created.
Normal    Retained          deletionPolicy is Retain: leaving RavenDB Cloud cluster 11-A running.
```

(The Warning is the lab account's open allow list, said once when the cluster is created.)

That last line is the one to look for after somebody deletes something they should not
have. It names the product id, which is what you need to find the cluster in the portal. To
bring it back under Kubernetes, apply the manifest again: its name finds the cluster, and the
new object takes it over.

## The finalizer, and what it buys

Look at any live `CloudCluster`:

```bash
kubectl -n apps get rcc lab06-nocred-terminate -o jsonpath='{.metadata.finalizers}'
# ["cloud.ravendb.net/finalizer"]
```

A finalizer buys exactly one thing: `kubectl delete` stops being a deletion and becomes a
request. The API server sets `metadata.deletionTimestamp` and leaves the object in place
until every finalizer on it is removed. That is the only window in which the operator gets
to run code about a cluster that is on its way out, and it is the window `Terminate` needs.
Without it, the object would vanish on the first `delete` and the cloud cluster would
survive every policy, including the one that asked for termination.

Nothing in the CRD puts that finalizer there. The operator does, first thing in `Reconcile`,
before it reads the account or asks RavenDB Cloud anything
([`cloudcluster_controller.go`](../internal/controller/cloudcluster_controller.go)):

```go
if cluster.DeletionTimestamp.IsZero() {
    if err := r.addFinalizer(ctx, &cluster); err != nil {
        return ctrl.Result{}, err
    }
}
```

First, so that no cluster is ever created without it: a create is the one call the finalizer
exists to answer for. Only on a live object, because Kubernetes rejects a new finalizer on an
object that already has a `deletionTimestamp`, so adding one there would be an API error, not
a rescue.

So even an object whose `accountRef` names a `CloudAccount` that does not exist has the
finalizer. Part 3 of the lab creates exactly that object and deletes it:

```
    PASS STATUS with no account to resolve = WaitingForAccount
    PASS spec.deletionPolicy = Terminate
    PASS the finalizer, before the account resolves = cloud.ravendb.net/finalizer
    PASS delete with nothing created = gone
```

The finalizer held it for one pass. That pass found no cluster bound and no create ever
issued, so there was nothing for `Terminate` to act on, and it let the object go. A misspelled
account costs a delete nothing.

Note also what does *not* need the finalizer once it is there. `Retain` could work without
one, because it has nothing to do. It keeps one anyway so both policies take the same path,
and so a policy edited from `Retain` to `Terminate` on a live object is honoured without the
operator having to add a finalizer to something that is already being deleted.

## Terminate, and the event that outlives the object

Part 2 creates an identical cluster with `deletionPolicy: Terminate` and deletes it. The
operator records an event *before* it calls the API, then terminates
([`deletion.go`](../internal/controller/deletion.go)):

```go
r.event(cluster, corev1.EventTypeNormal, v1alpha1.EventTerminating,
    "deletionPolicy is Terminate: terminating RavenDB Cloud cluster %s.", productID)
```

The lab checks the object is gone first, and only then reads the events, which is the point
of the ordering:

```
    PASS the CloudCluster object = gone
Normal    Terminating   deletionPolicy is Terminate: terminating RavenDB Cloud cluster 12-A.
    PASS the cloud cluster after the delete = Terminated
    PASS and it is still listed, under the same display name = 1
```

A terminated cluster does not disappear from RavenDB Cloud. Its status becomes `Terminated`
and the record stays, keeping its display name, until somebody hides it in the portal. So
applying the same manifest again is refused by RavenDB Cloud ("Provided Display Name is already
taken."), and the operator's message says why: a terminated cluster keeps its name until it is
hidden. It never binds to one either: a terminated cluster is skipped when a name is looked up.

Events are independent objects with their own lifetime, not dependents of the thing they
describe, so no owner reference drags them out with it. That is what the lab is asserting by
reading them only after `kubectl get` returns not found. It looks them up by
`involvedObject.uid` rather than by name, because the object names here are stable across
runs and an event from an earlier run would otherwise answer for this one:

```bash
kubectl -n apps get events --field-selector involvedObject.uid=$UID \
  -o custom-columns=TYPE:.type,REASON:.reason,MESSAGE:.message
```

The uid has to be read before the delete. Afterwards there is nothing left to read it from.

In a namespace where a cluster has disappeared, that log is the difference between "the
operator did this, deliberately, because the manifest said so" and an unexplained missing
database. It is a debugging aid and not an audit log: events are garbage collected by the
API server on a TTL (`--event-ttl`, one hour unless your distribution changed it), so the
record is gone long before a quarterly review. It is the record that exists at exactly the
moment somebody starts asking questions, which is usually the same afternoon.

`Normal` is deliberate. The operator saves `Warning` for problems, so that an alert on
Warning events pages somebody for something that is wrong. A termination the manifest asked
for is the operator doing its job, and it is in `kubectl events` and `kubectl describe` like
every other event.

## When the operator cannot finish

Part 3 is the failure mode people meet in practice: the credential is broken at the moment
you delete something. The lab creates a second `CloudAccount` with its own Secret so nothing
here touches the shared credential, takes over two clusters through it, one with
`deletionPolicy: Terminate` and `creationMode: TakeOverOnly` (Part 4 shows why both), then
deletes the Secret:

```bash
kubectl -n apps delete secret lab06-guest-key
```

```
    PASS lab06-guest-account Ready = False
    PASS lab06-guest-account reason = KeyMissing
```

Now delete both objects. They behave differently, and the difference is one branch taken
when the account cannot be used, before the deletion path is ever reached
([`deletion.go`](../internal/controller/deletion.go)):

```go
func (r *CloudClusterReconciler) deleteWithoutAccount(...) (ctrl.Result, error) {
    if nothingToTerminate(cluster) {
        return r.releaseFinalizer(ctx, cluster)
    }
    r.deletionStuck(ctx, cluster, deletionBlockedMessage(cluster))
    return r.finish(ctx, original, cluster, result, err)
}
```

The `Retain` object deletes normally. There is nothing to ask the cloud for, so there is
nothing a missing API key can stop:

```
    PASS Retain deletes even with no usable credential = gone
    PASS its cloud cluster is untouched = Active
    PASS deletion event on the broken-credential Retain path = none
```

That third line is the part worth noticing, and it is the reason the lab checks for an
absence. This branch returns straight to `releaseFinalizer`. It never enters
`reconcileDelete`, which is where the `Retained` event is written, so the delete that Part
1 narrated in the namespace log happens here in silence. Same policy, same outcome in the
cloud, no record naming the cluster that survived. When a broken credential and a delete
land in the same five minutes, the only trace left is the `Adopted` or `Created` event from
when the object was bound, and only until that expires.

The `Terminate` object cannot finish, and says so instead of pretending:

```
NAME                     STATUS     CHANGES   TIER         REGION         NODES   AGE
lab06-nocred-terminate   Deleting   Applied   Production   eu-central-1   3       9s

    message: deletionPolicy is Terminate, so RavenDB Cloud cluster 14-A cannot be terminated
    until CloudAccount "lab06-guest-account" and the API key Secret it names are usable again.
    Restore whichever is missing, or set spec.deletionPolicy to Retain to let this object go
    and leave the cluster running.
    PASS the object is still here = present
    PASS deletionTimestamp is set
    PASS the finalizer is still holding it = cloud.ravendb.net/finalizer
    PASS STATUS while wedged = Deleting
    PASS a Warning marks that it is stuck mentions 'Deleting'
    PASS the cloud cluster is still running = Active
```

`Deleting` records no event while a deletion simply proceeds, since most finish at once. One
that is stuck is somebody's to unstick, so it warns, once per thing it is stuck on. It retries
every 20 seconds. Restore the credential and the delete completes on its own, which is the
correct fix and the one to reach for. This is the shape a finalizer is supposed to have: a
delete that cannot be honoured stays visible as an object you can `describe`, rather than
completing anyway and leaving a running cluster nobody knows about.

If you ever genuinely need the object gone and accept the consequences, you take the
finalizer off by hand:

```bash
kubectl -n apps patch cloudcluster lab06-nocred-terminate \
  --type=merge -p '{"metadata":{"finalizers":null}}'
```

The API server deletes the object the instant the last finalizer is removed. The lab does
this, and then checks the cost:

```
    PASS the object after the forced release = gone
    PASS the cloud cluster it was told to terminate = Active
```

That cluster is now an orphan. It is running, it is billed, its `Terminate` policy was never
carried out, and nothing in Kubernetes manages it any more. The way back is a manifest with its
display name, which takes it over again, and the name and id are still in the namespace for as
long as the events last: in the `Adopted` event written when the object bound (this deletion
never reached the code that writes one), found by name, because the uid you would have filtered
on went with the object:

```bash
kubectl -n apps get events \
  --field-selector involvedObject.name=lab06-nocred-terminate \
  -o custom-columns=REASON:.reason,MESSAGE:.message
# Adopted   Found RavenDB Cloud cluster "lab06-legacy-b-t7a1" (14-A) already running; ...
```

Force releasing a finalizer is not "unsticking a delete", it is deciding to lose track of a
cluster. Do it after you have written the name down, not after the events expire.

## Deleting a cluster the object found

Taking a cluster over is the case where a delete is most likely to be casual: the object was
cheap to create, it did not build anything, and removing it feels like removing a bookmark.
Part 4 seeds a cluster in the control plane, takes it over by name, and deletes the object:

```
    PASS status.adopted = true
    PASS managementPolicy defaulted for a found cluster = Observe
    PASS deletionPolicy defaulted, found or created = Retain
    PASS the CloudCluster object = gone
    PASS the connection Secret = gone
    PASS the cloud cluster it found = Active
Normal   Retained     deletionPolicy is Retain: leaving RavenDB Cloud cluster 15-A running.
```

The guest leaves and takes only what it brought: the object it was, and the connection
Secret it published. Removing a bookmark really is all that happened. That is the behaviour
you want, and it falls out of the same default rather than out of a special case.

Then `deletionPolicy: Terminate` on a cluster the object found. Under the default
`creationMode`, `TakeOverOrCreate`, the object would have created a cluster had the name found
none, so finding one was not something the manifest asked for. A name can match a cluster
nobody meant this object to own, and a name alone is not enough to destroy one:

```
    PASS status.adopted = true
    PASS creationMode defaulted = TakeOverOrCreate
Warning   Retained   deletionPolicy is Terminate, but this object took over RavenDB Cloud
                     cluster 16-A under creationMode TakeOverOrCreate rather than creating it,
                     so it is left running. ...
    PASS the cloud cluster it found = Active
```

`creationMode: TakeOverOnly` says the cluster is meant to be there, and then `Terminate` means
what it says, on a cluster this operator never created:

```
    PASS status.adopted = true
    PASS managementPolicy is still Observe = Observe
Normal    Terminating   deletionPolicy is Terminate: terminating RavenDB Cloud cluster 18-A.
    PASS the found cloud cluster after the delete = Terminated
```

Be precise about what that object could and could not do while it was alive. Under `Observe`
it changes nothing in RavenDB Cloud: a field the manifest sets differently from the cluster is
reported as `CHANGES NotApplied` and left alone, which lab 04 shows. And yet it has destroyed
the cluster it did not create. `managementPolicy` and `deletionPolicy` answer different
questions. `managementPolicy` is about *convergence*: how much of the manifest the operator may
push onto the cluster. `deletionPolicy` is a single explicit instruction, with `Terminate`
spelled out in full, and `TakeOverOnly` is what makes it an instruction about *this* cluster.

## A cluster deleted behind the operator's back

The portal, another API key, a colleague's script: a cluster can go without this object
deciding anything. RavenDB Cloud keeps the record and reports it `Terminated`, and Part 5 uses
the stand-in's `/_lab` surface to do exactly that to a cluster an object is managing with
`managementPolicy: Full`:

```
NAME         STATUS           CHANGES   TIER         REGION         NODES   AGE
lab06-gone   DeletedInCloud   Applied   Production   eu-central-1   3       7s

    message: RavenDB Cloud cluster 17-A was terminated outside this operator. Delete this
    object; to get a new cluster, apply it again, after hiding the terminated one in the
    RavenDB Cloud portal, since it keeps the name "lab06-gone-t7a1" until then.
    PASS a Warning marks it mentions 'Warning'
    PASS the operator kept looking = 2
    PASS STATUS, a poll or two later = DeletedInCloud
    PASS still bound to the cluster that went = 17-A
    PASS clusters named lab06-gone-t7a1, a poll or two later = 1
```

`Full` may create a cluster, and does not create this one. A cluster that went away is reported,
never quietly replaced: a replacement is a second bill, and an empty database where the old one
was, behind the same Secret name. The object stays bound to the id it had, keeps polling, and
says what to do: delete it, and apply it again for a new cluster once the old name is free. A
cluster that vanishes outright, which RavenDB Cloud answers with a 404 once the object is
bound, reads `DeletedInCloud` the same way.

## Takeaway

`kubectl delete cloudcluster` deletes a Kubernetes object. Whether it also deletes a
database is decided by one field that defaults to the harmless answer, and the operator gets
to act on that field only because a finalizer it added itself holds the object open long
enough. Read the deletion path as three independent questions. What does `deletionPolicy`
say: with `creationMode`, that decides the cluster's fate (`Terminate` ends a cluster the object
created, or one it took over under `TakeOverOnly`), and `managementPolicy` does not get a vote. Can the operator reach the cloud right now: if not, `Retain` proceeds (quietly,
with no event) and `Terminate` waits, as `Deleting`. What does this object own: the connection
Secret, through ownerReferences, and nothing else, which is why the Secret goes and the events
stay. When a delete does not complete, the finalizer is doing its job. Removing it by hand
converts a stuck object into an untracked bill, and the name you need to find that cluster
again lives in an event that expires within the hour.

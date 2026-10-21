# Lab 05: resize, rescale and reshape a running cluster

**For:** whoever owns the cluster after it exists. The disk is filling up, the working set
outgrew 8 GB of RAM, or a read-heavy quarter wants two more nodes. RavenDB Cloud can do
exactly three things to a live cluster, and this lab drives all three from a manifest and
watches what the operator does with them. By the end you will have moved from PB10 to PB20
and grown the disk in one apply, been made to wait for a second disk change, gone 3 nodes to 5
and back, and seen two requests refused, both by the operator before they cost a call. Then
it restarts a node, which is an action rather than a state, and watches the restart wait for
the cluster's policy before it runs.

## Run it

```bash
labs/05_day_two_operations.sh
```

The complete example is [`05_day_two_operations.sh`](05_day_two_operations.sh). Everything in
the lab is one manifest applied repeatedly with different numbers in three fields:

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab05-prod
spec:
  accountRef:
    name: ravendb-cloud
  pollInterval: 3s        # lab only: 5m is the default, and this lab is impatient
  tier: Production        # immutable, rejected at admission
  instanceType: PB10      # mutable
  nodes: 3                # mutable
  storage:
    sizeGB: 20            # mutable upwards only
```

`pollInterval` sets the pace of everything you are about to read. A settled cluster is re-read
on it, and a cluster mid-change on it too when it is shorter than the 30 second transitional
interval, which in a lab it is. A `kubectl apply` bumps the generation and reconciles at once
either way.

The first apply provisions it. Every step after that changes the cluster, and one rule shapes
the order they come in: RavenDB Cloud allows one storage change per cluster every six hours on
AWS, twelve on Azure premium disks. This lab used to grow the disk on its own and then again a
few minutes later, and against the real service the second one was refused. So it
makes one storage change, inside the combined change below, and then asks for a second on
purpose to show what the operator does with the wait.

## Why only three fields can change

Of the spec fields that describe the cluster, only three do anything after creation. That is
not caution, it is the shape of the API: RavenDB Cloud publishes `products/instance-type`,
`products/storage`, `products/additional-node/add` and `products/additional-node/remove`, and
no other endpoint that reshapes a running product. Two more endpoints do touch a live cluster,
`products/restart-node` and `products/terminate/{id}`, but one bounces a node and the other
destroys the cluster; neither changes its shape. There is no endpoint to move a cluster between
regions, tiers, providers or subdomains, and none to edit the firewall list after creation.

So the CRD says so out loud. Five fields carry CEL rules and the apiserver rejects the edit
before the operator is involved: `tier`, `provider`, `region`, `subdomain` and `displayName`.
Read the rules and they are not all the same strength. `tier` is pinned unconditionally.
`subdomain` and `displayName` are pinned from the first apply, set or not: omitted, they cannot
be added later, because the display name is what the operator finds the cluster by and both
feed the create request. `provider` and `region` are written as
`!has(oldSelf.X) || (has(self.X) && self.X == oldSelf.X)`, so they lock the moment they are set,
and a manifest that left `region` to the CloudAccount default can still spell it out later. The
CRD tests in `api/v1alpha1` cover every one of those refusals. What matters here is the inverse:
`instanceType`, `storage.*` and `nodes` have no rule at all, because each of them maps to an
endpoint that exists.

## Why one change per reconcile pass

Change the instance type and the disk in the same `kubectl apply` and you might expect two API
calls. You get one, and then another one a poll later. `reconcileDrift` in
[`internal/controller/drift.go`](../internal/controller/drift.go) walks one list of
differences. The ones with no endpoint behind them (`provider`, `region`, `allowedIPs`,
`releaseChannel`) come first and are only written down. Then instance type, then storage, then
node count, and the first one that calls the cloud returns immediately:

```go
for i := range differences {
    diff := &differences[i]
    if diff.apply == nil {
        continue            // reported, never sent
    }
    err := diff.apply(ctx)
    ...
    pass.applied = diff
    return pass, nil        // one change, then out
}
```

The caller turns an applied change into `STATUS Updating` and `CHANGES Applying`, both
conditions `False`, and a transitional requeue. The rationale is written on the function: the
cloud API serialises these anyway, and stacking two would leave the second silently dropped.

The operator also cannot stack them by accident, which is the part you can read off the control
flow. `reconcileNormal` returns early on any transitional cloud status, several steps before
`reconcileDrift` is reached, so for as long as RavenDB Cloud reports `DeployingChanges` the
operator does nothing but poll it, and `STATUS` stays `Updating`.

The script proves it two ways. It reads the persisted status the moment the instance type lands
and finds the disk still at its old size:

```
==> Changing instanceType AND storage in one apply
    PB10 -> PB20 and 20 -> 40 GB, submitted together
    after the first change landed, instanceType/disk/STATUS/CHANGES = PB20/20/Updating/Applying
    PASS one change per pass: instance type done, disk not yet, both columns saying so = PB20/20/Updating/Applying
```

Both columns move on the pass that sends a change, so a cluster halfway through a resize never
says its spec matches. That snapshot is the second change going out: the disk is still 20 GB
in status, because status is what the operator read before it sent the resize.

And it reads the control plane's own request log through `GET /_lab/requests`, which records
every call the operator made:

```
    reshaping calls the control plane received: instance-type storage
    PASS the two changes arrived as separate, sequential calls = instance-type storage
    PASS instance-type calls = 1
    PASS storage calls = 1
```

Two calls, in the order `reconcileDrift` checks them, roughly one requeue apart. Nothing was
retried and nothing was dropped.

## A second storage change waits

The combined change was this cluster's storage change for the next six hours. The script asks
for another straight away, 40 to 80 GB, and RavenDB Cloud refuses it:

```
==> A second storage change, minutes after the first: 40 -> 80 GB
    Ready=True (Running)
    Synced=False (Waiting)

    Synced message: RavenDB Cloud will take a change later (resizing storage from 40 GB to
    80 GB): You've reached the maximum storage modification rate. Wait at least 6 hours
    between each storage change. The next storage change will be available in 6 hours 4
    minutes. The operator asks again at each poll.
    PASS CHANGES = Waiting
    PASS STATUS while it waits = Running
    PASS Ready while it waits = True
    PASS disk untouched = 40
```

A 4xx is normally final here: the operator reports it as `CHANGES Refused` and does not ask
again until the spec changes, because asking again only gets the same refusal. This one is
not final. It lifts on its own, so it is reported as
`Waiting`, the change is sent again at each poll, and it goes through when the window opens
without anybody touching the object. The cluster serves the whole time, so `STATUS` stays
`Running`; only `CHANGES` says the spec is not carried through yet, and why, in RavenDB Cloud's
own words. Entering `Waiting` records one Normal event, not a Warning: nothing is wrong, and
nothing needs a person. The countdown in the message changes on every poll, and that is not news
either, so it is one event however long the wait.

The only thing that tells this refusal apart from one that never lifts is its text, so the text
is matched in `internal/cloudapi`, where errors are classified, and never in the controller. It
is also the case the stand-in did not model until lab 05 met it for real: the simulator now
enforces the same window, with the same words.

The script then puts the size back, so the steps below start from a spec that matches.

## Node scaling moves one node at a time

`nodes` is the same mechanism made visible, because the API has no "set cluster size" call at
all. It has add-a-node and remove-a-node. Going from 3 to 5 is therefore two passes:

```
==> Scaling out 3 -> 5 nodes
    PASS an intermediate 4-node state was observed
    PASS node tags after scaling out = A B C D E
    PASS add-node calls for 3 -> 5 = 2
    PASS nothing else was reshaped = additional-node/add additional-node/add
```

The script watches for the 4-node state but does not require it, and the distinction is worth
copying into your own tests. `status.nodes` is 4 only for the window between the operator
observing the first added node and it deciding to add the second, so an assertion that insists
on catching that window is an assertion that will eventually fail for reasons that have nothing
to do with the operator. The call count is the claim that actually holds every time: two
separate `additional-node/add` requests, never one bulk resize, because there is no bulk resize
to make.

Scaling in is the interesting direction, because `RemoveNode` takes a node tag and somebody has
to choose it. The operator sorts the observed tags and takes the last one:

```go
tags := sortedCopy(observed.NodeTags)
victim := tags[len(tags)-1]
```

Highest tag first, every time. That is a deliberate choice rather than an obvious one: the
alternative, removing whichever node the API happened to list first, would make two identical
manifests produce different clusters. Deterministic beats clever here, and it also means the
node tags a five-node cluster hands out (`A` through `E`) are the ones it gives back when it
shrinks. The README lists this under known gaps, since the cloud side may eventually have an
opinion about which node it would rather lose.

```
==> Scaling in 5 -> 3 nodes
    PASS node tags after scaling in = A B C
    PASS remove-node calls for 5 -> 3 = 2
    PASS events mentions 'Removing node E (5 -> 4)'
    PASS events mentions 'Removing node D (4 -> 3)'
```

## What the operator refuses to do

Ask for a smaller disk and nothing happens, loudly. Cloud disks do not shrink, so
`storageDifference` returns a difference with nothing to apply, and `reconcileDrift` reports it
instead of sending it:

```
    Ready=True (Running)
    Synced=False (NotApplied)

    Synced message: storage size 20 GB is smaller than the current 40 GB; cloud disks cannot shrink.
```

Three things about that output are worth pausing on.

`Ready` is still `True`. A refusal is not an outage: the cluster is serving, the connection
Secret is current, and the only thing wrong is that the manifest is asking for something
impossible. Splitting "is it working" from "does it match spec" across `Ready` and `Synced` is
what makes that expressible.

The message names both numbers and the reason. You can act on it without reading the operator's
source or the provider's documentation.

And no call was made. That is the claim the script actually measures, rather than asserting the
condition and calling it proof. It resets the request log, applies the shrink, waits for the
drift, then waits out a full extra poll so the operator has demonstrably looked at the cluster
again, and only then counts:

```
    detail polls in the same window: 3
    PASS the operator did keep polling, so the request log is live = yes
    PASS ChangeStorage calls while refusing to shrink = 0
```

The first of those two lines is the positive control, and it is the reason the second one is
worth anything. A count of zero is also what you get from a dead port-forward, an empty log or
a typo in the path filter, so the script asserts in the same window that the operator's
`products/details` polls did arrive. Zero resize calls next to a non-zero poll count is
evidence; zero next to zero is nothing.

The other refusal in this family is `allowedIPs`, which the API accepts at creation only. It is
reported as `NotApplied` and never applied, by the `shapeDifference` in `drift.go` with a nil
`apply`. `NotApplied` is also what `managementPolicy: Observe` produces for a difference the
operator could apply but may not; lab 04 shows that one, with the message naming the policy.

## A size the instance type does not sell

RavenDB Cloud sells whole-GB disks from a fixed menu per instance type. PB10 offers
`{10, 20, 30, 50, 75}`, PB20 offers `{20, 40, 80, 100, 150}`. Ask for 100 GB and the create is
refused:

```
NAME            STATUS        CHANGES   TIER   REGION   NODES   AGE
lab05-offmenu   InvalidSpec                                     0s

    Ready message: spec.storage.sizeGB 100 is not offered for PB10; set it to one of: 10, 20, 30, 50, 75.
    PASS Ready reason = InvalidSpec
    1 details polls logged since the reset, so the log is live
    PASS create requests the control plane saw for the off-menu manifest = 0
```

Read the reason and the last line together. `InvalidSpec`, not `Refused`, and the control
plane was never asked: this is the operator's own refusal, and the request that was always
going to come back a 400 never went out.

That is worth spelling out because it is a rule with an edge. `buildCreateRequest` calls
`ListInstanceTypes` to check that PB10 exists and belongs to the Production tier, and the same
response carries `availableDiskSizes`. Checking the disk against a menu already in hand costs
nothing, and it turns what would arrive as an HTTP 400 into a sentence that reads like a typo
with the alternatives attached. The staleness objection does not apply either: the menu comes
from the same live call that authorised the instance type, not from the regions cached on the
`CloudAccount`.

The day-2 path deliberately does **not** do this, and the asymmetry is the interesting part.
`storageDifference` has no instance-type menu, and getting one would mean a
`ListInstanceTypes` call on every drift pass, forever,
to catch a mistake that is made once. So an off-menu *resize* on a live cluster really is the
control plane's call, and it lands differently: the create case leaves the object unbound with
reason `InvalidSpec`, while a rejected resize happens on a cluster that is already bound and
serving, so it goes on `CHANGES` as `Refused` and `STATUS` stays `Running`. The refused
change is not sent again until the spec changes, and the cluster goes on being polled.

The rule, then, is: validate against the menu where the operator already holds it, and let the
control plane judge where fetching it would cost a request that nothing else needs.

Note what did not happen here: `status.productID` is empty, so nothing was created and nothing
is billing. The object stays `InvalidSpec` and looks again at its poll interval, with
whatever you have edited the number to by then. The `InvalidSpec` Warning is emitted once, when
the problem starts, rather than on every poll it lasts; the condition carries it from then on.

## The audit trail

Every reshape the operator issues also emits a Kubernetes event, so `kubectl get events` is a
record of what changed and when, in the same place as everything else in the namespace. The
event recorder collapses repeats of an identical message into one entry with a count, so every
line below is a distinct thing that happened:

```
TYPE      REASON            MESSAGE
Normal    Created           Created RavenDB Cloud cluster "lab05-prod" (10-A) in AWS/eu-central-1.
Warning   OpenAllowList     allowedIPs contains 0.0.0.0/0, so this cluster accepts connections from any address. ...
Normal    SecretPublished   Connection Secret "lab05-prod-connection" created.
Normal    Applying          Changing instance type from PB10 to PB20
Normal    Applying          Resizing storage from 20 GB to 40 GB
Normal    Waiting           RavenDB Cloud will take a change later (resizing storage from 40 GB to 80 GB): ...
Normal    Applying          Adding a node (3 -> 4)
Normal    Applying          Adding a node (4 -> 5)
Normal    Applying          Removing node E (5 -> 4)
Normal    Applying          Removing node D (4 -> 3)
```

Read top to bottom, that is the whole lab. Note that the sequence of individual node operations
is visible, which the manifest alone never shows you: `nodes: 5` is a number, but
`Adding a node (3 -> 4)` followed a poll later by `Adding a node (4 -> 5)` is what the cluster
actually did. Every poll rewrites the connection Secret's endpoints, so an application that
mounts it picks up the new node's URL without anybody telling it a node was added.

Normal events are actions; the one Warning is the open allow list the lab account uses, which is
fine for a lab and said once. The script asserts every `Applying` message in that list, so if a
future change to `reconcileDrift` stops emitting one, the lab fails rather than the audit trail
quietly thinning out.

## A restart is an action, not a state

Everything above was desired state: you write what the cluster should look like and the
operator converges on it. A restart is not that. Nothing about the cluster afterwards is
different, so no field could hold the value "restarted", and a field that meant "restart" would
re-fire on every resync. So it is an object of its own, `CloudClusterRestart`, which runs once
and is then the record of having run, the way a Job is.

It changes the cluster, so it obeys the cluster's policy. The lab sets `managementPolicy:
Observe` first and asks for a restart:

```
NAME            CLUSTER      NODE   STATUS    AGE   MESSAGE
lab05-restart   lab05-prod   A      Waiting   1s    CloudCluster "lab05-prod" has managementPolicy Observe, so the operator may not change it. Set spec.managementPolicy to Full and this runs.
    PASS nothing was asked of RavenDB Cloud yet =
    PASS restart calls while it waits = 0
```

It used to fail here, and since a restart runs once, failing meant writing another one after
fixing the policy. `Waiting` keeps the request. Setting the policy to `Full` is all it takes:

```
NAME            CLUSTER      NODE   STATUS      AGE
lab05-restart   lab05-prod   A      Succeeded   6s
    PASS restart calls = 1
    PASS a Normal event when the node goes down, because it was asked for mentions 'Normal Restarting: Restarting node A'
    PASS and a Normal event when it is back mentions 'Normal Restarted: Node A of CloudCluster "lab05-prod" restarted.'
```

The restart asks RavenDB Cloud whether the node came back rather than reading the
`CloudCluster`'s status, which is only as fresh as that cluster's last poll. A node the cluster
does not have is refused without a call, as `Failed`, with the nodes it does have in the
message; the record is immutable, so the message says to create a new one.

## Takeaway

Day 2 on RavenDB Cloud is three fields, and the operator's job is to be boring about them. It
compares desired to observed, applies at most one difference, records it as an event, marks the
object `Updating` and `Applying`, and comes back to do the next one. Differences it cannot apply
become `NotApplied` on `CHANGES` while `STATUS` stays `Running`, because a manifest that asks for
the impossible is a problem with the manifest, not with the database, and the same goes for a
change RavenDB Cloud postpones or refuses. If you remember one thing:
a change that does not appear in the request log did not happen, and the lab checks the log
rather than the intention.

# Lab 05: resize, rescale and restart a running cluster

> **Reference:** [Clusters](../docs/how-to-use/clusters.md) and
> [Restarting a node](../docs/how-to-use/restarting-a-node.md) cover the same ground as guides.

The disk fills up, the working set outgrows the memory, a busy quarter wants more nodes. RavenDB
Cloud can change three things on a running cluster: the instance type, the disk, and the node
count. In this lab you change all three by editing one manifest, and then restart a node.

## Do this

One manifest, applied again and again with different numbers:

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab05-prod
spec:
  accountRef:
    name: ravendb-cloud
  tier: Production
  instanceType: PB10
  nodes: 3
  storage:
    sizeGB: 20
  pollIntervalSeconds: 3       # lab only; 300 is the default
```

```bash
labs/05_day_two_operations.sh
```

The script creates the cluster, then steps through the changes below. Every event they leave is
listed at the end, so you can read the whole lab back with `kubectl get events`.

## Two changes in one apply

Change `instanceType: PB20` and `sizeGB: 40` together. You might expect two calls at once. The
operator sends one, waits for RavenDB Cloud to finish it, and sends the other:

```
after the first change landed, instanceType/disk/STATUS/CHANGES = PB20/20/Updating/Applying
reshaping calls the simulator received: instance-type storage
```

Changes go out one at a time because RavenDB Cloud applies them one at a time; while one is in
progress, the operator only watches. `STATUS` says `Updating` and `CHANGES` says `Applying` until
the manifest is carried through, so a cluster halfway through a resize never claims to match.

## A second disk change waits

RavenDB Cloud allows one storage change per cluster every six hours on AWS (twelve on Azure
premium disks), and the combined change above was this cluster's. Ask for 80 GB straight away:

```
NAME         STATUS    CHANGES   ...
lab05-prod   Running   Waiting   ...

RavenDB Cloud will take a change later (resizing storage from 40 GB to 80 GB): You've reached the
maximum storage modification rate. Wait at least 6 hours between each storage change. The next
storage change will be available in 6 hours 4 minutes. The operator asks again at each poll.
```

The cluster serves throughout, so `STATUS` stays `Running`. `CHANGES` says the spec is not
carried through yet, in RavenDB Cloud's own words, and the operator sends the change again on
each poll until the window opens. Nobody has to touch the object. The script then puts the size
back so the next steps start from a manifest that matches.

## Nodes, one at a time

```yaml
  nodes: 5
```

RavenDB Cloud has no "set the size" call, only add a node and remove a node, so 3 to 5 is two
additions, each once the cluster is Active again:

```
node tags after scaling out: A B C D E
Adding a node (3 -> 4)
Adding a node (4 -> 5)
```

Back to `nodes: 3` removes the highest tags first, `E` then `D`, so the same manifest always
leaves the same nodes. A Production cluster keeps its three standard nodes; the CRD refuses fewer.

## What it refuses

**A smaller disk.** Cloud disks do not shrink:

```
CHANGES NotApplied   storage size 20 GB is smaller than the current 40 GB; cloud disks cannot shrink.
```

`Ready` stays `True`: the cluster is fine, the manifest asks for something impossible. The script
waits through another poll and checks the simulator's log: no resize was sent.

**A disk size the instance type does not sell.** PB10 sells 10, 20, 30, 50 and 75 GB. A new
cluster asking for 100:

```
NAME            STATUS        ...
lab05-offmenu   InvalidSpec   ...

spec.storage.sizeGB 100 is not offered for PB10; set it to one of: 10, 20, 30, 50, 75.
```

The operator already had PB10's menu from checking the instance type, so it refused with the
alternatives, and the create never went out. Nothing was created, so fixing `sizeGB` is an edit.

## Restart a node

A restart is not a state you can write into the cluster's spec: afterwards nothing is different,
and a field meaning "restart" would fire again on every pass. So it is an object of its own that
runs once, the way a Job does:

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudClusterRestart
metadata:
  name: lab05-restart
spec:
  clusterRef:
    name: lab05-prod
  nodeTag: A
```

```bash
kubectl -n apps create -f restart.yaml
```

It changes the cluster, so it follows the cluster's policy. The lab sets `managementPolicy:
Observe` first:

```
NAME            CLUSTER      NODE   STATUS    MESSAGE
lab05-restart   lab05-prod   A      Waiting   CloudCluster "lab05-prod" has managementPolicy Observe, so the operator may not change it. Set spec.managementPolicy to Full and this runs.
```

Set the policy back to `Full` and it runs on its own, ending in `Succeeded` with a `Restarting`
and a `Restarted` event. It cannot be edited afterwards; to restart again, create another. A node
the cluster does not have, such as `Z`, ends in `Failed` without a call, and the message lists
the nodes there are. Create restarts with `kubectl create` and keep them out of git, since each
one is a one-off action.

## Takeaway

Editing the manifest is how you change a running cluster, and the operator applies one change at
a time, says on `CHANGES` what it is doing or waiting for, and records each step as an event.
What RavenDB Cloud cannot do is refused with the reason, without a call. A restart is an object
that runs once, not a field.

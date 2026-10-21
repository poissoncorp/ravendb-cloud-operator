# Lab 04: take over a cluster that already exists

> **Reference:** [Taking over](../docs/how-to-use/taking-over.md) covers the same ground as a guide.

Your clusters probably exist already, made in the portal before anyone talked about an operator.
A `CloudCluster` that names one finds it and manages it, without creating anything and without
changing it until you say so. This is also how a Kubernetes cluster is rebuilt from git: apply the
same manifests somewhere new and every object finds its cluster again.

## Do this

The script first seeds a three node `PB10` cluster in the simulator, called `lab04-legacy-<run>`
(four characters unique to the run), standing in for one a colleague made months ago. Then a name
and a tier are enough:

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab04-legacy-wi5o      # the cluster's name in RavenDB Cloud
spec:
  accountRef:
    name: ravendb-cloud
  tier: Production             # must be what the cluster is
```

```bash
labs/04_take_over_an_existing_cluster.sh
```

## What you see

```
NAME                STATUS    CHANGES   TIER         REGION         NODES   AGE
lab04-legacy-wi5o   Running   Applied   Production   eu-central-1   3       2s
```

`-o wide` adds `ADOPTED true`, `POLICY Observe` and `INSTANCE PB10`. No create, no provisioning
wait: the operator looked the name up, found one cluster, read it and
published its connection Secret. The script checks the simulator's log to be sure no create was
sent. And it left a Warning:

```
Warning Adopted: Found RavenDB Cloud cluster "lab04-legacy-wi5o" (7-A) already running and took
it over; managing it with managementPolicy Observe, which changes nothing, until
spec.managementPolicy is Full. Nothing in the manifest asked for an existing cluster: set
spec.creationMode to TakeOverOnly to confirm this one is meant.
```

A Warning, because this manifest would have created a cluster had the name been free. A name can
match by accident, and this is how you notice. `creationMode: TakeOverOnly` says you expect the
cluster to exist; then the event is Normal, and a missing cluster is reported rather than created.

**Observe** is the policy a found cluster gets unless you choose one. The operator reads the
cluster, publishes its Secret and reports how your manifest differs, and changes nothing in
RavenDB Cloud.

`CHANGES` says `Applied` because only what a manifest sets is compared. This one sets a tier, and
the tier matches.

## A field written by hand

Now the manifest says `instanceType: PB20` for a cluster that runs `PB10`, the kind of slip you
make copying a shape from the wrong environment:

```
NAME                STATUS    CHANGES      ...
lab04-legacy-wi5o   Running   NotApplied   ...

instanceType differs (desired PB20, actual PB10); managementPolicy is Observe, the default for a
cluster this object found already running, so no changes were applied; set
spec.managementPolicy to Full to apply them.
```

The cluster still serves, so `STATUS` stays `Running`. Nothing was resized: that is why a found
cluster starts under Observe. When the difference is what you want, say so:

```yaml
spec:
  tier: Production
  instanceType: PB20
  managementPolicy: Full
```

and the operator changes the instance type, with a `Normal Applying` event.

## Write the manifest from the cluster

Guessing a cluster's shape by hand is where the slips come from. The export writes the manifest
from the cluster itself:

```bash
kubectl ravendb-cloud export --account ravendb-cloud -n apps lab04-legacy-wi5o > legacy.yaml
```

It reads the API key from the account's Secret with your kubeconfig. The lab's simulator is only
reachable inside kind, so the script runs the same command in a pod from the operator image
instead. What comes out:

```yaml
# Written from RavenDB Cloud cluster "lab04-legacy-wi5o" (7-A) on 2026-09-30T14:15:16Z.
spec:
  accountRef:
    name: ravendb-cloud
  creationMode: TakeOverOnly
  displayName: lab04-legacy-wi5o
  instanceType: PB20
  managementPolicy: Full
  networking:
    allowedIPs:
    - 0.0.0.0/0
  nodes: 3
  provider: AWS
  region: eu-central-1
  releaseChannel: Stable72
  storage:
    sizeGB: 20
    type: SsdStandard
  tier: Production
```

Every field the cluster has is written down, so nothing differs, and `managementPolicy: Full`
has nothing to change. The script deletes the object it had (the cluster keeps running, since
`deletionPolicy` defaults to `Retain`), applies the export, and checks that no call that creates
or reshapes anything went out. Commit that file and the cluster is in git.

## When a name does not lead to one cluster

The operator would rather stop than guess, because binding to the wrong cluster is silent and
lasting. Each of these leaves `status.productID` empty, writes no Secret and says why:

- **A second object for the same cluster:** `NameTaken`, naming the object that already manages
  it. Two objects would take turns reshaping one cluster.
- **A tier that does not match:** `NameTaken`. A Production cluster cannot be the Development
  cluster the manifest describes. The message says to set another `spec.displayName` for a new
  cluster, or the cluster's own values to manage that one; both are edits, since nothing is bound.
- **A name nothing carries, under explicit `managementPolicy: Observe`:** `ClusterNotFound`.
  Observe never creates, so a typo in the name is a report, never a new cluster.
- **Two clusters with one name:** `NameTaken`, naming both. RavenDB Cloud refuses a create whose
  name is taken, so this only happens by accident; the simulator can stage it, a real account
  cannot. One of the two may be production, so the operator binds to neither.

## Takeaway

The name is the identity: a manifest that names an existing cluster takes it over and changes
nothing until `managementPolicy` is `Full`. Write the manifest with the export and taking over is
one `kubectl apply` that changes nothing. When a name does not lead to exactly one cluster this
object may manage, the operator stops and says why.

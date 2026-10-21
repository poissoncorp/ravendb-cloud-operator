# Lab 04: Take over a cluster that already exists

> **Why this lab matters more than it looks.** The obvious framing is "somebody made a cluster
> in the portal", which is true and forgettable. The version worth remembering is that this is
> also how a Kubernetes cluster is rebuilt: apply the same manifests to a new one, and every
> `CloudCluster` finds the RavenDB Cloud cluster its name belongs to instead of creating a
> second. There is no separate "adopt" mode to remember to switch on. A manifest names a
> cluster, and the operator either finds it or creates it.

**For:** teams whose RavenDB Cloud clusters exist already, made in the portal long before anyone
proposed an operator. You want the cluster's URLs and client certificate delivered into a
namespace as a Secret, and you want that without a controller deciding it now owns production.
The lab takes over a cluster with a two-field manifest, shows a hand-written manifest that gets
a field wrong being reported and not applied, writes a manifest from the cluster itself with the
export tool, and then shows the four ways binding refuses. It deletes its own objects at the end
and removes the clusters it seeded.

## Run it

```bash
labs/04_take_over_an_existing_cluster.sh
```

The script seeds a cluster through the lab control plane first, a three node `PB10` called
`lab04-legacy-<run>`, where `<run>` is four characters unique to the run. It stands in for
something a colleague set up in the portal months ago. The name carries the run id for a reason
the rest of the lab keeps coming back to: in RavenDB Cloud a display name belongs to one cluster,
and a name that was used once stays taken.

The complete example is [`04_take_over_an_existing_cluster.sh`](04_take_over_an_existing_cluster.sh).
The core is a name and a tier:

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab04-legacy-wi5o     # the display name the cluster has in the portal
spec:
  accountRef:
    name: ravendb-cloud
  tier: Production            # has to say what the cluster is
```

There is no `spec.displayName` here because it defaults to the object name, and no provisioning
to wait for. The object settles as fast as the operator can list the account's clusters, read
the one it matched, and download a certificate:

```
NAME                STATUS    CHANGES   TIER         REGION         NODES   AGE   POLICY    INSTANCE   CLUSTER-ID   URL                                              MESSAGE
lab04-legacy-wi5o   Running   Applied   Production   eu-central-1   3       2s    Observe   PB10       7-A          https://a.lab04-legacy-wi5o.acme.ravendb.cloud   The cluster is Active with 3 node(s), and Secret "lab04-legacy-wi5o-connection" holds its URLs and client certificate.
```

Product ids and counts in your run depend on what the earlier labs created, so `7-A` may be
another number. The script reads all of them from the control plane rather than assuming them.

## What finding a cluster does

The operator looked the name up, found one cluster, and bound to it. It records that it found
the cluster rather than creating it, and that decides the policy it runs under:

```
==> Taking it over with a name and a tier
    PASS STATUS = Running
    PASS CHANGES = Applied
    PASS status.productID = 7-A
    PASS status.adopted = true
    PASS managementPolicy for a cluster it found = Observe
    PASS status.instanceType, read from the cluster = PB10
    PASS the Adopted event says what it found and how it manages it mentions
         'Warning Adopted: Found RavenDB Cloud cluster "lab04-legacy-wi5o" (7-A) already running
          and took it over; managing it with managementPolicy Observe, which changes nothing,
          until spec.managementPolicy is Full'
    PASS and, since nothing asked for an existing cluster, how to say it was meant mentions
         'set spec.creationMode to TakeOverOnly to confirm this one is meant'
```

The event is a Warning because this manifest never said it expected a cluster to exist. Under
the default `creationMode`, `TakeOverOrCreate`, a name that matches by accident is taken over
just the same, and the Warning is how that gets noticed. `creationMode: TakeOverOnly` says the
cluster is meant to be there, and then the event is Normal; the export tool further down writes
it into every manifest it produces.

`Observe` changes nothing in RavenDB Cloud, creating included. It still reads the cluster, still
publishes the connection Secret, and still reports on `CHANGES` how the manifest differs. The
point of making it the default for a found cluster is in the next section.

`CHANGES` is `Applied` because only the fields a manifest sets are managed. This one sets a tier,
and the tier matches. Instance type, disk, node count and allow list are all whatever the cluster
has, and none of them are asked for, so nothing differs.

"It did not create anything" is the kind of claim a lab should not ask you to take on trust, so
the script measures it. It resets the control plane's request log first, then counts:

```
==> Nothing was created
    PASS cluster count after taking over = 1
    PASS clusters named lab04-legacy-wi5o = 1
    PASS recorded POST /api/v1/products/create calls = 0
```

Everything downstream of binding is shared with a cluster the operator created. The same code
publishes the same eleven-key Secret, owned by the `CloudCluster` through `ownerReferences`, and
the script compares the full sorted key list rather than a sample of it. Lab 03 walks through
what each key is for.

## A manifest written by hand

Writing the manifest yourself is where it goes wrong. The lab writes down `instanceType: PB20`
for a cluster that runs `PB10`, the kind of slip anybody makes copying a shape from the wrong
environment:

```
NAME                STATUS    CHANGES      TIER         REGION         NODES   AGE
lab04-legacy-wi5o   Running   NotApplied   Production   eu-central-1   3       5s

    Synced message: instanceType differs (desired PB20, actual PB10); managementPolicy is
    Observe, the default for a cluster this object found already running, so no changes were
    applied; set spec.managementPolicy to Full to apply them.
    PASS instance-type calls while Observe = 0
    PASS the cluster is still PB10 = PB10
```

`STATUS` stays `Running`, because the cluster serves either way. `CHANGES` says the manifest and
the cluster disagree, which field, both values, why nothing was done and what would do it. The
script waits out a full poll before it counts the instance-type calls, because zero calls in a
window the operator never looked in proves nothing.

That is the reason a found cluster defaults to `Observe`. If it defaulted to `Full`, the first
pass after `kubectl apply` would resize a production cluster to match a typo.

When the difference is what you want, say so:

```yaml
spec:
  tier: Production
  instanceType: PB20
  managementPolicy: Full
```

```
==> managementPolicy: Full, and the change goes out
    PASS status.managementPolicy = Full
    PASS status.instanceType = PB20
    PASS CHANGES = Applied
    PASS instance-type calls once Full = 1
    PASS the change has an event mentions 'Normal Applying: Changing instance type from PB10 to PB20'
    PASS status.adopted is still true: Full is a choice, not a claim of authorship = true
```

`status.adopted` does not change. It records how the object came by its cluster, which is
history; `managementPolicy` is what the object may do now, which is yours to set.

## Writing the manifest from the cluster itself

A hand-written manifest is a guess about a cluster that already exists. The export tool writes
the manifest from the cluster instead, so it matches exactly and applying it changes nothing.

It ships in the operator image, so it runs in the cluster, next to the Secret that holds the API
key, with nothing to download:

```bash
kubectl -n apps run export --rm -i --quiet --restart=Never \
  --image=<the operator image> \
  --override-type=strategic \
  --overrides='{"spec":{"containers":[{"name":"export","env":[
      {"name":"RAVENDB_CLOUD_API_KEY",
       "valueFrom":{"secretKeyRef":{"name":"ravendb-cloud-api-key","key":"apiKey"}}}]}]}}' \
  --command -- /kubectl-ravendb_cloud export --account ravendb-cloud -n apps lab04-legacy-wi5o \
  > lab04-legacy.yaml
```

The `--overrides` is there because `kubectl run` has no flag for an environment variable read
from a Secret, and the key is exactly what should not be typed on a command line: `--env` would
put it in shell history, in the pod spec, and in the output of `kubectl get pod -o yaml`. With a
`secretKeyRef` it goes from the Secret into the container's environment and nowhere else, and
the script checks the printed manifest does not contain it. Against the stand-in the script also
points the pod at the lab control plane, the same way the lab's `CloudAccount` is pointed there.

`--all` in place of the names exports every cluster in the account that has not been terminated.
In Git Bash on Windows, put `MSYS_NO_PATHCONV=1` in front of the command, or the shell rewrites
`/kubectl-ravendb_cloud` into a Windows path that does not exist in the image.

What it prints:

```yaml
# Written from RavenDB Cloud cluster "lab04-legacy-wi5o" (7-A) on 2026-09-30T14:15:16Z.
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab04-legacy-wi5o
  namespace: apps
spec:
  accountRef:
    name: ravendb-cloud
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

Every field the cluster has is written down, so every field is managed from now on, and
`managementPolicy` is `Full` because there is nothing to protect the cluster from: the manifest
already says what the cluster is. `subdomain` is left out, since it is only ever sent when the
operator creates a cluster.

The lab deletes the object it had (the cluster keeps running, because `deletionPolicy` defaults
to `Retain`), applies the export, and measures:

```
==> Applying it takes the cluster over with no change
    PASS STATUS = Running
    PASS CHANGES = Applied
    PASS status.productID = 7-A
    PASS status.adopted = true
    PASS managementPolicy, from the manifest = Full
    PASS the Adopted event names the policy it runs under mentions 'already running; managing it with managementPolicy Full'
    PASS calls that create or reshape anything = 0
```

Commit that file and the cluster is in git. The first change anybody makes to it is the one they
edit in, reviewed like any other change, and a Kubernetes cluster rebuilt from the same
repository finds the same RavenDB Cloud cluster again.

## When binding refuses

Four manifests in this lab do not bind, and each one is refused rather than guessed at, because
a wrong guess is silent and durable: the object would record `status.productID` for the wrong
cluster, and from then on it would be bound to it for good.

```
NAME                STATUS            CHANGES   TIER         REGION         NODES   AGE
lab04-legacy-wi5o   Running           Applied   Production   eu-central-1   3       5s
lab04-not-there     ClusterNotFound                                                 2s
lab04-ambiguous     NameTaken                                                       0s
```

The objects that never bound show only `STATUS`, because every other column reads status the
operator never wrote for them. In every case `Ready` is `False`, `status.productID` is empty, no
Secret was written, nothing was created, and a Warning event marks the moment it started.

**A second object for the same cluster.** Two objects managing one cluster would take turns
reshaping it, so the second is refused before it binds, and the message names the first:

```
RavenDB Cloud cluster "lab04-legacy-wi5o" (7-A) is already managed by CloudCluster
apps/lab04-legacy-wi5o, and two objects managing one cluster would fight over it, so this one
is not bound. Delete one of the two.
```

**A name on a cluster of another tier.** Tier cannot change on either side, so a manifest that
says `Development` for a `Production` cluster describes some other cluster than the one the name
found. The same holds for `provider` and `region` when the manifest sets them:

```
A RavenDB Cloud cluster named "lab04-legacy-wi5o" exists, but it is Production and spec.tier
says Development, so this object was not bound to it and nothing was changed. Those fields cannot
be edited, so delete this object and apply it again: with another displayName to create a new
cluster, or with the cluster's own values to manage that one.
```

**A name nothing carries, under `Observe`.** Without a policy the operator would create the
cluster, which is what a new manifest is for. With an explicit `managementPolicy: Observe` it
never creates anything, so a typo in the name is a report rather than a new cluster:

```
No RavenDB Cloud cluster named "lab04-missing-wi5o" in this account, and managementPolicy Observe
never creates one. Set spec.managementPolicy to Full to create it. If the name is wrong, delete
this object and apply it again with the right spec.displayName; nothing was created.
```

A `Free` tier object gets the same `ClusterNotFound` without being asked, because RavenDB Cloud
creates Free clusters only in the portal. It finds one by its name or waits for one to appear.

**Two clusters, one name.** RavenDB Cloud checks the name on every create, so two clusters can
only share one if they were created at the same moment. The stand-in can seed that directly,
and the lab does; a real account cannot be made to, which is why this step runs against the
stand-in only:

```
Not bound: 2 RavenDB Cloud clusters are named "lab04-twin-wi5o" (8-A, 9-A). RavenDB Cloud cannot
rename a cluster, so terminate or hide in the portal the one this object should not manage, and
the next pass binds the other.
```

One of the two may be production and the operator has no way to tell which, so it binds to
neither. The way out is on the RavenDB Cloud side, because no manifest edit can make one name
mean one cluster.

## Why the name is a safe identity

A display name is unique per account among clusters not hidden in the portal, terminated ones
included, and RavenDB Cloud has no way to rename a cluster. So a name means one cluster for as
long as that cluster exists, and `spec.displayName` can be neither changed nor added later.

Once an object is bound, `status.productID` is the operator's memory and outranks the name: it
is never looked up again for that object. A terminated cluster keeps its name but is skipped when
a name is looked up, so an object never binds to something that no longer runs; applying a
manifest for a name only a terminated cluster holds is refused by RavenDB Cloud, and the message
says to hide the terminated one in the portal first.

## Deleting

Every object in this lab is deleted at the end, and the cluster it found keeps running:
`deletionPolicy` defaults to `Retain`, whether the object found its cluster or created it.
`Terminate` on a cluster the object found is allowed and does what it says, whatever
`managementPolicy` is; lab 06 shows both.

## Takeaway

The display name is the identity. A manifest that names an existing cluster takes it over, reads
it, publishes its Secret and changes nothing until `managementPolicy` is `Full`; one that names
nothing creates it. Write the manifest with the export tool rather than by hand, and taking a
cluster over is one `kubectl apply` that changes nothing. When a name does not lead to exactly
one cluster this object may manage, the operator stops and says why in `kubectl get`, because
binding to the wrong production database silently is the one failure mode worse than not
starting.

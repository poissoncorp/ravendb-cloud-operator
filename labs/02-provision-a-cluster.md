# Lab 02: provision your first cluster

> **Reference:** [Clusters](../docs/how-to-use/clusters.md) and [status and monitoring](../docs/how-to-use/status-and-monitoring.md) covers the same ground as a guide rather than a walkthrough.

**For:** anyone who has a Ready `CloudAccount` and now wants a RavenDB Cloud cluster that
Kubernetes owns. You will write the smallest manifest that creates one, read back everything
the operator decided on your behalf, watch the `STATUS` and `CHANGES` columns on the way from
nothing to `Running`, and see the one status field that lets a create interrupted by a crash
find its own cluster again. By the end you will know which values live in `spec`, which live
in `status`, and why the split is where it is.

## Run it

```bash
labs/02_provision_a_cluster.sh
```

The complete example is [`02_provision_a_cluster.sh`](02_provision_a_cluster.sh). The core is
three fields:

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab02-orders         # also the display name in the portal, and the identity
  namespace: apps
spec:
  accountRef:
    name: ravendb-cloud      # the verified credential from lab 01
  tier: Development          # which set of shapes you are allowed to order from
  storage:
    sizeGB: 20               # must be on the chosen instance type's menu
```

Nothing else is required. Provider, region, release channel and the firewall allow list come
from the `CloudAccount`. The instance type is chosen from the live list. And the display name
is the object name: the operator looks for a cluster called `lab02-orders` in the account,
finds none, and creates one. Had it found one, it would have taken that one over instead,
which is [lab 04](04-take-over-an-existing-cluster.md).

## Two columns, two questions

`kubectl get cloudcluster` answers two questions with one word each:

```
NAME           STATUS     CHANGES   TIER          REGION         NODES   AGE
lab02-orders   Creating             Development   eu-central-1   1       0s
```

`STATUS` is the reason on the `Ready` condition: can I use it? It is `Running` exactly when
`Ready` is `True`, and otherwise it names the stage or the blocker. `CHANGES` is the reason on
the `Synced` condition: did my manifest reach RavenDB Cloud? It is `Applied` when everything
the manifest sets matches the cluster.

There is no phase. Every word the table shows is a condition reason, so the column you read,
the condition an alert rule matches on and the value a script waits for are the same string,
listed with its meaning in `kubectl explain cloudcluster.status.conditions`.

The lab starts with the stage you normally see for a second at most. A throwaway object points
at a `CloudAccount` that does not exist:

```
==> WaitingForAccount: a cluster whose prerequisite is not there
    message: CloudAccount "lab02-missing-account" not found in namespace "apps". If the name is
    wrong, delete this object and apply it again with the right spec.accountRef; nothing was created.
    PASS lab02-pending STATUS = WaitingForAccount
    PASS lab02-pending Ready condition = False
    PASS nothing was created in the cloud = 0
```

The account is a hard prerequisite for the whole reconcile, so an unusable credential means the
operator never reaches the code that can spend money. The message says how out of a typo, too:
`accountRef` cannot be edited, so the edit you would reach for first is one the API server
refuses. `WaitingForAccount` records no event, because every object applied in the same file as
its account passes through it; the account warns about its own problems.

Then the minimal manifest. The pass that sends the create reads the new cluster straight back,
so `TIER`, `REGION` and `NODES` are filled in before the cluster can be used. That is the
stand-in; a real create may report less this early, which is why the script counts the empty
cells below rather than assuming one:

```
==> Provisioning from the smallest manifest that works
    NAME           STATUS     CHANGES   TIER          REGION         NODES   AGE
    lab02-orders   Creating             Development   eu-central-1   1       0s

    PASS lab02-orders STATUS = Creating
    PASS raw cloud status, passed through = Creating
    PASS Ready while the cloud builds = False
    PASS status.productID is set
    PASS status.provisionAttemptedAt is set
    PASS status.provisionAttemptedName, the name the create carried = lab02-orders
    PASS CHANGES, not set until there is a running cluster to compare =
    PASS columns in the header = 7
    PASS fields in the row, one short for each empty cell (1) = 6
```

`CHANGES` is empty, not `Applied`: there is nothing to compare a manifest against until
RavenDB Cloud says the cluster is `Active`. That empty cell is worth a minute, because it is a
trap you will hit in your own scripts. Server-side printing leaves the cell blank, the blank
collapses into the whitespace around it, and the row has one field fewer than the header. So
`kubectl get ... | awk '{print $4}'` reads the region as the tier. Parse these by field name,
with `-o jsonpath` or `-o custom-columns`, never by position.

`status.cloudStatus` carries the raw RavenDB Cloud string (`Creating` here) unmodified, so a
status the operator has never heard of is visible rather than flattened.

A minute later everything has settled:

```
NAME              STATUS    CHANGES   TIER          REGION         NODES   AGE
lab02-orders      Running   Applied   Development   eu-central-1   1       32s
lab02-analytics   Running   Applied   Production    eu-west-1      3       30s
```

```
==> Running: the cloud says Active and the Secret is published
    PASS lab02-orders STATUS = Running
    PASS raw cloud status = Active
    PASS Ready condition = True
    PASS the Ready message counts the nodes mentions 'The cluster is Active with 1 node(s)'
    PASS and names the Secret it published mentions 'Secret "lab02-orders-connection" holds its URLs and client certificate'
    PASS lab02-orders CHANGES = Applied
    PASS Synced message = Everything this manifest sets matches RavenDB Cloud.
```

`Running` means the cluster is `Active` **and** its connection Secret is published, because
that is what an application needs before it can connect. A cluster that is up with a Secret the
operator could not write is `SecretNotWritten`, not `Running`.

`-o wide` adds five columns, declared with `priority=1` so they stay out of the default line:

```
POLICY   INSTANCE   CLUSTER-ID   URL                                         MESSAGE
Full     Dev10      1-A          https://a.lab02-orders-dev.acme.ravendb.cloud   The cluster is Active with 1 node(s), and Secret "lab02-orders-connection" holds its URLs and client certificate.
```

`POLICY` is the resolved `managementPolicy`: `Full` for a cluster this object created, and
`Observe` for one it found already running. `MESSAGE` is the `Ready` message, which is where
the operator says what happens next or what to do.

(Product ids are assigned by the control plane in creation order, so yours will differ.)

## What the operator filled in

The resolved shape is readable straight off the object:

```
STATUS    INSTANCE   CHANNEL    PROVIDER   REGION         DISK
Running   Dev10      Stable72   AWS        eu-central-1   20
```

Each of those came from somewhere different, and the script asserts every one:

```
==> What the operator resolved for the minimal manifest
    PASS provider, from the CloudAccount default = AWS
    PASS region, from the CloudAccount default = eu-central-1
    PASS channel, the account default the cloud reported = Stable72
    PASS instance type, smallest for Development = Dev10
    PASS Dev10 vCPUs = 2
    PASS Dev10 RAM in MB = 512
    PASS nodes that come with Dev10 = 1
    PASS disk, the one thing you asked for = 20
    PASS disk class default = SsdStandard
    PASS management policy for a cluster this object created = Full
    PASS subdomain, derived from the display name = https://a.lab02-orders-dev.acme.ravendb.cloud
```

- **provider** and **region** fall back to `CloudAccount.spec.defaults`. Set them once for the
  team and cluster manifests stay down to what differs.
- **releaseChannel** has one more fallback than the others. `spec.releaseChannel`, then
  `CloudAccount.spec.defaults.releaseChannel`, then `CloudAccount.status.defaultReleaseChannel`,
  which is whatever RavenDB Cloud says the account default is (`Stable72` in the lab data). The
  lab account sets no channel default, so the value you see was reported by the cloud, not
  written by anybody.
- **instanceType** is the smallest shape offered for the tier: fewest vCPUs, then least RAM,
  then name. In the Development tier that is `Dev10` (2 vCPU, 512 MB) ahead of `Dev20`
  (2 / 1 GB) and `Dev30` (2 / 2 GB). "Smallest" is defined that way on purpose, so the default
  is the cheapest thing that satisfies the tier rather than whichever entry the list happened to
  return first.
- **subdomain** is the one that catches people. It is not in the minimal manifest, so the
  operator derived it from the display name, and the node URLs above are the proof:
  `https://a.lab02-orders-dev.acme.ravendb.cloud`, where RavenDB Cloud adds `-dev` for a
  Development cluster. The OpenAPI document marks `subdomainName`
  nullable and says nothing about its shape, but RavenDB Cloud feeds it to a domain-name
  validator that rejects an empty value and caps it at **13 characters**, letters digits and
  dashes only, no dash at either end, and not already taken by anybody. So the field is
  effectively required, and a display name longer than 13 characters cannot supply it. The
  operator refuses rather than truncating, because a silently invented DNS name is worse than
  an error that tells you which field to set:

  ```
  STATUS InvalidSpec: the subdomain derived from displayName "orders-production" is
  "orders-production", which is 17 characters, and RavenDB Cloud accepts at most 13.
  spec.subdomain cannot be added to an existing object, so delete this one and apply it again
  with a subdomain of up to 13 characters; nothing was created.
  ```

  That is why the fully specified manifest below sets `subdomain: lab02-stats` rather than
  reusing its own 15-character object name.
- **storage.type** has no CRD default on purpose: the right answer depends on the provider
  (`SsdPremium` is the only class Google Cloud products offer), which a static default cannot
  know. `deletionPolicy: Retain` and `pollInterval: 5m` do default through the CRD, so the
  apiserver writes them into `spec` at admission and they are there in `kubectl get -o yaml`.

The events record what the operator did, which is usually the fastest way to answer "why is
this cluster in Frankfurt":

```
TYPE      REASON            MESSAGE
Normal    Created           Created RavenDB Cloud cluster "lab02-orders" (1-A) in AWS/eu-central-1.
Warning   OpenAllowList     allowedIPs contains 0.0.0.0/0, so this cluster accepts connections from any address. That is fine for a lab and worth narrowing before it holds anything real.
Normal    SecretPublished   Connection Secret "lab02-orders-connection" created.
```

Normal events are actions in RavenDB Cloud and in the namespace. A Warning is something a person
may have to act on, and a create that went to plan has at most this one: the lab account's
default allow list is open to the world, which is right for a lab and worth saying once about
anything real. Against a real account the labs use a documentation-only address instead, and the
script then asserts no Warning at all.

## Why defaulting is resolved into status and not into your spec

The fields above are resolved on every reconcile and written to `status`. The operator never
writes to `spec`, and the script proves it:

```
    PASS spec.instanceType is still unset =
    PASS spec.provider is still unset =
    PASS spec.displayName is still unset, so the object name is the identity =
    PASS generation, untouched by the operator = 1
    PASS the finalizer it wrote instead = cloud.ravendb.net/finalizer
    PASS observedGeneration matches what was applied = 1
    PASS pollInterval is still the string in the manifest = 5m
```

`metadata.generation` is 1, and it took a bug fix to be. For a custom resource with a status
subresource the apiserver bumps `generation` only when `spec` changes; a write that touches
metadata alone, such as adding a finalizer, does not.

The finalizer used to go out as an `Update`, which sends the object back whole, and a spec that
has been through Go and back is not the spec that was applied: `pollInterval: 5m` came back as
`5m0s`, because that is how `metav1.Duration` renders itself, and `storage` and `networking`
appeared as `{}` because `omitempty` does nothing for a struct. So the apiserver saw a changed
spec on every object's first reconcile, and the object was left permanently unequal to the
manifest it came from. That is drift a GitOps tool reports and can never resolve. Finalizers are
a merge patch of the difference now, and `generation` is once again a reliable "did anybody
rewrite my spec" signal.

What is missing is a mutating admission webhook filling in the interesting values, and that
absence is a design decision rather than an unfinished one. Three reasons:

**The answer is not knowable at admission time.** "The smallest Development shape" is a fact
about the live instance types for one provider and one region in one account, which the apiserver
cannot see. Anything a webhook baked into `spec` would be a snapshot read at
`kubectl apply` time that then quietly rots.

**It would fight whatever generates your manifests.** If the operator wrote `instanceType:
Dev10` into a spec that Git says has no `instanceType`, the next `kubectl apply` or Argo sync
removes it and the next reconcile puts it back. That flapping is not a hypothetical failure
mode of mutating defaults, it is the usual one.

**`status` is an observation, not a memory.** `status.instanceType` is not "what the operator
decided when it created this". It is overwritten from the cluster the operator read back out
of RavenDB Cloud on the current poll. If somebody resizes the cluster in the portal, that
field tells you the truth within one poll instead of reporting an old decision. Only the fields
you set are managed, so a field you left out is left as the cluster has it, and `CHANGES` stays
`Applied`; a field you set that the cluster no longer matches shows up on `CHANGES` instead.

`status.adopted` is a plain boolean with `omitempty`, so a cluster this object created never
stores the field at all. It decides the default `managementPolicy`, which is why `POLICY` is
the column to read rather than this one:

```
    PASS status.adopted, omitted from status while it is false =
    PASS the same field via custom-columns = <none>
```

## Crash safety: the marker stamped before the create call

Provisioning has one genuinely dangerous window. The operator calls create, RavenDB Cloud starts
building, and then the operator dies before it can write the returned product id into `status`.
On restart it has an object with no id, and a cluster in the account with the object's name.

The name alone would bind the object to that cluster: finding a cluster by its name is what
taking over is. What the name cannot say is whose cluster it is. Taken for one somebody else
made, it would be marked `adopted`, managed under `Observe`, and this object's own cluster would
quietly stop being managed.

The fix is `status.provisionAttemptedAt`, and the order of operations is the whole trick:

```go
// internal/controller/binding.go
attempted := metav1.Now()
cluster.Status.ProvisionAttemptedAt = &attempted
cluster.Status.ProvisionAttemptedName = request.DisplayName
r.ready(ctx, cluster, v1alpha1.ReasonCreating, "Asking RavenDB Cloud for a new cluster.")
if err := r.Status().Update(ctx, cluster); err != nil {   // persisted BEFORE the side effect
    return "", ctrl.Result{}, err
}
productID, err := cloud.CreateCluster(ctx, request)
```

The marker is written to the apiserver before the create call goes out, so any crash after that
point leaves evidence behind. The name goes in beside it, and recovery looks the cluster up by
that name rather than by spec, because the name a create actually carried is the only one it can
have landed under.

The stand-in lets the lab stage the crash for real. `lab02-twin` starts under
`managementPolicy: Observe`, so nothing is created; the lab writes the marker into its status the
way the create path would; then a cluster called `lab02-twin` appears in the control plane, as if
the create had landed; and the policy is dropped, which is the first pass after the restart:

```
==> Crash safety: an interrupted create finds its own cluster, and knows it is its own
    PASS lab02-twin STATUS under Observe = ClusterNotFound
    status now carries the marker a create stamps: provisionAttemptedAt 2026-09-30T14:05:13Z
    and the control plane holds lab02-twin as 3-A, which no status names yet
    dropping Observe: this is the first pass after the restart
    PASS it bound the cluster its create made = 3-A
    PASS no second cluster was created for the name = 1
    PASS status.adopted, left false because the marker says this object created it =
    PASS so managementPolicy resolves to Full, not Observe = Full
    PASS the event says what it found mentions 'Created: Found the cluster an interrupted create made: "lab02-twin" (3-A)'
    PASS CHANGES = Applied
```

No second create, and the cluster is managed as the object's own. The same lookup without the
marker is the takeover lab 04 shows: `adopted` true, `Observe`, and an `Adopted` event instead of
`Created`.

`Observe` is doing real work in that setup, and it is worth knowing why it was enough. An
object under `Observe` with no cluster to find reports `ClusterNotFound` and creates nothing. It is
the control for when the operator should keep its hands off, and it holds creating back as firmly
as changing.

## Requeue cadence

The operator paces its calls, so how often it looks is deliberate:

| Situation | Requeue |
|-----------|---------|
| Waiting on a prerequisite (`WaitingForAccount`) | 20s |
| Transitional (`Creating`, `Updating`) | 30s, or `spec.pollInterval` if that is shorter |
| Settled | `spec.pollInterval`, default 5m |

```
==> Requeue cadence
    the operator read lab02-orders 3 time(s) between the create call and Running
    PASS a whole provision cost at most 6 reads, so nothing is spinning = yes
    sampling a 16 second window with both clusters settled and Running
    lab02-twin   (pollInterval 5s)  : 3 call(s)
    lab02-orders (pollInterval 5m)  : 0 call(s)
    PASS a settled cluster is re-read on spec.pollInterval = yes
    PASS the 5m default is re-read far less often than the 5s one = yes
```

Those numbers come from the lab control plane's request log, not from the operator. A create
that the stand-in settles in five seconds costs three reads in a clean run: the one right after
the create call, the one 30 seconds later that finds it Active, and one more when writing the
connection Secret wakes the controller through its own `Owns` watch. Where the create lands
inside the 30 second requeue moves that number, so the script asserts six or fewer rather than
pinning it to three. The point either way is the order of magnitude: nothing polls in between,
because the controller ignores status-only updates (`ignoreStatusOnlyUpdates` in
`internal/controller/shared.go`), so its own status writes do not re-trigger it and the cadence
really is the requeue.

The lab gives `lab02-twin` a `pollInterval: 5s` purely so the settled cadence is observable
inside a lab run. Leave it at the 5m default in production and lower it only for a cluster you
are actively watching.

## The same cluster, spelled out

Every default above can be written explicitly. This is the contrast the lab provisions
alongside the minimal one:

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab02-analytics
  namespace: apps
spec:
  accountRef:
    name: ravendb-cloud

  displayName: lab02-analytics   # the name shown in the portal, and the identity
  tier: Production
  provider: AWS
  region: eu-west-1              # overrides the CloudAccount default
  instanceType: PB20             # omit and the operator picks the smallest for the tier
  releaseChannel: Stable62       # overrides what the cloud reports as the account default
  nodes: 3
  subdomain: lab02-stats         # at most 13 characters, and immutable

  storage:
    sizeGB: 100                  # must be on PB20's menu: 20, 40, 80, 100, 150
    type: SsdPremium
    iops: 3000
    throughputMBps: 125

  networking:
    allowedIPs:
      - 10.0.0.0/8
      - 203.0.113.7/32

  managementPolicy: Full
  deletionPolicy: Retain
  connectionSecret:
    name: lab02-analytics-connection
    includeCertificate: true
  pollInterval: 5m
```

```
==> The fully specified cluster resolved to exactly what it asked for
    PASS region = eu-west-1
    PASS instance type = PB20
    PASS tier = Production
    PASS release channel = Stable62
    PASS nodes = 3
    PASS vCPUs = 2
    PASS RAM in MB = 4096
    PASS disk = 100
    PASS disk class = SsdPremium
    PASS first allowed range = 10.0.0.0/8
    PASS CHANGES = Applied
```

Two things to notice. `sizeGB: 100` is not a free number: RavenDB Cloud sells whole-GB disks
from a fixed menu per instance type, and `PB20` offers 20, 40, 80, 100 and 150. Ask for 90 and
the operator refuses before anything is sent, with the menu attached, as `InvalidSpec`. It is
also why the field is a plain integer rather than a `resource.Quantity`, which would invite
`200Gi`. And `nodes: 3` is not what makes this a three-node cluster: three nodes come with every
Production shape offered. Setting `nodes` higher or lower is a day-2 instruction to add or remove
nodes one at a time, which is [lab 05](05-day-two-operations.md).

Only `instanceType`, `storage.sizeGB`, `storage.type` and `nodes` are day-2 changes the operator
acts on. `tier`, `subdomain` and `displayName` can never change, and `provider` and `region`
can never change away from a value they already had; CEL rules refuse the edit at admission with
messages that tell you to create a new `CloudCluster` instead. Those rules are covered by the CRD
tests (`make test-integration`).

A few fields sit between those categories. `releaseChannel`, `networking.allowedIPs` and the
dedicated `storage.iops` and `storage.throughputMBps` pass admission when you edit them and are
then never applied, because the cloud API takes them at creation only. You do get told: the
difference is reported as `CHANGES NotApplied`, and the message names the portal as the place to
fix it. `managementPolicy`, `deletionPolicy`, `connectionSecret` and `pollInterval` are
genuinely live and take effect on the next reconcile. `accountRef` is pinned by its own CEL rule,
so a cluster cannot be moved between accounts.

## Cleaning up

`deletionPolicy` defaults to `Retain`, so deleting these three objects leaves three clusters
running and billing:

```
==> Cleaning up
    PASS Retain left the cloud cluster running mentions '"id":"1-A"'
    PASS lab02 clusters left in the control plane = 0
    PASS lab02 CloudClusters left in apps = 0
```

That default is not timidity, it is the only safe answer for a resource whose accidental
deletion is unrecoverable. The lab therefore has to remove the clusters from the control plane
itself, through the `/_lab` surface, after Kubernetes has let go of them. In production the
equivalent is either terminating in the portal or setting `deletionPolicy: Terminate` on
objects you are certain about, which is [lab 06](06-delete-safely.md).

## Takeaway

A manifest states what you want to be true and nothing more. Everything else is resolved at
reconcile time against the live instance types and the account defaults, and lands in `status`,
where it is a reading of the cluster that exists rather than a record of a decision. Read
`STATUS` to know whether you can connect and `CHANGES` to know whether your manifest has been
carried through; both are condition reasons, so the table, the conditions and your alerts all
use the same words. And a crashed create can safely find its cluster again by name because of
one timestamp written before the call that needed it: without `status.provisionAttemptedAt`,
the operator could not tell its own cluster from a stranger's.

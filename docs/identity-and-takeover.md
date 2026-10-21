# Identity and take-over

How a CloudCluster finds the RavenDB Cloud cluster it manages, and when it may
create one. Read before changing `internal/controller/binding.go`, the
`creationMode` field, or anything that writes `status.productID`.

## The name is the identity

A CloudCluster names its cluster by `spec.displayName`, which defaults to
`metadata.name`. RavenDB Cloud keeps display names unique among the products not
hidden in the portal, terminated ones included, and compares them ignoring case,
so a name is enough to find a cluster again: after a restore, a second install,
or a manifest applied by hand.

The lookup (`FindClusterByName`) ignores case, skips Terminated clusters, and
returns `ErrAmbiguous` rather than a guess when two live clusters share a name.

## What a found cluster must match

A name can find a cluster nobody meant this object to own. Before binding,
`identityDifferences` compares what cannot change on a cluster: the tier always,
the provider and region when the manifest sets them. A mismatch means the name
belongs to some other cluster, and the object reports `NameTaken` and binds
nothing.

One cluster binds to one CloudCluster. `holderOf` looks the product id up
through a field index across every namespace the operator watches; a second
object finding a held cluster reports `NameTaken` naming the first. Two
Kubernetes clusters running this operator against one account cannot see each
other's objects. That risk is accepted; the README says why.

## creationMode

| Mode | Finds a cluster | Finds nothing |
|---|---|---|
| `TakeOverOrCreate` (default) | takes it over, with a Warning `Adopted` event saying how to confirm it | creates one |
| `TakeOverOnly` | takes it over, Normal `Adopted` | `ClusterNotFound`, creates nothing |
| `CreateOnly` | `NameTaken`, binds nothing | creates one |

CEL makes it immutable except `TakeOverOrCreate` to `TakeOverOnly`, which
confirms a take-over, and refuses `CreateOnly` with `managementPolicy: Observe`,
which could never do anything.

`managementPolicy` defaults per outcome: a cluster the object created runs under
`Full`, one it took over under `Observe` (reports differences, applies none)
until the manifest says `Full`. `status.adopted` records which happened, and it
decides deletion too: see [deletion.md](deletion.md).

## Creating without creating twice

RavenDB Cloud takes no idempotency key, so a create whose answer is lost may
have happened. Before sending one, the object stamps
`status.provisionAttemptedAt` and `status.provisionAttemptedName` in a status
write of its own (`writeStatusNow`), and the next pass looks the attempted name
up instead of creating again. A cluster found that way binds as created, not
adopted, after the same identity checks. A definite refusal clears the stamp in
the same write that reports it; only an unknown outcome keeps it.

## The export

`kubectl ravendb-cloud export` writes a manifest from a running cluster with
`creationMode: TakeOverOnly` and `managementPolicy: Full`, describing it as it
runs, so applying it changes nothing and a missing cluster is never replaced by
an empty one. It reads clusters through the same `cloudapi.Client` as the
operator, and the key from the CloudAccount's Secret.

## Don't

- Don't bind on a name match alone; identity checks and the holder check run on
  every path that binds, the interrupted-create path included.
- Don't make `TakeOverOrCreate` stop warning when it takes a cluster over: the
  Warning is how an accidental match gets noticed.

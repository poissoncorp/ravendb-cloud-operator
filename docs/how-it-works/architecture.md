# Architecture

The operator is three reconcilers around one client. Everything it knows about
RavenDB Cloud comes through `cloudapi.Client`; everything it promises a user is
in the three CRDs.

```
CloudAccount ──┐
               ├─> internal/cloudclients.Cache ──> cloudapi.Client (interface)
CloudCluster ──┘                                        │
                                                        └── rest.go  the one implementation
                                                                     │
                                                     ┌───────────────┴───────────────┐
                                            api.cloud.ravendb.net        internal/cloudsim
                                              (production)                (tests and labs)
```

`cloudapi.Client` is the only door to the control plane, and the controllers
depend on the interface rather than on the implementation. There is exactly one
implementation, on purpose. A second one would be a second reading of an API
that keeps changing, and the cheap one is always the one the tests use, so the
suite would slowly stop saying anything about the requests the operator really
sends. The export tool goes through the same door.

Instead the tests run the same client against `internal/cloudsim`, a simulator
of the RavenDB Cloud API, over real HTTP: the controller tests in process, the
labs as a pod. So they need no credentials and no cluster and still cover the
JSON on the wire.

The domain types are kept separate from the wire types (`internal/cloudapi/wire.go` owns the
translation), so a rename on the cloud side does not ripple into the CRDs. It
already earns its keep: the API spells providers `Aws`/`Gcp`, the CRD spells them
`AWS`/`GCP`.

```
api/v1alpha1/             CRD types, CEL rules, conditions
cmd/                      the manager
cmd/kubectl-ravendb_cloud/ the kubectl plugin, whose export writes manifests
internal/cloudapi/        the client interface, its one REST implementation, wire mapping
internal/cloudclients/    CloudAccount -> client, with caching
internal/controller/      the three reconcilers and the Secret builder
internal/export/          CloudCluster manifests from running clusters
internal/cloudsim/        a simulated RavenDB Cloud, used by the tests and the labs
config/                   CRDs, RBAC, manager deployment (kustomize)
charts/                   the Helm chart
examples/                 annotated manifests
labs/                     runnable, self-checking guides
labs/mockcloud/           serves internal/cloudsim over HTTP for the labs
.reference/               vendored copy of the RavenDB Cloud OpenAPI document
```

`internal/cloudsim` was written from the OpenAPI document rather than from the client, and it
keeps its own wire structs for that reason: if both sides shared one set, a shared misreading of
the spec would be invisible. `internal/cloudsim/contract_test.go` drives the real client against
it, so a disagreement between two independent readings of the spec shows up as a test failure
rather than as a surprise in production. Keeping the simulator honest as the API evolves is a
real cost, and the section below says what pays for it.

## Keeping up with an API that keeps moving

Nothing here makes the simulator automatically correct. What it does is make each way of being
wrong show up somewhere specific:

- **The client and the simulator disagree.** `internal/cloudsim/contract_test.go` fails. This is
  the common case and the cheap one.
- **Both agree, and both are wrong.** No test can catch this, because both sides were written
  by reading the same document. Only real traffic settles it, which is what `labs/e2e_smoke.sh`
  is for: a read-only pass against a real account, no writes, no cost.
- **The document itself moved.** `hack/cloud-drift` compares the published OpenAPI document
  with the vendored [`.reference/swagger.json`](../../.reference/swagger.json) every week, and every
  contract test checks the client's requests and the simulator's answers against that copy.
- **Behaviour the document never described.** The rules that are not in the spec at all, such as
  Free being portal-only or the subdomain length limit, were found by reading the RavenDB Cloud
  source. Each one is a comment in `internal/cloudsim` saying how it was verified, because a rule
  with no citation is a rule nobody can re-verify later.

## One pass over a CloudCluster

1. Add the finalizer, before anything else.
2. Read the CloudAccount and get its client ([accounts-and-credentials.md](accounts-and-credentials.md)).
3. If the object is being deleted, honour `deletionPolicy` and stop
   ([deletion.md](deletion.md)).
4. Bind: find the cluster by name, check it is the one the manifest describes,
   or create one ([identity-and-takeover.md](identity-and-takeover.md)).
5. Read the cluster and copy what it is into status.
6. Publish the connection Secret ([connection-secret.md](connection-secret.md)).
7. Compare the manifest with the cluster and apply at most one change
   ([changes-and-drift.md](changes-and-drift.md)).
8. Write status once, set the conditions, and requeue
   ([status-events-and-logs.md](status-events-and-logs.md)).

Every call in steps 2 to 7 goes through the client, paced and classified
([calling-ravendb-cloud.md](calling-ravendb-cloud.md)). A pass has a deadline;
a requeue on a timer waits behind changes people make.

## Design decisions

**The API key is the boundary, and it covers the whole account.**
A RavenDB Cloud API key reaches every cluster in its account; the API offers no
key limited to some of them. Whoever may create a CloudCluster in the namespace
of a CloudAccount can therefore name any cluster in that account, and the
operator takes it over and publishes its client certificate, which administers
every database on it, into that namespace. Grant that right only to people you
would give the key itself. Keys limited to chosen clusters would have to come
from RavenDB Cloud.

**Defaults are the safe answer, not the convenient one.**
`deletionPolicy` defaults to `Retain`, so `kubectl delete cloudcluster` drops the
Kubernetes object and leaves the database running. Destroying a paid production
cluster requires writing `deletionPolicy: Terminate` on purpose, and even then
it ends only a cluster the object created, or one it took over under
`creationMode: TakeOverOnly`, whatever `managementPolicy` says. A found cluster defaults to
`managementPolicy: Observe`, because finding a cluster you did not create should
not hand a controller permission to resize it on the next reconcile.

**The CRD refuses what the cloud cannot do.**
RavenDB Cloud can change a cluster's instance type, storage and node count. It
cannot move one between providers, regions or tiers, and it cannot rename one.
Those fields are immutable via CEL, so you get a rejection with a reason instead
of a manifest that differs forever. `subdomain` and `displayName` are pinned
harder still: they cannot be added after the fact either, because both feed the
create call and `displayName` is what the operator finds the cluster by.

```
The CloudCluster "orders-prod" is invalid: spec.region: Invalid value: "object":
region is immutable: create a new CloudCluster instead
```

Where a field cannot be edited, the operator's own messages say so too: the
advice is to delete the object and apply it again, which under the default
`Retain` touches nothing in RavenDB Cloud.

**Bad input fails before it costs anything.**
Region is checked against the account's cached region list, and the instance type
against the live list for that region, before anything is created. The error
tells you what you could have written:

```
region "moon-base-1" is not available for AWS;
available regions: eu-central-1, eu-west-1, us-east-1, ...
```

**Creating is crash-safe without being greedy.**
The operator stamps `status.provisionAttemptedAt` and `status.provisionAttemptedName`
*before* calling create. If it dies before recording the returned id, the next pass finds
the cluster by the name it recorded, never by whatever `spec` says at that moment, and the
marker tells it the cluster is its own, so it keeps `Full`. Without the marker a cluster the
name finds is somebody else's until proven otherwise, and it is observed, never reshaped.

**Two claims on one cluster are refused, not guessed between.**
A second CloudCluster whose name finds a cluster another one already manages gets
`NameTaken`, naming the first. Two live clusters sharing a name, which RavenDB Cloud's check on
every create leaves only to two creates racing, get `NameTaken` too, with the advice to terminate
or hide the one this object should not manage. What the operator cannot see is another Kubernetes cluster: two
installations pointed at one account can both bind the same cluster, and nothing in RavenDB
Cloud records an owner, so there is nowhere to keep a lease. That risk is accepted because the
defaults bound it: under `Observe`, the default for a found cluster, both sides only read; under
`Full` with the same manifest they agree, and with different ones each undoes the other's change
once per poll and says so. What stays dangerous is `Terminate` on a side allowed to use it, which
ends the cluster for both.

**The wire is not the CRD.**
Domain types are kept apart from wire types so a rename on the cloud side cannot reach the CRD, and
it already pays for itself twice. The API spells providers `Aws` and `Gcp` while users write `AWS`
and `GCP`. And product ids on the public API are the short form (`123-A`): the server strips the
`products/` prefix on the way out and re-adds it on the way in, which the OpenAPI document does not
say anywhere. Both of those live in `internal/cloudapi/wire.go` and nowhere else.

**The OpenAPI document is not the whole contract.**
Four rules matter enough to encode, and none of them are in the spec:

- `subdomainName` is marked nullable, but the server passes it to a domain-name validator that
  rejects an empty value, so it is required in practice. The operator derives it from the display
  name rather than letting a minimal manifest fail.
- That same validator caps it at **13 characters**, letters digits and dashes only, no dash at
  either end, and it must not already be taken by anyone. The CRD enforces the shape; a derived
  name that does not fit is refused with a message naming the field to set, rather than being
  silently truncated into a URL nobody chose.
- The `Free` tier **cannot be created through the API at all**. RavenDB Cloud rejects it on create,
  so a Free CloudCluster never sends a create, and the cheapest thing a real test can create is a
  Development-tier cluster.
- A display name stays taken after its cluster is terminated, until somebody hides that cluster in
  the portal (verified against RavenDB Cloud's source). The operator terminates without hiding, so
  the portal keeps the record, and a create refused on the name says why.

The lab control plane enforces all four server-side, so the labs fail the same way production would.

**Light on the API.**
The client paces its own calls, clients are cached per account so that pacing
survives across reconciles, status-only updates are filtered out of the watch
so the operator does not trigger itself, and the manager runs a single replica.

# Architecture

The operator is three reconcilers around one client. Everything it knows about
RavenDB Cloud comes through `cloudapi.Client`; everything it promises a user is
in the three CRDs.

```
CloudAccount ────────┐
CloudCluster ────────┼─> internal/cloudclients.Cache ──> cloudapi.Client (interface)
CloudClusterRestart ─┘                                        │
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
  with the vendored [`.reference/swagger.json`](../../.reference/swagger.json) every week and opens
  an issue when it moved, and every
  contract test checks the client's requests and the simulator's answers against that copy.
- **Behaviour the document never described.** The rules that are not in the spec at all, such as
  Free being portal-only or the subdomain length limit, were found by reading the RavenDB Cloud
  source. Each one is a comment in `internal/cloudsim` saying how it was verified, because a rule
  with no citation is a rule nobody can re-verify later.

## One pass over a CloudCluster

1. Add the finalizer, before anything else.
2. Read the CloudAccount and get its client ([accounts-and-credentials.md](accounts-and-credentials.md)).
   A deletion takes the client without waiting for the account to be Ready.
3. Stop if the account's key now answers for another account than the one the
   cluster was bound in (`status.accountDomain`).
4. If the object is being deleted, honour `deletionPolicy` and stop
   ([deletion.md](deletion.md)).
5. Bind: find the cluster by name, check it is the one the manifest describes,
   or create one ([identity-and-takeover.md](identity-and-takeover.md)). A
   create stamps status first, in a write of its own (`writeStatusNow`).
6. Read the cluster and copy what it is into status. A cluster that is not
   Active ends the pass here, with the reason on Ready.
7. Publish the connection Secret ([connection-secret.md](connection-secret.md)).
   If that fails, the pass ends here.
8. Compare the manifest with the cluster and apply at most one change
   ([changes-and-drift.md](changes-and-drift.md)).
9. Write status, if it changed, set the conditions, and requeue
   ([status-events-and-logs.md](status-events-and-logs.md)).

Every call in steps 2 to 8 goes through the client, paced and classified
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
cluster requires writing `deletionPolicy: Terminate` on purpose; then it ends
the cluster the object holds, created or taken over, whatever
`managementPolicy` says. It cannot depend on which of the two happened, because
only status records that and a restore loses status. A found cluster defaults to
`managementPolicy: Observe`, because finding a cluster you did not create should
not hand a controller permission to resize it on the next reconcile, and a
take-over the manifest did not ask for records a Warning saying how to let go of
the cluster without ending it.

**The CRD refuses what the cloud cannot do.**
RavenDB Cloud can change a cluster's instance type, storage and node count. It
cannot move one between accounts, providers, regions or tiers, and it cannot
rename one. Once the object has found or created its cluster, CEL rules on the
object fix those fields, so you get a rejection with a reason instead of a
manifest that differs forever. `subdomain` and `displayName` are pinned harder
still: after that `subdomain` cannot be added either, and `displayName` only as
the object name it already defaulted to, because both feed the create call and
`displayName` is what the operator finds the cluster by.

```
spec.region: Invalid value: "object": region cannot change or be removed once
this object has found or created its cluster: RavenDB Cloud cannot move a
cluster between regions. ...
```

Until then nothing exists in RavenDB Cloud, so all of them can be edited, and
the operator's messages about a field that is wrong at that stage say to set
it. After that, the advice is to delete the object and apply it again, which
under the default `Retain` touches nothing in RavenDB Cloud.

**Bad input fails before it costs anything.**
Region is checked against the account's cached region list, and the instance type
against the live list for that region, before anything is created. The error
tells you what you could have written:

```
spec.region is "moon-base-1", which is not available for AWS; available regions:
eu-central-1, eu-west-1, us-east-1. Set spec.region to one of those; nothing
was created.
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
once per poll and says so. What stays dangerous is `Terminate` on either side, which ends the
cluster for both.

**The wire is not the CRD.**
Domain types are kept apart from wire types so a rename on the cloud side cannot reach the CRD, and
it already pays for itself: the API spells providers `Aws` and `Gcp` while users write `AWS` and
`GCP`, and that lives in `internal/cloudapi/wire.go` and nowhere else. Product ids on the public
API are the short form (`123-A`), and the operator passes them through unchanged.

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
- A display name stays taken after its cluster is terminated, until that cluster is hidden in the
  portal (verified against RavenDB Cloud's source). The operator's terminate asks RavenDB Cloud to
  hide the cluster, which it does once termination completes, so the name comes free. One
  terminated in the portal keeps it, and a create refused on the name says why.

The lab control plane enforces all four server-side, so the labs fail the same way production would.

**Light on the API.**
The operator paces its calls: every client shares the one pace
`cloudclients.Cache` holds, and clients are cached per account. Status-only
updates are filtered out of the watch so the operator does not trigger itself,
and the manager runs a single replica.

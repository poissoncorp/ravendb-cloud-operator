# RavenDB Cloud Operator (POC)

A standalone Kubernetes operator for **RavenDB Cloud**, separate from the
self-hosted [ravendb-operator](https://github.com/ravendb/ravendb-operator).
It lets you manage RavenDB Cloud clusters as Kubernetes objects, and it hands
your applications the endpoints and client certificate they need without anyone
opening the portal.

Built with Kubebuilder / Operator SDK layout, controller-runtime, Go.

> Status: proof of concept. The API group is `v1alpha1` and nothing here is
> stable yet. See [What is deliberately missing](#what-is-deliberately-missing).

---

## One model: the name finds the cluster

A CloudCluster names its cluster by display name, which defaults to the object
name. On the first pass the operator looks that name up in the account, and
what it finds decides what happens:

| The account has | The operator | `managementPolicy` when unset | `status.adopted` |
|---|---|---|---|
| no cluster by that name | creates one | `Full` | `false` |
| one, of the manifest's `tier` (and `provider` and `region`, where set) | takes it over | `Observe` | `true` |
| one of another tier, provider or region | binds nothing: `NameTaken` | | |
| one another CloudCluster already manages | binds nothing: `NameTaken` | | |

The name is a safe identity because RavenDB Cloud makes it one. Display names
are unique per account among clusters not hidden in the portal, terminated ones
included (RavenDB Cloud checks this on every create, verified against its
source), and nothing renames a cluster after it is created. So a name
picks out one cluster for as long as that cluster exists, and `displayName` is
immutable here for the same reason.

Five rules follow, and they are the whole of it:

- **Only what the manifest says is managed.** A field left out is left as the
  cluster has it. On a cluster the operator creates it gets a default, which
  status reports.
- **A found cluster is observed until you say otherwise.** `Observe` changes
  nothing in RavenDB Cloud, creating included. Anything the manifest sets that
  differs is reported, not applied, until `spec.managementPolicy` is `Full`.
- **You choose what a name may do.** `creationMode` is `TakeOverOrCreate` by
  default. `TakeOverOnly` never creates, and `CreateOnly` never takes over.
  `deletionPolicy: Terminate` ends a cluster the object created, or one it took
  over under `TakeOverOnly`; a cluster found under the default is left running.
  A takeover under the default raises a Warning, and changing the mode to
  `TakeOverOnly` afterwards confirms it was meant.
- **Free can only be found.** The API refuses to create a Free cluster, so a
  `tier: Free` CloudCluster reports `ClusterNotFound` until one by its name
  exists in the portal, and then binds to it.
- **Git is enough to rebuild.** Lose the Kubernetes cluster, apply the same
  manifests to a new one, and every object finds its cluster again. Write
  `managementPolicy: Full` and `creationMode: TakeOverOnly` in manifests you
  keep in git, as the export tool does: a restored object cannot know it
  created its cluster, so without the first it would resume under `Observe`,
  and without the second a name that finds nothing would create an empty
  cluster in its place.

### How other operators do it

Every operator for a cloud service answers the same questions: what identifies
a resource, what happens when one already exists, and what deleting the
Kubernetes object does.

| Operator | Identity | A resource that already exists | Change nothing | On delete, by default | Manifests from what exists |
|---|---|---|---|---|---|
| MongoDB Atlas | deployment name in a project | taken over and made to match the spec | `mongodb.com/atlas-reconciliation-policy: skip` | kept, since 2.0 | `atlas kubernetes config generate` |
| Crossplane | `crossplane.io/external-name` | imported | `managementPolicies: ["Observe"]` | deleted, unless `deletionPolicy: Orphan` | copy `status.atProvider` by hand |
| AWS (ACK) | per resource | only with an adoption policy of `adopt` or `adopt-or-create` | `services.k8s.aws/read-only` | deleted, unless `deletion-policy: retain` | `adopt` fills the spec from AWS |
| Azure Service Operator | `azureName`, default `metadata.name` | adopted and made to match the spec | `reconcile-policy: skip` | deleted, unless `detach-on-delete` | `asoctl import azure-resource` |
| Google Config Connector | `resourceID`, default `metadata.name` | acquired and made to match the spec | none | deleted, unless `deletion-policy: abandon` | `config-connector bulk-export` |
| This operator | `displayName`, default `metadata.name` | taken over, under `Observe` unless the manifest says `Full`; `creationMode` can allow only one of taking over and creating | `managementPolicy: Observe` | kept (`deletionPolicy: Retain`) | `kubectl ravendb-cloud export` |

Name as identity with take-over is the common model; ACK alone makes taking
over opt-in, which `creationMode` offers here too. Taking over usually means
reshaping at once; here a found cluster is only observed until the manifest
says `Full`, which is Crossplane's import sequence applied by default. And
`Retain` was the default from the start, for the reason Atlas changed to it: a
stray `kubectl delete` must not destroy a database.

## Quick start

The fastest way to see all of it working is the [labs](labs/), which run on a throwaway kind
cluster against a stand-in control plane, so they need no RavenDB Cloud account and cost nothing:

```bash
make lab-setup && make lab-run
```

Against a real account, install a released operator with its Helm chart, which
pins the image of that release:

```bash
helm install ravendb-cloud-operator oci://ghcr.io/ravendb/charts/ravendb-cloud-operator \
  --version 0.1.0 --namespace ravendb-cloud-operator-system --create-namespace
kubectl create namespace apps
kubectl apply -f examples/00-account.yaml    # edit the API key first
kubectl apply -f examples/01-provision-minimal.yaml
```

No release has been published yet, so until one is, build the image and install the chart from
a checkout. `make docker-build` tags it `ravendb-cloud-operator:dev`; on kind, load it with
`kind load docker-image ravendb-cloud-operator:dev` first.

```bash
make docker-build
helm install ravendb-cloud-operator charts/ravendb-cloud-operator \
  --namespace ravendb-cloud-operator-system --create-namespace \
  --set image.repository=ravendb-cloud-operator --set image.tag=dev
```

`make deploy IMG=<image>` is the kustomize path, for development. `make install` installs
only the CRDs, so objects applied after it would be accepted and then sit there with nothing
reconciling them.

```
$ kubectl -n apps get cloudcluster
NAME         STATUS    CHANGES   TIER          REGION         NODES   AGE
orders-dev   Running   Applied   Development   eu-central-1   1       6m
```

[`examples/`](examples/) has the minimal and the full create, taking over an
existing cluster, observing one without managing it, an application consuming
the Secret, and a node restart.

## Taking over clusters that already exist

Because a CloudCluster takes over the cluster its name finds, bringing an
existing estate under the operator is one manifest per cluster. Write those
from the clusters rather than by hand:

```bash
kubectl ravendb-cloud export --account ravendb-cloud -n apps --all > clusters.yaml
kubectl apply -f clusters.yaml
```

The plugin reads the API key from the CloudAccount's own Secret, with your
permissions (`get` on CloudAccounts and Secrets in that namespace), so the key
never has to be on your machine. To export before the account is applied, set
`RAVENDB_CLOUD_API_KEY` instead.

Each manifest describes its cluster exactly as it runs, with
`managementPolicy: Full` and `creationMode: TakeOverOnly`, so applying it
changes nothing, never creates a cluster in place of one it cannot find, and
every object settles on `Running` and `Applied`. From then on the manifest is how the cluster
changes. `--all` skips terminated clusters; name clusters instead to export only
those. Object names are the display names, lower-cased and made DNS-safe, and
`spec.displayName` keeps the original.

A hand-written manifest works too, and is safer than it looks: with
`managementPolicy` unset a found cluster runs under `Observe`, so a wrong field
shows up as `NotApplied` rather than reshaping anything. Writing `Full` is the
step that hands over control, and the export tool is how to take it without a
surprise. Set `creationMode: TakeOverOnly` so a mistyped name reports
`ClusterNotFound` instead of creating a cluster, and `CreateOnly` on a manifest
meant to create, so it never picks up a cluster somebody else named the same.

The plugin ships as binaries on each GitHub release, for Linux, macOS and
Windows: put the one for your system on the PATH as `kubectl-ravendb_cloud`
(`.exe` on Windows) and kubectl finds it. The operator image carries it too, as
`/kubectl-ravendb_cloud`, for running inside the cluster next to the key.

## Using a cluster managed somewhere else

A cluster can stay in the portal or in Terraform and still be used from
Kubernetes. `managementPolicy: Observe` makes the operator read it and never
change it, creating included, and that still earns three things:

- **A connection Secret nobody copies by hand.** Node URLs and the client
  certificate land in a Secret pods mount, and stay current: a new node adds
  its URL, and RavenDB Cloud's regenerated certificate replaces the old one
  three weeks before it expires.
- **A look before handing over control.** The CHANGES column reports how a
  manifest differs from the running cluster without applying anything. When
  the difference is the one you mean, `managementPolicy: Full` applies it.
- **Status where the rest of the platform looks.** `kubectl get`, events and
  alerts on `Ready`, including `DeletedInCloud` when somebody removes the
  cluster in the portal.

[`examples/04-observe-only.yaml`](examples/04-observe-only.yaml) is that
manifest. It is not how an application team should reach a cluster another
team manages with this operator: one cluster binds to one CloudCluster, and the
object would need the account's API key, which reaches every cluster. Copy that
team the connection Secret instead.

## Standing the operator down

To have the operator leave one cluster alone for a while without deleting the
object, set `managementPolicy: Observe`: it keeps reading the cluster and
publishing its Secret, and changes nothing until the policy is `Full` again.
Its `deletionPolicy` still applies if the object is deleted meanwhile.

## Moving to a new API version

`v1alpha1` can still change in ways old manifests do not survive, and when it
does there will be no conversion webhook. None is needed: a CloudCluster finds
its cluster by name, so moving to a new version is deleting the old objects and
applying new ones, and nothing in RavenDB Cloud changes.

1. Set `deletionPolicy: Retain` on every object that says `Terminate`.
   Skipping this terminates those clusters in step 3.
2. Write the new manifests. `kubectl ravendb-cloud export` reads the running clusters
   and writes each with `creationMode: TakeOverOnly`.
3. Delete the old objects. The clusters keep running. Their connection Secrets
   go with them: pods already running keep what they read, and a pod starting
   before step 5 waits for its Secret.
4. Install the new CRDs and operator.
5. Apply the new manifests. Each takes its cluster over by name and publishes
   its Secret again, and under `TakeOverOnly` a `Terminate` written there means
   what it says.

## The API

Three kinds, in group `cloud.ravendb.net`. Two describe state; the third
records an action.

### CloudAccount

The credential, plus the defaults its clusters inherit when the operator
creates them. It also mirrors the region and release channel lists into status,
so `kubectl describe cloudaccount` answers "what can I actually ask for here"
without a trip to the portal.

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata: {name: ravendb-cloud, namespace: apps}
spec:
  apiKeySecretRef: {name: ravendb-cloud-api-key, key: apiKey}
  defaults:
    provider: AWS
    region: eu-central-1
    allowedIPs: ["0.0.0.0/0"]
```

Rotating the key takes effect immediately: the controller watches Secrets
through a field index and re-verifies the accounts that reference the changed
one.

### CloudCluster

One RavenDB Cloud cluster. In RavenDB Cloud's own vocabulary this is a
"product"; it is called a cluster here because that is what it is, and because
that is the word the people writing the manifest already have in their heads.

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata: {name: orders-prod, namespace: apps}
spec:
  accountRef: {name: ravendb-cloud}
  tier: Production
  instanceType: PB40        # omit and the operator picks the smallest for the tier
  nodes: 3
  storage: {sizeGB: 200, type: SsdPremium}
  networking:
    allowedIPs: ["10.0.0.0/8"]
  deletionPolicy: Retain
```

`accountRef`, `tier` and `displayName` never change, `provider` and `region`
never change once set, and `subdomain` is sent only when the operator creates
the cluster. `instanceType`, `storage` and `nodes` are applied to a running
cluster under `Full`. `releaseChannel` and `networking.allowedIPs` are taken at
creation only, so a later edit is reported rather than applied.

### CloudClusterRestart

One node, restarted once. It is a separate kind rather than a field on
CloudCluster because a CRD spec describes desired state, and "restart node A"
is not a state: nothing about the cluster afterwards is different, so no field
value could mean "restarted", and a field would re-fire on every resync.

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudClusterRestart
metadata: {name: orders-a-after-oom, namespace: apps}
spec:
  clusterRef: {name: orders-prod}
  nodeTag: A
```

```
$ kubectl -n apps get cloudclusterrestart
NAME                 CLUSTER       NODE   STATUS      AGE
orders-a-after-oom   orders-prod   A      Succeeded   4m
```

The spec is immutable, so a request cannot be re-aimed after it has gone out;
to restart again, apply another object, which is also how you get a history of
what was done to the cluster. It obeys `managementPolicy` on the cluster:
while that holds it back, or the cluster is not Running yet, it waits and says
why. The operator makes each record a child of the
CloudCluster, so they are collected with it.

When RavenDB Cloud does not answer the restart request, the operator does not
send it again, since that could restart the node twice. It watches the cluster
instead, and the message says the answer was lost. If the cluster stays Active
throughout, the restart ends `Succeeded` although it may never have reached
the node.

Create restarts with `kubectl create` and keep them out of the repository a
GitOps tool syncs. Like a Job, a restart remembers that it ran only in its
status, so one recreated without it, after a restore or when its CloudCluster
is deleted and applied again, restarts the node again.

## Reading status

A CloudCluster answers two questions, one column each. **STATUS** is the reason
on the `Ready` condition: can I use it? **CHANGES** is the reason on `Synced`:
did my manifest reach RavenDB Cloud? Both lists are closed, and every message
says what happened and what to do next, naming the spec field to change.

```
$ kubectl -n apps get cloudcluster
NAME          STATUS    CHANGES      TIER          REGION         NODES   AGE
orders-dev    Running   Applied      Development   eu-central-1   1       6m
legacy-prod   Running   Applied      Production    eu-central-1   3       5m
legacy-dr     Running   NotApplied   Production    eu-west-1      3       1m
```

`-o wide` adds POLICY, INSTANCE, CLUSTER-ID, URL and the Ready message.

| STATUS | Ready | Means |
|---|---|---|
| `Running` | True | Active, and the connection Secret is published (or publishing is off) |
| `Creating`, `Updating` | False | RavenDB Cloud is creating the cluster, or applying a change |
| `WaitingForCertificate` | False | Active; RavenDB Cloud has not issued the client certificate yet |
| `WaitingForAccount` | False | the CloudAccount is not usable; the message says why |
| `InvalidSpec` | False | the operator will not send this; the message names the field |
| `Refused` | False | RavenDB Cloud refused to create the cluster, in its own words |
| `QuotaReached` | False | the account is at its product quota; retried every ten minutes |
| `NameTaken` | False | the name belongs to another cluster or another CloudCluster, or the Secret name is taken |
| `ClusterNotFound` | False | no cluster by that name, and this object may not create one (`Observe`, or Free) |
| `DeletedInCloud` | False | the cluster was deleted outside the operator |
| `AwaitingPayment` | False | RavenDB Cloud waits for payment; resolve it in the portal |
| `CreationFailed` | False | RavenDB Cloud could not create it and opened a support request |
| `SecretNotWritten` | False | Active, but the connection Secret could not be written |
| `Deleting` | False | this object is being deleted; the message says what it waits for |
| `CloudUnreachable`, `KeyRejected`, `UnexpectedResponse` | Unknown | the operator could not look; the cluster is probably still serving |

| CHANGES | Synced | Means |
|---|---|---|
| `Applied` | True | everything the manifest sets matches RavenDB Cloud |
| `Applying` | False | a change was sent |
| `Waiting` | False | RavenDB Cloud takes it later: a storage change window, or the cluster busy with another change or a restart; asked again each poll |
| `Refused` | False | RavenDB Cloud refused it; not asked again until the spec changes |
| `NotApplied` | False | a difference the operator may not apply (`Observe`) or cannot (a field the API only takes at creation) |

A CloudAccount's STATUS is `Verified`, `KeyMissing`, `KeyRejected`,
`CloudUnreachable`, `UnexpectedResponse`, or `InUse` while its deletion waits
for the clusters still naming it. A CloudClusterRestart's is `Waiting`,
`Restarting`, `Succeeded` or `Failed`. As on a Job, `Complete` is True only once
the node came back, and a failed restart sets `Failed` instead, so
`kubectl wait --for=condition=Complete` never reports a failure as done.

Every kind also carries the two conditions [kstatus](https://github.com/kubernetes-sigs/cli-utils/tree/master/pkg/kstatus)
reads, and with it Flux and `kubectl wait`: `Reconciling` while the operator is
still working towards the spec, and `Stalled` while it is stuck until a person
changes something, such as a refused create or a rejected key. Each is present
only while True and repeats the reason and message of the condition behind it.
A Flux Kustomization with `wait: true` therefore fails on a `Stalled` object
instead of waiting for its timeout.

Argo CD needs a health check for a kind it does not know. This one reads the
same conditions:

```yaml
# argocd-cm
data:
  resource.customizations.health.cloud.ravendb.net_CloudCluster: |
    hs = { status = "Progressing", message = "" }
    for _, c in ipairs((obj.status or {}).conditions or {}) do
      if c.type == "Stalled" and c.status == "True" then
        return { status = "Degraded", message = c.message }
      end
      if c.type == "Ready" and c.status == "True" then
        hs = { status = "Healthy", message = c.message }
      end
    end
    return hs
```

The same check, keyed `cloud.ravendb.net_CloudAccount`, works for accounts.

`status.cloudStatus` carries RavenDB Cloud's own status string unchanged, for
anyone who needs more than the operator's vocabulary.

Events are for `kubectl describe`: Normal ones for what the operator did
(`Created`, `Adopted`, `Applying`, `SecretPublished`, `Retained`, `Terminating`,
`Restarting`, `Restarted`), and a Warning when a problem starts, once rather
than on every poll, so an alert on Warning events pages for problems and not
for work somebody asked for. A wait that usually ends by itself, an account or
a key Secret applied a moment later, warns only once it has lasted a minute.

The log has one line per condition change, `condition changed` with the
condition and its old and new reason as `condition`, `from` and `to`. The
per-request trail to RavenDB Cloud is at debug level (`logging.level: debug`
in the chart).

Calls to RavenDB Cloud have their own metrics next to controller-runtime's:
`ravendb_cloud_requests_total` and `ravendb_cloud_request_duration_seconds` by
method and status code (`none` when no answer came),
`ravendb_cloud_retries_total`, and `ravendb_cloud_request_queued_seconds`, how
long a request waited for its turn. When polls slow down, these say whether
RavenDB Cloud is slow, refusing, or not answering.

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

### The connection Secret

The point of the whole thing. RavenDB Cloud serves the client certificate as a
ZIP holding a PFX pair, PEM material and a password file. The operator unpacks
it into conventional Kubernetes keys, so a pod just mounts it:

| key | contents |
|---|---|
| `url`, `urls` | first / all node endpoints |
| `nodeTags` | RavenDB node tags, aligned with `urls` |
| `studioURL` | RavenDB Studio address |
| `productID` | RavenDB Cloud cluster id |
| `tls.crt`, `tls.key` | client certificate and key, PEM |
| `client.pem` | both, in one file |
| `client.pfx`, `client.with-password.pfx` | PKCS#12 forms |
| `certificate.password` | password for the protected PKCS#12 |

The Secret is owned by the CloudCluster, so it is garbage collected with it. The
certificate is downloaded once, not on every poll, and again three weeks before
it expires, by when RavenDB Cloud has issued its replacement.

An application usually reads the certificate once, when it opens its document
store, so it needs a restart to use the new one. A tool that rolls workloads
when a Secret changes, such as Reloader, does that; `status.certificateExpiresAt`
says when the current one runs out.
[`examples/05-consume-from-app.yaml`](examples/05-consume-from-app.yaml) shows a
Deployment consuming it.

## Architecture

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

### Keeping up with an API that keeps moving

Nothing here makes the simulator automatically correct. What it does is make each way of being
wrong show up somewhere specific:

- **The client and the simulator disagree.** `internal/cloudsim/contract_test.go` fails. This is
  the common case and the cheap one.
- **Both agree, and both are wrong.** No test can catch this, because both sides were written
  by reading the same document. Only real traffic settles it, which is what `labs/e2e_smoke.sh`
  is for: a read-only pass against a real account, no writes, no cost.
- **The document itself moved.** `hack/cloud-drift` compares the published OpenAPI document
  with the vendored [`.reference/swagger.json`](.reference/swagger.json) every week, and every
  contract test checks the client's requests and the simulator's answers against that copy.
- **Behaviour the document never described.** The rules that are not in the spec at all, such as
  Free being portal-only or the subdomain length limit, were found by reading the RavenDB Cloud
  source. Each one is a comment in `internal/cloudsim` saying how it was verified, because a rule
  with no citation is a rule nobody can re-verify later.

## Development

Docker is the only prerequisite; the Go toolchain runs in a container via
`hack/go.sh`. Set `GO_LOCAL=1` to use a host Go instead.
[CONTRIBUTING.md](CONTRIBUTING.md) says how to add a spec field, an endpoint or
a condition reason, and [`docs/`](docs/) explains each mechanism of the
operator, one file per mechanism.

Tests are split by what each tier needs to run, because that is what decides
where it can run and what it can cost:

```bash
make test              # unit + integration: no cluster and no account needed
make test-unit         # pure Go, seconds
make test-integration  # CEL rules against a real kube-apiserver (envtest)
make test-e2e          # the operator as a container, in a kind cluster
make test-chart-install # helm install into that cluster, scoped to one namespace
make test-e2e-cloud    # a real RavenDB Cloud account. Needs a key, costs money
```

`make test-integration` is worth calling out: it starts a real
`kube-apiserver`, applies the generated CRDs and checks that the validation
rules actually fire, because reading the generated YAML is not evidence that
they compile. Note that `api/v1alpha1` reports `ok` with every one of those
tests skipped when the envtest binaries are absent, which is why `make
test-unit` says so explicitly.

```bash
make hooks         # install the pre-commit checks
make build         # the manager and the kubectl plugin, into bin/
make docker-build
make deploy        # CRDs, RBAC and the manager, via kustomize
make chart-lint    # the Helm chart matches config/, lints and renders
```

## What is deliberately missing

The RavenDB Cloud public API is cluster-scoped. It exposes account info,
metadata, and product create / list / details / terminate / instance-type /
storage / add-node / remove-node / restart-node / certificate download. Every
one of those reaches a manifest. `restart-node` is the odd one and gets a kind
of its own, `CloudClusterRestart`, because a restart is an event rather than a
state, as the CloudClusterRestart section explains. So this operator does not
model:

- **Databases.** There is no database endpoint in the cloud API. A `CloudDatabase`
  CRD would have to talk to each cluster's own RavenDB API with the client
  certificate, which is a separate design decision, not an oversight.
- **Backups.** Same reason.
- **Scoped client certificates.** The API downloads *the* cluster certificate.
  It cannot mint one with specific database permissions.
- **Changing `allowedIPs` after creation.** The API accepts the list at creation
  only. The operator reports the difference on CHANGES as `NotApplied` and tells
  you to fix it in the portal, rather than pretending to reconcile it.
- **Renaming a cluster.** RavenDB Cloud has no way to, so `displayName` cannot
  change either.

Adding CRDs for things the API cannot do would have made the demo look richer
and the operator lie. Everything modelled here maps to a real endpoint.

### What has and has not run against a real environment

Everything is verified against the stand-in control plane, and every lab also ran against a
real RavenDB Cloud environment, one small Development cluster at a time, each terminated
afterwards, most recently on 21 Oct 2026. Those runs found bugs the whole simulated suite had
agreed were working; what each lab skips there is in [labs/README.md](labs/README.md).

On 21 Oct 2026 one Production cluster also ran there, created from the smallest manifest and
terminated afterwards. It showed the default Production shape, a change of shape on a running
cluster, scaling from three nodes to six and back one node at a time with the connection
Secret following each step, the product quota refusing a create and the slow retry creating it
once a slot freed, and display names held by terminated clusters and by names differing only
in case. [labs/README.md](labs/README.md) has the numbers.

### Known gaps in the POC itself

- No validating webhook. Everything possible is expressed as CEL on the CRD,
  which needs no certificates and no webhook deployment, but cross-object checks
  (does this region exist for *this* account) happen in the controller and
  surface as a condition rather than an admission error.
- Scaling down removes the additional node with the highest tag, the newest.
  RavenDB Cloud never removes A, B or C and asks which additional node to
  take; there is no field to choose another.

## Why an operator, and not a Crossplane provider

There is no Terraform provider for RavenDB Cloud to generate one from: the
`ravendb/ravendb` provider installs self-hosted nodes over SSH. A native
Crossplane provider would ask every user to run Crossplane for three kinds of
object. So this is a plain operator that borrows Crossplane's vocabulary where
it fits: `managementPolicy` Observe or Full, `deletionPolicy` Retain or
Terminate, and a name that finds an existing resource.

## Reference

- RavenDB Cloud API docs: <https://docs.ravendb.net/cloud/cloud-api>
- OpenAPI document: <https://api.cloud.ravendb.net/api/v1/swagger.json>
  (vendored at [`.reference/swagger.json`](.reference/swagger.json))
- Related issue: RavenDB-25166, Kubernetes Operator: Support for RavenDB Cloud

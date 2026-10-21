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
picks out one cluster for as long as that cluster exists, and `displayName`
cannot change here once the object has its cluster, for the same reason.

Six rules follow, and they are the whole of it:

- **Only what the manifest says is managed.** A field left out is left as the
  cluster has it. On a cluster the operator creates it gets a default, which
  status reports.
- **A found cluster is observed until you say otherwise.** `Observe` changes
  nothing in RavenDB Cloud, creating included. Anything the manifest sets that
  differs is reported, not applied, until `spec.managementPolicy` is `Full`.
- **You choose what a name may do.** `creationMode` is `TakeOverOrCreate` by
  default. `TakeOverOnly` never creates, and `CreateOnly` never takes over. A
  takeover under the default raises a Warning, and changing the mode to
  `TakeOverOnly` afterwards confirms it was meant.
- **Deleting keeps the cluster unless you say otherwise.** `deletionPolicy` is
  `Retain` by default. `Terminate` ends whatever cluster the object holds,
  created or taken over, and RavenDB Cloud then hides it, which frees its name
  for a new one.
- **Free can only be found.** The API refuses to create a Free cluster, so a
  `tier: Free` CloudCluster reports `ClusterNotFound` until one by its name
  exists in the portal, and then takes it over.
- **Git is enough to rebuild.** Lose the Kubernetes cluster, apply the same
  manifests to a new one, and every object finds its cluster again. Write
  `managementPolicy: Full` in manifests you keep in git, as the export tool
  does: a restored object cannot know it created its cluster, so without it the
  cluster comes back under `Observe`. `creationMode: TakeOverOnly`, which the
  export also writes, makes a name that finds nothing report it instead of
  creating an empty cluster in its place.

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
helm repo add ravendb-cloud-operator https://ravendb.github.io/ravendb-cloud-operator/helm
helm install ravendb-cloud-operator ravendb-cloud-operator/ravendb-cloud-operator \
  --version <version> --namespace ravendb-cloud-operator-system --create-namespace
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

## Documentation

[`docs/`](docs/) has two halves. **How to use it**: installing, accounts,
clusters, taking over, connecting an application, restarting a node, deleting,
status and monitoring, and moving to a new API version. **How it works**: the
architecture, then one file per mechanism. [docs/README.md](docs/README.md) is
the index.

[CONTRIBUTING.md](CONTRIBUTING.md) is how to work on the code, and
[SECURITY.md](SECURITY.md) the security model and how to report a problem.

### With an AI assistant

[`skills/ravendb-cloud-operator`](skills/ravendb-cloud-operator/SKILL.md) is an
[Agent Skill](https://agentskills.io) for writing and troubleshooting these
manifests: it gives the assistant the model above and points it at the guides
in `docs/how-to-use/`. In Claude Code:

```
/plugin marketplace add ravendb/ravendb-cloud-operator
/plugin install ravendb-cloud-operator@ravendb-cloud-operator
```

Other assistants that read Agent Skills can take the same folder.

## What is deliberately missing

The RavenDB Cloud public API is cluster-scoped. It exposes account info,
metadata, and product create / list / details / terminate / instance-type /
storage / add-node / remove-node / restart-node / certificate download. Every
one of those reaches a manifest. `restart-node` is the odd one and gets a kind
of its own, `CloudClusterRestart`, because a restart is an event rather than a
state, as [restarting a node](docs/how-to-use/restarting-a-node.md) explains. So this operator does not
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
  change either once the cluster exists.

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

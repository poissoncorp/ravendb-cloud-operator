# Documentation

Three kinds, in group `cloud.ravendb.net`: `CloudAccount` holds the API key, `CloudCluster`
names a cluster, and `CloudClusterRestart` records one action on it.

## How to use it

Each guide has a [lab](../labs/README.md) that does the same on a throwaway kind cluster,
against a stand-in for RavenDB Cloud or a real account.

| | | Lab |
|---|---|---|
| [Installing](how-to-use/install.md) | Helm, a checkout, the values worth knowing, upgrading and removing the operator | [00](../labs/00_setup.sh) |
| [Accounts](how-to-use/accounts.md) | the API key, account defaults, namespaces, who an account lets in | [01](../labs/01-verify-credential.md), [06](../labs/06-delete-safely.md#deleting-an-account) |
| [Clusters](how-to-use/clusters.md) | creating, changing, instance types, manifests kept in git | [02](../labs/02-provision-a-cluster.md), [05](../labs/05-day-two-operations.md) |
| [Taking over](how-to-use/taking-over.md) | clusters that already exist, the export, observing without managing | [04](../labs/04-take-over-an-existing-cluster.md) |
| [Connecting an application](how-to-use/connecting-applications.md) | the connection Secret, a document store, the first database | [03](../labs/03-connect-an-application.md) |
| [Restarting a node](how-to-use/restarting-a-node.md) | CloudClusterRestart | [05](../labs/05-day-two-operations.md) |
| [Deleting](how-to-use/deleting.md) | what a delete does to the cluster, retiring one | [06](../labs/06-delete-safely.md) |
| [Status and monitoring](how-to-use/status-and-monitoring.md) | STATUS and CHANGES, kstatus, Argo CD, events, logs, metrics | [02](../labs/02-provision-a-cluster.md), [05](../labs/05-day-two-operations.md) |
| [Moving to a new API version](how-to-use/upgrading.md) | delete and apply again, safely | [04](../labs/04-take-over-an-existing-cluster.md) |

### Words used in the guides

| Word | Means |
|---|---|
| take over | an object starts managing a cluster that already exists, found by its display name. The event is called `Adopted`, and `status.adopted` is then true |
| `Full`, `Observe` | the two `managementPolicy` values: the operator may change the cluster, or only read it |
| tier | `Free`, `Development` (one node) or `Production` (three to six nodes); RavenDB Cloud cannot move a cluster between them |
| release channel | the RavenDB version stream a cluster follows, such as `Stable72` |
| product quota | how many clusters the RavenDB Cloud account may have; RavenDB Cloud calls a cluster a product |
| hidden in the portal | RavenDB Cloud keeps a terminated cluster's record, and its name stays taken until the record is hidden |
| finalizer | a marker that makes Kubernetes keep an object until the operator has finished with it, such as terminating its cluster |
| STATUS, CHANGES | the two columns of `kubectl get cloudcluster`: can I use it, and did my manifest reach RavenDB Cloud ([status-and-monitoring.md](how-to-use/status-and-monitoring.md)) |

## How it works

Start with [the architecture](how-it-works/architecture.md); each of the others
explains one mechanism, why it is built that way, and what not to break.

| | |
|---|---|
| [Architecture](how-it-works/architecture.md) | the pieces, one pass, the design decisions, how other operators compare |
| [Identity and take-over](how-it-works/identity-and-takeover.md) | finding, binding and creating |
| [Deletion](how-it-works/deletion.md) | the finalizer, Retain and Terminate |
| [Changes and drift](how-it-works/changes-and-drift.md) | what is compared, applied, reported |
| [Status, events and logs](how-it-works/status-events-and-logs.md) | the reason tables, kstatus, events |
| [Calling RavenDB Cloud](how-it-works/calling-ravendb-cloud.md) | the client, deadlines, what is retried |
| [The connection Secret](how-it-works/connection-secret.md) | keys, field managers, the certificate |
| [Accounts and credentials](how-it-works/accounts-and-credentials.md) | verifying keys, the client cache |
| [Restarts](how-it-works/restarts.md) | once, and never twice |
| [Validation and defaults](how-it-works/validation-and-defaults.md) | where a rule goes, where a default comes from |
| [Simulator and tests](how-it-works/simulator-and-tests.md) | the tiers, the simulator, labs |

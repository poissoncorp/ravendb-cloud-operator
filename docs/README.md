# Documentation

Three kinds, in group `cloud.ravendb.net`: `CloudAccount` holds the API key, `CloudCluster`
names a cluster, and `CloudClusterRestart` records one action on it.

## How to use it

| | |
|---|---|
| [Installing](how-to-use/install.md) | Helm, a checkout, the values worth knowing |
| [Accounts](how-to-use/accounts.md) | the API key, account defaults, who an account lets in |
| [Clusters](how-to-use/clusters.md) | creating, changing, and standing the operator down for one |
| [Taking over](how-to-use/taking-over.md) | clusters that already exist, the export, observing without managing |
| [Connecting an application](how-to-use/connecting-applications.md) | the connection Secret and its certificate |
| [Restarting a node](how-to-use/restarting-a-node.md) | CloudClusterRestart |
| [Deleting](how-to-use/deleting.md) | what a delete does to the database |
| [Status and monitoring](how-to-use/status-and-monitoring.md) | STATUS and CHANGES, kstatus, Argo CD, events, logs, metrics |
| [Moving to a new API version](how-to-use/upgrading.md) | delete and apply again, safely |

## How it works

Start with [the architecture](how-it-works/architecture.md); each of the others
explains one mechanism, why it is built that way, and what not to break.

| | |
|---|---|
| [Architecture](how-it-works/architecture.md) | the pieces, one pass, the design decisions, the layout |
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

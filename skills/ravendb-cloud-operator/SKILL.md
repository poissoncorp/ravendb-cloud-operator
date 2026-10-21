---
name: ravendb-cloud-operator
description: Writes and troubleshoots manifests for the RavenDB Cloud Kubernetes operator (CloudAccount, CloudCluster, CloudClusterRestart in cloud.ravendb.net/v1alpha1). Use when someone creates or takes over a RavenDB Cloud cluster from Kubernetes, connects an application through the connection Secret, reads STATUS and CHANGES on these objects, restarts a node, or deletes a cluster safely.
license: MIT
---

# RavenDB Cloud operator

The operator runs RavenDB Cloud clusters from Kubernetes manifests. RavenDB
Cloud hosts the database; the operator creates or finds the cluster there and
publishes how to reach it into a Secret in the cluster's namespace.

Three kinds, all namespaced:

| Kind | What it is |
|---|---|
| `CloudAccount` | a RavenDB Cloud API key, read from a Secret, plus defaults its clusters inherit |
| `CloudCluster` | one RavenDB Cloud cluster, named by its display name |
| `CloudClusterRestart` | one restart of one node, done once |

## The smallest working setup

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: ravendb-cloud-api-key
stringData:
  apiKey: <a key from the RavenDB Cloud portal>
---
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: ravendb-cloud
spec:
  apiKeySecretRef:
    name: ravendb-cloud-api-key
  defaults:
    provider: AWS
    region: eu-central-1
    allowedIPs: ["203.0.113.0/24"]   # CIDR; a single address needs /32
---
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: orders
spec:
  accountRef:
    name: ravendb-cloud
  tier: Development                  # Free, Development or Production
```

`kubectl get cloudclusters` shows STATUS `Running` once the cluster is up and
its Secret, `orders-connection`, is written. Free clusters cannot be created
through the API, only found: create one in the portal first.

## The three decisions on every CloudCluster

- **`creationMode`**, what the display name means. `TakeOverOrCreate` (the
  default) uses the cluster of that name if one exists and creates it if not.
  `CreateOnly` refuses a name that already exists. `TakeOverOnly` never
  creates; use it for a manifest that describes a cluster made elsewhere, and
  for manifests restored from git. `kubectl ravendb-cloud export` writes
  `TakeOverOnly`.
- **`managementPolicy`**, what the operator may change. `Full` applies changes
  to instance type, node count and storage. `Observe` changes nothing and only
  reports differences. Unset, it is `Full` for a cluster the object created
  and `Observe` for one it found, so a manifest in git that should keep
  applying changes after a restore states `Full`.
- **`deletionPolicy`**, what `kubectl delete` does to the database. `Retain`
  (the default) leaves the cluster running. `Terminate` ends whatever cluster
  the object holds, created or taken over, and cannot be undone; the name is
  free again once termination completes.

## Rules that surprise people

- Until the object has found or created its cluster, every field can be
  edited. After that, `accountRef`, `tier`, `provider`, `region`, `subdomain`
  and `displayName` are fixed, because RavenDB Cloud cannot move a cluster.
  To change one, the cluster has to be replaced.
- `releaseChannel` and `networking.allowedIPs` are taken at creation only. A
  later change is reported on CHANGES as `NotApplied`; change it in the portal.
- `subdomain` is at most 13 characters. Left unset it is derived from the
  display name, so a longer name needs `spec.subdomain` set.
- Intervals are whole seconds: `pollIntervalSeconds` (default 300) on a
  CloudCluster, `refreshIntervalSeconds` (default 3600) on a CloudAccount.
- An API key reaches every cluster in its account. Whoever can create a
  CloudCluster next to a CloudAccount can take over any of them.
- The connection Secret holds a client certificate with Operator clearance:
  whoever reads the Secret administers the databases.
- A new cluster has no database. Create one in RavenDB Studio (`studioURL` in
  the Secret) or from code before the application uses it.
- `CloudClusterRestart` is created with `kubectl create`, never kept in git: it
  restarts once, and applying it again does nothing.

## Reading what it says

`kubectl get cloudclusters` has two columns. STATUS (the `Ready` condition's
reason) answers "can I use it": `Running`, or what it waits for or is blocked
on. CHANGES (`Synced`) answers "did my manifest reach RavenDB Cloud":
`Applied`, `Applying`, `Waiting`, `Refused` or `NotApplied`.
`kubectl describe` shows the message, which names the spec field to change,
and the events. A `Refused` is RavenDB Cloud's answer in its own words; the
operator does not retry it until the spec changes.

## Where the details are

Each page below is a guide in the repository. In a plugin install they sit
next to this skill at the relative path; otherwise read them on GitHub.

| Task | Guide |
|---|---|
| Install the operator | [install.md](../../docs/how-to-use/install.md) ([GitHub](https://github.com/ravendb/ravendb-cloud-operator/blob/main/docs/how-to-use/install.md)) |
| The API key, account defaults, several teams | [accounts.md](../../docs/how-to-use/accounts.md) ([GitHub](https://github.com/ravendb/ravendb-cloud-operator/blob/main/docs/how-to-use/accounts.md)) |
| Create, size and change a cluster | [clusters.md](../../docs/how-to-use/clusters.md) ([GitHub](https://github.com/ravendb/ravendb-cloud-operator/blob/main/docs/how-to-use/clusters.md)) |
| Take over clusters that already exist, export them | [taking-over.md](../../docs/how-to-use/taking-over.md) ([GitHub](https://github.com/ravendb/ravendb-cloud-operator/blob/main/docs/how-to-use/taking-over.md)) |
| Connect an application, the Secret's keys | [connecting-applications.md](../../docs/how-to-use/connecting-applications.md) ([GitHub](https://github.com/ravendb/ravendb-cloud-operator/blob/main/docs/how-to-use/connecting-applications.md)) |
| Restart a node | [restarting-a-node.md](../../docs/how-to-use/restarting-a-node.md) ([GitHub](https://github.com/ravendb/ravendb-cloud-operator/blob/main/docs/how-to-use/restarting-a-node.md)) |
| Delete safely, retire a cluster | [deleting.md](../../docs/how-to-use/deleting.md) ([GitHub](https://github.com/ravendb/ravendb-cloud-operator/blob/main/docs/how-to-use/deleting.md)) |
| Every STATUS and CHANGES reason, events, metrics | [status-and-monitoring.md](../../docs/how-to-use/status-and-monitoring.md) ([GitHub](https://github.com/ravendb/ravendb-cloud-operator/blob/main/docs/how-to-use/status-and-monitoring.md)) |
| Move to a new API version | [upgrading.md](../../docs/how-to-use/upgrading.md) ([GitHub](https://github.com/ravendb/ravendb-cloud-operator/blob/main/docs/how-to-use/upgrading.md)) |

`kubectl explain cloudcluster.spec` documents every field from the CRD itself,
and matches the operator version that is installed.

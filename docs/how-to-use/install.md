# Installing

> **See it run:** [labs/00_setup.sh](../../labs/00_setup.sh) installs the operator into a throwaway kind cluster, with a stand-in for RavenDB Cloud, in a couple of minutes.

## With Helm

```bash
helm install ravendb-cloud-operator oci://ghcr.io/ravendb/charts/ravendb-cloud-operator \
  --version 0.1.0 --namespace ravendb-cloud-operator-system --create-namespace
```

The chart pins the image of its release. The values worth knowing:

| Value | Default | Does |
|---|---|---|
| `watchNamespace` | empty, every namespace | keep the operator to one namespace; its permissions follow |
| `rbac.scope` | empty, follows `watchNamespace` | force `cluster` or `namespace` permissions |
| `metrics.enabled` | `false` | serve metrics, over HTTPS with authentication by default ([status-and-monitoring.md](status-and-monitoring.md#metrics)) |
| `logging.level` | `info` | `debug` adds every call to RavenDB Cloud |
| `imagePullSecrets` | none | pull from a private registry or a mirror |
| `extraEnv` | none | extra environment, such as a proxy |

Run one replica. If you run two, only one acts at a time and the other stands
by.

## From a checkout

```bash
make docker-build
helm install ravendb-cloud-operator charts/ravendb-cloud-operator \
  --namespace ravendb-cloud-operator-system --create-namespace \
  --set image.repository=ravendb-cloud-operator --set image.tag=dev
```

On kind, `kind load docker-image ravendb-cloud-operator:dev` first.
`make deploy IMG=<image>` is the kustomize path, for development. `make install`
installs only the CRDs, so objects applied after it would be accepted and then
sit there with nothing reconciling them.

## Upgrading the operator

Helm installs the CRDs from the chart's `crds/` directory once, and never
upgrades them. Apply the new ones yourself, then upgrade, with `<chart>` being
what you installed from:

```bash
helm show crds <chart> --version <new> | kubectl apply --server-side -f -
helm upgrade ravendb-cloud-operator <chart> --version <new> \
  --namespace ravendb-cloud-operator-system
```

A new API version, rather than a new release of `v1alpha1`, is a different
procedure: [upgrading.md](upgrading.md).

## While the operator is not running

Nothing happens to the clusters: they keep serving, and the connection Secrets
keep what they hold. What waits until it is back:

- changes to manifests, and status, which stays as it was at `lastSyncTime`;
- deleting a CloudCluster or CloudAccount, held by its finalizer, a marker that
  makes Kubernetes keep an object until the operator has cleaned up;
- a renewed client certificate. The operator puts it in the Secret three weeks
  before the old one expires, so an outage shorter than that costs nothing.

## Uninstalling

`helm uninstall` removes the operator and leaves the CRDs and every object, so
an object deleted afterwards waits for an operator that is not coming. To
remove everything:

1. Delete the CloudClusters while the operator still runs. With
   `deletionPolicy: Retain`, the default, the clusters keep running.
2. Delete the CloudAccounts.
3. `helm uninstall ravendb-cloud-operator -n ravendb-cloud-operator-system`.
4. Delete the CRDs:
   `kubectl delete crd cloudaccounts.cloud.ravendb.net cloudclusters.cloud.ravendb.net cloudclusterrestarts.cloud.ravendb.net`.

## Then

Create an account ([accounts.md](accounts.md)) and a cluster
([clusters.md](clusters.md)), or take over the clusters you already have
([taking-over.md](taking-over.md)).

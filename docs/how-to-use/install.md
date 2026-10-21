# Installing

> **See it run:** [labs/00_setup.sh](../../labs/00_setup.sh) installs the operator into a throwaway kind cluster, with a stand-in for RavenDB Cloud, in a couple of minutes.

## With Helm

```bash
helm repo add ravendb-cloud-operator https://ravendb.github.io/ravendb-cloud-operator/helm
helm install ravendb-cloud-operator ravendb-cloud-operator/ravendb-cloud-operator \
  --version <version> --namespace ravendb-cloud-operator-system --create-namespace
```

The versions are on the [releases page](https://github.com/ravendb/ravendb-cloud-operator/releases).
The chart pins the image of its release, `docker.io/ravendb/ravendb-cloud-operator`. The values
worth knowing:

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

## Without Helm

Every release carries `install.yaml`: the CRDs, the RBAC and the operator in one file.

```bash
kubectl apply --server-side -f https://github.com/ravendb/ravendb-cloud-operator/releases/download/v<version>/install.yaml
```

It installs into `ravendb-cloud-operator-system` with cluster-wide permissions; the Helm chart
is the way to keep the operator to one namespace.

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
upgrades them. Apply the new ones yourself, then upgrade:

```bash
helm repo update ravendb-cloud-operator
helm show crds ravendb-cloud-operator/ravendb-cloud-operator --version <new> | kubectl apply --server-side -f -
helm upgrade ravendb-cloud-operator ravendb-cloud-operator/ravendb-cloud-operator --version <new> \
  --namespace ravendb-cloud-operator-system
```

With `install.yaml`, apply the new release's file the same way you applied the first.

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

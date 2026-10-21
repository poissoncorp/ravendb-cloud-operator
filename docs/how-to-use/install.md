# Installing

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
| `metrics.enabled` | `false` | serve metrics, over HTTPS with authentication by default |
| `logging.level` | `info` | `debug` adds every call to RavenDB Cloud |
| `imagePullSecrets` | none | pull from a private registry or a mirror |
| `extraEnv` | none | extra environment, such as a proxy |

Run one replica. Leader election keeps a second one passive if you run it
anyway.

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

## Then

Create an account ([accounts.md](accounts.md)) and a cluster
([clusters.md](clusters.md)), or take over the clusters you already have
([taking-over.md](taking-over.md)).

# Taking over clusters that already exist

> **See it run:** [Lab 04](../../labs/04-take-over-an-existing-cluster.md) takes a cluster over by name, exports it, and shows the ways binding refuses.

A CloudCluster takes over the cluster its display name finds, so bringing
existing clusters under the operator is one manifest per cluster. ("Take over"
is the word these guides use. The event it records is called `Adopted`, and
`status.adopted` is true afterwards; they mean the same thing.)

## Exporting manifests

Write the manifests from the clusters rather than by hand:

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
every object settles on `Running` and `Applied`. From then on, editing the
manifest is how the cluster changes. `--all` skips clusters that are
terminated or being terminated; name clusters instead to export only those.
Object names are the display names, lower-cased and made DNS-safe, and
`spec.displayName` keeps the original.

The plugin is a binary named `kubectl-ravendb_cloud` (`.exe` on Windows); put
it on the PATH and kubectl finds it. With Go installed,
`go build ./cmd/kubectl-ravendb_cloud` in a checkout builds it for your
machine. The operator image carries it too, as `/kubectl-ravendb_cloud`, for
running inside the cluster next to the key.

## Writing one by hand

The smallest manifest is a name and the tier the cluster runs on:

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata: {name: legacy-prod, namespace: apps}
spec:
  accountRef: {name: ravendb-cloud}
  tier: Production
  creationMode: TakeOverOnly
```

That is safer than it looks:

- With `managementPolicy` unset, a cluster taken over runs under `Observe`: a
  wrong field shows up as CHANGES `NotApplied` rather than reshaping anything.
  Writing `Full` is the step that hands over control.
- `creationMode: TakeOverOnly` makes a mistyped name report `ClusterNotFound`
  instead of creating a new cluster. Until the object has a cluster, the name
  can simply be corrected.
- A cluster of another tier, or of another provider or region where the
  manifest names one, is somebody else's: the object reports `NameTaken` and
  binds nothing. So does a cluster another CloudCluster already holds.

Under the default `creationMode`, `TakeOverOrCreate`, an object that finds a
cluster nobody told it about takes it over and records a Warning event. If
that cluster is the one you meant, set `creationMode: TakeOverOnly` to say so.
If it is not, delete the object, which leaves the cluster as it is, unless
`deletionPolicy` is `Terminate`: then set `Retain` first, because deleting an
object terminates whatever cluster it holds, taken over or created.

## Using a cluster managed somewhere else

A cluster can stay managed in the portal or in Terraform and still be used
from Kubernetes. `managementPolicy: Observe` makes the operator read it and
never change it, creating included, and that still gives three things:

- **A connection Secret nobody copies by hand.** Node addresses and the client
  certificate land in a Secret pods mount, and stay current when a node is
  added or RavenDB Cloud renews the certificate.
- **A look before handing over control.** CHANGES reports how a manifest
  differs from the running cluster without applying anything. When the
  difference is the one you mean, `managementPolicy: Full` applies it.
- **Status where the rest of the platform looks.** `kubectl get`, events and
  alerts on `Ready`, including `DeletedInCloud` when somebody removes the
  cluster in the portal.

[`examples/04-observe-only.yaml`](../../examples/04-observe-only.yaml) is that
manifest. Keep `deletionPolicy: Retain` on it, so deleting it never ends a
cluster managed elsewhere.

It is not how an application team should reach a cluster another team manages
with this operator: one cluster binds to one CloudCluster, and the object would
need the account's API key, which reaches every cluster. Deliver that team the
connection Secret with a tool that keeps it in sync instead
([accounts.md](accounts.md#accounts-and-namespaces)).

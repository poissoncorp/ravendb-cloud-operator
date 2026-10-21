# Taking over clusters that already exist

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

[`examples/04-observe-only.yaml`](../../examples/04-observe-only.yaml) is that
manifest. It is not how an application team should reach a cluster another
team manages with this operator: one cluster binds to one CloudCluster, and the
object would need the account's API key, which reaches every cluster. Copy that
team the connection Secret instead.

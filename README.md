# RavenDB Cloud Operator

Manage RavenDB Cloud clusters as Kubernetes objects. Create a cluster from a
manifest, or take over one that already exists, and get its URLs and client
certificate in a Secret your applications mount.

> `v1alpha1`, not released yet. For RavenDB running on your own Kubernetes
> nodes, see [ravendb-operator](https://github.com/ravendb/ravendb-operator).

## What you write

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: ravendb-cloud
  namespace: apps
spec:
  apiKeySecretRef:
    name: ravendb-cloud-api-key
  defaults:
    provider: AWS
    region: eu-central-1
    allowedIPs: ["203.0.113.0/24"]
---
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: orders-dev
  namespace: apps
spec:
  accountRef:
    name: ravendb-cloud
  tier: Development
```

```
$ kubectl -n apps get cloudcluster
NAME         STATUS    CHANGES   TIER          REGION         NODES   AGE
orders-dev   Running   Applied   Development   eu-central-1   1       6m
```

The cluster's URLs and client certificate are in the Secret `orders-dev-connection`.

## How it behaves

| Kind | Does |
|---|---|
| `CloudAccount` | holds the API key, through a Secret, and the defaults its clusters inherit |
| `CloudCluster` | finds its cluster by display name or creates it, applies changes to instance type, node count and storage, and publishes the connection Secret |
| `CloudClusterRestart` | restarts one node, once |

- **The display name finds the cluster.** If one exists, the object takes it
  over and only observes it until `managementPolicy` is `Full`. If none does,
  the operator creates it. `creationMode` can allow only one of the two.
- **Only what the manifest sets is managed.** A field left out stays as the
  cluster has it.
- **`kubectl delete` keeps the cluster** unless the manifest says
  `deletionPolicy: Terminate`.
- **Git is enough to rebuild.** The same manifests applied to a new Kubernetes
  cluster find the same RavenDB Cloud clusters.

## Try it

The [labs](labs/) run on a kind cluster against a simulator of RavenDB Cloud,
so they need no account and cost nothing:

```bash
make lab-setup && make lab-run
```

## Install

```bash
helm repo add ravendb-cloud-operator https://ravendb.github.io/ravendb-cloud-operator/helm
helm install ravendb-cloud-operator ravendb-cloud-operator/ravendb-cloud-operator \
  --version <version> --namespace ravendb-cloud-operator-system --create-namespace
```

Until the first release, install from a checkout: [installing](docs/how-to-use/install.md).
Then the key, the account and a cluster:

```bash
kubectl create namespace apps
read -rsp 'RavenDB Cloud API key: ' key && echo
kubectl -n apps create secret generic ravendb-cloud-api-key --from-literal=apiKey="$key"
unset key
kubectl apply -f examples/00-account.yaml    # set allowedIPs to your addresses first
kubectl apply -f examples/01-provision-minimal.yaml
```

The cluster is billed until it is terminated, and deleting the object leaves it
running. To end it:

```bash
kubectl -n apps patch cloudcluster orders-dev --type=merge -p '{"spec":{"deletionPolicy":"Terminate"}}'
kubectl -n apps delete cloudcluster orders-dev
```

## Documentation

- [How to use it](docs/how-to-use/): installing, accounts, clusters, taking
  over, connecting an application, restarting a node, deleting, status and
  monitoring. [`examples/`](examples/) has a manifest for each.
- [How it works](docs/how-it-works/): the architecture and one file per
  mechanism.
- [`skills/ravendb-cloud-operator`](skills/ravendb-cloud-operator/SKILL.md): an
  [Agent Skill](https://agentskills.io) for writing these manifests with an AI
  assistant. In Claude Code:
  `/plugin marketplace add ravendb/ravendb-cloud-operator`, then
  `/plugin install ravendb-cloud-operator@ravendb-cloud-operator`.
- [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md).

## Not covered

What the RavenDB Cloud API cannot do, the operator does not offer:

- databases and backups, for which the API has no endpoints;
- client certificates limited to some databases;
- changing `allowedIPs` or the display name after creation.

## License

[MIT](LICENSE)

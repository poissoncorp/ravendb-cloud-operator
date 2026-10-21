# Lab 02: provision your first cluster

> **Reference:** [Clusters](../docs/how-to-use/clusters.md) covers the same ground as a guide.

Three lines of spec give you a running RavenDB Cloud cluster. In this lab you apply them, watch
the cluster come up in two columns of `kubectl get`, and see what the operator chose for
everything you left out.

## Do this

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab02-orders           # also the cluster's name in RavenDB Cloud
  namespace: apps
spec:
  accountRef:
    name: ravendb-cloud        # the account from lab 00
  tier: Development
  storage:
    sizeGB: 20
```

```bash
kubectl apply -f orders.yaml
kubectl -n apps get cloudcluster lab02-orders -w
```

## What you see

```
NAME           STATUS     CHANGES   TIER          REGION         NODES   AGE
lab02-orders   Creating             Development   eu-central-1   1       0s
lab02-orders   Running    Applied   Development   eu-central-1   1       32s
```

Two columns answer two questions:

- **STATUS**: can I use it? `Running` means RavenDB Cloud reports the cluster Active and its
  connection Secret is published, which is everything an application needs. Before that it names
  the stage, here `Creating`, or what is in the way.
- **CHANGES**: does RavenDB Cloud match my manifest? `Applied` means yes. It stays empty until
  there is a running cluster to compare with.

Each word is the reason on a condition (`Ready` and `Synced`), so what you read in the table is
what an alert or a `kubectl wait` matches on. `kubectl explain cloudcluster.status.conditions`
lists them all.

`-o wide` adds more columns, among them the policy, the instance type, the cluster id, the first
URL and the message:

```
POLICY   INSTANCE   CLUSTER-ID   URL                                             MESSAGE
Full     Dev10      1-A          https://a.lab02-orders-dev.acme.ravendb.cloud   The cluster is Active with 1 node(s), and Secret "lab02-orders-connection" holds its URLs and client certificate.
```

## What the operator filled in

Nothing in the manifest named a provider, a region, a release channel, an instance type or a
subdomain. The operator worked each one out and wrote it to `status`:

```
STATUS    INSTANCE   CHANNEL    PROVIDER   REGION         DISK
Running   Dev10      Stable72   AWS        eu-central-1   20
```

- **provider and region** come from the account's `spec.defaults`.
- **release channel** comes from the account default RavenDB Cloud reports, since neither the
  cluster nor the account named one.
- **instance type** is the smallest the tier offers: fewest CPUs, then least memory. Dev10 here,
  the cheapest Development shape.
- **subdomain** is derived from the name, `lab02-orders`, and RavenDB Cloud built the node URLs
  from it. It allows at most 13 characters; a longer name gets `STATUS InvalidSpec` with a
  message to set `spec.subdomain`, and nothing is created until you do.

Your `spec` is left exactly as you wrote it. The operator never writes there, so
`metadata.generation` stays at 1 and a GitOps tool sees nothing to correct. What it decided is
in `status`, and in the events:

```
TYPE      REASON            MESSAGE
Normal    Created           Created RavenDB Cloud cluster "lab02-orders" (1-A) in AWS/eu-central-1.
Warning   OpenAllowList     allowedIPs contains 0.0.0.0/0, so this cluster accepts connections from any address. That is fine for a lab and worth narrowing before it holds anything real.
Normal    SecretPublished   Connection Secret "lab02-orders-connection" created.
```

The one Warning is the lab account's open allow list, said once.

## A typo, fixed with an edit

The lab then applies a cluster whose `accountRef` says `ravendb-clod`. It uses
`managementPolicy: Observe`, which never creates anything, so nothing here can cost money:

```
NAME            STATUS              CHANGES   TIER   REGION   NODES   AGE
lab02-pending   WaitingForAccount                                     2s

CloudAccount "ravendb-clod" not found in namespace "apps". If the name is wrong, correct
spec.accountRef; nothing was created.
```

```bash
kubectl -n apps patch cloudcluster lab02-pending --type=merge \
  -p '{"spec":{"accountRef":{"name":"ravendb-cloud"}}}'
```

The edit is accepted, and the object moves on at once to `ClusterNotFound`, the honest answer
under Observe. Until an object has found or created its cluster, every field can be corrected
like this. After that, the account, tier, provider, region, subdomain and name are fixed, because
RavenDB Cloud cannot move a cluster between any of them.

## The same cluster, spelled out

Every field you can set, for reference. The script applies this one too, against the simulator:

```yaml
spec:
  accountRef:
    name: ravendb-cloud
  displayName: lab02-analytics   # the name in RavenDB Cloud, and how the operator finds it
  tier: Production
  provider: AWS
  region: eu-west-1              # overrides the account default
  instanceType: PB20
  releaseChannel: Stable62
  nodes: 3
  subdomain: lab02-stats         # 13 characters at most
  storage:
    sizeGB: 100                  # must be one of the sizes PB20 sells
    type: SsdPremium
    iops: 3000
    throughputMBps: 125
  networking:
    allowedIPs: [10.0.0.0/8, 203.0.113.7/32]
  managementPolicy: Full
  deletionPolicy: Retain
  connectionSecret:
    name: lab02-analytics-connection
  pollIntervalSeconds: 300
```

## Run the whole lab

```bash
labs/02_provision_a_cluster.sh
```

Beyond the above, the script checks two things you would otherwise have to take on trust. If the
operator stops right after sending a create, before it has recorded the new cluster's id, it
finds that cluster by name on the next pass and still treats it as one it created. And a settled
cluster is read again every `pollIntervalSeconds` (300 by default), every 30 seconds while
RavenDB Cloud is changing it, and never in a tight loop. The lab deletes its clusters at the end.

## Takeaway

Write what you care about; the operator fills in the rest from the account and from what RavenDB
Cloud offers, and shows its choices in `status` instead of rewriting your manifest. `STATUS`
tells you whether you can use the cluster, `CHANGES` whether it matches what you wrote.

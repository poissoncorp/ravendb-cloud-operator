# Clusters

> **See it run:** [Lab 02](../../labs/02-provision-a-cluster.md) creates one from the smallest manifest, and [lab 05](../../labs/05-day-two-operations.md) changes it once it runs.

A CloudCluster is one RavenDB Cloud cluster (RavenDB Cloud calls it a
product). The operator finds it by its display name, which defaults to the
object name: if the account has a cluster by that name, the object takes it
over; if not, the operator creates one.

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata: {name: orders-prod, namespace: apps}
spec:
  accountRef: {name: ravendb-cloud}
  tier: Production
  instanceType: PB40        # omit and the operator picks the smallest for the tier
  nodes: 3
  storage: {sizeGB: 200, type: SsdPremium}
  networking:
    allowedIPs: ["10.0.0.0/8"]
  managementPolicy: Full
```

Fields left out of a cluster the operator creates get a default (from the
CloudAccount, or the smallest the tier offers), and status reports what it got.
On a cluster that already exists, a field left out is left as the cluster has
it.

## What the operator may change: `managementPolicy`

- **`Full`**: the operator creates the cluster when none exists, and applies
  changes to a running one.
- **`Observe`**: the operator changes nothing in RavenDB Cloud, creating
  included. It reads the cluster, publishes its connection Secret, and reports
  on CHANGES how the manifest differs.

Left unset, it is `Full` for a cluster this object created and `Observe` for
one it found already running, so taking a cluster over never reshapes it until
you write `Full`. `status.managementPolicy` says which applies.

## What can change, and when

Until the object has found or created its cluster (`status.productID` is
empty), every field can be edited, so a typo in the account, region or name is
an edit. After that:

| Field | After the object has its cluster |
|---|---|
| `instanceType`, `nodes` | applied to the running cluster under `Full`; nodes are added or removed one at a time |
| `storage.sizeGB`, `type`, `iops` | applied under `Full`; a disk only grows. `throughputMBps` goes along with one of the others, and on its own is reported, not applied |
| `releaseChannel`, `networking.allowedIPs` | RavenDB Cloud takes them at creation only, so an edit is reported as `NotApplied`; change them in the portal |
| `accountRef`, `tier`, `displayName`, `subdomain` | fixed: RavenDB Cloud cannot move or rename a cluster. To get a different one, delete the object and apply a new one |
| `provider`, `region` | fixed once set; can still be added, to write down a value inherited from the account |
| `creationMode` | can only become `TakeOverOnly` |

After a storage change, RavenDB Cloud refuses the next one for a while: six
hours on AWS, twelve on Azure premium disks. The operator waits that out
(CHANGES `Waiting`) and sends the change when it is allowed.

`pollIntervalSeconds`, 300 by default, is how often the operator re-reads a
settled cluster. While RavenDB Cloud is creating or changing it, the operator
looks every 30 seconds.

## Choosing an instance type

Instance types depend on the tier, the provider and the region. Ask RavenDB
Cloud for the list, with the account's API key:

```bash
curl -s -H "X-Api-Key: $RAVENDB_CLOUD_API_KEY" \
  https://api.cloud.ravendb.net/api/v1/metadata/instance-types/Aws/eu-central-1 |
  jq -r '.instanceTypes[] | [.tier, .name, .parameters.virtualCpus, .parameters.ram,
         (.parameters.availableDiskSizes | map(tostring) | join(","))] | @tsv'
```

The provider is spelled `Aws`, `Azure` or `Gcp` in that path. Each line is the
tier, the name to put in `spec.instanceType`, CPUs, memory in GB, and the disk
sizes in GB it sells. Name one the tier does not offer and the object reports
`InvalidSpec` with the list instead; nothing is created.

The smallest Production type, the default, has a shared CPU and cannot grow
past three nodes. Name a larger type for a cluster that will grow.

## Manifests kept in git

A manifest applied again after a restore, or to a rebuilt Kubernetes cluster,
finds its cluster by name, but the object no longer knows it created that
cluster: that was only in status. Two fields make the same file behave the same
way both times:

- **`managementPolicy: Full`**, written down. Otherwise the cluster comes back
  under `Observe`, the default for a cluster found already running, and edits
  stop being applied.
- **`creationMode`**, chosen on purpose:
  - `TakeOverOrCreate`, the default: creates the cluster the first time, finds
    it again later. A takeover it did not expect raises a Warning event.
  - `TakeOverOnly`: never creates. Use it for restores and exported
    manifests, so a missing cluster is reported (`ClusterNotFound`) rather than
    replaced by an empty one.
  - `CreateOnly`: never takes over, for a cluster that must be new. After a
    restore it reports `NameTaken` for its own cluster; change it to
    `TakeOverOnly` to confirm the cluster is this object's.

`deletionPolicy` behaves the same either way: `Terminate` ends whatever cluster
the object holds, created or taken over. See [deleting.md](deleting.md).

## Leaving one cluster alone for a while

Set `managementPolicy: Observe`. The operator keeps reading the cluster and
publishing its Secret, changes nothing until the policy is `Full` again, and
holds node restarts back. `deletionPolicy` still applies if the object is
deleted meanwhile.

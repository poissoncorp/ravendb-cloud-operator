# Clusters

One RavenDB Cloud cluster. In RavenDB Cloud's own vocabulary this is a
"product"; it is called a cluster here because that is what it is, and because
that is the word the people writing the manifest already have in their heads.

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
  deletionPolicy: Retain
```

`accountRef`, `tier` and `displayName` never change, `provider` and `region`
never change once set, and `subdomain` is sent only when the operator creates
the cluster. `instanceType`, `storage` and `nodes` are applied to a running
cluster under `Full`. `releaseChannel` and `networking.allowedIPs` are taken at
creation only, so a later edit is reported rather than applied.

## Standing the operator down for one cluster

To have the operator leave one cluster alone for a while without deleting the
object, set `managementPolicy: Observe`: it keeps reading the cluster and
publishing its Secret, and changes nothing until the policy is `Full` again.
Its `deletionPolicy` still applies if the object is deleted meanwhile.

# Restarting a node

> **See it run:** [Lab 05](../../labs/05-day-two-operations.md) restarts a node, including one held back by the cluster's policy.

A CloudClusterRestart restarts one node, once. It is a separate kind rather
than a field on CloudCluster because "restart node A" is not a state: nothing
about the cluster is different afterwards, so no field value could mean
"restarted", and a field would restart the node again on every resync.

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudClusterRestart
metadata: {generateName: orders-prod-a-, namespace: apps}
spec:
  clusterRef: {name: orders-prod}
  nodeTag: A
```

```bash
kubectl create -f restart.yaml
kubectl -n apps wait --for=condition=Complete cloudclusterrestart/<name it printed> --timeout=15m
```

```
$ kubectl -n apps get cloudclusterrestart
NAME                  CLUSTER       NODE   STATUS      AGE
orders-prod-a-x7k2q   orders-prod   A      Succeeded   4m
```

STATUS goes `Waiting`, `Restarting`, then `Succeeded` or `Failed`. Give
`kubectl wait` a timeout: its default is 30 seconds, shorter than a restart. A
failed restart never satisfies `Complete`, so the wait times out on it rather
than reporting success.

The node is down while it restarts. On a Development cluster, which has one
node, that is an outage. On Production the other nodes keep serving, and a
database stays available if it has a copy on another node (a replication
factor above 1).

The node tags are in the cluster's `status.nodeTags`. The restart waits, and
says why, while the CloudCluster is not `Running` yet or its
`managementPolicy` is `Observe`.

The spec cannot be edited once applied. To restart again, or to retry a failed
restart, create another object; the old ones are the history of what was done
to the cluster. They belong to the CloudCluster and are deleted with it.

When RavenDB Cloud does not answer the restart request, the operator does not
send it again, since that could restart the node twice. It watches the cluster
instead, and the message says the answer was lost. If the cluster stays Active
throughout, the restart ends `Succeeded` although it may never have reached
the node.

## Keep restarts out of git

Create restarts with `kubectl create` (which is what lets `generateName` pick a
fresh name), and keep them out of the repository a GitOps tool syncs. Like a
Job, a restart remembers that it ran only in its status, so one recreated
without it, after a restore or when its CloudCluster is deleted and applied
again, restarts the node again.

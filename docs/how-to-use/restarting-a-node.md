# Restarting a node

> **See it run:** [Lab 05](../../labs/05-day-two-operations.md) restarts a node, including one held back by the cluster's policy.

One node, restarted once. It is a separate kind rather than a field on
CloudCluster because a CRD spec describes desired state, and "restart node A"
is not a state: nothing about the cluster afterwards is different, so no field
value could mean "restarted", and a field would re-fire on every resync.

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudClusterRestart
metadata: {name: orders-a-after-oom, namespace: apps}
spec:
  clusterRef: {name: orders-prod}
  nodeTag: A
```

```
$ kubectl -n apps get cloudclusterrestart
NAME                 CLUSTER       NODE   STATUS      AGE
orders-a-after-oom   orders-prod   A      Succeeded   4m
```

The spec is immutable, so a request cannot be re-aimed after it has gone out;
to restart again, apply another object, which is also how you get a history of
what was done to the cluster. It obeys `managementPolicy` on the cluster:
while that holds it back, or the cluster is not Running yet, it waits and says
why. The operator makes each record a child of the
CloudCluster, so they are collected with it.

When RavenDB Cloud does not answer the restart request, the operator does not
send it again, since that could restart the node twice. It watches the cluster
instead, and the message says the answer was lost. If the cluster stays Active
throughout, the restart ends `Succeeded` although it may never have reached
the node.

Create restarts with `kubectl create` and keep them out of the repository a
GitOps tool syncs. Like a Job, a restart remembers that it ran only in its
status, so one recreated without it, after a restore or when its CloudCluster
is deleted and applied again, restarts the node again.

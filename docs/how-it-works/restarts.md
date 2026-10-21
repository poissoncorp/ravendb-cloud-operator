# Restarts

How a CloudClusterRestart restarts one node, once. Read before changing
`internal/controller/cloudclusterrestart_controller.go`.

## An event, not a state

A restart is a kind of its own because nothing about the cluster is different
afterwards, so no spec field could mean "restarted", and a field would fire
again on every resync. The spec is immutable; restarting again means creating
another object. Like a Job, a restart remembers that it ran only in its status,
so one recreated from a manifest, after a restore or a re-apply, restarts the
node again. Create them with `kubectl create` and keep them out of the
repository a GitOps tool syncs.

Each restart is made a child of its CloudCluster and is collected with it.

## Before sending

It waits, as `Complete Waiting` with the reason in the message, while:

- the CloudCluster does not exist yet or is not bound to a cluster;
- its `managementPolicy` is not `Full`;
- its account cannot be used;
- the CloudCluster's Ready is not `Running`;
- another restart of the same cluster was issued and has not finished
  (`restartUnderWay`): RavenDB Cloud restarts one node at a time;
- RavenDB Cloud says the cluster is not Active, read live rather than from the
  CloudCluster's status, which is only as fresh as its last poll.

A node tag missing from the CloudCluster's `status.nodeTags` is `Failed` at
once.

## Sending once

`status.issuedAt` is stamped before the request. After it:

- answered: `Restarting`, with a Normal event;
- busy, or certainly not carried out: the stamp is cleared, back to
  `Waiting`, sent again later;
- answer lost: not sent again, since that could restart the node twice. It
  watches the cluster instead and says the answer was lost; if the cluster
  stays Active throughout, it ends `Succeeded` although the restart may never
  have reached the node;
- refused: `Failed`, final.

The controller runs one worker (`MaxConcurrentReconciles = 1`), so two restarts
of one cluster are never issued in the same instant; `restartUnderWay` covers
one issued on an earlier pass. Together they are the "once": don't raise the
worker count without replacing that guarantee.

## Finishing

It asks RavenDB Cloud, not the CloudCluster's status, whether the node came
back: transitional means still `Restarting`, serving means `Succeeded` with
`completedAt` and a Normal `Restarted` event, anything else means `Failed`, and
so does a cluster RavenDB Cloud no longer has. Any other failed read keeps it
`Restarting`.
`Complete` is True only on success; `Failed` is True on failure, and either one
finishes the object.

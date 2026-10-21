# Moving to a new API version

> **See it run:** [Lab 04](../../labs/04-take-over-an-existing-cluster.md) runs the export this relies on.

`v1alpha1` can still change in ways old manifests do not survive, and when it
does there will be no conversion webhook. None is needed: a CloudCluster finds
its cluster by name, so moving to a new version is deleting the old objects and
applying new ones, and nothing in RavenDB Cloud changes.

1. Set `deletionPolicy: Retain` on every object that says `Terminate`.
   Skipping this terminates those clusters in step 3.
2. Write the new manifests. `kubectl ravendb-cloud export` reads the running clusters
   and writes each with `creationMode: TakeOverOnly`.
3. Delete the old objects. The clusters keep running. Their connection Secrets
   go with them: pods already running keep what they read, and a pod starting
   before step 5 waits for its Secret.
4. Install the new CRDs and operator.
5. Apply the new manifests. Each takes its cluster over by name and publishes
   its Secret again, and under `TakeOverOnly` a `Terminate` written there means
   what it says.

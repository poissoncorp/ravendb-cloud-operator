# Accounts

> **See it run:** [Lab 01](../../labs/01-verify-credential.md) verifies a key and rotates it, and [lab 06](../../labs/06-delete-safely.md#deleting-an-account) deletes an account a cluster still names.

A CloudAccount holds one RavenDB Cloud API key, through a Secret it names, and
the defaults its clusters inherit when the operator creates them. Every
CloudCluster names one CloudAccount in its own namespace.

```yaml
apiVersion: v1
kind: Secret
metadata: {name: ravendb-cloud-api-key, namespace: apps}
stringData:
  apiKey: "<a key from the RavenDB Cloud portal>"
---
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata: {name: ravendb-cloud, namespace: apps}
spec:
  apiKeySecretRef: {name: ravendb-cloud-api-key, key: apiKey}
  defaults:
    provider: AWS
    region: eu-central-1
    allowedIPs: ["203.0.113.0/24"]   # who may reach the clusters it creates
```

`defaults.allowedIPs` is the firewall of every cluster created without its own
list, and RavenDB Cloud takes it at creation only: narrowing it later means the
portal. `0.0.0.0/0` opens a cluster to the whole internet; the operator allows
it, for labs, and records a Warning event when it creates such a cluster.

Once the key is verified, STATUS is `Verified` and status lists what this
account can order, so `kubectl describe cloudaccount ravendb-cloud` answers
"which regions and release channels are there" without the portal. A release
channel is the RavenDB version stream a cluster follows, such as `Stable72`.
The lists are re-read every `refreshIntervalSeconds`, 3600 by default.
Instance types are not listed here; [clusters.md](clusters.md#choosing-an-instance-type)
says how to find them.

| STATUS | What to do |
|---|---|
| `Verified` | nothing |
| `KeyMissing` | create the Secret, or the key in it, that `apiKeySecretRef` names |
| `KeyRejected` | put a new key from the portal in the Secret; the key is mistyped, deactivated or expired |
| `CloudUnreachable` | nothing; retried with backoff. If it lasts, check the operator's egress or proxy (`extraEnv` in the chart) |
| `UnexpectedResponse` | the operator could not read RavenDB Cloud's answer; retried every five minutes, and an operator upgrade may be needed |
| `InUse` | the account is being deleted and CloudClusters still name it; delete those first |

## Replacing the key

Write the new key into the same Secret. The account verifies it at once, and
every cluster uses it from its next call.

The new key must belong to the same RavenDB Cloud account. A cluster records
the account it lives in (`status.accountDomain`), and if the Secret now holds a
key for another account, the operator leaves the cluster alone and its STATUS
is `WaitingForAccount`, naming both accounts, until the right key is back.
Calls with the wrong key would find no such cluster, which looks exactly like
the cluster having been deleted.

## Deleting an account

An account stays, reporting `InUse`, while any CloudCluster in its namespace
names it, and goes once none does. Delete the clusters first: a cluster with
`deletionPolicy: Terminate` needs the key to terminate.

## Accounts and namespaces

`accountRef` reaches only the CloudCluster's own namespace, and the connection
Secret is written there too. So where accounts and clusters live decides who
can do what:

- **Clusters next to the applications.** Each team's namespace holds a
  CloudAccount, its key Secret and the CloudClusters, and pods mount the
  connection Secret directly. Simple, and the Secret stays current by itself.
  Every team with such an account can reach every cluster in that RavenDB
  Cloud account, so use it for teams that may, or give each team its own
  RavenDB Cloud account.
- **Clusters in a platform namespace.** One namespace holds the account and
  every CloudCluster; applications live elsewhere. Their connection Secrets
  land in the platform namespace, so deliver them with a tool that keeps the
  copy in sync, such as a Secret replicator or External Secrets' Kubernetes
  provider. Never copy one by hand: the copy misses the next node and the next
  certificate.

## Who an account lets in

A RavenDB Cloud API key reaches every cluster in its account; RavenDB Cloud has
no narrower one. Whoever may create or edit a CloudCluster next to a
CloudAccount can therefore name any cluster in that account that no other
CloudCluster holds, and the operator takes it over:

- it publishes that cluster's client certificate into their namespace, which
  administers every database on it;
- under `managementPolicy: Full` it reshapes the cluster to their manifest;
- with `deletionPolicy: Terminate`, deleting their object terminates the
  cluster.

Grant that right only to people you would give the key itself.

# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| 0.1.x   | :white_check_mark: |

## What the operator is trusted with

**A RavenDB Cloud API key.** A `CloudAccount` names a Secret holding it. The
operator reads it when it builds a client, keeps it in memory, sends it only as
the `X-Api-Key` header to RavenDB Cloud, and never writes it to status, events
or logs; a log line names a key only by a short hash. No field of a
CloudAccount can send it anywhere else, because the Secret it names could hold
anything: an account that chose its own destination would let whoever can
write CloudAccounts read every Secret in their namespace. A key reaches every cluster in its
account, and RavenDB Cloud has no narrower one, so whoever may create a
CloudCluster next to a CloudAccount can name any cluster in that account that
no other CloudCluster holds: receive its client certificate, reshape it under
`managementPolicy: Full`, and terminate it by deleting the object under
`deletionPolicy: Terminate`. Put an account only in a namespace where everyone
who can write CloudClusters may manage every cluster in it.

**Write access to Secrets.** Publishing connection details is what the
operator is for. The connection Secret holds each cluster's client
certificate, which RavenDB Cloud issues with Operator clearance: whoever can
read that Secret administers the cluster's databases. Grant read on it as you
would on a database administrator's credential.

## Permissions

| Resource | Verbs | Why |
|---|---|---|
| Secrets | get, list, watch, create, patch | read API keys, publish connection Secrets by server-side apply |
| CloudAccounts, CloudClusters, CloudClusterRestarts | get, list, watch, patch | reconcile them; patch for finalizers and owner references |
| their status | get, update | report |
| CloudClusters' finalizers | update | hold an object until its deletion policy is honoured |
| events.k8s.io events | create, patch | report |

There is no delete on Secrets, which are collected through owner references,
and no create or delete on the operator's own kinds. Both Secret watches are
metadata only, so Secret contents never reach the operator through a watch.

By default the grant is cluster-wide. With `watchNamespace` set, the Helm chart
grants it in that one namespace instead.

## The container

Distroless, one static binary for the manager and one for the kubectl plugin,
no shell, user 65532. Both the Helm chart and the kustomize manifests set
`runAsNonRoot`, `seccompProfile: RuntimeDefault`,
`allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true` and drop all
capabilities. Nothing is written to disk at runtime.

Metrics are off by default; turned on, they are served over HTTPS and require
authentication and authorization.

Answers from RavenDB Cloud are capped in size before they are decoded, and the
certificate archive is unpacked in memory into a fixed set of keys, with every
entry and the archive capped, so no entry name reaches a file path.

## Reporting a Vulnerability
Please report security vulnerabilities to security@ravendb.net

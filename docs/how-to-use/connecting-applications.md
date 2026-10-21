# Connecting an application

> **See it run:** [Lab 03](../../labs/03-connect-an-application.md) publishes the Secret and has a pod mount it.

For every CloudCluster the operator writes a Secret, `<object name>-connection`
unless `spec.connectionSecret.name` says otherwise, in the same namespace. It
holds the node addresses and the cluster's client certificate, unpacked from
the archive RavenDB Cloud serves:

| Key | Contents |
|---|---|
| `url`, `urls` | the first node's address; all of them, comma separated |
| `nodeTags` | RavenDB node tags (`A,B,C`), in the order of `urls` |
| `studioURL` | RavenDB Studio, the web console |
| `productID` | the RavenDB Cloud cluster id |
| `tls.crt`, `tls.key` | client certificate and key, PEM |
| `client.pem` | certificate and key in one PEM file |
| `client.pfx` | PKCS#12 without a password |
| `client.with-password.pfx`, `certificate.password` | PKCS#12 with a password, and that password, for tools that refuse one without |

STATUS is `Running` once the Secret is published. Right after creation it may
read `WaitingForCertificate` for a few minutes while RavenDB Cloud issues the
certificate. If the archive ever cannot be unpacked, the Secret holds it raw
under `certificate.bundle` and STATUS reads `UnexpectedResponse`, which an
operator upgrade fixes.

## What the certificate allows

RavenDB Cloud issues the cluster certificate with Operator clearance: it can
create and delete databases and administer everything in them. Every pod that
can read this Secret has that. RavenDB Cloud's API offers no narrower
certificate; for an application that should reach one database only, create a
certificate with that access in RavenDB Studio and distribute it yourself.

## Mounting it

Mount the whole Secret as a volume. Mounted files follow the Secret, so a new
node or a renewed certificate reaches the pod without redeploying; a value
taken into an environment variable is read once, when the container starts.

```yaml
spec:
  securityContext:
    fsGroup: 1000          # lets a non-root container read the 0440 files
  containers:
    - name: api
      volumeMounts:
        - {name: ravendb, mountPath: /etc/ravendb, readOnly: true}
  volumes:
    - name: ravendb
      secret: {secretName: orders-prod-connection, defaultMode: 0440}
```

[`examples/05-consume-from-app.yaml`](../../examples/05-consume-from-app.yaml)
is a complete Deployment.

## Opening a document store

RavenDB clients take a list of node addresses and a client certificate. They
learn the rest of the cluster from the nodes, so the list only needs one node
that answers.

.NET:

```csharp
var store = new DocumentStore
{
    Urls = File.ReadAllText("/etc/ravendb/urls").Split(','),
    Database = "orders",
    Certificate = new X509Certificate2("/etc/ravendb/client.pfx"),
}.Initialize();
```

Node.js:

```js
import { readFileSync } from "node:fs";
import { DocumentStore } from "ravendb";

const store = new DocumentStore(
  readFileSync("/etc/ravendb/urls", "utf8").split(","),
  "orders",
  { type: "pem", certificate: readFileSync("/etc/ravendb/client.pem", "utf8") },
);
store.initialize();
```

## Creating the first database

A new cluster has no databases. Create one from code, with the same store:

```csharp
store.Maintenance.Server.Send(
    new CreateDatabaseOperation(new DatabaseRecord("orders"), replicationFactor: 3));
```

```js
import { CreateDatabaseOperation } from "ravendb";
await store.maintenance.server.send(new CreateDatabaseOperation({ databaseName: "orders" }, 3));
```

A replication factor of 3 keeps a copy on three nodes of a Production cluster;
a Development cluster has one node, so use 1 there. Or create it in RavenDB
Studio: export the protected certificate, import it into your browser with its
password, and open `studioURL`.

```bash
kubectl -n apps get secret orders-prod-connection -o jsonpath='{.data.client\.with-password\.pfx}' | base64 -d > orders.pfx
kubectl -n apps get secret orders-prod-connection -o jsonpath='{.data.certificate\.password}' | base64 -d; echo
kubectl -n apps get secret orders-prod-connection -o jsonpath='{.data.studioURL}' | base64 -d; echo
```

## When the certificate is renewed

RavenDB Cloud issues client certificates for five years and replaces one a
month before it expires. The operator puts the replacement in the Secret three
weeks before expiry; `status.certificateExpiresAt` says when the current one
runs out. Most applications read the certificate once, when they open the
store, so restart them to pick it up. A tool that rolls workloads when a Secret
changes, such as Reloader, does that for you.

## Turning it off

`spec.connectionSecret.enabled: false` removes every key the operator wrote and
leaves the Secret object. A pod naming one of the keys, through `secretKeyRef`
or a volume's `items`, then fails to start until they are back; one mounting the
whole Secret sees an empty directory. `includeCertificate: false` removes the
certificate keys only, for an application that authenticates another way.

A Secret by that name that the operator did not create is never touched:
STATUS reads `NameTaken`, and setting `spec.connectionSecret.name` to a free
name fixes it. Edits to the Secret's keys are put back at the next poll. The
Secret belongs to the CloudCluster and is deleted with it, and carries the
label `cloud.ravendb.net/cluster: <object name>`.

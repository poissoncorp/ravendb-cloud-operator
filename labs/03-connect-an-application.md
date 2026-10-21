# Lab 03: connect an application to the cluster

> **Reference:** [Connecting an application](../docs/how-to-use/connecting-applications.md)
> covers the same ground as a guide.

This is what the operator is for. Once a cluster is running, a Secret appears next to it with
everything a client needs: the node addresses and a client certificate. In this lab a pod mounts
that Secret and reads it.

One thing to know up front: the simulator has no database behind it. Its addresses resolve to
nothing, so the pod in this lab reads the Secret and checks the certificate, and does not connect.
Against RavenDB Cloud the same Secret connects; the client code for that is below.

## Do this

The lab needs a cluster first. It seeds a three node Production cluster called `lab03-app` in the
simulator and applies a `CloudCluster` by the same name, which finds it ([lab 04](04-take-over-an-existing-cluster.md)
is about that part). A cluster the operator creates gets exactly the same Secret.

```bash
labs/03_connect_an_application.sh
```

## What you see

```
NAME        STATUS    CHANGES   TIER         REGION         NODES   AGE
lab03-app   Running   Applied   Production   eu-central-1   3       2s

The cluster is Active with 3 node(s), and Secret "lab03-app-connection" holds its URLs and client certificate.
```

The Secret is `<cluster>-connection` unless you name it with `spec.connectionSecret.name`. It has
eleven keys:

| key | what it is |
|---|---|
| `urls` | every node address, comma separated: what a client wants |
| `url` | the first of them, for anything that takes one address |
| `nodeTags` | the node tags (A, B, C), in the same order as `urls` |
| `studioURL` | RavenDB Studio, in a browser |
| `productID` | the cluster's id in RavenDB Cloud |
| `tls.crt`, `tls.key` | the client certificate and its private key, PEM |
| `client.pem` | both in one PEM file |
| `client.pfx` | PKCS#12 without a password |
| `client.with-password.pfx`, `certificate.password` | the same, with a password, and the password |

RavenDB Cloud hands out the certificate as a ZIP built for someone clicking in the portal, with
the subdomain and a date in every file name. The operator unpacks it into these fixed names, so a
pod mounts one Secret and a chart that already knows `tls.crt` needs nothing special.

The script then proves the material is real with `openssl`:

```
subject=O=RavenDB Cloud lab mock, CN=lab03-app.acme.ravendb.cloud.client.certificate
notAfter=Sep  1 13:44:02 2027 GMT
client.pem holds 2 PEM blocks
```

## Mount it

```yaml
containers:
  - name: app
    volumeMounts:
      - name: ravendb
        mountPath: /etc/ravendb
        readOnly: true
volumes:
  - name: ravendb
    secret:
      secretName: lab03-app-connection
      items:
        - {key: urls,       path: urls}
        - {key: client.pfx, path: client.pfx}
        - {key: client.pem, path: client.pem}
```

Read the addresses from the mounted file rather than from an environment variable. The operator
rewrites the Secret on every poll, so a node that joins the cluster appears in `urls` within a
poll, and the kubelet refreshes mounted files in place, within about a minute. An environment
variable is read once, when the container starts, and keeps the old list until the pod restarts.
The Job in this lab reads `urls` from its environment, which is fine for something that runs once
and exits; a long-running application should not.

Then, in a .NET application:

```csharp
var store = new DocumentStore
{
    Urls = File.ReadAllText("/etc/ravendb/urls").Split(','),
    Database = "Orders",
    Certificate = new X509Certificate2("/etc/ravendb/client.pfx"),
}.Initialize();
```

Node.js takes `client.pem` instead: `new DocumentStore(urls, "Orders", { type: "pem", certificate })`.
A new cluster has no databases, so create `Orders` first, in Studio or from code. The certificate
has full access to every database on the cluster, so mount it only where it is needed.

What the lab's Job printed:

```
urls      : https://a.lab03-app.acme.ravendb.cloud,https://b.lab03-app.acme.ravendb.cloud,https://c.lab03-app.acme.ravendb.cloud
nodeTags  : A,B,C
mounted   : client.pem tls.crt tls.key
subject   : lab03-app.acme.ravendb.cloud.client.certificate
```

## What the operator does with it afterwards

- **Downloads the certificate once.** Polls refresh the addresses; the certificate is fetched
  again only when the stored one nears expiry or is gone. The script counts the downloads in the
  simulator's log while the cluster is polled every five seconds: it stays at one.
- **Puts it back if you delete it.** `kubectl delete secret lab03-app-connection`, and a new one
  appears within seconds.
- **Owns it.** The Secret carries an owner reference to the `CloudCluster`, so Kubernetes deletes
  it with the cluster object, and nothing has to remember to.
- **Will not take someone else's.** A second `CloudCluster` aiming `spec.connectionSecret.name` at
  a Secret it does not own gets `STATUS NameTaken`, and that Secret is left untouched.

Three switches under `spec.connectionSecret` change what is written, and the script tries each:

- `name` moves publication to another Secret. The old one is emptied, not left holding a copy
  of the key.
- `includeCertificate: false` writes the addresses only, for certificates delivered some other
  way, and removes a certificate already published.
- `enabled: false` stops publishing and empties the Secret. `STATUS` stays `Running`, and the
  message says so.

## Takeaway

One Secret, owned by the cluster object, holds everything a client needs, with the addresses kept
current on every poll. Mount it, read `urls` and the certificate from files, and you are
connected: no portal download, no copied certificate, no redeploy when the cluster changes shape.

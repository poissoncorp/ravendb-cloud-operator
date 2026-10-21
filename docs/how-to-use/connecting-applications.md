# Connecting an application

> **See it run:** [Lab 03](../../labs/03-connect-an-application.md) publishes the Secret and has a pod mount it.

The point of the whole thing. RavenDB Cloud serves the client certificate as a
ZIP holding a PFX pair, PEM material and a password file. The operator unpacks
it into conventional Kubernetes keys, so a pod just mounts it:

| key | contents |
|---|---|
| `url`, `urls` | first / all node endpoints |
| `nodeTags` | RavenDB node tags, aligned with `urls` |
| `studioURL` | RavenDB Studio address |
| `productID` | RavenDB Cloud cluster id |
| `tls.crt`, `tls.key` | client certificate and key, PEM |
| `client.pem` | both, in one file |
| `client.pfx`, `client.with-password.pfx` | PKCS#12 forms |
| `certificate.password` | password for the protected PKCS#12 |

The Secret is owned by the CloudCluster, so it is garbage collected with it. The
certificate is downloaded once, not on every poll, and again three weeks before
it expires, by when RavenDB Cloud has issued its replacement.

An application usually reads the certificate once, when it opens its document
store, so it needs a restart to use the new one. A tool that rolls workloads
when a Secret changes, such as Reloader, does that; `status.certificateExpiresAt`
says when the current one runs out.
[`examples/05-consume-from-app.yaml`](../../examples/05-consume-from-app.yaml) shows a
Deployment consuming it.

## Turning it off

`spec.connectionSecret.enabled: false` removes every key the operator wrote and
leaves the Secret object. A pod naming one of the keys, through `secretKeyRef`
or a volume's `items`, then fails to start until they are back; one mounting the
whole Secret sees an empty directory. `includeCertificate: false` removes the
certificate keys only, for an application that authenticates another way.
`spec.connectionSecret.name` picks the Secret's name; an existing Secret the
operator did not create is never taken over.

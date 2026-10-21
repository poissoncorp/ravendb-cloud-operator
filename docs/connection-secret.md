# The connection Secret

What the operator publishes for applications, and how. Read before changing
`internal/controller/connection_secret.go` or the Secret's keys.

## Keys are a contract

| Key | Contents |
|---|---|
| `url`, `urls` | first and all node endpoints, `https://` included |
| `nodeTags` | node tags, aligned with `urls` |
| `studioURL`, `productID` | RavenDB Studio address, RavenDB Cloud cluster id |
| `tls.crt`, `tls.key`, `client.pem` | client certificate and key, PEM |
| `client.pfx`, `client.with-password.pfx`, `certificate.password` | PKCS#12 forms |

Renaming a key breaks every pod that names it. RavenDB Cloud serves the
certificate as a ZIP; the operator unpacks it into these keys.

## Two writers, one Secret

The endpoints and the certificate are written by server-side apply under two
field managers (`ravendb-cloud-operator` and
`ravendb-cloud-operator-certificate`), so turning the certificate off removes
exactly its keys and nothing else. The Secret carries a controller owner
reference to its CloudCluster and is garbage collected with it.

An existing Secret this object does not own is never taken over: the object
reports `NameTaken` on Ready and names `spec.connectionSecret.name`. Renaming
the Secret in the spec moves the keys to the new name and removes them from the
old one, which status remembers.

`connectionSecret.enabled: false` removes every key the operator wrote and
leaves the Secret object; `includeCertificate: false` removes the certificate
keys only.

## The certificate

Downloaded once, not on every poll, and again when the stored one is within
`certificateRenewWindow` (21 days) of expiry. RavenDB Cloud issues certificates
for five years and regenerates them 30 days before expiry, so the replacement is
there when the window opens. `status.certificateExpiresAt` says when the current
one runs out. Applications usually read the certificate once, so they need a
restart to use a new one.

## No addresses, no Secret

A cluster reported Active with no node addresses publishes nothing and reads as
an error: a renamed field upstream looks exactly like that, and an empty `url`
on a cluster that says it is ready is worse than no Secret.

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
| `certificate.bundle` | the archive as RavenDB Cloud sent it, only when it would not unpack |

Renaming a key breaks every pod that names it. RavenDB Cloud serves the
certificate as a ZIP; the operator unpacks it into these keys. An archive that
does not unpack is published raw as `certificate.bundle`, any certificate
already in place stays, and Ready says `UnexpectedResponse`. With no
certificate in place, the unpack is tried again on those bytes at every poll,
without a second download.

The Secret carries the label `cloud.ravendb.net/cluster` with the CloudCluster's
name, which is why that name is capped at 63 characters, a label value's limit.

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

The operator watches the Secret for deletions only, which bring it back at
once. A hand edit to the endpoint keys is put right at the next poll; the
certificate keys are written only when a certificate is downloaded.

## The certificate

Downloaded once, not on every poll, and again when the stored one is within
`certificateRenewWindow` (21 days) of expiry. RavenDB Cloud issues certificates
for five years and regenerates them 30 days before expiry, so the replacement is
there when the window opens. `status.certificateExpiresAt` says when the current
one runs out. Applications usually read the certificate once, so they need a
restart to use a new one.

A cluster can be Active before its certificate exists: RavenDB Cloud answers 404
until it does, the endpoints are published anyway, and Ready says
`WaitingForCertificate`. Any other failed download ends the pass and is reported
on Ready like any other failed call. A stored certificate that does not parse is
never downloaded again by itself, since downloading on every poll would turn one
bad Secret into a loop of calls.

## No addresses, no Secret

A cluster reported Active with no node addresses publishes nothing and reads as
an error: a renamed field upstream looks exactly like that, and an empty `url`
on a cluster that says it is ready is worse than no Secret.

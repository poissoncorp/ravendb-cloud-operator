# Lab 03: Connect an application to the cluster

**For:** you have a Ready `CloudCluster` and now need a pod in the same namespace to actually
talk to it. This lab walks the connection Secret the operator publishes: all eleven keys and
what each one is for, proof that the certificate inside is a real certificate, a pod that
mounts it, and the switches that change what gets written. You end up knowing exactly what to
reference from a Deployment, and what deleting each piece does.

## Run it

```bash
labs/03_connect_an_application.sh
```

The complete example is [`03_connect_an_application.sh`](03_connect_an_application.sh). It
needs `openssl` on your PATH, and the one in-pod step needs the `busybox:1.36` image (see
[Mounting it from a pod](#mounting-it-from-a-pod)). The core is a cluster and a pod that reads
what the operator wrote for it:

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudCluster
metadata:
  name: lab03-app            # the name of a three-node PB10 the script seeds in
                             # the lab control plane first
spec:
  accountRef:
    name: ravendb-cloud
  tier: Production
  pollInterval: 5s           # faster than the 5m default, so polls are visible
---
# ...and the half of a pod spec that consumes it. No name is invented here:
# lab03-app-connection is <cluster>-connection, the default.
env:
  - name: RAVENDB_URLS       # endpoints are not secret, and the client wants the whole list
    valueFrom:
      secretKeyRef: {name: lab03-app-connection, key: urls}
volumeMounts:
  - {name: ravendb, mountPath: /etc/ravendb, readOnly: true}
volumes:
  - name: ravendb
    secret:
      secretName: lab03-app-connection
      defaultMode: 0400
      items:                 # mount only the material you need
        - {key: tls.crt,    path: tls.crt}
        - {key: tls.key,    path: tls.key}
        - {key: client.pem, path: client.pem}
```

The lab is about the Secret and not about how the cluster got there, so it seeds a three node
`PB10` through the lab control plane, and a `CloudCluster` by the same name takes it over. Lab 04
covers taking over properly. A cluster the operator created publishes exactly the same Secret
through exactly the same code: `publishConnectionSecret` runs before the drift logic and does
not look at `managementPolicy`, which is why a found cluster, left on the `Observe` it defaults
to, still gets one. Writing a Secret in this namespace changes nothing in RavenDB Cloud.

## The eleven keys

Once the cluster is Active the operator writes `<cluster>-connection`, type `Opaque`, labelled
`cloud.ravendb.net/cluster=<cluster>`, and reports it in `status.connectionSecretName`. `STATUS`
is `Running` only once the Secret is written, because that is the moment an application can
connect, and the `Ready` message says which Secret holds what:

```
NAME        STATUS    CHANGES   TIER         REGION         NODES   AGE
lab03-app   Running   Applied   Production   eu-central-1   3       2s

The cluster is Active with 3 node(s), and Secret "lab03-app-connection" holds its URLs and client certificate.
```

The keys:

| key | what it is |
|---|---|
| `url` | first node endpoint, for anything that takes a single address |
| `urls` | every node endpoint, comma separated, in node-tag order |
| `nodeTags` | RavenDB node tags, aligned index for index with `urls` |
| `studioURL` | RavenDB Studio address, `url` plus `/studio/index.html` |
| `productID` | RavenDB Cloud cluster id, the handle the portal and support use |
| `tls.crt` | client certificate, PEM |
| `tls.key` | client private key, PEM |
| `client.pem` | certificate and key concatenated, for clients that want one file |
| `client.pfx` | PKCS#12 with no password, for .NET clients that load a store |
| `client.with-password.pfx` | the same PKCS#12, encrypted |
| `certificate.password` | the password for `client.with-password.pfx`, whitespace trimmed |

The names are deliberately dull. `tls.crt` and `tls.key` are what every other Kubernetes
workload already expects, so an existing chart or sidecar that knows how to mount a TLS Secret
needs no special case for this one.

The script prints what those keys hold:

```
    url        https://a.lab03-app.acme.ravendb.cloud
    urls       https://a.lab03-app.acme.ravendb.cloud,https://b.lab03-app.acme.ravendb.cloud,https://c.lab03-app.acme.ravendb.cloud
    nodeTags   A,B,C
    studioURL  https://a.lab03-app.acme.ravendb.cloud/studio/index.html
    productID  4-A
    password   lab-pfx-password
```

`urls` is what a RavenDB client constructor wants: the whole list, so it can fail over. `url` is
the first entry, for the many things that only accept one address. `nodeTags` lines up with
`urls` position by position, so `B` is the second URL. The operator rewrites all of these on
every poll, so a cluster that gains a node has a current Secret within one poll interval. The
`productID` value depends on how many clusters the lab control plane has created before you, so
yours will differ.

There is a twelfth key you should never see: `certificate.bundle`. The operator writes it, and
reports `STATUS UnexpectedResponse` instead of `Running`, when the archive from RavenDB Cloud
could not be unpacked. The endpoints and the raw bundle are still published, because a payload
you have to unzip yourself beats no payload at all. The lab checks that the key is absent.

## Why one ZIP becomes eleven keys

RavenDB Cloud does not serve a certificate. It serves a download: a ZIP holding a PFX pair, a
`PEM/` directory, a `PASSWORD.TXT` and a README. The filenames inside carry the subdomain and
a date stamp, which is fine for a human clicking a button in the portal and useless to a pod.

So `internal/controller/connection_secret.go` unpacks it and maps entries onto fixed names by
suffix and directory rather than by exact filename:

```
PEM/<subdomain>.client.certificate.crt                    -> tls.crt
PEM/<subdomain>.client.certificate.key                    -> tls.key
PEM/<subdomain>.client.certificate.pem                    -> client.pem
<subdomain>.client.certificate.<date>.pfx                 -> client.pfx
<subdomain>.client.certificate.<date>.with.password.pfx   -> client.with-password.pfx
PASSWORD.TXT                                              -> certificate.password
```

That mapping is the whole reason to run the operator for connectivity. Without it somebody
downloads a ZIP from the portal, unpacks it, works out which of the five certificate files
their client wants, creates a Secret by hand, and repeats all of it in every namespace and
every cluster. With it, a pod mounts one Secret.

The extraction is bounded on purpose: 1 MiB per entry and 16 MiB for the archive, with the
entry read through a `LimitReader` so a zip header that lies about its size cannot make the
operator allocate on trust.

## Proving the material is real

The script pulls `tls.crt` out of the Secret and hands it to `openssl`:

```
    subject=O=RavenDB Cloud lab mock, CN=lab03-app.acme.ravendb.cloud.client.certificate
    notAfter=Sep  1 13:44:02 2027 GMT
    serial=11A65AE3129C7B3C9D3376D26E3B68AC4775D213
    client.pem holds 2 PEM blocks
```

The common name is `<subdomain>.<domain>.client.certificate`, which is how a RavenDB client
certificate is named. `client.pem` really is both blocks in one file: the script greps for
`-----BEGIN CERTIFICATE-----` and `-----BEGIN RSA PRIVATE KEY-----` separately. `tls.key`
passes `openssl rsa -check`. Both PKCS#12 forms are present, they are different bytes, and
`client.pfx` starts with a DER `SEQUENCE` tag.

The lab control plane generates this certificate for real rather than serving a fixture, so
none of this is a canned string. Nothing trusts the certificate, of course. It exists to prove
the path from the cloud API to a file inside a container.

## Mounting it from a pod

The script also runs a Job that mounts the Secret and reads it from inside the cluster:

```
    urls      : https://a.lab03-app.acme.ravendb.cloud,https://b.lab03-app.acme.ravendb.cloud,https://c.lab03-app.acme.ravendb.cloud
    nodeTags  : A,B,C
    mounted   : client.pem tls.crt tls.key
    subject   : lab03-app.acme.ravendb.cloud.client.certificate
    pem blocks: 2
```

Endpoints arrive as environment variables through `secretKeyRef` and the certificate arrives
as files through a volume, which is the split you want in a real Deployment: URLs are config
and get logged, private keys are not and should never be in an env var.

That split has a consequence worth knowing before you copy it. A `secretKeyRef` env var is
resolved once, when the kubelet starts the container, and never again: a pod holding `urls` in
the environment keeps the list it booted with even after the operator has rewritten the Secret.
A mounted volume is different, because the kubelet refreshes projected Secret files in place
(within its sync period, about a minute by default). So if your application has to notice a node
being added without a restart, read `urls` from a mounted file, or watch the Secret, or accept
that a rollout is what picks up the change. This lab's Job is short lived, so it does not care.

BusyBox has no `openssl`, so the container gets the subject the hard way: strip the PEM
banners, `base64 -d` the body, turn every non-printable byte into a newline, and grab the
token ending in `.client.certificate`. Ugly, and it makes the point that this is genuine DER
sitting in the container's filesystem.

That step is the only part of the lab with an external dependency. The script tries to
`docker pull busybox:1.36` and `kind load` it into the node first, so the kubelet needs no
registry. If neither the local Docker nor the registry can produce the image the Job never
completes; the script says so, prints the tail of `kubectl describe job`, and skips those five
checks. Everything else in the lab still runs. Set `LAB03_IMAGE` to use a different image that
has a shell.

## Why the certificate is downloaded once and not on every poll

The operator paces its calls, so every avoidable call is a turn taken from some other
cluster's reconcile, and the operator asks a question before downloading:

```go
// certificateNeeded reports whether the certificate has to be downloaded.
func certificateNeeded(existing *corev1.Secret) bool {
    if existing == nil {
        return true
    }
    stored := existing.Data[SecretKeyTLSCert]
    if len(stored) > 0 {
        return expiringSoon(stored)
    }
    return len(existing.Data[SecretKeyRawBundle]) == 0
}
```

Missing Secret, or a Secret with no certificate in it: download. A stored certificate close to
expiry: download its replacement. Otherwise skip. The two halves of the Secret are written by
separate field managers, so a poll that refreshed only the URLs leaves the certificate keys it
did not fetch untouched.

The lab measures this against the control plane's request log. One download gets the cluster to
`Running`, and it stays at one while the operator keeps polling every 5 seconds:

```
    so far for 4-A: 5 detail polls, 1 certificate downloads
    waiting for two more polls at the 5s pollInterval
    detail polls are now 7 (was 5)
    PASS certificate downloads after those polls = 1
```

Delete the Secret and the count moves. The controller is wired with `Owns(&corev1.Secret{})`,
so the delete event enqueues the owning CloudCluster immediately instead of waiting out the
poll interval, the reconcile finds nothing at `<cluster>-connection`, and it fetches again. The
script proves the fetch was real rather than replayed from memory by comparing serial numbers:
the lab control plane mints a fresh certificate per download, so the subject stays the same and
the serial changes.

## ownerReferences, and what garbage collection buys you

The operator sets itself as controller reference on the Secret:

```
    ownerReferences[0]:
      apiVersion         cloud.ravendb.net/v1alpha1
      kind               CloudCluster
      name               lab03-app
      controller         true
      blockOwnerDeletion true
```

Two consequences worth understanding. First, cleanup is not the operator's job: delete the
CloudCluster and Kubernetes garbage collects the Secret, including any Secret the cluster
published under an older name. No finalizer, no reconcile, no chance of leaking credentials in
a namespace after the cluster object is gone. The lab deletes the cluster at the end and waits
for all three Secrets it accumulated to disappear. The cloud cluster itself survives, because
`deletionPolicy` defaults to `Retain`, so the script removes that through the lab control
plane instead.

Second, `controller: true` means the Secret has exactly one controlling owner, and a second
CloudCluster cannot take it over. The lab proves that with a second cluster seeded in the
control plane, `lab03-squatter`, and an object that takes it over while aiming its Secret at
the name `lab03-app` already uses:

```
    The cluster is Active, but Secret "lab03-app-connection" already exists and was not created
    for this cluster. Set spec.connectionSecret.name to a name nothing else owns, or delete that Secret.
    PASS lab03-squatter Ready = False
    PASS lab03-squatter STATUS = NameTaken
    PASS its own cluster is bound all the same = 5-A
    PASS the Secret still belongs to lab03-app = lab03-app
    PASS and was not overwritten = certificate.password client.pem ... urls
    PASS the url in it is still lab03-app's = https://a.lab03-app.acme.ravendb.cloud
    PASS the conflict cost no certificate download = 0
```

The operator refuses before anything is written, so the Secret is untouched. Two objects
fighting over one Secret is a configuration mistake, and it shows up on the object that made it
rather than as two controllers taking turns rewriting a pod's credentials. The cluster behind it
is fine and bound; what is taken is the name, so the reason is `NameTaken`, the same word the
operator uses for a cluster name that belongs to somebody else. The message names the field to
change.

A name that belongs to something else will still belong to it on the next pass, so this is not
retried in a loop. It sets the condition, emits one Warning, and goes back to the ordinary poll
interval, which is enough to notice if somebody frees the name. Handing it to controller-runtime
instead would requeue on exponential backoff, and every retry would be another
`products/details/<id>` call, forever, for a mistake no
retry can fix.

The step needs a second cluster, which the stand-in seeds for free, so against a real account it
is skipped and counted. Aimed at `lab03-app`'s own cluster instead, the second object would be
refused one step earlier, for managing a cluster another object already manages, and never reach
the Secret at all; lab 04 shows that refusal.

The refusal covers a case `SetControllerReference` does not. That function only rejects a Secret
some *other controller* already owns, which is this case. A Secret created by hand has no
controller reference at all, so without the explicit check the operator would have adopted it,
merged the cluster's keys in, and garbage collected it on delete. That would have made "may
create a CloudCluster" mean "may delete any Secret in this namespace".

## Renaming the Secret, and turning the certificate off

Three fields under `spec.connectionSecret` change what gets written, and the lab exercises all
three in order.

**`name`** points publication somewhere else, which is what you want when a chart already
expects a particular Secret name:

```bash
kubectl -n apps patch cloudcluster lab03-app --type=merge \
  -p '{"spec":{"connectionSecret":{"name":"lab03-app-endpoints"}}}'
```

The new Secret gets the same eleven keys and the same owner. The old Secret is emptied and
kept: not deleted, because it is still owned and the owner reference is what collects it with
the CloudCluster, but not left holding the credential either. It used to be left exactly as it
was, which put a copy of `tls.key` and `certificate.password` at an address the spec no longer
mentioned, for as long as the cluster lived. A pod still mounting the old Secret now reads
empty files, which is a visible failure rather than a silent second copy of the key.

Because the new Secret starts out with no certificate in it, the operator downloads one. A
rename is the only routine operation that costs a second certificate call, which the lab
verifies by watching the count go up by exactly one.

**`includeCertificate: false`** stops the download entirely. The Secret then carries five keys,
`url`, `urls`, `nodeTags`, `studioURL` and `productID`, and no certificate material. Use it
when certificates reach your pods through cert-manager, a CSI driver, or anything else that
already owns that problem. `STATUS` stays `Running`, because the URLs are everything an
application asked this Secret for, and the message says what is in it:

```
The cluster is Active with 3 node(s), and Secret "lab03-app-urls-only" holds its URLs.
includeCertificate is false, so the client certificate is not in it.
```

Flipping it to false on a Secret that already holds a certificate **removes** the certificate,
private key included, and leaves the endpoints in place. That is worth stating because it was
not always true: the write path used to merge keys in and never take any out, so the private
key stayed published after the user asked for it to stop, and this guide used to tell you to
rename the Secret as a workaround. The lab now checks the in-place case, which is the one a
rename hides.

What makes it work is that the operator writes the Secret with server-side apply under two
field managers, one for the endpoints and one for the certificate. Fields a manager stops
sending are pruned, so the certificate manager applying nothing is what deletes the certificate
keys, and the endpoint keys are untouched because they belong to somebody else. The split is
needed rather than tidy: the certificate is downloaded once and most reconciles have no opinion
about it, so a single manager owning both halves would delete the certificate on every
endpoints-only pass.

**`enabled: false`** stops publication and takes back what was published.
`status.connectionSecretName` is cleared, `STATUS` stays `Running`, since publishing is off on
purpose rather than failing, and the Secret it names is emptied: both field managers apply
nothing, so every key they own is pruned. The message says so, because `Running` alone would
suggest there is a Secret to mount:

```
The cluster is Active with 3 node(s). spec.connectionSecret.enabled is false, so Secret
"lab03-app-urls-only" holds none of this operator's keys.
``` The Secret object stays, because the operator holds no delete on
Secrets and the owner reference is what collects it with the CloudCluster; anything still
mounting it reads empty files.

This lab used to assert the opposite, on the reasoning that turning publication off stops
writing rather than retracting. Measured against the real API, what stopped was the writing and
what stayed was `tls.key` and `certificate.password`, in a namespace whose owner had just asked
for them to stop being published.

Turning publication off and renaming the Secret are the same situation reached two different
ways, so one function handles both: `retirePreviousSecret` asks whether the Secret the operator
last published into, recorded in `status.connectionSecretName`, is still the one it publishes
into. The spec cannot answer that, because the name to clean up is the one the spec no longer
says. By the end of this lab every Secret the cluster owns is empty.

## Takeaway

The connection Secret is the operator's entire output as far as an application is concerned.
RavenDB Cloud hands out a ZIP built for a human with a browser; the operator unpacks it once,
per cluster, into eleven conventional Kubernetes keys, keeps the endpoint keys current on every
poll, and never re-downloads the certificate it already has. The Secret is owned by the
CloudCluster, so its lifetime is the cluster's lifetime and nothing has to remember to clean it
up. Point a pod at `urls` and `tls.crt` and you are done: no portal download, no copied PFX, no
redeploy when the cluster changes shape.

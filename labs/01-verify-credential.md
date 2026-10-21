# Lab 01: verify a RavenDB Cloud credential

> **Reference:** [Accounts](../docs/how-to-use/accounts.md) covers the same ground as a guide rather than a walkthrough.

**For:** anyone about to point this operator at a RavenDB Cloud account for the first time.
A `CloudAccount` is the single place an API key lives, and everything else in the operator
depends on it being good. By the end you will know what `Ready=True` actually proves, what the
operator caches in status and why, and what each of the three ways a credential can break looks
like in `kubectl describe`, including a key rotation that takes effect in seconds.

## Run it

```bash
labs/01_verify_credential.sh
```

The complete example is [`01_verify_credential.sh`](01_verify_credential.sh). The core is two
objects:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: lab01-api-key
type: Opaque
stringData:
  apiKey: lab-api-key          # in production, a key from Account -> API Keys in the portal
---
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: lab01-account
spec:
  apiKeySecretRef:
    name: lab01-api-key
    key: apiKey                # defaults to apiKey, spelled out here for the failure case below
  endpoint: http://mockcloud.ravendb-cloud-lab.svc.cluster.local
  defaults:                    # inherited by every CloudCluster that references this account
    provider: AWS
    region: eu-central-1
    allowedIPs: ["0.0.0.0/0"]
  refreshInterval: 1h   # the documented default
```

That is the whole surface. The `CloudAccount` never contains the key, only a reference to the
Secret that does. Outside the labs, leave the `endpoint` line out and put a real key in the
Secret, and the same two objects work against RavenDB Cloud.

Within a few seconds:

```
NAME            STATUS     DOMAIN               AGE
lab01-account   Verified   acme.ravendb.cloud   4s
```

## Why Ready is a claim the operator can back

`Ready=True` on a `CloudAccount` is not "the YAML parsed" and not "a Secret with that name
exists". The controller resolves the Secret, builds an HTTP client with the key in the
`X-Api-Key` header, and calls `GET /api/v1/account/info`. Only a 2xx answer sets the
condition, and the domain in its message is read out of the response body:

```
Ready reason:  Verified
Ready message: RavenDB Cloud accepted the API key for account acme.ravendb.cloud.
```

`STATUS` in `kubectl get` is that reason, word for word. The same is true of every kind this
operator defines, so the one-word answer in the table and the reason an alert rule matches on
are never two different vocabularies.

The lab proves the call happened rather than taking the condition's word for it. The lab control
plane keeps a request log, and the script counts `/api/v1/account/info` entries before and after
applying the account:

```
==> Ready is not a guess: the operator called GET /api/v1/account/info
    account/info calls in the control plane log: 3 -> 4
    PASS account/info was called while the account was verifying = yes
    PASS the caller identifies itself mentions 'ravendb-cloud-operator'
    PASS status.domainName (only the API can supply it) is set
```

`status.domainName` is the second witness: it is `acme.ravendb.cloud` in the lab and the DNS
domain of your account in production, and there is nowhere for it to come from except the
response body.

A call count cannot say which account made the calls, though, and the lab shares its control
plane with the account from lab 00. The third witness closes that gap, further down: when the
key is deliberately broken, the control plane logs a `401` on `/api/v1/account/info`. No other
credential in the cluster is bad, so that `401` has exactly one possible author.

The key travels one way, from the Secret into a request header. It is never copied into the
`CloudAccount`, into status, into events or into the logs, and the operator's client cache is
indexed by a SHA-256 of endpoint plus key rather than by the key itself. It is not a secret
store: the live HTTP client holds the value in memory, because it has to send it. The script
greps the whole `CloudAccount` object for the key value and asserts it is not there.

## What the cached lists buy you

Once the credential is accepted, the controller reads the region and release channel lists and mirrors them into
status:

```
==> What this account may order, mirrored into status
    providers:        AWS Azure GCP
    AWS regions:      eu-central-1 eu-west-1 us-east-1 us-west-2 ap-southeast-1
    Azure regions:    westeurope northeurope eastus
    release channels: Stable72 Stable71 Stable62
    default channel:  Stable72
```

This is `status.providers`, `status.releaseChannels` and `status.defaultReleaseChannel`. Copying a
remote list into Kubernetes looks like duplication, and it earns its place twice.

It answers "what can I order here" without a browser. `kubectl describe cloudaccount` tells you
the regions this account may use, per provider, and which RavenDB version streams are on offer.
That question comes up every time someone writes a new `CloudCluster`, and the portal is a bad
place to answer it from a terminal or a CI job.

It also turns a class of runtime failure into a validation failure. When a `CloudCluster`
provisions, the operator checks the requested region against `status.providers` for that provider
before it sends anything, so `eu-central-1` under `provider: Azure` fails locally with the valid
list attached instead of arriving as an opaque `400` from the control plane. The lab checks that
`eu-central-1` is in the AWS list and is not in the Azure list, which is exactly the distinction
that check relies on.

## Two timestamps, because they cost different amounts

Status carries `lastVerifiedTime` and `refreshedAt`, and they are separate fields because
the work behind them is not the same size. Verifying the credential is one call. Refreshing the
refresh is four: one for the release channels, one for the regions of each of the three
providers. The operator paces its calls, so it does not spend four of them every time something
pokes an account.

Verification happens on every reconcile, including the ones a Secret change triggers. The
lists are re-read only when `status.providers` is empty or older than `refreshInterval`
(default `1h`). That same interval is also the requeue a successful reconcile returns, so on a
quiet account the two coincide: nothing else wakes it, and the once-an-hour wake-up both
re-verifies the key and re-reads the lists. Every wake-up in between, from a Secret write
or a spec edit, re-verifies and leaves the lists alone.

The lab makes the two move independently: it records both timestamps, breaks and repairs the
key, and then checks that `lastVerifiedTime` advanced while `refreshedAt` did not.

```
==> Only one of the two timestamps moved
    lastVerifiedTime   2026-09-01T09:14:02Z -> 2026-09-01T09:14:41Z
    refreshedAt 2026-09-01T09:14:02Z -> 2026-09-01T09:14:02Z
    PASS refreshedAt is unchanged = 2026-09-01T09:14:02Z
    PASS lastVerifiedTime advanced = yes
```

A refresh that fails is logged and dropped, not promoted to a condition. The previous
values stay in status and the account stays usable, because a stale region list is a much smaller
problem than an account that reports itself broken.

## Defaults, inherited

`spec.defaults` is not decoration. When a `CloudCluster` provisions, the operator fills any of
`provider`, `region`, `releaseChannel` and `networking.allowedIPs` that the cluster did not set
from the account, and only then errors if the field is still empty. That is why a cluster manifest
in the later labs can be five lines: the account already said AWS, `eu-central-1`, and which
addresses may connect.

`releaseChannel` has one more fallback: if neither the cluster nor the account names one, the
operator uses `status.defaultReleaseChannel`, the channel RavenDB Cloud itself picks. So the
account can stay silent about versions and still produce a valid order.

## Three ways a credential fails

All three land on `Ready=False`, and the lab runs each one. The operator's answer is the same in
all three cases, stop and wait for a Secret to change, but the reason is not, because the reason
says where the failure was found. The first two never left the cluster: the key is not there to
send, so the reason is `KeyMissing`, and the message names the Secret and the key it looked for.
The third reached RavenDB Cloud and was turned away, so the reason is `KeyRejected`, and the
message says what RavenDB Cloud answered and which Secret the rejected key came from.

`KeyMissing` records no event and `KeyRejected` records a Warning. A Secret applied a moment after
its account is the normal order of things, and warning about it would teach people to ignore the
warning that matters.

**The Secret does not exist yet.** Common, and usually temporary: a `CloudAccount` and its Secret
are applied together and one of them wins the race.

```
==> Failure 1: the Secret has not been created yet
    message: Secret "lab01-late-key" not found in namespace "apps".
    PASS Ready reason = KeyMissing
    PASS no domain was reported =
```

Nothing needs to be done about it. The controller requeues after 30 seconds and, more to the
point, it watches Secrets, so creating the missing Secret wakes the account at once:

```
    recovered in 2s, against a 30s blind requeue for a missing Secret
    PASS Ready after the Secret appeared = True
    PASS recovery beat the 30s requeue = yes
```

Two seconds is the lab script's own polling granularity, not a measurement of the controller. It
is also not proof of anything: a 30 second requeue that happened to be due would look similar
enough. The rotation case below is where the watch is actually demonstrated, because there the
fallback is 300 seconds and no requeue can be mistaken for it.

**The Secret exists but the key inside it does not.** A typo in `apiKeySecretRef.key`, or a Secret
built by a tool that named the entry something else. The message names both:

```
==> Failure 2: the Secret exists but the key inside it does not
    message: Secret "lab01-api-key" has no key "token".
    PASS Ready reason = KeyMissing
```

The failure is per `CloudAccount`, not per Secret. This second account points at the Secret the
first one is already using happily, and the lab checks that the first stays `Ready` throughout.
A misspelled key name breaks the object that misspelled it, and nothing else.

**RavenDB Cloud rejects the key.** Here the operator did reach the control plane and got a 401,
so the message says so, and says where the key it sent lives:

```
==> Failure 3: RavenDB Cloud rejects the key
    message: RavenDB Cloud answered 401 Unauthorized: it does not accept the API key in Secret
    apps/lab01-api-key, key "apiKey". The key is mistyped, deactivated or past its expiry date.
    Create a key in the RavenDB Cloud portal and put it in that Secret; the account verifies it
    again as soon as the Secret changes.
    PASS Ready reason = KeyRejected
    PASS the rejected key value is not echoed into status = no
    401 answers to account/info in the control plane log: 0 -> 1
    PASS the rejection is a real HTTP answer, not a local guess = yes
```

There is no server message in it, and that is not the lab cutting a corner. RavenDB Cloud
answers a rejected key with a bare, bodiless 401, and the control plane here copies that on
purpose, so the reason text the operator publishes reads the same in a lab as in production.
The message used to quote the request line instead, `cloudapi: GET /api/v1/account/info: 401
Unauthorized`, which told a reader which call failed and not what to do about it. The request
line is still in the operator's log at debug level (`-zap-log-level=debug`), one line per call,
which is where it is useful. At the default level the log has one line per condition change
instead, naming the condition and the reason it now gives.

That last pair is the check the whole "Ready is not a guess" claim rests on. Everything up to
here could in principle have been produced by an operator that never opened a socket. A 401
recorded by the control plane could not, and it can be attributed: this is the only bad
credential in the cluster.

Two more details worth noticing. The rejected value never appears in the message, only the
server's description of the problem. And what is already in status is left alone: a bad key
does not erase what the operator learned while the key was good, so the account comes back with
its region lists intact.

A 401 is not retried. `IsRetryable` returns false for it, and the reconcile requeues after five
minutes instead of hammering an endpoint that will keep saying no until a human intervenes. Which
raises the obvious question.

## Rotating a key without waiting

If a rejected key is only re-checked every five minutes, does fixing it mean waiting up to five
minutes? No, and this is the part worth internalising.

The `CloudAccount` controller registers a field index on `.spec.apiKeySecretRef.name` and watches
`Secret` objects. When a Secret changes, the controller lists the accounts whose index entry
matches that Secret's name and enqueues exactly those. The five minute requeue is the fallback for
when nothing happens; the watch is what happens when something does.

So rotating a key is one `kubectl apply` on the Secret, and the account follows within a poll
interval of the lab script, not within the requeue interval of the controller:

```
==> Rotating the key back: the recovery is the point
    recovered in 4s; the blind requeue for a rejected key is 300s
    PASS Ready after rotating the key back = True
    PASS Ready reason = Verified
    PASS recovery took seconds, not the 5m requeue = yes
```

The client cache follows the same rule. The operator caches one HTTP client per `CloudAccount` so
the per-account rate limiter survives across reconciles, and the cache key is a hash of the
endpoint and the key. Change the key and the fingerprint changes, so the next reconcile builds a
new client instead of quietly reusing the one holding the old credential.

## Why credential health is not cluster health

The `CloudAccount` controller verifies a credential and caches those lists. It does nothing else,
and in particular it never looks at clusters. That split is deliberate.

Suppose one key is revoked and ten `CloudCluster` objects reference the account that holds it.
Without the split, you get ten clusters reporting cloud errors and no obvious common cause. With
it, you get one object saying `KeyRejected` and naming the Secret, and ten clusters saying
`KeyRejected` too, each quoting the account. A rejected key and an unreachable RavenDB Cloud keep
their names on every cluster that depends on the account, so the whole fleet says the same word
about the same problem; anything else wrong with the account shows on its clusters as
`WaitingForAccount`. The last lab step builds the small version of this: a
`CloudAccount` whose Secret does not exist, and one `CloudCluster` pointing at it. The cluster
says `managementPolicy: Observe`, which never creates anything, so it could not spend money even
if the account were fixed behind the lab's back.

```
==> A cluster on a broken account reports the account, not a cloud error
    cluster message: CloudAccount "lab01-broken": Secret "lab01-broken-key" not found in namespace "apps".
NAME           STATUS              CHANGES   TIER   REGION   NODES   AGE
lab01-orphan   WaitingForAccount                                     3s
    PASS cluster STATUS = WaitingForAccount
    PASS cluster Ready = False
    PASS CHANGES was never set, because nothing is bound =
    PASS no productID was resolved =
```

The cluster is `WaitingForAccount`, which records no event, because nothing has gone wrong with
the cluster: the account warns about its own problems, once. `CHANGES` was never set and no product
ID was resolved, which is the operator saying it never tried: with no usable credential there is
no point calling the API, so it does not, and it does not manufacture a cloud failure to describe
a Kubernetes one. The empty `TIER`, `REGION` and `NODES` cells are the same statement: those
columns read `status`, and nothing has been read from RavenDB Cloud yet.

The cluster message is the account's own message with a prefix, and that is worth knowing when
you read one. The `CloudCluster` controller does not watch `CloudAccount`, the way the account
controller watches Secrets. It requeues an unmet prerequisite every 20 seconds and quotes
whatever the account says at that moment, so the two objects can disagree for a few seconds after
a change: a cluster reconciled before its account had a condition reports only
`CloudAccount "lab01-broken" is not usable yet.`, and picks up the account's words on the next
pass. The lab waits for the full message rather than for the reason, which is why the check is on
the whole string. Fix the account and every waiting cluster proceeds within one of those 20 second passes,
with no intervention.

The lab cleans up everything it created and finishes by checking that the shared `ravendb-cloud`
account from lab 00 is still `Ready`, because every later lab needs it.

## Takeaway

A `CloudAccount` is a credential that has been checked, plus what that check learned. `Ready=True`
means a real call to `GET /api/v1/account/info` was answered, so an account that says it is Ready
is one you can order against. Everything expensive is cached with its own timestamp and its own
refresh clock, and everything cheap runs on every reconcile. Rotation is a Secret write, not a
restart and not a wait, because the controller watches the Secrets it depends on. And the object
is separate from your clusters so that one broken key produces one clear failure with a name
attached, instead of a fleet of confusing ones.

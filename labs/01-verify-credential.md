# Lab 01: verify a RavenDB Cloud credential

> **Reference:** [Accounts](../docs/how-to-use/accounts.md) covers the same ground as a guide.

A `CloudAccount` is the one place an API key lives. In this lab you give the operator a key,
look at what it learns from it, break it three ways, and fix it. The point to take away: an
account that says `Verified` is one RavenDB Cloud has just accepted, not one whose YAML parsed.

## Do this

A Secret with the key, and an account that names it:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: lab01-api-key
stringData:
  apiKey: lab-api-key
---
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: lab01-account
spec:
  apiKeySecretRef:
    name: lab01-api-key
    key: apiKey                  # the default, spelled out for the failure below
  defaults:
    provider: AWS
    region: eu-central-1
    allowedIPs: ["0.0.0.0/0"]
```

```bash
kubectl -n apps apply -f account.yaml
kubectl -n apps get cloudaccount lab01-account
```

## What you see

```
NAME            STATUS     DOMAIN               AGE
lab01-account   Verified   acme.ravendb.cloud   4s
```

To get there the operator read the key from the Secret, called RavenDB Cloud with it, and wrote
`Verified` only when the call was accepted. The domain is what that call answered; nothing else
could have supplied it. The key itself never leaves the Secret: it is not in the account, its
status, its events or the operator's log.

`kubectl describe cloudaccount lab01-account` also shows what the account may order, which the
script prints as:

```
providers:        AWS Azure GCP
AWS regions:      eu-central-1 eu-west-1 us-east-1 us-west-2 ap-southeast-1
release channels: Stable72 Stable71 Stable62
default channel:  Stable72
```

That is `status.providers` and `status.releaseChannels`, read once and refreshed every
`refreshIntervalSeconds`, an hour by default. It answers "which regions can I use here" from a
terminal, and it is what lets the operator refuse `region: eu-central-1` under `provider: Azure`
with the list of valid regions, before anything is sent.

`spec.defaults` is what makes cluster manifests short. A `CloudCluster` that leaves out
provider, region or allowed IPs gets them from here, and one that leaves out the release channel
gets the default RavenDB Cloud reports.

## Break it three ways

All three end in `Ready=False`. The reason says where the problem was found.

**The Secret does not exist yet.** Common, and usually over in a second: the account and its
Secret were applied together and the account won.

```
STATUS KeyMissing   Secret "lab01-late-key" not found in namespace "apps".
```

Create the Secret and the account is `Verified` within seconds. The operator watches the Secrets
accounts name, so it does not wait for a timer.

**The Secret exists, the key in it does not.** A typo in `apiKeySecretRef.key`:

```
STATUS KeyMissing   Secret "lab01-api-key" has no key "token".
```

Only the account with the typo breaks. Another account reading the same Secret under the right
key stays `Verified`.

**RavenDB Cloud rejects the key.** The lab writes a key nobody issued into the Secret:

```
STATUS KeyRejected   RavenDB Cloud answered 401 Unauthorized: it does not accept the API key in
Secret apps/lab01-api-key, key "apiKey". The key is mistyped, deactivated or past its expiry
date. Create a key in the RavenDB Cloud portal and put it in that Secret; the account verifies
it again as soon as the Secret changes.
```

This one reached RavenDB Cloud and was turned away, so the reason is different and a Warning
event records it. The rejected value is never repeated in the message, and the region lists the
account learned while the key was good stay in status.

## Rotate the key

Put the good key back:

```bash
kubectl -n apps create secret generic lab01-api-key \
  --from-literal=apiKey=lab-api-key --dry-run=client -o yaml | kubectl apply -f -
```

```
recovered in 4s
STATUS Verified
```

A rejected key is only retried every five minutes on its own, because asking again with the same
key cannot help. Changing the Secret is what brings the account back, at once. That is all a key
rotation is: one write to a Secret, no restart.

## A cluster on a broken account

Last, the lab points a `CloudCluster` at an account whose Secret does not exist:

```
NAME           STATUS              CHANGES   TIER   REGION   NODES   AGE
lab01-orphan   WaitingForAccount                                     3s

CloudAccount "lab01-broken": Secret "lab01-broken-key" not found in namespace "apps".
```

The cluster quotes the account rather than inventing a cloud error of its own, and it never calls
RavenDB Cloud. With ten clusters on one revoked key you get one account saying what is wrong and
ten clusters pointing at it.

## Run the whole lab

```bash
labs/01_verify_credential.sh
```

The script does all of the above and checks it: that the operator really called RavenDB Cloud
for each verification (the simulator logs every call), that the key appears nowhere in the
account, and that a rotation updated `lastVerifiedTime` without re-reading the region lists,
which only `refreshIntervalSeconds` does. It removes everything it created and leaves the shared
`ravendb-cloud` account from [lab 00](00-setup.md) as it found it.

## Takeaway

`Verified` means RavenDB Cloud accepted the key a moment ago. A missing or rejected key shows up
once, on the account, in words that name the Secret to fix, and fixing it is one Secret write
that takes effect in seconds.

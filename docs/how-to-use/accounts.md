# Accounts

The credential, plus the defaults its clusters inherit when the operator
creates them. It also mirrors the region and release channel lists into status,
so `kubectl describe cloudaccount` answers "what can I actually ask for here"
without a trip to the portal.

```yaml
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata: {name: ravendb-cloud, namespace: apps}
spec:
  apiKeySecretRef: {name: ravendb-cloud-api-key, key: apiKey}
  defaults:
    provider: AWS
    region: eu-central-1
    allowedIPs: ["0.0.0.0/0"]
```

Rotating the key takes effect immediately: the controller watches Secrets
through a field index and re-verifies the accounts that reference the changed
one.

## Deleting an account

An account stays, reporting `InUse`, while any CloudCluster names it, and goes
once none does: its clusters need the key to terminate.

## Who an account lets in

A RavenDB Cloud API key reaches every cluster in its account; RavenDB Cloud has
no narrower one. Whoever may create a CloudCluster in the namespace of a
CloudAccount can therefore name any cluster in that account, and the operator
takes it over and publishes its client certificate into that namespace. Grant
that right only to people you would give the key itself.

# Lab 00: set up the lab cluster

> **Reference:** [Installing](../docs/how-to-use/install.md) covers installing the operator on
> a cluster of your own.

Every other lab starts from what this one builds: a kind cluster with the operator in it, the
simulator standing in for RavenDB Cloud, and one verified `CloudAccount` the labs share.

## Do this

```bash
labs/00_setup.sh
```

The script does five things, in this order:

1. creates a kind cluster called `ravendb-lab`, or reuses it if it is already there;
2. builds two images from this checkout, the operator and the simulator, and loads them into
   the cluster;
3. installs the three CRDs: `CloudAccount`, `CloudCluster` and `CloudClusterRestart`;
4. deploys the simulator in the `ravendb-cloud-lab` namespace and the operator in
   `ravendb-cloud-operator-system`;
5. creates the `apps` namespace with a Secret holding an API key and a `CloudAccount` that
   points at it, and waits until the operator has verified the key.

The first run takes a couple of minutes, mostly the image builds. A second run rebuilds the
images and restarts both deployments, so the labs always test the code in your checkout.

## What you see

```bash
kubectl -n apps get cloudaccount
```

```
NAME            STATUS     DOMAIN               AGE
ravendb-cloud   Verified   acme.ravendb.cloud   40s
```

That object is the shared account every lab names in `spec.accountRef`. Its manifest is
[`manifests/account.yaml`](manifests/account.yaml), and it is two objects:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: ravendb-cloud-api-key
  namespace: apps
stringData:
  apiKey: lab-api-key            # the key the simulator accepts
---
apiVersion: cloud.ravendb.net/v1alpha1
kind: CloudAccount
metadata:
  name: ravendb-cloud
  namespace: apps
spec:
  apiKeySecretRef:
    name: ravendb-cloud-api-key
    key: apiKey
  endpoint: http://mockcloud.ravendb-cloud-lab.svc.cluster.local
  defaults:                      # what a CloudCluster gets when it does not say
    provider: AWS
    region: eu-central-1
    allowedIPs:
      - 0.0.0.0/0
  refreshIntervalSeconds: 120
```

`spec.endpoint` is the one line that makes this the lab: it sends the account's calls to the
simulator. Against RavenDB Cloud you leave it out and put a real key, created in the portal under
Account, API Keys, in the Secret. Everything else is what you would write for real.

`STATUS Verified` is not a formality. The operator read the key from the Secret, called RavenDB
Cloud (here, the simulator) with it, and only wrote `Verified` once the call was accepted. The
`DOMAIN` column is the account domain that call returned. [Lab 01](01-verify-credential.md) looks
at that more closely.

`allowedIPs: 0.0.0.0/0` means any address may connect to a cluster created from this account.
That is fine in a lab, where the simulator has no firewall and nothing connects anyway, and it is
the first thing to narrow on a real account.

## When you are done

```bash
labs/00_setup.sh --teardown
```

deletes the kind cluster and everything in it. Already have a cluster you would rather use?
`labs/00_setup.sh --no-cluster` installs into whichever cluster `kubectl` points at.

## Takeaway

The operator needs two things from you: its CRDs installed, and a `CloudAccount` naming a Secret
with an API key. Everything the later labs do starts from `accountRef: ravendb-cloud`.

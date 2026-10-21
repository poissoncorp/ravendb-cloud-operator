# Accounts and credentials

What a CloudAccount does and how clusters use it. Read before changing
`internal/controller/cloudaccount_controller.go` or `internal/cloudclients`.

## Verifying the key

A CloudAccount names a Secret and a key in it holding a RavenDB Cloud API key.
Ready is `Verified` once `GetAccountInfo` answers, and `status.domainName` is
the account it answered for. A missing Secret or key is `KeyMissing`, looked at
again every 30 seconds and warned about once it has lasted a minute, since it
usually lands moments after the account. A rejected key is `KeyRejected`,
retried every five minutes: only a person can fix it.

Every pass verifies the key, and a settled account passes once every
`spec.refreshIntervalSeconds`, an hour by default. So a key revoked in the
portal shows on the account up to that late; a cluster that calls with it first
reports `KeyRejected` on its own Ready.

The Secret is watched as metadata only, so its contents never reach the
operator through a watch. Updating it wakes the account at once, which is how a
rotated key takes effect.

## What the account mirrors

Regions and release channels are mirrored into `status` and refreshed every
`spec.refreshIntervalSeconds`, so they can be that stale. A refresh that fails
keeps the lists it had and is only logged: an account with lists to fall back on
stays usable, and one with none tries again in a minute. When the spec or the
verified domain changed since the last refresh (`catalogueMoved`), the lists are
dropped first, since lists from another account are worse than none. Instance
types and their disk offers are not mirrored: the create path reads them live.
`spec.defaults` (provider, region, allowed IPs, release channel) fill in what a
CloudCluster leaves out, at creation.

## One client per account, one pace for all

`cloudclients.Cache` builds one `cloudapi.Client` per account and rebuilds it
when the key changes. All of them share one pace. Deleting an account drops its
client, so the key stops living in memory.

## Reading an account from a cluster

`usableAccount` is the one way a cluster or restart reads its account: the
account, its client, or why it cannot be used yet, in the account's own words.
It waits for the account to be Ready. A cluster waits as `WaitingForAccount`, or
with the account's own reason when that says more (`KeyRejected`,
`CloudUnreachable`). A cluster being deleted does not wait: see
[deletion.md](deletion.md).

A cluster records the account it was bound in, as `status.accountDomain`. If the
account later verifies a key for another account (its `status.domainName`
differs), every call would ask an account that has no such cluster: a read would
report it deleted, and a terminate would end nothing. So the cluster does
nothing (`inAnotherAccount`): Ready says `WaitingForAccount` and names both
accounts until the keys match again.

## Deleting an account

The account holds its own finalizer while any CloudCluster names it, as
`InUse`, and lets go once none does. Clusters need the key to terminate, so an
account deleted first would strand them. `InUse` keeps Ready's status as the
last verification left it, since being deleted does not change whether the key
works.

## The key is the boundary

An API key reaches every cluster in its account; RavenDB Cloud has no narrower
key. Whoever may create a CloudCluster next to a CloudAccount may therefore name
any cluster in that account. Namespaces and RBAC are the boundary: put an
account only where everyone who can write CloudClusters may manage every cluster
in it.

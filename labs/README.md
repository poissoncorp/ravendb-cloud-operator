# Labs

Short walkthroughs of the RavenDB Cloud operator on a cluster on your laptop. Each one has a
guide to read and a script that does every step and checks what it shows.

**In a hurry? Run [00](00-setup.md), then [03](03-connect-an-application.md).** That is the
shortest way to see what the operator is for: a Secret a pod can mount, with the cluster's
addresses and a client certificate in it. It takes about two minutes.

**They cost nothing and need no RavenDB Cloud account.** The labs run against the simulator, a
small HTTP server that answers the RavenDB Cloud API the way RavenDB Cloud does and runs in the
same kind cluster. The operator talks to it exactly as it talks to RavenDB Cloud. The same
scripts are also run against RavenDB Cloud itself, on small Development clusters, because a
simulator cannot catch the case where it and the operator read the API the same wrong way.

## What you need

`docker`, `kubectl`, `bash`, `curl` and [`kind`](https://kind.sigs.k8s.io/docs/user/quick-start/#installation).
No Go toolchain and no `jq`. Lab 03 also needs `openssl`. If `kind` is not on your `PATH`, put
the binary at `bin/kind`.

## The labs

| Lab | What you do | What you take away |
|-----|-------------|--------------------|
| [00](00-setup.md) | Start a kind cluster with the operator and the simulator | the shared account every lab uses |
| [01](01-verify-credential.md) | Give the operator an API key, break it three ways, rotate it | Ready on an account means RavenDB Cloud accepted the key |
| [02](02-provision-a-cluster.md) | Create a cluster from three lines of YAML | the operator fills in the rest and writes it to status, never to your spec |
| [03](03-connect-an-application.md) | Mount the connection Secret in a pod | an application needs one Secret and nothing from the portal |
| [04](04-take-over-an-existing-cluster.md) | Point a manifest at a cluster that already exists | a name finds a cluster, and nothing changes until you say so |
| [05](05-day-two-operations.md) | Resize, rescale and restart a running cluster | one change at a time, and the refusals cost nothing |
| [06](06-delete-safely.md) | Delete clusters and accounts | `kubectl delete` removes a database only when the manifest says Terminate |

The numbers are an order: create, use, take over, change, delete. Each lab cleans up after
itself, so you can also run one on its own once 00 has run.

[docs/how-to-use](../docs/how-to-use/) covers the same ground as reference guides, one per
topic, each pointing back at its lab.

## Running them

```bash
labs/00_setup.sh
```

Creates a kind cluster called `ravendb-lab`, builds the operator and the simulator from this
checkout, installs the CRDs, and waits until the shared account is verified. A couple of minutes
the first time. `labs/00_setup.sh --no-cluster` uses the cluster `kubectl` already points at
instead, and `labs/00_setup.sh --teardown` deletes the kind cluster when you are done.

Then any lab, or all of them:

```bash
labs/03_connect_an_application.sh
labs/run_all.sh
```

Each script prints what it does, the `kubectl` output worth seeing, and a line per check. It
ends with a tally and exits non-zero if a check failed.

## What the simulator can do that RavenDB Cloud will not

Some situations are worth seeing and impossible to arrange on purpose against a real service: a
cluster terminated behind the operator's back, two clusters sharing a name, an interrupted
create. The simulator has a small `/_lab` surface for exactly those, kept apart from the API it
simulates, and the guides say whenever a step uses it. Against a real account those steps are
skipped and counted.

## If a lab fails

```bash
kubectl -n apps describe cloudcluster <name>
kubectl -n ravendb-cloud-operator-system logs deploy/ravendb-cloud-operator-controller-manager --tail=50
```

`describe` is usually enough: the operator writes every refusal into a condition with a message
that says what to do, and the reason is the word in the `STATUS` or `CHANGES` column of
`kubectl get cloudcluster`.

If a lab stops at "could not reach the simulator through the port-forward", another program is
listening on that lab's port. `LAB_PORT_BASE` moves the whole range.

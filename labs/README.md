# ravendb-cloud-operator: labs

Runnable, self-checking guides for the RavenDB Cloud operator. Each lab ships a script next to
it, so you can run the exact code the guide shows and watch it assert its own claims.

**They cost nothing and need no RavenDB Cloud account.** By default the labs run against
[`mockcloud`](mockcloud/), a real HTTP server that implements the documented RavenDB Cloud API.
The operator talks to it over the same HTTP API it uses with RavenDB Cloud.

**Every lab has also passed against a real RavenDB Cloud environment**, which is a different
claim. See [What has run against RavenDB Cloud](#what-has-run-against-ravendb-cloud).

## Prerequisites

`docker`, `kubectl`, `bash`, `curl`, and [`kind`](https://kind.sigs.k8s.io/docs/user/quick-start/#installation).
No Go toolchain and no `jq` are required. If `kind` is not on your `PATH`, drop the binary at
`bin/kind` and the setup script will use it.

Already have a cluster you want to use instead? `labs/00_setup.sh --no-cluster`.

## Start here

```bash
labs/00_setup.sh
```

That creates a kind cluster, builds and loads the operator and the lab control plane, installs the
CRDs, and waits until the credential verifies. It takes a couple of minutes the first time. Then
work through the labs in order.

Finished? `labs/00_setup.sh --teardown`.

## The labs

| Lab | Covers | Read it if | Cloud API |
|-----|--------|-----------|-----------|
| [00](00_setup.sh) | Set up the cluster, the operator and the lab control plane | everyone, first | - |
| [01](01-verify-credential.md) | `CloudAccount`: what Ready proves, the cached lists, key rotation | you are deciding whether to trust this thing with a key | read |
| [02](02-provision-a-cluster.md) | **Create**: the smallest manifest, defaulting, the STATUS and CHANGES columns, crash safety | you want to know what the operator fills in and what it leaves alone | write |
| [03](03-connect-an-application.md) | The connection Secret, and a pod that mounts it | you are evaluating: this is the payoff, and the only lab that runs a workload | read |
| [04](04-take-over-an-existing-cluster.md) | **Take over**: a manifest naming an existing cluster, the export tool, and the four ways binding refuses | you already have clusters and want them managed without recreating them | read, and one change |
| [05](05-day-two-operations.md) | Resize storage, change instance type, scale nodes, one change per pass, restart a node | you are wondering what happens after the cluster exists | write |
| [06](06-delete-safely.md) | Delete the object without deleting the database, and a cluster deleted behind the operator's back | you are about to run `kubectl delete` on something that costs money | write |
| [07](07_delete_a_credential_in_use.sh) | Deleting a `CloudAccount` that a `CloudCluster` still names | you share one credential between clusters | none |

**In a hurry?** 00, then 03. That is the shortest path to seeing the thing the
operator exists to produce, a Secret a pod can mount, and it takes about a
minute. 01 through 06 in order is the full lifecycle. 07 is a single failure
mode and stands alone.

The numbers are the order to read them in, and they trace one lifecycle: prove access, create,
use, take over, change, dispose. Labs 02 through 06 clean up after themselves, so you can run one in
isolation or all of them in sequence. Lab 01 leaves the `CloudAccount` Ready, because everything
after it depends on that.

### What used to be here

Four labs were removed rather than renumbered around, because they were not labs. A lab exists to
show something only a real cluster can show; these were unit tests wearing a costume, and between
them they broke five times in one afternoon from renames alone.

| Was | Where it lives now |
|---|---|
| 06 guardrails | `api/v1alpha1/crd_validation_test.go`, against a real API server in 7 seconds |
| 07 drift and managementPolicy | `TestObservePolicyReportsDriftWithoutMutating` and friends in `internal/controller` |
| 09 rate limits | `TestDefaultPacingStaysAboveOneSecondPerCall`, `TestSequentialCallsArePaced`, `TestAccountsShareOneRateLimiter` |
| 10 extend the operator | `CONTRIBUTING.md` |

## Running them all

```bash
labs/run_all.sh
```

## What has run against RavenDB Cloud

Running against the stand-in catches a disagreement between the operator and the mock. It
cannot catch the case where both read the specification the same wrong way, which is a
demonstrated failure mode here rather than a theoretical one. So every lab has also run
against a real RavenDB Cloud environment, one lab at a time, on small Development clusters that
each lab terminates when it is done. Steps only the stand-in can stage, and steps that need a
bigger cluster, were skipped and counted rather than silently dropped.

The current suite ran there on 21 Oct 2026, after the review before publication: creation
modes, the rule for terminating a cluster that was taken over, and the condition and event
changes. One lab at a time, with the default frugal budget, on the same code as the stand-in
column.

| Lab | Stand-in, 21 Oct 2026 | RavenDB Cloud, 21 Oct 2026 | Clusters | Wall clock on RavenDB Cloud |
|-----|------------------------|-----------------------------|----------|-----------|
| [01](01-verify-credential.md) | 63 checks, none skipped | 59 checks, 4 skipped | none | under a minute |
| [02](02-provision-a-cluster.md) | 74 checks, none skipped | 47 checks, 3 skipped | one Development | 4 min |
| [03](03-connect-an-application.md) | 94 checks, none skipped | 79 checks, 5 skipped, and a pod mounts a credential RavenDB Cloud really issued | one Development | 5 min |
| [04](04-take-over-an-existing-cluster.md) | 90 checks, none skipped | 73 checks, 7 skipped, including the export run in the cluster against the real API | one Development | 10 min |
| [05](05-day-two-operations.md) | 80 checks, none skipped | 58 checks, 10 skipped; one storage change, then a second one shown waiting, because RavenDB Cloud allows one per six hours, and a restart that waits on the policy and then runs | one Development | 9 min |
| [06](06-delete-safely.md) | 84 checks, none skipped | 27 checks, 1 skipped | two Development | 32 min |
| [07](07_delete_a_credential_in_use.sh) | 26 checks, none skipped | 23 checks, 1 skipped (deleting an account together with a cluster it made, which LAB_BUDGET=full runs) | none | under a minute |
| [`e2e_smoke.sh`](e2e_smoke.sh) | not run by `run_all.sh` | 8 checks, read-only, on 29 Sep 2026 | none | seconds |

Every skip is reported by the lab that makes it, with the reason, and falls into one of three
kinds: a count of API calls, which only the stand-in's request log can answer; a cluster only
the stand-in can conjure, such as one Kubernetes never created or two sharing a name; or a step
that needs a second cluster or a Production shape. Node scaling is the one lesson a Development
cluster cannot show, because every Development shape is exactly one node.

Wall clock is the thing to plan around. The same Development shape took anything from four to
over twelve minutes to provision across runs, and the whole suite took about an hour, against
the stand-in's few minutes.

### What it measured that the stand-in cannot

Each of these is a number or a shape the labs had written down against the stand-in and were
quietly treating as a fact about RavenDB Cloud.

- A create took **between four and twelve minutes**, not the 25 seconds the stand-in settles in.
  The labs' old hardcoded 240 second deadline would have failed the run it existed to survive.
- **One storage change per cluster every six hours** on AWS. Lab 05 used to make two a few
  minutes apart, and the second was refused with "Wait at least 6 hours between each storage
  change". The operator treated that as final; it now waits, as `CHANGES Waiting`, and asks again.
  The stand-in enforces the same window, and lab 05 shows a second change waiting on both.
- **A refusal's shape.** RavenDB Cloud answers a refused request with a map from field name to
  messages, general problems under `generalError`. The stand-in used its own `{"error": ...}`,
  so nothing noticed that the operator was quoting the raw JSON at users.
- A 429 comes with a body that parses to nothing, which a script reads as "no such cluster".
  The helpers that ask the control plane directly retry a 429 and fail loudly on anything else,
  and the labs run one at a time there.
- A terminate is **minutes of work**, not instant, and a terminated cluster **stays listed**
  under its name, and **keeps the name**: a create with that display name is refused until the
  terminated cluster is hidden in the portal. That is why every name a lab creates carries a run
  id, and why the stand-in now refuses such a create the same way.
- A re-downloaded client certificate comes back with **the same serial**. The stand-in mints a
  new one each time, and lab 03 had been asserting the change as proof of a real download.
- A Development cluster's DNS is `<subdomain>-dev.<account>`. The stand-in models that rewrite
  now; on a real account the run id in the subdomain still keeps the whole URL from being
  asserted.
- **Production, on one cluster created from the smallest manifest.** The operator picked PS10,
  the smallest Production shape: a shared CPU, 512 MB of RAM, three nodes, 10 GB SsdStandard,
  Active in under four minutes. PS shapes are sold without additional nodes, so the cluster was
  moved to PB10 first, which RavenDB Cloud did in place in 13 minutes. Scaling from three nodes
  to six took 18 minutes, one node at a time, each added only once the cluster was Active again;
  back to three took four, removing F, E and D in that order. The connection Secret listed every
  node after each step.
- **The product quota counts clusters still terminating.** With one Production and one
  Development cluster alive, two more creates were refused with 412, because clusters from the
  run before were still on their way out. The refusal names the quota and where to raise it,
  the object waits as `QuotaReached`, and its slow retry created the cluster once a slot freed.
- **Taken names.** A create reusing a terminated cluster's display name was refused, and so was
  the same name in capitals.
- **Terminating a cluster taken over needs `creationMode: TakeOverOnly`.** Lab 03 cleans up by
  taking its cluster over once more with `deletionPolicy: Terminate`, which the stand-in never
  exercises. After the rule changed, that cleanup left a cluster running, and only this run
  showed it. The cleanup now asks for `TakeOverOnly` and checks the cluster is terminating.

## What the lab control plane can do that the real one cannot

Some things are worth seeing but impossible to arrange on purpose against a real service: a cluster
deleted behind the operator's back, a portal edit that creates drift, an API that fails once. The
mock exposes a small `/_lab` surface for exactly those, and the labs use it to manufacture the
situations that matter. It is clearly separated from the API being simulated, and the guides always
say when a step is reaching for it.

The control plane itself lives in [`internal/cloudsim`](../internal/cloudsim),
which [`mockcloud/main.go`](mockcloud/main.go) serves over HTTP; that split is
what lets the Go tests start the same simulator in process. It is held honest by
[`contract_test.go`](../internal/cloudsim/contract_test.go): it is an independent reading of the same
OpenAPI document as the operator's client, so if the two ever disagree, `go test ./internal/cloudsim`
fails before a lab does.

## If a lab fails

The scripts print a check tally at the end and exit non-zero on failure. The two things worth
looking at first:

```bash
kubectl -n ravendb-cloud-operator-system logs deploy/ravendb-cloud-operator-controller-manager --tail=50
kubectl -n apps describe cloudcluster <name>
```

`describe` is usually enough: every refusal this operator makes is written into a condition with a
reason and a message that says what to do instead. The reason is the word in the `STATUS` or
`CHANGES` column, so `kubectl get cloudcluster` is often enough on its own.

If a lab stops at "could not reach the lab control plane through the port-forward", something
else on the machine is listening on that lab's port. The labs step past a port that answers as
something other than the stand-in, and `LAB_PORT_BASE` moves the whole range.

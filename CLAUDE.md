# RavenDB Cloud operator

A standalone Kubernetes operator for RavenDB Cloud. A `CloudAccount` holds the
credential; a `CloudCluster` names a cluster by display name, takes it over if
it already exists or creates it if not, and publishes its connection details
into a Secret. `CloudClusterRestart` restarts one node, once.

The CRDs are the product. Everything else exists to make them behave the way a
manifest promises.

## Read before changing

`docs/how-it-works/` explains the operator: start with
[architecture.md](docs/how-it-works/architecture.md), then each file covers one
mechanism, how it works, why, and what not to break. Read the one for what you
are about to touch. `docs/how-to-use/` is what a user reads; keep it true when
behaviour changes.

| Changing | Read |
|---|---|
| a validation rule, a default, a new spec field | [docs/how-it-works/validation-and-defaults.md](docs/how-it-works/validation-and-defaults.md), then CONTRIBUTING's checklist |
| how a cluster is found, taken over or created | [docs/how-it-works/identity-and-takeover.md](docs/how-it-works/identity-and-takeover.md) |
| deletion, the finalizer, `deletionPolicy` | [docs/how-it-works/deletion.md](docs/how-it-works/deletion.md) |
| what is compared and applied on a running cluster | [docs/how-it-works/changes-and-drift.md](docs/how-it-works/changes-and-drift.md) |
| conditions, events, logs, metrics | [docs/how-it-works/status-events-and-logs.md](docs/how-it-works/status-events-and-logs.md) |
| `internal/cloudapi`, retries, error classes | [docs/how-it-works/calling-ravendb-cloud.md](docs/how-it-works/calling-ravendb-cloud.md) |
| the connection Secret and the certificate | [docs/how-it-works/connection-secret.md](docs/how-it-works/connection-secret.md) |
| CloudAccount, keys, the client cache | [docs/how-it-works/accounts-and-credentials.md](docs/how-it-works/accounts-and-credentials.md) |
| CloudClusterRestart | [docs/how-it-works/restarts.md](docs/how-it-works/restarts.md) |
| tests, the simulator, labs | [docs/how-it-works/simulator-and-tests.md](docs/how-it-works/simulator-and-tests.md) |

`SECURITY.md` holds the security model, `CONTRIBUTING.md` the mechanics.

## Rules that are not negotiable

- No em dashes or en dashes anywhere: prose, comments, commit messages, docs.
- Never `git push` without explicit confirmation. Committing locally is fine.
- A claim about how the RavenDB Cloud API behaves is settled by RavenDB Cloud's
  source, read from a local checkout (`CLOUD_SRC`) at the commit production
  runs. Not by the OpenAPI document, which has been wrong, and not by tests
  passing, since the client and the simulator can be wrong the same way.
- That source is private and this repository is public. Record the fact and a
  neutral note of how it was verified ("RavenDB Cloud checks this on create"),
  never its file names, class names, line numbers, branches or commit hashes.
  The same goes for commit messages, pull requests and CI logs.
  `hack/cloud-drift` keeps what has to point there in `.cloud-drift.env`
  (ignored by git) and in repository secrets.
- The pacing in `cloudclients` is silent code: the operator paces its calls,
  and nothing here says more about it than that.
- Never name a RavenDB Cloud environment other than production, and never
  document how to point the operator at another API.
- A 4xx from RavenDB Cloud that is not 429 is terminal: report it and stop. Do
  not requeue it on a timer. The few that lift without the object changing
  (busy, too soon, the quota, a rejected key) are listed in
  [calling-ravendb-cloud.md](docs/how-it-works/calling-ravendb-cloud.md); add
  one there before treating it differently.

## Toolchain

Every Go command can run through `hack/go.sh`, which is Docker-based; the
Makefile uses it unless `GO_LOCAL=1`.

```bash
./hack/go.sh go test ./internal/controller -run TestName
make generate manifests chart-sync   # after touching api/v1alpha1/
```

Scale verification to the change: the few tests that cover a small one, the
whole package when a change alters something many tests share (a log format,
the harness, a helper every pass calls), the labs when behaviour a real cluster
shows has moved.

## Writing the code

Write for a programmer who joined last week.

- **KISS.** A plain function beats an abstraction, a concrete type beats an
  interface.
- **YAGNI.** No option nobody sets, no extension point nobody extends.
- **Say it in the code.** A long comment usually means a name is wrong, a
  function does two things, or a value never got a name. Comments are for what
  code cannot carry: an external constraint, why the obvious alternative is
  wrong, a deliberate deviation. Not for history; commit messages hold that.
- **`api/v1alpha1/` is the exception.** Its comments become `kubectl explain`,
  so they are user documentation and should be generous.
- **Go naming:** no stutter, initialisms fully upper (`APIKey`, `ID`, `URL`),
  name length inversely proportional to scope.

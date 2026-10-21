# Contributing

Thanks for looking. This file is the mechanics of working on the code. How each
part of the operator works, and why, is in [`docs/`](docs/), one file per
mechanism; read the one for the part you are changing.

## What you need

Docker. Every Go target in the Makefile runs inside a container through
`hack/go.sh`, so no Go toolchain is needed. With Go 1.26 installed, set
`GO_LOCAL=1` to use it instead.

The labs and `make test-e2e` also need `kubectl` and
[kind](https://kind.sigs.k8s.io/).

## Common commands

| Command | What it runs | Time |
|---|---|---|
| `make test` | unit tests under `-race`, then the CRD tests against a real API server | seconds |
| `make lint` | golangci-lint, as CI runs it | seconds |
| `make generate manifests` | deepcopy, CRDs and RBAC from the markers; run after touching `api/v1alpha1/` | seconds |
| `make chart-sync` | copy the generated CRDs and RBAC into the Helm chart | seconds |
| `labs/run_all.sh --setup` | the operator in a kind cluster, against the simulator | minutes |

CI runs the tests, the linter, the chart checks, a vulnerability scan and the
labs on every pull request.

## Adding a spec field

A field reaches a lot of places, and nothing but this list and the tests makes
sure it reaches all of them.

1. **The type**, in `api/v1alpha1/cloudcluster_types.go`, with a generous doc
   comment: it is what `kubectl explain` shows. Validation that needs only the
   object goes here as markers or CEL, with a case in
   `crd_validation_test.go`. [validation-and-defaults.md](docs/validation-and-defaults.md)
   says where every other kind of rule goes.
2. **A default**, if it has one: static in a marker, dynamic in
   `internal/controller/defaults.go`, read everywhere through an `Effective*`
   method.
3. **The create request**, in `buildCreateRequest` and, if RavenDB Cloud takes
   it, `cloudapi.CreateClusterRequest`, `wire.go` and the simulator.
4. **Drift**, in `shapeDifferences`: one difference, applied or reported. See
   [changes-and-drift.md](docs/changes-and-drift.md).
5. **Status**, if it is observed, in `applyObservedState`.
6. **The export**, in `internal/export`, so an exported manifest still changes
   nothing when applied.
7. **Regenerate**: `make generate manifests chart-sync`.

## Adding an endpoint

1. Add the method to `cloudapi.Client` in `internal/cloudapi/client.go`;
   `rest.go` stops compiling until it is implemented.
2. Implement it in `rest.go`, with wire structs in `wire.go` and every
   wire-to-domain fixup in `toDomain()`.
3. Teach the simulator in `internal/cloudsim`, with its **own** wire structs:
   two independent readings are what lets the contract tests catch a
   misreading, and `internal/arch` enforces the separation. Name the route's
   operation after the client method.
4. Add a contract test. It also checks the traffic against the OpenAPI
   document.
5. Use it from the controller, which depends on the interface, never on
   `restClient`.

## Adding a condition reason

Add the constant in `api/v1alpha1` beside the others for that condition, add it
to its table in `internal/controller/conditions.go`, which decides its status,
its event and whether it stalls, and document it in the condition's comment on
the status type. Set it through `ready`, `synced` or `complete`, never directly.
See [status-events-and-logs.md](docs/status-events-and-logs.md).

## Tests or a lab

A lab shows what only a real cluster can: RBAC, the image, a pod mounting the
connection Secret, deletion order. Everything else is a test, at the cheapest
level that can see it. [simulator-and-tests.md](docs/simulator-and-tests.md)
says which.

## Claims about the RavenDB Cloud API

The OpenAPI document is not enough and has been wrong. If your change depends
on how RavenDB Cloud behaves, say in the pull request what the document says
and what you saw the service answer; a maintainer confirms it against RavenDB
Cloud itself before it is merged.

## Style

- No em dashes or en dashes, anywhere.
- Comments say why, not what. A long comment usually means a name is wrong or a
  function does two things.
- Go naming: no stutter, initialisms fully upper (`APIKey`, `ID`, `URL`).
- One change per pull request, with the tests that prove it.

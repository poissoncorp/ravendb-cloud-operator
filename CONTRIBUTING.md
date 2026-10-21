# Contributing

Thanks for looking. This file is the mechanics of working on the code. How each
part of the operator works, and why, is in
[`docs/how-it-works/`](docs/how-it-works/), starting with the architecture;
read the file for the part you are changing.

## What you need

Docker. Every Go target in the Makefile runs inside a container through
`hack/go.sh`, so no Go toolchain is needed. With Go 1.26 installed, set
`GO_LOCAL=1` to use it instead. controller-gen and setup-envtest are pinned in
`go.mod` and run with `go tool`, so there is nothing else to install.

The labs and `make test-e2e` also need `kubectl` and
[kind](https://kind.sigs.k8s.io/).

## Common commands

| Command | What it runs | Time |
|---|---|---|
| `make test` | `test-unit` under `-race`, then `test-integration`: no cluster and no account needed | seconds |
| `make test-unit` | pure Go: wire mapping, retries, controller logic against the simulator | seconds |
| `make test-integration` | CEL rules and defaults against a real API server (envtest) | seconds |
| `make test-e2e` | the operator as a container in a kind cluster, through the labs | minutes |
| `make test-chart-install` | the Helm chart installed into that cluster, scoped to one namespace | minutes |
| `make test-e2e-cloud` | read-only requests against a real RavenDB Cloud account; needs a key | a minute |
| `make lint` | golangci-lint, as CI runs it | seconds |
| `make generate manifests` | deepcopy, CRDs and RBAC from the markers; run after touching `api/v1alpha1/` | seconds |
| `make chart-sync`, `make chart-lint` | copy the generated CRDs and RBAC into the chart; check it matches, lints and renders | seconds |
| `make build`, `make docker-build` | the manager and the kubectl plugin into `bin/`; the image | seconds |
| `make deploy`, `make hooks` | CRDs, RBAC and the manager via kustomize; the pre-commit checks | seconds |

`api/v1alpha1` reports `ok` with every test skipped when the envtest binaries are
absent, so `make test-unit` says so out loud; run `make test-integration` to
actually exercise the CRDs.

CI runs the tests, the linter, the chart checks, a vulnerability scan and the
labs on every pull request.

A build through `hack/go.sh` that fails with an error that makes no sense, such
as `dwarf: definition of unknown kind`, usually means its Go build cache volume
is damaged. `./hack/go.sh go clean -cache` empties it.

## Adding a spec field

A field reaches a lot of places, and nothing but this list and the tests makes
sure it reaches all of them.

1. **The type**, in `api/v1alpha1/cloudcluster_types.go`, with a generous doc
   comment: it is what `kubectl explain` shows. Validation that needs only the
   object goes here as markers or CEL, with a case in
   `crd_validation_test.go`. [validation-and-defaults.md](docs/how-it-works/validation-and-defaults.md)
   says where every other kind of rule goes.
2. **A default**, if it has one: static in a marker, dynamic in
   `internal/controller/defaults.go`, read everywhere through an `Effective*`
   method.
3. **The create request**, in `buildCreateRequest` and, if RavenDB Cloud takes
   it, `cloudapi.CreateClusterRequest`, `wire.go` and the simulator.
4. **Drift**, in `shapeDifferences`: one difference, applied or reported. See
   [changes-and-drift.md](docs/how-it-works/changes-and-drift.md).
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
See [status-events-and-logs.md](docs/how-it-works/status-events-and-logs.md).

## Tests or a lab

A lab shows what only a real cluster can: RBAC, the image, a pod mounting the
connection Secret, deletion order. Everything else is a test, at the cheapest
level that can see it. [simulator-and-tests.md](docs/how-it-works/simulator-and-tests.md)
says which.

## Claims about the RavenDB Cloud API

The OpenAPI document is not enough and has been wrong. If your change depends
on how RavenDB Cloud behaves, say in the pull request what the document says
and what you saw the service answer; a maintainer confirms it against RavenDB
Cloud itself before it is merged.

## Releasing

Two workflows, as in [ravendb-operator](https://github.com/ravendb/ravendb-operator):

1. **Prepare Release**, run from the Actions tab with the version (`1.2.0`, or
   `1.2.0-rc.1` for a release candidate). It writes the version into the
   Makefile, the chart and the skill's plugin manifest, packages the chart into
   `helm/`, and opens a pull request labelled `release:auto`.
2. Merging that pull request runs **Release**: the tests, `install.yaml` on a
   kind cluster, the arm64 image under QEMU, and only then the image on Docker
   Hub, the tag, and a GitHub Release with `install.yaml` and the kubectl
   plugin. Remove the label to merge without publishing. A release candidate
   does not move `latest`.

Set up once, by a repository admin:

- secrets `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` (a token that can push
  `ravendb/ravendb-cloud-operator`), and `RAVENDB_BOT_PAT`, which opens the
  release pull request so CI runs on it;
- GitHub Pages serving the `main` branch, which publishes `helm/` as the Helm
  repository `https://ravendb.github.io/ravendb-cloud-operator/helm`;
- that Helm repository registered in Artifact Hub, which reads
  `helm/artifacthub-repo.yml`;
- branch protection on `main` requiring CI, so a release pull request cannot
  merge red.

## Style

- No em dashes or en dashes, anywhere.
- Comments say why, not what. A long comment usually means a name is wrong or a
  function does two things.
- Go naming: no stutter, initialisms fully upper (`APIKey`, `ID`, `URL`).
- One change per pull request, with the tests that prove it.

# Simulator and tests

How behaviour is tested, and what each layer can and cannot catch. Read before
adding a test, changing `internal/cloudsim`, or adding a lab.

## Four tiers

| Tier | Command | Needs | Covers |
|---|---|---|---|
| unit | `make test-unit` | Go | wire mapping, retries and pacing, controller logic against the simulator, import rules |
| integration | `make test-integration` | envtest | CEL rules and defaults against a real API server |
| e2e | `make test-e2e` | Docker and kind | the operator as a container: RBAC, image, a Secret a pod mounts |
| e2e-cloud | `make test-e2e-cloud` | a real account | that the client and the simulator were not wrong the same way |

An in-process HTTP server counts as unit: `httptest` needs no binary and no
network. Go tiers by package, not file, so a package mixing pure tests with an
`httptest` harness stays one tier. `api/v1alpha1` reports `ok` without envtest
having run anything; `make test-unit` warns about that.

## The simulator is a second reading

`internal/cloudsim` is a stateful RavenDB Cloud over HTTP, used by the Go tests
in process and by the labs as `labs/mockcloud`. It keeps its own wire structs,
separate from `internal/cloudapi/wire.go`, on purpose: two independent readings
are what let `contract_test.go` catch a misreading. `internal/arch` forbids
sharing them.

Every contract test also runs through `swagger_test.go`, which checks each
request the real client sends and each answer the simulator gives against
`.reference/swagger.json`, a third reading by somebody else.

What none of this catches is both readings being wrong the same way. Only the
real service settles that, through the labs run against it and e2e-cloud. The
simulator's README lists what it still does not model.

## The controller harness

`newHarness` wires the reconcilers to a fake Kubernetes client and a simulator
over `httptest`. It runs with no pacing and no client retries, so `FailNext`
fails exactly one call; tests about retries build a client with production
retries. Its fake client refuses a context that has ended, as the real one does.
The simulator settles creates and changes on the next read unless a test sets a
duration.

## Labs

A lab shows something only a real cluster can: RBAC, the image, a pod mounting
the Secret, finalizers and deletion order. Anything else is a test: a rule about
a manifest goes in `crd_validation_test.go`, one about what RavenDB Cloud offers
in `shape_test.go`, reconcile behaviour in `internal/controller`, wire mapping in
`internal/cloudapi`. A lab that reads Go source is a unit test in the wrong
place.

Labs run against the simulator by default and against a real account with
`LAB_TARGET=cloud`, one at a time, each cleaning up what it created.
[labs/README.md](../../labs/README.md) lists them and what each has passed where.

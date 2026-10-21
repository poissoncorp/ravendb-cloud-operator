# Calling RavenDB Cloud

How the operator talks to the control plane. Read before changing
`internal/cloudapi` or anything that decides whether to call again.

## One door, one implementation

`cloudapi.Client` is the only way to the control plane. The controllers depend
on the interface, never on the REST type. There is exactly one implementation,
`restClient`, on purpose: a second one would be a second hand-written reading of
an API that keeps moving. Tests point the real client at `internal/cloudsim`
over HTTP; see [simulator-and-tests.md](simulator-and-tests.md).

Wire types live in `wire.go` and stay separate from domain types; every
wire-to-domain fixup (provider spelling, URL scheme) happens once, in
`toDomain()`.

## Pacing and deadlines

Every client in the process shares one pace, held by `cloudclients.Cache`, so
more accounts do not mean more calls per second. A call waits for its turn
before it is sent.

- One attempt times out after 30 seconds.
- One reconcile has a two minute deadline (`passTimeout`); the status write that
  ends it has its own.
- A requeue on a timer is low priority, so an edit somebody applied goes before
  routine polls.

## Whether to call again

| What happened | Class | GET | A change (POST) |
|---|---|---|---|
| 429 | retryable | retried | retried |
| 5xx, connection lost, answer unreadable | retryable, may have applied | retried | **not sent again** |
| connection never opened | retryable, not sent | retried | retried |
| given up before sending, the pass out of time | not sent | the next pass | the next pass |
| any other 4xx | terminal | not retried | not retried |

RavenDB Cloud takes no idempotency key, so a change that may have happened is
never sent twice: the next pass reads the cluster and sees (`MayHaveApplied`).
The controller decides policy from these classes, never from message text:
`IsNotFound`, `IsAmbiguous`, `IsUnauthorized`, `IsRetryable`, `IsBusy`,
`IsQuotaReached`, `MayHaveApplied`.

A terminal refusal is reported on a condition with a Warning, and nothing
retries it on a timer; a spec change wakes the object. The product quota is the
one 4xx that lifts by itself, so it keeps a slow timer.

## Messages

RavenDB Cloud's refusals are a map from field to messages. `Describe` turns
them into one sentence, and the controller translates wire field names to spec
field names, so a message says `spec.displayName`, not `displayName`.

## Verifying a claim about the API

The OpenAPI document is not sufficient and has been wrong. A claim about
behaviour is settled by RavenDB Cloud itself; record the fact and how it was
verified ("RavenDB Cloud checks this on create"), never where in its source it
lives.

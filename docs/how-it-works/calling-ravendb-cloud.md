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

The operator paces its calls. Every client in the process shares one pace, held
by `cloudclients.Cache`, and a call waits for its turn before it is sent. The
four workers each for CloudClusters and CloudAccounts, and the one for
restarts, all share it.

- One attempt times out after 30 seconds.
- One reconcile has a two minute deadline (`passTimeout`); the status write that
  ends it has its own.
- A requeue on a timer is low priority, so an edit somebody applied goes before
  routine polls.

## Whether to call again

| What happened | Class | GET | A change (POST) |
|---|---|---|---|
| 429 | retryable | retried | retried |
| 5xx, connection lost | retryable, may have applied | retried | **not sent again** |
| a 2xx this client cannot decode | not retryable, may have applied | not retried | **not sent again** |
| connection never opened | retryable, not sent | retried | retried |
| given up before sending, the pass out of time | not sent | the next pass | the next pass |
| 401 or 403 | unauthorized | not retried | not retried |
| any other 4xx | terminal | not retried | not retried |

RavenDB Cloud takes no idempotency key, so a change that may have happened is
never sent twice: the next pass reads the cluster and sees (`MayHaveApplied`).
The controller decides policy from these classes: `IsNotFound`, `IsAmbiguous`,
`IsUnauthorized`, `IsRetryable`, `IsBusy`, `IsTooSoon`, `IsQuotaReached`,
`MayHaveApplied`. It never reads a message itself. Busy and too soon are the
two classes `rest.go` can only tell from the text of RavenDB Cloud's 400; if
RavenDB Cloud rewords one, it degrades to an ordinary terminal refusal.

A terminal refusal is reported on a condition with a Warning, and nothing
retries it on a timer; a spec change wakes the object. Three 4xx lift by
themselves: busy and too soon are asked again at each poll (see
[changes-and-drift.md](changes-and-drift.md)), and the product quota (412)
keeps a ten minute timer. A terminate answered 404 or 410 counts as done,
and one refused for the key is tried again every poll, since a new key wakes
only the account.

## Messages

RavenDB Cloud's refusals are a map from field to messages. `Describe` turns
them into one sentence, and the controller translates wire field names to spec
field names, so a message says `spec.displayName`, not `displayName`.

## Verifying a claim about the API

The OpenAPI document is not sufficient and has been wrong. A claim about
behaviour is settled by RavenDB Cloud itself; record the fact and how it was
verified ("RavenDB Cloud checks this on create"), never where in its source it
lives.

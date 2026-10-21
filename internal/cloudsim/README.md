# cloudsim

A stand-in for the RavenDB Cloud control plane. The labs run it as an HTTP
server (`labs/mockcloud`), the Go tests start it in process on an `httptest`
server. Either way the operator reaches it with the real REST client over real
HTTP, so there is no second implementation of `cloudapi.Client` that could
drift from the first.

## Staying current

`hack/cloud-drift` checks every week whether RavenDB Cloud's public OpenAPI
document moved away from `.reference/swagger.json`, and opens an issue when it
did. The RavenDB Cloud source is private, so whether it moved in the places the
operator depends on is checked outside this repository. When either reports a
change, read it, bring the simulator and the operator in line, and update the
copy.

Staleness here is not theoretical: an earlier comparison ran against a local
checkout nearly two hundred commits behind production, which is most of why a
drift went unnoticed for so long.

## Why staying current matters more than usual

The wire structs in `api.go` are deliberately a **separate reading** of the API
from the operator's own in `internal/cloudapi/wire.go`. Two independent readings
are the only reason `contract_test.go` can catch a misreading, and
`internal/arch` enforces that they stay separate.

What that test cannot catch is **both sides being wrong in the same direction**.
That is not hypothetical. This project shipped exactly that twice:

- The simulator emitted `https://` on `dns` and the operator passed it through,
  so every test agreed while the real API would have handed applications URLs
  they could not dial.
- The simulator never looked at `iops` and the operator discarded the range that
  says when it is required, so an AWS SsdPremium create looked fine here and
  would have been refused for real.

So a claim about API behaviour is settled by the RavenDB Cloud source, never by
the tests passing. Record the fact and how it was verified, not where in that
source it lives (see CLAUDE.md).

## Known gaps

Verified against the RavenDB Cloud source and deliberately not fixed yet. None
of these break the operator today; they mean a lab shows something production
would not.

| Gap | Effect |
|---|---|
| No per-provider cap on `allowedIps` | Prod allows at most 24 entries on AWS and 200 on Azure and GCP. The operator enforces the cap before it asks. |
| `Error` and `AwaitingPayment` unreachable | Two of prod's seven statuses cannot occur through this state machine, so the operator's non-serving branches (`CreationFailed`, `AwaitingPayment`) are exercised only by the `/_lab` escape hatch. `Terminated` is reachable now. |
| No PN or PS instance types | The catalogue has only PB types, so the rule that those are sold without additional nodes is never reached here. The operator checks it itself and reports instead of asking. |
| No hiding after the fact | A terminate sent with `hideInPortal` hides the product and frees its name, here as in prod. Hiding a product that is already terminated is a portal action the public API does not offer and the simulator does not model, so here a terminated product the operator left visible keeps its name for good. |

## Rules the simulator enforces that are easy to miss

Verified against the RavenDB Cloud source and modelled:

- **Hostnames follow the tier.** A Production cluster is
  `{node}.{subdomain}.{account}`, a Development one `{node}.{subdomain}-dev.{account}`,
  and every Free cluster shares `{node}.free.{account}`, all lowercased.
- **The account has a product quota.** A create is refused with 412 once the
  account holds as many products that are neither Free nor Terminated as its
  quota allows, whatever tier the new one asks for, and before the request is
  validated. RavenDB Cloud's default is three. The simulator models it behind
  `Options.ProductQuota` and `State.SetProductQuota`, off by default, because
  the labs hold more than three clusters at once.
- **Allowed IPs are checked as networks.** Two entries that are one network
  once normalised are refused on every provider, and two that overlap are
  refused on Azure only.

The next two matter most, because the take-over model depends on them:

- **Display names are unique among products not hidden in the portal,
  terminated ones included.** RavenDB Cloud checks every non-hidden product in
  the account on create, whatever its status, and the comparison ignores case.
  A duplicate is refused with "Provided Display Name is already taken." A
  subdomain, unlike a name, is free again once its cluster is terminated.
- **A refusal can name its field.** Prod sends a validation failure under the
  name of the field it is about, and under `generalError` only when it is about
  the request as a whole. The simulator sends the display name refusal under
  `displayName`, which is what lets the operator translate it to
  `spec.displayName` and add that a terminated cluster keeps its name until
  hidden. Its other refusals are still general ones.

## The OpenAPI document as a third reading

The client and the simulator are written by the same people, so the contract
tests catch a typo on either side but not a shared misunderstanding. Every
contract test therefore also runs through `swagger_test.go`, which checks each
request the real client sends, and each answer the simulator gives, against
`.reference/swagger.json`. That is what verifies, for one, that `AddNode` sends
no `nodeTag` while remove and restart do. It found the simulator sending
`"iopsRange": null` the first time it ran, which RavenDB Cloud never sends.

## The swagger diff

All 14 paths in `.reference/swagger.json` match the RavenDB Cloud source. Three
documented fields are not modelled in `internal/cloudapi/wire.go`:

| Field | Status |
|---|---|
| `ProductCreateRequest.diskLayout` | Not sent, so production falls back to RootOnly. That is the layout the root-disk cap in `cloud_limits.go` assumes, so this confirms rather than contradicts it. |
| `ProductDetailsResponse.paymentPlan` | Billing information the operator has no use for. |
| `StorageResponse.availableDiskSizes` | **Fixed.** The sizes *this* product may resize to, not the create-time menu. The operator reads it as `Storage.ResizableTo` and reports an unreachable size as drift, and the simulator reports current-size-and-up rather than the whole menu. |

Worth recording honestly: `iopsRange` was in this document all along. The
operator discarded it anyway. The swagger is not sufficient, but on that
occasion it was not the problem.

## Still unchecked

Eight lower-severity findings from the original comparison were never
adversarially verified, and 37 more were argued down by verifiers while the run
was losing agents to a session limit. Several in that second group read as
substantive on a second look, so the bucket is worth re-opening rather than
trusted.

One has since been confirmed by hand, and it was an operator bug rather than a
simulator gap: RavenDB Cloud **renews client certificates automatically**, while
the operator used to download the bundle once and never again. Fixed:
`certificateNeeded` now fetches the replacement once the stored certificate is
within `certificateRenewWindow` (21 days) of expiry, and RavenDB Cloud
regenerates one 30 days before. The simulator still issues a fresh certificate
on every download, which prod does not.

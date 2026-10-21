# Validation and defaults

Where a rule about what a user may ask for goes, and where a default comes from.
Read before adding a check, a default, or a spec field.

## Where a rule goes

Every rule has one home. Find it by asking what the rule needs to look at:

| The rule needs | Home | The user finds out |
|---|---|---|
| only the object being applied | `api/v1alpha1/` | at `kubectl apply`, before anything is stored |
| only the outgoing request | `cloudapi.ValidateCreate` | on the first reconcile, before a request goes out |
| which regions, instance types and disk shapes exist, or a limit RavenDB Cloud does not publish | `internal/controller` | on the first reconcile, told which spec field to change |
| the account, the bill, or objects we cannot see | nowhere: RavenDB Cloud refuses it | after a round trip, in the cloud's own words |

Put a rule in the first row it fits: earlier is sooner for the user, cheaper in
calls, and the message is ours to write.

**`api/v1alpha1`.** `+kubebuilder:validation` markers and CEL rules: enums,
bounds, formats, required-together, mutually exclusive, immutability. The only
home that rejects a manifest before it is stored and shows in `kubectl explain`.
CEL transition rules (`oldSelf`) do not fire on create, so a rule that must also
hold on create needs a second, non-transition rule. A rule with no case in
`crd_validation_test.go`, against a real API server, is not done.

**`cloudapi.ValidateCreate`.** Knows only the request. Defence in depth for a
caller that did not come through the CRD; it cannot see what RavenDB Cloud
offers and cannot name a spec field, so it does not grow.

**`internal/controller`.** The only home that sees what RavenDB Cloud offers:
`shape.go` checks against the published lists, `cloud_limits.go` holds limits
RavenDB Cloud enforces but never publishes (each saying how it was verified),
`defaults.go` derives what the user left out. Messages name the spec field to
change and, when the value came from the account, say so. An empty list means
"this deployment did not tell us", not "nothing is allowed": let RavenDB Cloud
judge rather than block on missing data.

## Where a default comes from

| Kind | Home |
|---|---|
| static | `+kubebuilder:default` in `api/v1alpha1`; if the API server can apply it, nothing else may |
| dynamic, needing what RavenDB Cloud offers or the account | `internal/controller`: cheapest instance type for a tier, subdomain from the display name, `CloudAccount.spec.defaults` |
| reading a defaulted field | the `Effective*` method on the type, the single read path; where it backstops a static default, a test pins the two to the same value |

Some fields have no default on purpose: `storage.type` depends on the provider,
which the API server cannot see, and the obvious static default, SsdStandard,
is one GCP refuses.

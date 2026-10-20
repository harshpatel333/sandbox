# Commercial offer assignments

An assignment records accepted commercial terms. It does not activate Pro, issue
credit, create a provider object, or change funding eligibility. Existing paid
and sponsored contracts continue to use their existing grant engine.

The catalog publishes immutable `(offer_key, version)` rows. The initial YC
version has a waived $12,000 annual fee, one year of Pro, three $30,000 monthly
allowances followed by nine $1,000 allowances, monthly expiry without rollover,
no proration, and a default `pro_annual` successor family. Eligible sponsored
credit does not require a card; a paid successor requires verified payment.
The successor family is resolved to its then-approved version by the transition
consumer, not changed retrospectively on the original assignment.

## Assignment boundary

Operator-token routes under `/internal/teams/:team_id/billing`:

- `POST /commercial-offer-assignment/preflight`
- `POST /commercial-offer-assignment`
- `GET /commercial-offer-assignment`

All require a Google-verified platform staff actor. Reads/preflight require
`platform:billing:read`; apply requires `platform:billing:write`. The request
contains `idempotency_key`, `offer_key`, `offer_version`, `approval_id`,
`company_identity`, `home_region`, `reason`, and `reference`. Authority must
already exist for that exact company, team, region and version. This boundary
never creates approval authority.

The internal `assignCommercialOffer` service is also available to a future
trusted activation consumer. It verifies and locks the same shared approval.
There is one immutable assignment per team and approval. Retries must use the
same request; they retain the originally accepted catalog version, pricing
card, source rate identities and resolved rate values. Current pricing follows
the existing active team pricing resolver with its `payg` fallback. Custom
pricing that differs from the catalog reference fails closed. Historical
provider accounts and contracts are not enrolled here.

Preflight creates only an audit record. Apply atomically writes the assignment
and audit record. Status is audited. A pending assignment has no activation
anchor or periods: provider setup delay must not consume the benefit year.

## Materialization boundary

`materializeCommercialOffer` accepts the team, assignment, contract, merchant
and mode binding from the trusted activation service. It accepts no timestamp.
It verifies current shared approval and uses the persisted active contract's
original anchor, account association and sole allowance schedule. The database
also requires either the existing YC annual authority with its exact zero-fee
bounded waiver, or active sponsored annual authority. Merchant/mode, region,
company where present, annual fee semantics and the accepted resource rates
must match. Bare sponsorship is insufficient.

Materialization atomically creates an immutable execution binding and twelve
periods calculated from the original UTC anchor. Each period expires at its
exclusive end. Replays return the same binding even after the original contract
ends; they confer no new eligibility. A changed binding fails.

No automatic caller is enabled by this change. A new provider architecture must
add a separately reviewed authority adapter to this gate. The existing provider
and grant workers remain responsible for activation, current debt/revocation
checks, access readiness and issuing each allowance once. Materialized status
reports historical term capture, not current access authorization.

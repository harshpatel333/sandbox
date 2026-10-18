# Funding-based sandbox access

Sandbox admission and billing suspension use `team_sandbox_billing_eligible`.
For opted-in teams, an explicit abuse restriction wins. Otherwise either of
these sources authorizes usage:

- Positive, unexpired spendable usage credit, after outstanding usage. Credit
  works regardless of plan state or device reuse.
- A verified reusable payment method and a working usage collection route.
  A reused or unknown signup device additionally needs a captured positive USD
  payment belonging to the same billing customer, with no refund or dispute.

A past payment proves collection history; it is not an unspent balance. Plan
names and promotion state do not authorize or deny usage. Managed billing keeps
its existing delegated payer rule. User, IP, and domain abuse checks remain at
the existing admission boundary.

## Observation and limits

`billing_funding_account` stores bounded background observations. Admission
performs local indexed reads through the existing cache; it never calls Stripe.
Credit observations expire within one minute, clipped to grant expiry. Provider
observations expire within five minutes, clipped to card expiry. The existing
admission cache can retain a positive answer for another 30 seconds. These are
bounded consumption windows, not per-request credit reservations.

Before Stripe credit activation, local grants are authoritative. A customer ID
alone is not activation. After a subscription, activation grant, contract, or
invoice-close history establishes Stripe authority, only Stripe credit counts;
local mirrors are never added. Provider ledger balances are reduced by locally
unsettled usage, including live intervals. Unknown reconciliation, unsupported
scope, or oversized inventories cannot establish positive credit. The current
observer supports USD usage grants, not arbitrary Stripe cash balances. Multiple
overlapping credit cohorts can produce a conservative lower balance.

Card-backed access also requires the current account to match enrolled usage
resources and the exporter configuration. The current collector supports active
subscriptions; a card attached to a canceled subscription is insufficient. A
verified terminal plan can be released to Free and then use the existing PAYG
checkout to establish a replacement collection route. Credit remains usable
while that setup is incomplete.

Generation changes fence observations across balance, account mapping, period
finalization, correction, or enrollment changes. A stale worker cannot publish
against a newer generation. Failed refreshes do not extend evidence lifetimes.

## Rollout

1. Apply the migration and deploy all writers and the funding worker. New and
   existing rows default to `observe=false,enforce=false`, retaining the previous
   rule. Opt a small candidate cohort into `observe=true` first. The database
   limits observation to four teams initially; unselected accounts consume no
   provider refresh capacity.
2. Observe worker errors and the funding backlog before opting in a small
   cohort. Investigate unsupported credit inventories, unknown fingerprints,
   missing invoice enrollment, and missing export configuration. Do not enable
   an entire fleet with missing or stale observations. The singleton
   `billing_funding_policy.max_observed_teams` is a capacity budget, not a rollout
   target. Increase it only after measuring refresh completion under provider
   latency and failures. Require oldest due age below 20 seconds and completion
   before the shortest evidence expiry; leave headroom for slow provider calls.
3. Inspect each candidate using the read-only query below. Verify either fresh
   credit or fresh reusable-payment evidence with collection readiness and
   fingerprint/cash clearance. Also inspect the relevant abuse restrictions.
4. Set `enforce=true` only for the reviewed team IDs. Exercise creation, resume,
   and the billing suspension sweep. Compare with the previous rule during the
   canary, then expand only while refresh capacity and coverage remain healthy.
5. Roll back an affected cohort by setting `enforce=false`. This restores the
   previous eligibility rule; it does not undo a plan release or provider work.

```sql
SELECT f.team_id, f.observe, f.enforce, f.generation = f.observed_generation AS current,
       f.credit_usd, f.credit_valid_until, f.payment_method_ready,
       f.collection_ready, f.cash_collected, f.provider_valid_until,
       billing_funding_fingerprint(f.team_id) AS signup_device,
       f.next_check_at, f.checked_at, f.failures, f.last_error,
       team_sandbox_billing_eligible(f.team_id) AS currently_allowed
FROM billing_funding_account f
WHERE f.team_id = :'team_id'::uuid;
```

## Explicit transition after a terminated plan

The authenticated `POST /stripe/plan-choice` request with `{"offer":"free"}`
requires the existing billing-write permission. It refuses pending checkout or
ambiguous provider attempts. For an existing subscription it retrieves terminal
provider proof (`canceled` or `incomplete_expired`). A revoked YC approval with
no provider attempt can also be released.

The transaction retains an immutable `billing_plan_release` receipt, ends future
benefits of the old contract, preserves issued grants and payment history, and
selects Free. It does not cancel a subscription or make a provider write. The
old subscription binding stays until verified replacement checkout completes.
Delayed callbacks cannot resurrect the released subscription or YC operation,
and replacement checkout does not reissue the activation promotion. Ordinary
invoice reconciliation retains responsibility for historical usage.

After release, the existing PAYG checkout establishes a fresh subscription and
invoice enrollment. Preserve the purchased resource and rounding price IDs when
carrying scoped credits into that subscription. Its invoices use the verified
credit ledger, including expiry and scope, without granting another allowance.
An issued allowance that disappears, or expires across the replacement invoice
boundary while covering part of that usage, holds invoice collection for
reconciliation instead of silently charging the customer for promised credit.
Enable the funding access cohort only after inspecting the
resulting observation. This runbook does not authorize production changes.

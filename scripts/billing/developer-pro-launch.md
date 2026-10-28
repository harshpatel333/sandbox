# Developer and Pro launch runbook

Launching Developer and Pro is configuration: catalog publishes through the
staff catalog API (`catalog.md`), the trial settings row, and a Stripe
promotion code. No step here is a migration, and no value below is in code.
Run everything in staging first, then repeat in production only with explicit
authorization after the staging test passes.

## Inputs

Take the values from the approved launch record. Public values:

| Input | Developer | Pro |
| -- | -- | -- |
| `monthly_fee_cents` | 2000 | 25000 |
| Included credit (`credit_schedule` amount, expires after 1 month) | 2000 | 25000 |
| `funding_policy` | `prepaid` | `postpaid` |
| `topoff` | amount 500, `default_enabled` true, threshold `<topoff_threshold_cents>` | none |
| `max_concurrent` | 100 | 2000 |
| `max_vcpu` / `max_memory_mib` | 4 / 16384 | 16 / 65536 |

Internal caps are never published, so they are not written here. Pass them
from the launch record: `<disk_mib>` (`max_disk_mib`), `<developer_templates>`
and `<pro_templates>` (`max_templates`), and the snapshot caps once the catalog
carries them.

Trial settings: `credit_usd` 0.05, `max_sandboxes` 1, and from the launch
record `<trial_vcpu>`, `<trial_memory_mib>`, `<trial_disk_mib>` and
`<trial_low_credit_usd>` (the banner's "almost used up" level).

## Staging

**S0. Compare catalogs.** Run in staging and in production and diff the output.

```sql
SELECT family, version, state,
       to_jsonb(v) - ARRAY['id','version','created_by','created_at','updated_at','published_by','published_at','retired_by','retired_at'] AS terms
FROM billing_plan_version v WHERE pricing_version = 'v2' ORDER BY family, version;
SELECT pricing_version FROM billing_pricing_switch;
```

**S1. Free templates.** Run `free-zero-templates.md` against staging's published
v2 Free (dry run, then `COMMIT`). It publishes a catalog version only; no teams
move.

**S2. Pro.** Confirm from S0 that staging's published Pro matches production.

**S3. Configure and publish,** after the funding-policy, trial signup and
messaging changes are deployed:

1. Set the trial settings (below).
2. Publish a Pro version with the launch terms: a draft copying the published
   Pro, with the inputs above, then publish.
3. Publish Developer the same way.
4. Check `SELECT billing_paid_first_sellable(), billing_trial_configured();`
   returns `t, t`, then release the staging new-team gate with the usual
   release procedure.
5. Create the staging YC promotion code (below) for the Pro version from step 2.

**S4. End-to-end test.**

- A new signup starts on the trial with the configured credit and one sandbox,
  and `GET /billing/plan` reports `entry_state: trial`.
- When the trial credit runs out, sandboxes pause, `entry_state` becomes
  `trial_ended`, the console shows the plan picker, and billing members get
  exactly one "free trial has ended" email. No "running low" email is sent
  during the trial.
- Developer checkout with Auto Topoff on and with it off both activate. With it
  off, the low-credit email arrives once when credit falls below the plan's
  top-off threshold; with it on, none arrives.
- Pro checkout with the YC code activates with a $0 first invoice.
- A team that signed up on Free before the test keeps its assignment, limits
  and `entry_state: active`. Enterprise still books a call.

## Production

Only after S4 passes and the publish is authorized:

1. Re-run S0 against production.
2. Set the trial settings. Until Developer is published, signups keep getting
   Free.
3. Publish the Pro launch version, then Developer. Publishing Developer is the
   go-live: from then on every new signup gets the trial.
4. Create the production YC coupon and promotion code for the final Pro version.

Rollback is retiring Developer (`POST /internal/billing/catalog/plans/{version_id}/retire`).
New signups then fall back to Free. Teams already on any plan, including early
Free teams, keep their frozen terms.

## Trial settings

`billing_trial_policy` is a single row edited with the environment's privileged
database connection:

```sql
\set ON_ERROR_STOP on
BEGIN;
UPDATE billing_trial_policy
SET credit_usd = :'credit_usd', max_sandboxes = :'max_sandboxes', max_vcpu = :'max_vcpu',
    max_memory_mib = :'max_memory_mib', max_disk_mib = :'max_disk_mib',
    low_credit_threshold_usd = :'low_credit_usd', updated_at = clock_timestamp()
WHERE singleton RETURNING *;
SELECT billing_trial_configured();
COMMIT;
```

Pass the values with `psql -v credit_usd=... -v max_sandboxes=... ...`. A team's
credit and limits are frozen when its trial starts; later edits apply to new
signups. `low_credit_threshold_usd` is read live and only changes messaging.

## YC promotion code

`yc_promotion_code.py` creates a coupon (100% off, `duration=once`,
`applies_to` the Pro version's product only) and a promotion code limited to
first-time customers with a redemption cap. It previews by default and writes
only with `--apply`; reruns reuse what exists and fail if it differs.

```bash
python3 scripts/billing/yc_promotion_code.py --environment staging \
  --expected-account <acct_id> --plan-version-id <pro_version_id> \
  --code <CODE> --max-redemptions <cap> [--apply]
```

The API creates the version's Pro price when it opens the first Pro checkout
session for that version, so open one (it need not be paid) before running
the script. If the API is configured with an explicit Pro price
ID that still matches the fee, pass `--price <price_id>` instead of
`--plan-version-id`. The key comes from `STRIPE_SECRET_KEY` or `--pass-entry`,
and must be a live key for `--environment production`.

Create the code again after every new Pro version, including limit-only edits:
the old coupon does not apply to the new version's product.

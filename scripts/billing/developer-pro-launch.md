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

## Regions

Production has two regions with separate databases. Every step below (S0,
trial settings, publish, retire) runs in both, and both must end with the same
version ids. The console's admin plan page writes to every region with one
minted version id; when using the API directly, send the same `id` and
`version` in each region's draft. Before creating a YC code, check the version
ids match:

```sql
SELECT family, version, id FROM billing_plan_version
WHERE pricing_version = 'v2' AND state = 'published' ORDER BY family;
```

## Staging

**S0. Compare catalogs.** Run in staging and in each production region and diff
the output.

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
3. Publish Developer the same way. Check the published row has
   `funding_policy = 'prepaid'` and its top-off columns set; an editor that
   does not send them publishes a postpaid plan.
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
  during the trial, and none after a paid checkout.
- Developer checkout with Auto Topoff on and with it off both activate. No
  low-credit email arrives before the included grant is issued. With Auto
  Topoff off, the email arrives once when credit falls below the plan's top-off
  threshold; with it on, none arrives.
- Pro checkout with the YC code activates with a $0 first invoice.
- A team that signed up on Free before the test keeps its assignment, limits
  and `entry_state: active`. Enterprise still books a call.
- Try the YC code from a Free team that already paid a $0 invoice: Stripe may
  treat it as a returning customer and reject `first_time_transaction`. Record
  the result; such teams need a separate path.

## Production

Preconditions:

- S4 passed and the publish is authorized.
- The catalog carries snapshot caps, and the Developer and Pro drafts set them.
- Each region has a usable published v2 Free with `max_templates = 0`, so a
  rollback has somewhere to send new signups.

Then, in both regions:

1. Re-run S0.
2. Set the trial settings. Until Developer is published, signups keep getting
   Free.
3. Publish the Pro launch version, then Developer. Publishing Developer is the
   go-live: from then on every new signup gets the trial.
4. Check the published version ids match across regions, then create the
   production YC coupon and promotion code for the final Pro version.

Rollback is retiring Developer in both regions
(`POST /internal/billing/catalog/plans/{version_id}/retire`). New signups then
fall back to Free. Teams already on any plan, including early Free teams, keep
their frozen terms. Teams already in `trial` or `trial_ended` follow the
signup flow's rollback rules; check them against that flow before retiring.

## Trial settings

`billing_trial_policy` is a single row per region, edited with that region's
privileged database connection:

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

## Low-credit messaging

The low-credit email waits for each period's included grant. It stops
waiting when the renewal invoice's collection fails, or
`billing_funding_policy.low_credit_grant_grace` (24 hours unless changed; per
region, editable like the trial row) after the period starts. Email links use the API's `CONSOLE_URL`;
without it the emails carry no link.

## YC promotion code

`yc_promotion_code.py` creates a coupon (100% off, `duration=once`,
`applies_to` the Pro price's product only) and a promotion code limited to
first-time customers with a redemption cap. It previews by default and writes
only with `--apply`; reruns reuse what exists and fail if it differs. It only
accepts the Pro monthly fee price (`commercial_offer` `pro_monthly`, licensed,
monthly, positive amount).

```bash
python3 scripts/billing/yc_promotion_code.py --environment staging \
  --expected-account <acct_id> --plan-version-id <pro_version_id> \
  --code <CODE> --max-redemptions <cap> [--apply]
```

The API creates the version's Pro price when it opens the first Pro checkout
session for that version, so open one (it need not be paid) before running
the script. If the API is configured with an explicit Pro price ID that still
matches the fee, pass `--price <price_id>` instead of `--plan-version-id`; that
price must carry the `commercial_offer` metadata. The key comes from
`STRIPE_SECRET_KEY` or `--pass-entry`, and must be a live key for
`--environment production`.

After every new Pro version, including limit-only edits:

1. Deactivate the previous promotion code in Stripe.
2. Run the script with a new code string for the new version. The redemption
   cap starts again from zero, and the new code has to be sent out again.

A version-keyed price has its own product, so the new version needs a new
coupon. A configured Pro price shared across versions keeps one product: the
script then reuses the existing coupon and only the promotion code is new.

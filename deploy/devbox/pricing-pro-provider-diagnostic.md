# Diagnose the preserved calendar subscription HTTP400

RC alone runs this root-on-box. Do not restart the calendar runner, recreate fixtures, erase the failed receipt, copy api.env, print the key or change API version. This diagnostic does not deploy or touch application teams/assignments/adoption.

The recorded actual failure is run40117611263, backend4cbd9ec570e822a8de0241dc4fa2a07cd5aaa46b, configured API2026-05-27.dahlia and independently confirmed test account acct_1UST2ILIAkKd3qgh. The failed calendar runner source SHA256 is519bd7856702f9869a937ea1d93efc92011892e48f9f3cb037547d03820cde1d. Its receipt contains the original run UUID, request ID and four already-created clock/customer/product/price IDs. The source receipt must remain immutable.

First install only this reviewed diagnostic/test/runbook commit into RC's next dev integration source. ROOTONBOX is that selected on-box checkout; it must retain the original calendar runner unchanged. It reads root api.env internally via the existing helper. No key appears in arguments, output, SSH payload or files created by this diagnostic.

```sh
set +x
set -o pipefail
ROOTONBOX=/path/to/RC/selected/onbox/checkout
STATE=/var/lib/superserve-devbox
SOURCE="$STATE/pro-qa/calendar-4cbd9ec570e822a8de0241dc4fa2a07cd5aaa46b.json"
sudo python3 "$ROOTONBOX/deploy/devbox/pricing-pro-provider-diagnostic.py" \
  --source-receipt "$SOURCE" --output "$STATE/pro-qa/subscription-inventory-1.json"
```

This default is read-only provider work: reverify account/balance, retrieve the exact original customer/clock/product/price and list ALL subscription statuses for that customer. It retains the original request ID and four fixture IDs in a NEW root600 receipt; source SHA256 identifies the original. A paginated inventory, ANY existing subscription, changed clock/fixture/account/version/source or replay age23h+ blocks mutation. It intentionally creates no new fixture or subscription.

Read the new NONSECRET diagnostic receipt on-box and decide deliberately whether to perform the original exact-idempotency replay. If customer inventory is incomplete or nonempty, reconcile those IDs instead. If no subscription exists and all binding checks passed:

```sh
sudo python3 "$ROOTONBOX/deploy/devbox/pricing-pro-provider-diagnostic.py" \
  --source-receipt "$SOURCE" --output "$STATE/pro-qa/subscription-exact-replay-1.json" \
  --replay-failed-subscription
```

Replay repeats the inventory first. It sends ONLY the original POST/v1/subscriptions form: customer and price from the original four IDs, collection_method=send_invoice, days_until_due=30, proration_behavior=none, billing_mode[type]=flexible, metadata[dev_pro_qa]=original run UUID. Idempotency key is exactly dev-pro-qa:<original_run_uuid>:2027:subscription. It does not change fields to try a guessed fix, generate a new key, create a calendar, attach a payment method, issue cash/credit or advance time.

Expected HTTP400 is a nonzero exit with the original and replay request IDs plus whitelisted error.type/code/param and a bounded redacted message in the NEW receipt. Full body, headers, URLs and unrelated error fields are never persisted. Actual cause is the returned structured error, not inferred permissions. Share only these sanitized fields and fixture/request IDs. No replacement key request without an actual credential rejection.

If the exact replay succeeds, stop and reconcile the returned subscription ID. Do not call the old runner to recreate its clock/customer or automatically start another subscription. If transport fails ambiguously, inventory again before any further attempt; do not blindly retry. Existing idempotency retention is bounded:23h+ is manual inventory/reconciliation, not an automatic new request.

The next source fix must follow the actual error. If our calendar request is invalid, change only QA request code, retain original failed/replay receipts, and obtain a separate deliberate recovery request/idempotency plan for the existing fixtures. An error in a licensed send_invoice calendar fixture does not by itself prove the Pro metered-subscription request fails or that the key lacks permissions.

No W6 provider calls or deployment occurred while preparing this diagnostic. Calendar/control, Pro cash/grants and next-boundary worker QA remain pending until separately evidenced.

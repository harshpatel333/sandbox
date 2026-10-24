# Resume the verified missing-email calendar fixture

Actual diagnostic40120077550 returned HTTP400 invalid_request_error, code/param null: `The customer needs a valid email when collection_method=send_invoice.` Account binding, credentials and API version are not the cause shown by this error. The scoped fix is QA-only customer email, not Pro application code or a key/version change.

RC alone runs the reviewed source on-box. The original failed calendar receipt and exact-replay diagnostic remain immutable. Do not rerun the old calendar command, erase receipts or recreate the2027 clock/customer/product/price. No deploy/restart/DB/flags/application customer or assignment writes are part of this operation.

The new entry point requires original source SHA256b98f905b20e5760676645c08446f6d992dec6c7125911a780485e6250a762174, matching exact diagnostic cause and original replay scope. It binds known test account/version, requires complete empty customer subscription inventory, unchanged original clock and replay age<23h. Before updating email, fresh GETs must prove original customer+clock+name and original product+name belong to this run. Missing email is set only to dev-pro-qa-<original_run_uuid>-2027@example.com. An exact previously set same email is recognized; any other nonempty value aborts without overwrite. No real email recipient or card is used.

After the email-only update, GET verifies it and a new all-status customer inventory immediately precedes the original subscription create. The2027 subscription uses the exact ORIGINAL customer/price, payload and idempotency key; there is no field/version/key change to that request. Hold/release/calendar checks then continue using that subscription and original clock. Only after2027 succeeds does the script create the new2028 fixture with its own reserved valid email and original run's2028 idempotency scope.

## Exact on-box commands

Use the selected reviewed checkout as ROOTONBOX. DIAGNOSTIC is the actual root-only exact replay receipt from run40120077550, not the inventory-only receipt. Supply its actual existing filename; do not fabricate or overwrite it.

```sh
set +x
set -o pipefail
ROOTONBOX=/path/to/RC/selected/onbox/checkout
STATE=/var/lib/superserve-devbox
SOURCE="$STATE/pro-qa/calendar-4cbd9ec570e822a8de0241dc4fa2a07cd5aaa46b.json"
DIAGNOSTIC="$STATE/pro-qa/REPLACE_WITH_ACTUAL_EXACT_REPLAY_RECEIPT.json"
sudo python3 "$ROOTONBOX/deploy/devbox/pricing-pro-calendar-resume.py" \
  --source-receipt "$SOURCE" --diagnostic-receipt "$DIAGNOSTIC" \
  --output "$STATE/pro-qa/email-repair-inventory-1.json"
```

Default mode is read-only provider inventory. The separately authorized execution command explicitly enables the scoped email repair and calendar continuation:

```sh
sudo python3 "$ROOTONBOX/deploy/devbox/pricing-pro-calendar-resume.py" \
  --source-receipt "$SOURCE" --diagnostic-receipt "$DIAGNOSTIC" \
  --resume-calendar --output "$STATE/pro-qa/email-repair-calendar-resume-1.json"
```

The key is read directly from existing root api.env, never printed or transported. Existing RC binding receipt is required; no replacement key. New outputs are root600/O_EXCL, preserve source+diagnostic hashes, original IDs and request IDs, and record continuation/new2028 IDs and each request's nonsecret status/ID. Original receipt files are checked unchanged. No cleanup runs.

Any error, nonempty subscription inventory, changed ownership/clock, expired replay window or unknown email stops for deliberate reconciliation. If a subscription was successfully created before a later failure, its ID is retained in the new output. Do not blindly rerun this recovery: inventory that ID and resume the failed later stage under a separately reviewed plan. Do not create a replacement calendar. Diagnostic-only source runner hash checks intentionally continue to identify the OLD failed payload; this new entry point is the narrow reviewed recovery for the actual missing-email cause.

Success means `passed_resumed_calendar_control_only`. It proves actual test-clock/month-end/pause-control capability if RC executes successfully; it does not prove Pro cash/grant/assignment, response-loss/OFF/predecessor, or next-boundary application workers. Those gates and the application/provider time-domain limitation remain unchanged.

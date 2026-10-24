# Isolated Pro provider QA

RC owns execution on the isolated integration box. This runner performs no deployment, adoption transition, production change, cleanup or real-card charge. Do not run it from an inference workspace or transport api.env/credentials off-box. Do not use shell tracing, curl verbose output or environment dumps.

The script reads the existing root-only api.env on-box using the existing devbox env parser. It requires the RC workflow's verified pricing-stripe-binding.json and an independently confirmed expected test acct_ identity, then rechecks test credential prefix, /v1/account and /v1/balance livemode=false. No mutation is allowed before that binding. Provider HTTP failures expose method/path/status/request ID only, never response bodies or authorization. Do not request another key based on an untested permission assumption.

## RC commands

Use the selected on-box checkout containing this script as ROOTONBOX. Keep the expected account and all artifacts on-box. EXPECTED_TEST_ACCOUNT is a nonsecret acct_ ID confirmed by RC/account administrator, not a secret and not silently learned from an arbitrary key. STATE is the existing services state directory, normally /var/lib/superserve-devbox. No sourcing api.env into an SSH command, no copying its contents and no key in process arguments.

```sh
set +x
set -o pipefail
ROOTONBOX=/path/to/selected/onbox/checkout
STATE=/var/lib/superserve-devbox
EXPECTED_TEST_ACCOUNT=acct_REPLACE_WITH_RC_CONFIRMED_TEST_ACCOUNT
sudo install -d -m 700 "$STATE/pro-qa"
sudo python3 "$ROOTONBOX/deploy/devbox/pricing-pro-provider-qa.py" \
  --state-dir "$STATE" --expected-account "$EXPECTED_TEST_ACCOUNT" \
  --output "$STATE/pro-qa/preflight-1.json"
```

Default mode only checks account/test binding. Once the current deployment is ready and that succeeds, the following explicitly creates isolated test objects. Each run uses a unique scope and records created object IDs immediately in a root-only receipt. It creates test clocks/customers/products/monthly USD250 prices/subscriptions with send_invoice (no payment method or cash charge); verifies separately idempotent pause_collection hold/release without anchor drift; advances Jan31->Feb28->Mar31 and Jan31->Feb29->Mar31; polls until the clock is ready; checks item periods and original billing anchor. It does not touch the application's customer, subscription, catalog, switch or grants.

```sh
sudo python3 "$ROOTONBOX/deploy/devbox/pricing-pro-provider-qa.py" \
  --state-dir "$STATE" --expected-account "$EXPECTED_TEST_ACCOUNT" \
  --run-test-calendar --output "$STATE/pro-qa/calendar-1.json"
```

Do not immediately retry an ambiguous mutation or reuse an output path. Reconcile the recorded IDs and Stripe request log first. Failed runs retain created objects; cleanup is intentionally an RC decision. The objects are tagged/named dev-pro-qa with a run UUID; cleanup must operate only on those recorded test fixtures. Restricted-key rejection is a concrete capability blocker only after its actual endpoint/status is recorded. A denied optional test-clock capability does not justify falling back to production.

## Actual application checkout and evidence

Calendar fixture success proves provider calendar/control capability, NOT the Pro application flow or cash/grant permissions. For actual checkout use the deployed Console /billing/pro?team=<region>:<team_uuid> with an RC-created isolated ordinary account/team, real authenticated browser session and Stripe's documented test card. Preserve the team's existing commercial anchor, existing customer/subscription and Free assignment ID before starting. Ensure no custom limits obscure the assignment-limit fixture. Do not replace an existing account's anchor or seed a successful payment/assignment to manufacture evidence.

1. Confirm public catalog is released v2. In a separate PREPARING phase, direct known pro_monthly fresh operation must return409 without journal/choice/provider price/session creation. Matching already accepted team/actor/offer/operation replays before gate denial.
2. Start Pro through the real page and capture browser operation UUID, HTTP status, backend upgrade UUID, original anchor/period, quoted timestamp and rounded prorated amount. Independently compute floor((25000*remaining+floor(duration/2))/duration), where duration=period_end.Unix()-period_start.Unix() and remaining=period_end.Unix()-quoted_at.Unix(), using the stored second timestamps; never assume the mid-month quote is12500. Complete the Stripe TEST Checkout. No discount first; repeat separate fixtures for partial/full first-payment promotions, including required off-session card setup for zero-total payment.
3. Observe the deployed recovery/worker reaching active. Query the real journal, assignment, team limits and contract coherently; compare Gin GET /billing/plan for assignment identity and effective limits. The recurring contract starts_at must equal original period_end, charge25000/month, entitlement_anchor and account anchor must equal original anchor. Grant must be paid/metered-only, equal gross quoted amount and non-expiring. Promotion changes cash, not quoted credit; recurring subscription must have no discount. Verify only one matching payment/session/grant from complete provider inventory, not merely one database row.
4. Capture a coherent root-only activation snapshot using the read-only SQL below. Supply UPGRADE_ID from the actual accepted operation. The script's optional upgrade verification reads only this selected snapshot plus provider objects; it does not connect to the DB or print raw Checkout URLs/client secrets/payment details.

```sh
UPGRADE_ID=REPLACE_WITH_ACTUAL_UPGRADE_UUID
sudo install -m 600 /dev/null "$STATE/pro-qa/active-upgrade.json"
sudo docker compose --project-name superserve-devbox \
  --file "$ROOTONBOX/deploy/devbox/services/compose.yml" --env-file "$STATE/services.env" \
  exec -T db psql -X -qAt -U supabase_admin -d postgres -p 54322 \
  -v ON_ERROR_STOP=1 -v upgrade_id="$UPGRADE_ID" <<'SQL' | sudo tee "$STATE/pro-qa/active-upgrade.json" >/dev/null
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SELECT row_to_json(e) FROM (
 SELECT u.id,u.state,u.customer_id,u.session_id,u.grant_id,u.subscription_id,u.amount_cents,u.contract_id,
  b.commercial_billing_anchor=u.anchor AND c.entitlement_anchor=u.anchor AS anchor_preserved,
  c.starts_at=u.period_end AS contract_starts_at_anchor,
  c.charge_amount_cents AS contract_fee,c.charge_interval_months AS contract_months,
  pa.id AS assignment_id,pa.limits,
  pa.contract_id=u.contract_id AND pa.plan_key='pro_monthly' AND pa.source='checkout' AS assignment_matches,
  NOT t.limits_customized AND t.max_sandboxes=(pa.limits->>'max_concurrent')::integer
   AND t.max_template_vcpu=(pa.limits->>'max_vcpu')::integer
   AND t.max_template_memory_mib=(pa.limits->>'max_memory_mib')::integer
   AND t.max_template_disk_mib=(pa.limits->>'max_disk_mib')::integer AS limits_match,
  u.initial_assignment_id,u.anchor,u.period_start,u.period_end,u.quoted_at,c.starts_at
 FROM billing_pro_upgrade u JOIN team_billing_account b ON b.team_id=u.team_id
 JOIN team t ON t.id=u.team_id JOIN billing_commercial_contract c ON c.id=u.contract_id
 JOIN team_plan_assignment pa ON pa.team_id=u.team_id AND pa.effective_to IS NULL
 WHERE u.id=:'upgrade_id'::uuid
) e;
COMMIT;
SQL
sudo python3 "$ROOTONBOX/deploy/devbox/pricing-pro-provider-qa.py" \
  --state-dir "$STATE" --expected-account "$EXPECTED_TEST_ACCOUNT" \
  --upgrade-json "$STATE/pro-qa/active-upgrade.json" --output "$STATE/pro-qa/active-verification-1.json"
```

Use shell pipefail in the RC execution shell; a failed SQL command or empty snapshot is a failure, not acceptance. The source snapshot verifies four team caps; max_templates is assignment-backed and must also match the authenticated plan response. This is a post-commit coherence observation, not proof that a transaction could not partially commit under fault: retain the separate DB commit-fault regression evidence.

5. Response-loss fixture: intercept/discard the first application response AFTER durable acceptance, then retry the exact captured operation/team/actor/offer with the same owned returns. Expect the same operation/session and no duplicate resources. Do not simulate this by creating a fresh UUID or transport a secret through a proxy. Repeat after RC's supported dev-only rollback closes fresh entry: accepted same-key recovery finishes OFF, fresh-key409, changed actor/offer/team409. Compare journal/session/grant/current assignment IDs before and after. Restore/release only using RC/W5's current operator protocol, never raw snapshot-remaining shortcuts.
6. Predecessor fixture: in a separate accepted-but-not-yet-committed checkout, use the supported staff/customer path to establish a newer legitimate assignment where allowed. Recovery must not overwrite it, and any already verified cash/grant remains durable pending/manual review. Do not disable guards or update assignment rows to force an impossible production interleaving. Accepted-absence and commit-fault cases remain real DB/provider-HTTP fixtures if the deployed UI cannot safely construct them; do not relabel those as real-provider evidence.
7. W1 release/reentry: use the real current-source terminal release route. Preserve trial/promo/payment/anchor history. New checkout is eligible only through CURRENT applied assignment/change/linked release and paid ended canceled-source proof, not an old release. incomplete_expired exit does not qualify for this current paid-reentry exception. Deliver an unseen late released invoice and verify durable queue before ACK, source-only cash/credit reconciliation and no subscription/assignment/new-purchase rewrite. Expired original monthly grant+late cash is review_required liability, not a replacement grant.

## First full fee and time-domain boundary

Do not claim advancing a provider clock executes application renewals. This binary's h.nowUTC defaults to wall time and DB clock_timestamp remains real time. The isolated2027/2028 clocks above are not installed on application teams. A real Pro Checkout fixture must have application-consistent current timestamps. The first full250 application invoice/contract/credit projection at the original next date requires either that real boundary, or a separately reviewed isolated clock-aware harness covering BOTH handler and DB timing. Do not edit production host time, backdate signed receipts, rewrite journal periods or patch the live binary to manufacture it. Until the runtime time-domain path is established, label this end-to-end renewal gate PENDING even if provider calendar/control and immediate cash/grant QA pass.

Keep evidence separate: exact deployed source/schema/worker pins; actual provider endpoint/status/request IDs; browser/payment/plan snapshots; fault-injection DB fixtures; calendar-only receipts; future-boundary worker evidence. No mock or source review substitutes for actual capability checks.

Reference contracts: https://docs.stripe.com/api/test_clocks/advance (advance asynchronously, wait ready, bounded by two billing intervals); https://docs.stripe.com/api/subscriptions/update (pause collection is an update); https://docs.stripe.com/testing (test payment methods).

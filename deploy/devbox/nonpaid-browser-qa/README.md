# Portable nonpaid email browser QA

RC alone executes this harness on the explicitly authorized devbox, or an existing authorized runner with private tunnels to that devbox. It does not deploy, reset, change flags/schema, seed identities, bypass Auth, or test payments/Google/staff approval. Two fresh `example.com` identities are created through the UI and intentionally retained for RC inspection. Passwords exist only in harness/browser memory; do not enable shell xtrace, browser tracing, HAR/video, storage-state export or debug logging. RC's SMTP sink must not relay externally. Application/SMTP/Auth operational logs are outside the harness; RC must protect them separately.

Requires Node >=20, npm, Playwright Chromium, reachable Console/Auth/Mailpit loopback ports and read-only database access through an existing onbox command. Browser/dependency installation is permitted only on the explicit devbox target; on an authorized runner use an already installed compatible browser/runtime or have RC obtain separate runner installation authorization. The script itself never installs software.

Copy this directory to the runner. On the explicitly selected devbox only:

```sh
npm ci --ignore-scripts
npx playwright install chromium
```

Use existing system dependencies; if Chromium requires OS packages, RC decides installation on that explicit devbox. No public ingress is needed.

RC supplies these files privately, mode600:

`runtime-proof.json` must be freshly measured from actual running services and adopted state, not copied from build arguments or this example. This is RC-supplied runtime evidence, not independently measured by the browser script. `observed_at` must be within30minutes. Backend/Console fields must be full exact40-character SHAs; arguments below must match. The harness requires actual released/publicv2 for this run and does not perform release/adoption.

```json
{"backend_sha":"<measured40hex>","console_sha":"<measured40hex>","observed_at":"<freshUTC-ISO>","phase":"released","pricing_version":"v2","epoch":"<actualUUID>","email_sink_only":true,"target_devbox":true}
```

`free-limits.json` is the expected effective Free limits from the exact reviewed published dev catalog, independently checked by RC. Include all effective fields being asserted, with numbers/null as appropriate. At least positive max_concurrent is required; do not copy observed response into expected values after a failure.

```json
{"max_concurrent":<expected>,"max_vcpu":<expected>,"max_memory_mib":<expected>,"max_disk_mib":<expected>,"max_templates":<expected>}
```

`psql-argv.json` is an argv array for an EXISTING authorized read-only command accepting SQL on stdin and outputting only unaligned tuple JSON. It must connect to the exact target DB with auth.users and public tables. Supply `-X -A -t -q -v ON_ERROR_STOP=1` to psql; use existing private env/socket credentials, never passwords in argv. For example, adapt the container name from actual RC runtime (the placeholder below is not a pin):

```json
["docker","exec","-i","<RC-measured-db-container>","psql","-XAtq","-v","ON_ERROR_STOP=1","-U","<authorized-role>","-d","postgres","-p","54322"]
```

The harness sends a BEGIN READ ONLY transaction and a10second statement timeout. It reads only aggregate counts from auth.users, team_member, team_plan_assignment, then rolls back. No fixture credentials, raw user metadata, cookie, token, mail body or SQL stderr is retained. A permission/schema mismatch fails the receipt rather than skipping DB proof.

Run with actual measured pins passed explicitly; do not use the literal placeholders:

```sh
umask 077
node nonpaid-signup.mjs \
  --console http://localhost:3000 \
  --auth http://localhost:54321 \
  --mailpit http://localhost:8025 \
  --backend-sha "$MEASURED_BACKEND_SHA" \
  --console-sha "$MEASURED_CONSOLE_SHA" \
  --runtime-proof /private/runtime-proof.json \
  --free-limits /private/free-limits.json \
  --psql-argv /private/psql-argv.json \
  --output /private/nonpaid-qa-result
```

Only loopback HTTP origins are accepted. Third-party browser resources/analytics/Calendly and explicit paid-write paths are blocked, not simulated. Bot checks requiring external network may therefore fail: report the exact QA step instead of disabling server guards. The raw failure URL/message is never emitted; network evidence records only origin category, allowlisted pathname, status and safe cancellation classification, with no queries, headers or bodies. Expected unauthenticated401s are recorded; internal5xx/non-cancelled network failures fail the run.

Assertions: actual UI signup and check-email state; actual sink confirmation link followed unchanged in same browser context; observed Console callback and expected destination; fresh-context password login; Free GET /api/billing/plan200 with kind/keyFree and expected effective limits; one confirmed Free user, one owned team and one current signup-Free assignment; Enterprise confirmation/login pending UI, dashboard access denied, plan endpoint denied, server-owned pending metadata with zero memberships, no approval email. Email Enterprise callback may sign out deliberately; this is expected. No endpoint wrappers, paid checkout, staff permissions, preparing-state exception or worker/adoption evidence is inferred from this released-state browser run.

Artifacts: receipt.json contains measured timings/checks, sanitized network outcomes, RC runtime pin receipt and aggregate DB counts. PNGs capture only known successful UI states, masking all inputs/iframes and generated email text. Unknown failure pages are not screenshotted to avoid token leakage. No Mailpit screenshots or HTML dumps are taken. Verify masks before sharing; keep all outputs private until reviewed. Final stdout is one sanitized result record; nonzero exit is FAIL, not partial success. Missing browser/dependencies/prerequisites produce FAIL, never a fabricated E2E pass.

Return receipt.json plus reviewed redacted PNGs and execution command with nonsecret pins. Preserve the test rows and sink messages for RC ownership; this harness performs no cleanup/deletion. Existing PM core smoke and W2 serializer proof remain distinct evidence.

Mailpit API contract: https://mailpit.axllent.org/docs/api-v1/

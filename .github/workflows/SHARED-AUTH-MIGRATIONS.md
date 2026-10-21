# Shared Auth authority migration

API and Terraform control-plane deployments run the existing shared Auth executor
before regional migrations: `authority`, `claims`, then `approvals`. Staging uses
its isolated authority; both production regions use the primary production
authority (`use4`). Proxy-only deployments retain their regional prerequisite.
All executors share the existing `database-migrations` queue. Do not add new shared versions to
`supabase_migrations.schema_migrations`: the ordinary runner has no matching input
and will refuse subsequent deployments.

The release contract must establish the actual project used by each deployed regional
`PROMOTION_AUTH_DATABASE_URL` through the approved secret-safe operational path.
Record only nonsecret target evidence. A staging-named secret does not prove an
isolated staging database. If staging points to production Auth, stop and establish
an explicitly approved scope or isolated staging target before any writes or
activation tests. Runtime retargeting requires coordination with its owning release.
Deployments do not repeatedly inspect runtime secrets or change their configuration.
The migration executor independently validates its actual privileged connection's
project, required SQL, ledger and catalog on every invocation.

The reusable prerequisite preserves the exact source and successful push-CI gate.
Compatible manual branch staging uses main's identical migration bundle; changes
to the shared helper or prerequisite source cannot reuse main's verification.
Only the Proxy caller may omit the shared prerequisite.

After shared and all selected regional migrations succeed, an executed step emits
a receipt binding the run, attempt, deployment and migration revisions, authority
target, required versions and source bundle. Each API-bearing full Terraform apply
and API deployment checks that receipt before changing the consumer. A failed-job
only rerun cannot reuse an earlier attempt's receipt: **re-run all jobs** to execute
the prerequisite again. These workflow receipts do not rewrite historical database
ledger revisions; identical previously applied content remains valid after live
executor verification. No receipt attests continuous runtime configuration state.

The manual **Apply shared Auth migration** workflow remains available for inspection
and recovery through separately selected operations.

Select `main`, action `inspect`, migration `authority`, the verified target (`staging` or `use4`), and its
verified runtime project. The workflow requires successful push CI for its exact
revision. It selects the existing privileged database connection for that target,
validates the connection's project, and checks existing Auth setup, roles, history,
and catalog. West is not a supported authority target.

After reviewing that result and confirming the target authorization, dispatch
`apply` at the same reviewed revision. GitHub environment approvals and the normal
`database-migrations` queue apply. The executor accepts only the pinned migration
SQL; it has no arbitrary file or SQL input. A short transaction locks the shared
executor, applies the schema, narrows inherited `service_role` grants to the
source's intended SELECT/INSERT/UPDATE access, and verifies RPC permissions. It
stores both original SQL and exact executed SQL (including the fixed ACL
normalization), SHA-256 of the original, source revision, and catalog in the private
`shared_auth_migrations.schema_migrations` ledger. It does not create an approval,
enroll a customer, or modify Stripe.

An identical retry verifies and skips the applied migration. Source/history/catalog
mismatch, an unrecorded existing schema, or an unknown ledger version stops the
executor without repair. After a timeout or uncertain commit, run `inspect` before
retrying. Never infer failure from a missing acknowledgement and replay SQL manually.
After authority application is verified, select migration `claims` and repeat
`inspect`, review, and separately authorized `apply`. Claims require the existing
authority receipt and unchanged catalog; this operation never applies authority
implicitly. It verifies the exact source hash and removes only the asserted outer
BEGIN/COMMIT from its execution copy. The claim schema, Auth lifecycle trigger,
and private receipt then commit atomically. The receipt retains original bytes
and executed SQL, including fixed restrictions on inherited trigger-function
grants. Only `promotion_evidence_proxy` may execute the claim RPC; runtime/client
roles have no direct claim-table access.

Inspection verifies all known receipts, claim indexes and the Auth lifecycle
trigger. Unknown versions, missing dependencies and unrecorded objects stop
execution. Use the current reviewed executor after applying claims: the older
authority-only executor deliberately refuses the additional ledger version.
Future migrations still require an explicit extension and review.

After claims, select `approvals` for the staff decision and notification schema.
It requires the claim receipt and verifies the intentional replacements of the
shared activation functions. Automated deployment prerequisites use the same
executor and private ledger, and stop if any application or verification fails.

Verify `inspect` reports `applied`, regional history is unchanged, and all deployed
regions use the verified RPC authority. Then perform the
[deployed staging activation checks](../../docs/billing/yc-annual-activation.md#recovery-and-validation).
Local database tests establish atomicity, privilege checks and regional-runner
compatibility; they do not establish staging card setup, product access, grants or
overage invoice behavior. Production customer activation remains a separate action.

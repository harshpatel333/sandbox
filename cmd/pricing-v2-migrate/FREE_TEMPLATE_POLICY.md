# Free template policy migration

The one new coordinated forward migration is `20261025150000_free_template_policy.sql`. It owns catalog constraints and immutable Free0 publication, allowance-origin preservation, the fenced human assignment successor, canonical provenance repair and release checks. It preserves the existing published `20261025140000_free_system_templates.sql` byte-for-byte (SHA256 `4b60836a4b1b139f44f3ae5b8da0e8c5d21d5fc9972311ffcaebe8f98f373f45`) as its required prefix. It neither recreates existing provenance columns nor rewrites that migration. The forward SQL checks the expected column shape and prior guard body and rejects unsupported prefixes with `55000`.

Do not assemble old `010000`, withdrawn catalog `060000`, or superseded split-history `130000` candidates. Source absence does not prove any artifact was never applied. Reconcile each environment's migration ledger and SQL hashes before application. Different previously applied artifacts require separately reviewed forward reconciliation; never overwrite an applied migration. Global non-application on dev environments remains unverified.

Before migration, bind the existing adoption schema to the canonical region/system team with the existing human prepare command. Unknown foreign template ownership causes migration to fail closed; it is never classified as system by exclusion.

This migration requires a coordinated, quiesced API/schema cutover. Old API writers omit template provenance and fail closed for Free system-template creation after the schema changes. It is not compatible with an ordinary schema-first rolling deployment. Drain and restart old in-flight acceptance requests as well as stopping new traffic: a request may have loaded old Free terms before taking its team lock. Old binaries or requests retaining pre-migration terms must not survive the cutover. Provenance backfill takes schema/data locks. Source approval alone does not authorize this cutover.

## Existing assignments

Catalog publication does not rewrite existing Free assignments. The operator must inspect them and apply a separate fenced transition:

```sh
pricing-v2-migrate policy-report
pricing-v2-migrate prepare --confirm --actor "$ACTOR" --reason "$REASON"
pricing-v2-migrate policy-apply --confirm --actor "$ACTOR" --reason "$REASON" \
  --team "$TEAM_ID" --expected-assignment "$ASSIGNMENT_ID"
pricing-v2-migrate policy-report
```

Use the existing authenticated operator switch transition to enter raw v2 before preparing. Public entry remains closed during preparation. The commands use the region database and configured system team from the existing CLI environment. Run them only within an explicitly authorized environment.

`policy-apply` uses a fresh READ COMMITTED transaction and calls `billing_migrate_free_template_assignment(text,uuid,uuid,uuid,text,text) RETURNS uuid`. The function is available only to a privileged human database caller. It is not an HTTP or service-role RPC. Actor and reason identify that human operation; they do not authenticate an HTTP caller.

The region and system-team arguments must match the prepared database even on replay. The lock order is switch/adoption SHARE, published Free0 version SHARE, account UPDATE NOWAIT, team UPDATE NOWAIT, account rescan, then current assignment UPDATE NOWAIT. A matching persisted transition replays before checking raw pricing or the current catalog. A fresh transition requires raw v2, matching prepared region, and the expected current Free assignment. It refuses unresolved financial authority, allowance retries, and unfinished pause targets. Any plan-change history currently requires owner reconciliation; the function refuses it rather than invalidating cancellation or undo ownership.

The transition closes the predecessor and appends its successor at the same boundary. Frozen terms and limits remain unchanged on the predecessor. The new assignment changes the template cap and version metadata while retaining the prior allowance terms and original allowance start. It does not reset the stored allowance window, consumption, exhausted-until time, credit grants, or ledgers. A prior verified credit-source link is retained in the transition audit before its current assignment reference follows the successor. The transition, audit and credit-source reference commit atomically.

SQLSTATE `55000` means eligibility, isolation or ownership must be resolved. `55P03` means a lock owner must finish before a deliberate retry. `22023` means required identifiers or human attribution are invalid. Database privilege denial remains `42501`. The CLI does not retry an ambiguous COMMIT: inspect the current assignment and immutable transition receipt before deciding whether to retry the same predecessor.

Release also refuses scheduled or applied old-Free plan changes, even when every current assignment is already present or Free0. Resolve their owning commitment before release; shared commitment writers stay ungated.

A zero unassigned count is insufficient. Every live nonsystem Free assignment must have a frozen numeric `max_templates=0` before release, including teams already assigned under the old Free version. Keep unresolved teams visible in `policy-report`; do not delete financial or plan-change history to make them eligible.

## Release evidence

The existing prepare, adoption apply, capture, consuming-worker probe, cache drain, explicit release and rollback checks remain required. Add this object to the existing evidence JSON using real evidence references:

```json
{
  "free_template_policy": {
    "existing_assignments_verified": true,
    "custom_policy_reference": "approved-existing-custom-use-resume-delete-policy-reference",
    "runtime_reference": "assembled-runtime-policy-regression-reference"
  }
}
```

These fields are human attestations, not substitute worker or runtime evidence. Disposable fixtures do not authorize an actual release. The evidence must identify the sole assembled migration/source, published Free0 version, retained historical Free version, and current-assignment inventory. Verify custom create/build, new custom-template sandbox creation and snapshot forks reject while system-template creation and Free size enforcement still work. Include a predecessor-to-successor test proving an exhausted allowance and financial history survive unchanged.

Existing custom data is retained. Current code continues to allow an existing custom sandbox to resume within Free size limits and retains existing authenticated read/delete behavior. This describes the implementation; it does not infer final approval of existing-custom use, resume or deletion policy. Resolve that policy explicitly before providing its release reference.

Raw pricing OFF preserves assignments, committed work, exits, undo and accepted build replay. Runtime custom-admission denial is `SS004`, mapped to HTTP 403 `custom_templates_unavailable`; the existing current-plan lock refusal remains retryable `SS003`. No new lifecycle database lookup or roundtrip is introduced.

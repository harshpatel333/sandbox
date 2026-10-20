# Additive production billing monitoring

This root owns six new alert policies in the `production/billing-alerts`
prefix of the production Terraform state bucket, using the default workspace.
It does not own the five pre-existing billing policies, create another
paid-missing policy, or change staging or issuance settings. Preserve the
existing billing notification channel.

The root and module live outside the ordinary `infra/**` deployment trigger.
Normal regional applies use different state prefixes and cannot delete these
policies. CI validates and plans this root. Merging source does not deploy it;
the **Billing Alert Rollout** workflow only accepts manual dispatches on main.
Application changes in a mixed push retain their normal deployment behavior.

## Initial deployment

1. Coordinate with the East issuance operator. Coverage liveness must exist
   before enabled background workers start, but a never-seen counter can alert
   after one minute. Traffic cutover is not the worker activation boundary.
2. Dispatch `Billing Alert Rollout` with `operation=plan` and the full reviewed
   main SHA as `expected_revision`. Review the saved plan, which must contain
   only the six alerts. Record the run ID and binary SHA256 from its summary.
3. When the activation sequence is ready, dispatch `operation=apply` at the
   same main SHA with that plan run ID and digest. The workflow checks source
   provenance, digest and resource boundaries, then applies that saved binary
   with state locking. It never regenerates an unreviewed plan during apply.
   If source or state has advanced, produce and review a fresh plan.
4. Record the new policy IDs and enabled state, verify the billing route and
   East coverage progress, and confirm the five existing policies are unchanged.
   Leave West issuance disabled. No synthetic notification is required.

The initial rollout guard permits creates and no-ops only. Updates, imports,
deletes, replacement, other resources and changed notification routes fail
closed. A later maintenance change needs its own reviewed deployment contract.
The workflow uses the existing production federated identity, read-only GitHub
contents/actions permissions and token issuance permission; it does not grant
new cloud IAM. Its cloud identity needs alert-policy creation/read, log
notification-rule creation, channel read and state-object/lock access.

## Ownership follow-up

Do not deploy an older regional composition that declares the same six alerts.
Future adoption of the five existing policies must reconcile this dedicated
state owner first, preserving these resource identities rather than duplicating
them. The existing paid-missing sustained-condition delay is intentionally
retained until that separately coordinated adoption.

See [the entitlement response runbook](../../docs/pro-billing.md#entitlement-alert-response)
for incident handling and the distinction between observation and recovery.

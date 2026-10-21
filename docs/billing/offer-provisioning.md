# Initial offer provisioning journal

The journal persists one operation per team and immutable assignment. Each
customer, waiver, fee, usage creation and usage-hold request has a saved body, provider account,
API version and stable idempotency key before dispatch. An uncertain request is
recovered by reading the original provider evidence; it is never automatically
posted again, including after provider idempotency retention expires.

This is an integration foundation. No production acceptance, provider, activation
or readiness adapter is registered. The API startup hook returns without database
access when any adapter is missing. Local tests use synthetic authority and
provider adapters and do not establish provider or runtime readiness.

## Adapter contract

`provisioning.Admission` resolves durable acceptance from server storage using only
team and assignment IDs. Its short transaction rechecks pinned terms, current
approval, tenant, region, rollout, existing customer/checkout conflicts and operator
authority. Acceptance never determines the commercial start time. Provider price
terms are frozen separately in `ProviderTerms`; they must not rewrite the opaque
acceptance snapshot or be remapped from ambient configuration on replay.

Before each create, the service commits the dispatch marker, then obtains a fresh
shared dispatch guard. That guard rechecks the exact acceptance and holds shared
authority through one bounded provider attempt. The callback takes no regional
row locks. Both admission and dispatch callbacks receive the guard-owned context;
all SQL, finalization and the provider POST must use that context. Adapters must
not retry creates internally.

`provisioning.Provider` prepares exact immutable requests and recovers complete
same-identity evidence. Its final verification must establish the entire supported
fee/usage pair, original calendar, bounded initial waiver and debt/grant inventory.
The usage collection hold is a separate journaled write; activation cannot precede
its confirmed receipt. Verification and recovery must not apply that hold themselves.
No test-only response is valid production evidence.

The unregistered HTTP transport sends saved bodies without POST replay or redirects,
checks the pinned account/mode/API version before the dispatch marker, and bounds
response sizes. Failed preflight reads leave the request prepared and retryable;
the adapter must retain the same immutable client through preflight and POST. Its raw
response is not a confirmed receipt: the concrete adapter still must validate and
project identity/economics before passing evidence back to the journal.

`provisioning.Activation.Finalize` runs in the guarded regional transaction. The
adapter must bind verified commercial authority, enroll invoice reconciliation,
materialize the assignment, and hand off to existing entitlement issuance
atomically. It must not issue another allowance or perform network I/O.

`Readiness` returns a fresh observation for that same assignment, customer and
usage subscription. Ready requires current authority, verified pair, materialized
assignment, invoice enrollment, known inventory and clear debt, a confirmed current
eligible grant, positive fresh funding and existing sandbox billing eligibility.
The status projection is informational; the existing billing gate still controls
access. Exhaustion does not auto-resume a sandbox when funds return.

## Recovery and controls

Short row-lock transactions serialize request preparation, the one permitted
dispatch transition, receipt confirmation and controls. Concurrent reconcilers may
read provider inventory, but only the caller that commits `prepared -> dispatched`
may send the POST. No database connection is reserved across provider I/O, so the
journal works with transaction pooling and a single application pool connection.

Controls persist their desired hold/revoke even when an old dispatch is unresolved.
Such controls return an unknown-outcome error and status exposes `held`, `revoked`
and `control_pending` with `ready=false`. No subsequent step or activation is
authorized. Read-only recovery records the original effect and clears only the
pending-outcome marker. A completed control acknowledgment means the old attempt
was accounted for, not that a remote effect was canceled or refunded. Confirmed
commercial authority continues to use the existing administrative revoke path.

The worker processes at most two operations sequentially per batch with a bounded
context per operation. Recovery remains scheduled for held/revoked operations with
unresolved dispatched requests. Requests and audit history must be retained during
rollback. Schema must precede writers; adapter registration and rollout enablement
require separate integrated verification.

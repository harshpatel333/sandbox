//go:build integration

package integration

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/superserve-ai/sandbox/internal/db"
)

func assignFreePlan(t *testing.T, teamID uuid.UUID, since time.Time) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO team_plan_assignment (team_id, plan_key, kind, terms, limits, effective_from, source, reason)
		VALUES ($1, 'free', 'free',
		        '{"allowance":{"amount_cents":13,"period":"day_window","on_exhausted":"pause","compute":{"seconds":7200,"vcpu":1,"memory_gib":1}}}',
		        '{"max_sandboxes":1,"max_concurrent":1,"max_vcpu":1,"max_memory_mib":1024,"max_disk_mib":4096}', $2, 'migration', 'test')`, teamID, since); err != nil {
		t.Fatalf("assign free plan: %v", err)
	}
}

func seedComputeInterval(t *testing.T, teamID, sandboxID uuid.UUID, vcpu, memoryMib int, started time.Time, ended *time.Time) {
	t.Helper()
	reason := any(nil)
	if ended != nil {
		reason = "paused"
	}
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO sandbox_compute_billing_interval (sandbox_id, team_id, vcpu_count, memory_mib, started_at, ended_at, end_reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, sandboxID, teamID, vcpu, memoryMib, started, ended, reason); err != nil {
		t.Fatalf("seed compute interval: %v", err)
	}
}

func eligible(t *testing.T, teamID uuid.UUID) bool {
	t.Helper()
	ok, err := testQueries.IsTeamSandboxBillingEligible(context.Background(), teamID)
	if err != nil {
		t.Fatalf("eligibility: %v", err)
	}
	return ok
}

func cents(t *testing.T, row db.GetFreeAllowanceRow) (allowance, used float64) {
	t.Helper()
	a, err := row.AllowanceUsd.Float64Value()
	if err != nil {
		t.Fatal(err)
	}
	u, err := row.UsedUsd.Float64Value()
	if err != nil {
		t.Fatal(err)
	}
	return a.Float64 * 100, u.Float64 * 100
}

func TestIntegration_FreeAllowance_UnassignedTeamKeepsLegacyRules(t *testing.T) {
	ctx := context.Background()
	trial, _ := seedTeamAndKey(t)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO team_credit_grant (team_id, amount_usd, remaining_usd, reason, expires_at)
		VALUES ($1, 5, 5, 'signup trial credit', now() - interval '1 second')`, trial); err != nil {
		t.Fatalf("seed expired trial: %v", err)
	}
	if eligible(t, trial) {
		t.Fatal("an unassigned team with an expired trial became eligible")
	}
	sbx := seedActiveSandbox(t, trial, "free-unassigned")
	seedComputeInterval(t, trial, sbx, 4, 8192, time.Now().Add(-10*time.Hour), nil)
	if _, err := testQueries.GetFreeAllowance(ctx, trial); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("free allowance for an unassigned team = %v, want no rows", err)
	}
	if blocked, err := testQueries.RefreshFreeAllowance(ctx, trial); err != nil || blocked {
		t.Fatalf("refresh for an unassigned team = %v, %v; want not blocked", blocked, err)
	}
	var reason *string
	if err := testPool.QueryRow(ctx, `SELECT billing_pause_reason($1, $2)`, trial, sbx).Scan(&reason); err != nil || reason == nil || *reason != "billing_ineligible" {
		t.Fatalf("pause reason = %v (err %v), want billing_ineligible", reason, err)
	}
}

func TestIntegration_FreeAllowance_FreeTeamNeedsNoCardOrCredit(t *testing.T) {
	ctx := context.Background()
	team, _ := seedTeamAndKey(t)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO team_credit_grant (team_id, amount_usd, remaining_usd, reason, expires_at)
		VALUES ($1, 5, 5, 'signup trial credit', now() - interval '1 second')`, team); err != nil {
		t.Fatalf("seed expired trial: %v", err)
	}
	assignFreePlan(t, team, time.Now().Add(-time.Hour))
	if !eligible(t, team) {
		t.Fatal("a Free team without card or credit is not eligible")
	}
	row, err := testQueries.GetFreeAllowance(ctx, team)
	if err != nil {
		t.Fatal(err)
	}
	allowance, used := cents(t, row)
	if row.HasWindow || math.Abs(allowance-13.32) > 1e-9 || used != 0 || row.Exhausted {
		t.Fatalf("fresh Free allowance = %+v (allowance %v used %v), want no window, 13.32c, nothing used", row, allowance, used)
	}
}

func TestIntegration_FreeAllowance_CapPausesAndRefusesUntilReset(t *testing.T) {
	ctx := context.Background()
	team, _ := seedTeamAndKey(t)
	now := time.Now()
	assignFreePlan(t, team, now.Add(-48*time.Hour))
	sbx := seedActiveSandbox(t, team, "free-capped")
	started := now.Add(-3 * time.Hour)
	seedComputeInterval(t, team, sbx, 1, 1024, started, nil)

	blocked, err := testQueries.RefreshFreeAllowance(ctx, team)
	if err != nil || !blocked {
		t.Fatalf("refresh after 3h of 1 vCPU + 1 GiB = %v, %v; want blocked", blocked, err)
	}
	if eligible(t, team) {
		t.Fatal("a capped Free team is still eligible")
	}
	admission, err := testQueries.GetTeamSandboxAdmission(ctx, team)
	if err != nil || !admission.AllowanceBlocked || admission.AllowanceResetsAt.Sub(started.Add(24*time.Hour)).Abs() > time.Millisecond {
		t.Fatalf("admission %+v (err %v), want the cap until window start + 24h = %v", admission, err, started.Add(24*time.Hour))
	}
	row, err := testQueries.GetFreeAllowance(ctx, team)
	if err != nil {
		t.Fatal(err)
	}
	allowance, used := cents(t, row)
	if !row.HasWindow || !row.Exhausted || used < allowance {
		t.Fatalf("capped allowance = %+v, want an exhausted window", row)
	}
	teams, err := testQueries.ListTeamsWithBillingPauseDue(ctx, db.ListTeamsWithBillingPauseDueParams{BatchLimit: 100000})
	if err != nil || !slices.Contains(teams, team) {
		t.Fatalf("teams with pauses due = %v (err %v), want the capped team", teams, err)
	}
	claimed, err := testQueries.ClaimBillingPauseSandbox(ctx, db.ClaimBillingPauseSandboxParams{ID: sbx, TeamID: team, LeaseSeconds: 90})
	if err != nil || claimed.PauseOpTrigger == nil || *claimed.PauseOpTrigger != "free_allowance_exhausted" {
		t.Fatalf("claim = %+v, %v; want trigger free_allowance_exhausted", claimed, err)
	}

	if _, err := testPool.Exec(ctx, `
		UPDATE sandbox_compute_billing_interval SET started_at = started_at - interval '24 hours', ended_at = now() - interval '22 hours'
		WHERE sandbox_id = $1`, sbx); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `
		UPDATE billing_free_allowance SET window_started_at = window_started_at - interval '24 hours',
		       resets_at = resets_at - interval '24 hours', blocked_until = blocked_until - interval '24 hours'
		WHERE team_id = $1`, team); err != nil {
		t.Fatal(err)
	}
	if !eligible(t, team) {
		t.Fatal("a Free team is still refused after its window reset")
	}
	row, err = testQueries.GetFreeAllowance(ctx, team)
	if err != nil || row.HasWindow || row.Exhausted {
		t.Fatalf("after reset = %+v (err %v), want no open window until the next start", row, err)
	}
}

func TestIntegration_FreeAllowance_WindowStartsAtFirstStartAfterPreviousWindow(t *testing.T) {
	ctx := context.Background()
	team, _ := seedTeamAndKey(t)
	now := time.Now()
	assignFreePlan(t, team, now.Add(-72*time.Hour))
	sbx := seedActiveSandbox(t, team, "free-window")
	first := now.Add(-50 * time.Hour)
	firstEnd := first.Add(30 * time.Minute)
	seedComputeInterval(t, team, sbx, 1, 1024, first, &firstEnd)
	second := now.Add(-10 * time.Hour)
	secondEnd := second.Add(20 * time.Minute)
	seedComputeInterval(t, team, sbx, 1, 1024, second, &secondEnd)

	row, err := testQueries.GetFreeAllowance(ctx, team)
	if err != nil {
		t.Fatal(err)
	}
	if !row.HasWindow || row.WindowStartedAt.Sub(second).Abs() > time.Millisecond {
		t.Fatalf("window started %v, want the first start after the earlier window ended (%v)", row.WindowStartedAt, second)
	}
	allowance, used := cents(t, row)
	want := 20 * 60 * (0.000014 + 0.0000045) * 100
	if math.Abs(used-want) > 0.01 || row.Exhausted || allowance <= used {
		t.Fatalf("used %v cents, want %v from the current window only", used, want)
	}
}

func TestIntegration_FreeAllowance_UsageBeforeFreeIsNotCounted(t *testing.T) {
	ctx := context.Background()
	team, _ := seedTeamAndKey(t)
	now := time.Now()
	since := now.Add(-time.Hour)
	assignFreePlan(t, team, since)
	sbx := seedActiveSandbox(t, team, "free-carried")
	seedComputeInterval(t, team, sbx, 8, 32768, now.Add(-5*time.Hour), nil)

	row, err := testQueries.GetFreeAllowance(ctx, team)
	if err != nil {
		t.Fatal(err)
	}
	if row.WindowStartedAt.Sub(since).Abs() > time.Millisecond {
		t.Fatalf("window started %v, want the Free assignment %v", row.WindowStartedAt, since)
	}
	if !row.Exhausted {
		t.Fatal("an 8 vCPU sandbox running for an hour on Free did not exhaust the allowance")
	}
}

func TestIntegration_TeamPauseAll_RetriesRevertsAndSparesLaterResumes(t *testing.T) {
	ctx := context.Background()
	team, _ := seedTeamAndKey(t)
	before := seedActiveSandbox(t, team, "pause-all-before")

	if n, err := testQueries.RequestTeamPauseAll(ctx, db.RequestTeamPauseAllParams{TeamID: team, Reason: "plan_switch_free"}); err != nil || n != 1 {
		t.Fatalf("request named %d sandboxes (err %v), want the one running", n, err)
	}
	if n, err := testQueries.RequestTeamPauseAll(ctx, db.RequestTeamPauseAllParams{TeamID: team, Reason: "plan_switch_free"}); err != nil || n != 0 {
		t.Fatalf("repeated request named %d more (err %v), want none", n, err)
	}
	after := seedActiveSandbox(t, team, "pause-all-after")
	due := func() []uuid.UUID {
		t.Helper()
		ids, err := testQueries.ListBillingPauseSandboxes(ctx, db.ListBillingPauseSandboxesParams{TeamID: team, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	if ids := due(); !slices.Equal(ids, []uuid.UUID{before}) {
		t.Fatalf("due = %v, want only the sandbox running at the request", ids)
	}
	teams, err := testQueries.ListTeamsWithBillingPauseDue(ctx, db.ListTeamsWithBillingPauseDueParams{BatchLimit: 100000})
	if err != nil || !slices.Contains(teams, team) {
		t.Fatalf("teams with pauses due = %v (err %v)", teams, err)
	}

	claimed, err := testQueries.ClaimBillingPauseSandbox(ctx, db.ClaimBillingPauseSandboxParams{ID: before, TeamID: team, LeaseSeconds: 90})
	if err != nil || claimed.PauseOpTrigger == nil || *claimed.PauseOpTrigger != "plan_switch_free" {
		t.Fatalf("claim = %+v, %v; want trigger plan_switch_free", claimed, err)
	}
	if _, err := testQueries.RevertPauseToActive(ctx, db.RevertPauseToActiveParams{SandboxID: before, TeamID: team, PauseOpID: claimed.PauseOpID, PauseOpLeaseVersion: &claimed.PauseOpLeaseVersion}); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if n, err := testQueries.SettleTeamPauseTargets(ctx, team); err != nil || n != 0 {
		t.Fatalf("settled %d targets (err %v) after a revert, want the target kept", n, err)
	}
	if ids := due(); !slices.Equal(ids, []uuid.UUID{before}) {
		t.Fatalf("due after revert = %v, want the reverted sandbox again", ids)
	}

	claimed, err = testQueries.ClaimBillingPauseSandbox(ctx, db.ClaimBillingPauseSandboxParams{ID: before, TeamID: team, LeaseSeconds: 90})
	if err != nil {
		t.Fatal(err)
	}
	if err := finalize(before, team, uuid.UUID(claimed.PauseOpID.Bytes), claimed.PauseOpLeaseVersion); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE sandbox SET status = 'active' WHERE id = $1`, before); err != nil {
		t.Fatal(err)
	}
	if ids := due(); len(ids) != 0 {
		t.Fatalf("due after the pause landed and the customer resumed = %v, want none", ids)
	}
	if n, err := testQueries.SettleTeamPauseTargets(ctx, team); err != nil || n != 1 {
		t.Fatalf("settled %d targets (err %v), want the paused one closed", n, err)
	}
	_ = after
}

func activationBucket(t *testing.T, teamID uuid.UUID) (string, string) {
	t.Helper()
	var bucket string
	var reason *string
	if err := testPool.QueryRow(context.Background(), `SELECT bucket, reason FROM pricing_v2_activation_bucket($1)`, teamID).Scan(&bucket, &reason); err != nil {
		t.Fatalf("activation bucket: %v", err)
	}
	if reason == nil {
		return bucket, ""
	}
	return bucket, *reason
}

func TestIntegration_PricingV2Activation_Buckets(t *testing.T) {
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := testPool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}

	noCard, _ := seedTeamAndKey(t)
	exec(`INSERT INTO team_credit_grant (team_id, amount_usd, remaining_usd, reason, expires_at)
		VALUES ($1, 5, 0, 'signup trial credit', now() - interval '1 second')`, noCard)

	payg, _ := seedTeamAndKey(t)
	exec(`INSERT INTO team_billing_account (team_id, stripe_customer_id, stripe_subscription_id, stripe_subscription_status)
		VALUES ($1, $2, $3, 'past_due')`, payg, "cus_"+payg.String(), "sub_"+payg.String())

	lapsed, _ := seedTeamAndKey(t)
	exec(`INSERT INTO team_billing_account (team_id, stripe_customer_id) VALUES ($1, $2)`, lapsed, "cus_"+lapsed.String())

	granted, _ := seedTeamAndKey(t)
	exec(`INSERT INTO team_credit_grant (team_id, amount_usd, remaining_usd, reason) VALUES ($1, 50, 50, 'staff credit')`, granted)

	assigned, _ := seedTeamAndKey(t)
	assignFreePlan(t, assigned, time.Now())

	blocked, _ := seedTeamAndKey(t)
	exec(`INSERT INTO team_billing_account (team_id, stripe_customer_id, stripe_subscription_id, stripe_subscription_status)
		VALUES ($1, $2, $3, 'active')`, blocked, "cus_"+blocked.String(), "sub_"+blocked.String())
	exec(`INSERT INTO abuse_restrictions (subject_type, subject_value, subject_team_id, action, source, reason)
		VALUES ('team', $1::text, $1::uuid, 'create', 'test', 'test')`, blocked)

	for _, tc := range []struct {
		name   string
		team   uuid.UUID
		bucket string
		reason string
	}{
		{"no card", noCard, "free", ""},
		{"card on file", payg, "legacy_payg", ""},
		{"customer without a live subscription", lapsed, "lapsed", ""},
		{"no card with granted credit", granted, "free", ""},
		{"abuse blocked with a card", blocked, "blocked", "abuse_blocked"},
		{"already assigned", assigned, "assigned", "already_assigned"},
	} {
		if bucket, reason := activationBucket(t, tc.team); bucket != tc.bucket || reason != tc.reason {
			t.Errorf("%s: bucket %q reason %q, want %q %q", tc.name, bucket, reason, tc.bucket, tc.reason)
		}
	}

	assignFreePlan(t, blocked, time.Now())
	if eligible(t, blocked) {
		t.Fatal("assigning Free lifted an abuse block")
	}
}

func TestIntegration_FreeShape_AdmissionCarriesLimitsAndResumeRefusesLarger(t *testing.T) {
	ctx := context.Background()
	unassigned, _ := seedTeamAndKey(t)
	if row, err := testQueries.GetTeamSandboxAdmission(ctx, unassigned); err != nil || row.ShapeLimited {
		t.Fatalf("unassigned admission = %+v, %v; want no shape limit", row, err)
	}

	team, _ := seedTeamAndKey(t)
	assignFreePlan(t, team, time.Now())
	row, err := testQueries.GetTeamSandboxAdmission(ctx, team)
	if err != nil || !row.Eligible || !row.ShapeLimited || row.MaxVcpu != 1 || row.MaxMemoryMib != 1024 || row.MaxDiskMib != 4096 {
		t.Fatalf("Free admission = %+v, %v; want eligible with 1 vCPU / 1024 MiB / 4096 MiB", row, err)
	}

	large := seedPausedSandbox(t, team)
	if _, err := testPool.Exec(ctx, `UPDATE sandbox SET vcpu_count = 2, memory_mib = 1024, disk_mib = 4096 WHERE id = $1`, large); err != nil {
		t.Fatal(err)
	}
	fits := seedPausedSandbox(t, team)
	if _, err := testPool.Exec(ctx, `UPDATE sandbox SET vcpu_count = 1, memory_mib = 1024, disk_mib = 4096 WHERE id = $1`, fits); err != nil {
		t.Fatal(err)
	}
	claim := func(id uuid.UUID) (db.ClaimResumeRow, error) {
		return testQueries.ClaimResume(ctx, db.ClaimResumeParams{ID: id, TeamID: team, LockKey: id.String(),
			MaxVcpu: &row.MaxVcpu, MaxMemoryMib: &row.MaxMemoryMib, MaxDiskMib: &row.MaxDiskMib})
	}
	if _, err := claim(large); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim of a sandbox above the Free shape = %v, want no rows", err)
	}
	if kept, err := testQueries.GetSandbox(ctx, db.GetSandboxParams{ID: large, TeamID: team}); err != nil || kept.Status != db.SandboxStatusPaused {
		t.Fatalf("refused sandbox = %s, %v; want it kept paused", kept.Status, err)
	}
	if c, err := claim(fits); err != nil || c.Sandbox.Status != db.SandboxStatusResuming {
		t.Fatalf("claim of a sandbox within the Free shape = %+v, %v", c, err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE sandbox SET status = 'paused' WHERE id = $1`, fits); err != nil {
		t.Fatal(err)
	}
	if c, err := testQueries.ClaimResume(ctx, db.ClaimResumeParams{ID: large, TeamID: team, LockKey: large.String()}); err != nil || c.Sandbox.Status != db.SandboxStatusResuming {
		t.Fatalf("claim without plan limits = %+v, %v; want the legacy claim unchanged", c, err)
	}
}

func TestIntegration_FreeAllowance_MissingAllowanceCapsAtOnce(t *testing.T) {
	ctx := context.Background()
	team, _ := seedTeamAndKey(t)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO team_plan_assignment (team_id, plan_key, kind, terms, limits, effective_from, source, reason)
		VALUES ($1, 'free', 'free', '{}', '{"max_sandboxes":1}', now() - interval '1 hour', 'migration', 'test')`, team); err != nil {
		t.Fatal(err)
	}
	sbx := seedActiveSandbox(t, team, "free-no-allowance")
	seedComputeInterval(t, team, sbx, 1, 1024, time.Now().Add(-time.Minute), nil)
	if blocked, err := testQueries.RefreshFreeAllowance(ctx, team); err != nil || !blocked {
		t.Fatalf("refresh with no frozen allowance = %v, %v; want capped rather than unlimited", blocked, err)
	}
}

func TestIntegration_FreeShape_AbsentCapsDoNotLimit(t *testing.T) {
	ctx := context.Background()
	team, _ := seedTeamAndKey(t)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO team_plan_assignment (team_id, plan_key, kind, terms, limits, effective_from, source, reason)
		VALUES ($1, 'free', 'free', '{}', '{"max_sandboxes":1,"max_vcpu":2}', now(), 'migration', 'test')`, team); err != nil {
		t.Fatal(err)
	}
	row, err := testQueries.GetTeamSandboxAdmission(ctx, team)
	if err != nil || !row.ShapeLimited || row.MaxVcpu != 2 || row.MaxMemoryMib != 0 || row.MaxDiskMib != 0 {
		t.Fatalf("admission = %+v, %v; want only the vCPU cap", row, err)
	}
	sbx := seedPausedSandbox(t, team)
	if _, err := testPool.Exec(ctx, `UPDATE sandbox SET vcpu_count = 2, memory_mib = 8192, disk_mib = 65536 WHERE id = $1`, sbx); err != nil {
		t.Fatal(err)
	}
	if c, err := testQueries.ClaimResume(ctx, db.ClaimResumeParams{ID: sbx, TeamID: team, LockKey: sbx.String(), MaxVcpu: &row.MaxVcpu}); err != nil || c.Sandbox.Status != db.SandboxStatusResuming {
		t.Fatalf("claim within the only cap = %+v, %v", c, err)
	}
}

func TestIntegration_FreeAllowance_CapLabelEndsWhenTeamLeavesFree(t *testing.T) {
	ctx := context.Background()
	team, _ := seedTeamAndKey(t)
	assignFreePlan(t, team, time.Now().Add(-48*time.Hour))
	sbx := seedActiveSandbox(t, team, "free-left")
	seedComputeInterval(t, team, sbx, 1, 1024, time.Now().Add(-3*time.Hour), nil)
	if blocked, err := testQueries.RefreshFreeAllowance(ctx, team); err != nil || !blocked {
		t.Fatalf("refresh = %v, %v; want capped", blocked, err)
	}
	if row, err := testQueries.GetTeamSandboxAdmission(ctx, team); err != nil || !row.AllowanceBlocked {
		t.Fatalf("admission = %+v, %v; want the cap reported", row, err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE team_plan_assignment SET effective_to = now() WHERE team_id = $1 AND effective_to IS NULL`, team); err != nil {
		t.Fatal(err)
	}
	if row, err := testQueries.GetTeamSandboxAdmission(ctx, team); err != nil || row.AllowanceBlocked || row.ShapeLimited {
		t.Fatalf("admission after leaving Free = %+v, %v; want no Free cap or shape", row, err)
	}
}

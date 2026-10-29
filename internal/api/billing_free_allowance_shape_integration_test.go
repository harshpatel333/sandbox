//go:build integration

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A deferred team busy with its invoice lock must not hold back allowance
// enforcement for every other Free team in the same sweep.
func TestFreeAllowanceSweepRunsWhileShapeCatchUpIsBlocked(t *testing.T) {
	h, blocked := newPlanAssignmentFixture(t)
	setPricingVersion(t, h, "v2")
	ctx := t.Context()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := h.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	free, err := h.planTermsForPricingVersion(ctx, "free", "v2")
	if err != nil {
		t.Fatal(err)
	}
	small := free
	vcpu := int32(1)
	small.Limits.MaxVCPU = &vcpu
	if _, err = assignInTx(t, h, planAssignment{Team: blocked, Terms: small, Source: planSourceStaff, Reason: "below the template shape"}); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO billing_plan_shape_deferred(team_id) VALUES($1) ON CONFLICT DO NOTHING`, blocked)
	t.Cleanup(func() {
		_, _ = h.Pool.Exec(context.Background(), `DELETE FROM billing_plan_shape_deferred WHERE team_id=$1`, blocked)
	})
	scanned, err := h.DB.CreateTeam(ctx, "allowance-scan-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = assignInTx(t, h, planAssignment{Team: scanned.ID, Terms: free, Source: planSourceStaff, Reason: "free team with a running sandbox"}); err != nil {
		t.Fatal(err)
	}
	// A due allowance retry is cleared once enforcement refreshes the team.
	exec(`INSERT INTO billing_free_allowance_retry(team_id,next_attempt_at) VALUES($1,'-infinity')`, scanned.ID)
	exec(`UPDATE sweep_lease SET locked_until=clock_timestamp()-interval '1 second',cursor_id=NULL WHERE name=$1`, freeAllowanceSweepName)

	holder, err := h.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.WithoutCancel(ctx))
	if _, err = holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('invoice:'||$1::text,0))`, blocked); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	sweep, cancel := context.WithTimeout(ctx, 3*time.Second)
	h.refreshFreeAllowances(sweep)
	cancel()
	took := time.Since(start)
	var retried, deferred bool
	if err = h.Pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM billing_free_allowance_retry WHERE team_id=$1),
	 EXISTS(SELECT 1 FROM billing_plan_shape_deferred WHERE team_id=$2)`, scanned.ID, blocked).Scan(&retried, &deferred); err != nil {
		t.Fatal(err)
	}
	if !retried || !deferred || took > 2*time.Second {
		t.Fatalf("blocked catch-up starved allowance enforcement: retried=%v deferred=%v took=%s", retried, deferred, took)
	}

	if err = holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var moved int
	if err = h.Pool.QueryRow(ctx, `SELECT moved FROM billing_raise_plan_shape(true)`).Scan(&moved); err != nil || moved < 1 {
		t.Fatal("released team was not raised by the next catch-up", moved, err)
	}
}

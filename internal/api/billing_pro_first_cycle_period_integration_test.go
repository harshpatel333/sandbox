//go:build integration

package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/superserve-ai/sandbox/internal/billing"
	"github.com/superserve-ai/sandbox/internal/db"
)

func TestProFirstCycleReportingTransitionPreservesFreeUsage(t *testing.T) {
	for _, mode := range []string{"paid", "unpaid", "touched", "leased", "close_attempted", "close_leased", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			h, team, offer, params := newProFirstCycleQuoteFixture(t, now)
			requestProFirstCycleQuote(t, h, team, offer, params, http.StatusAccepted)
			u, err := h.proUpgrade(t.Context(), team)
			if err != nil {
				t.Fatal(err)
			}
			start, end := billing.CurrentBillingPeriod(now)
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := h.Pool.Exec(t.Context(), sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`INSERT INTO team_billing_usage(team_id,period_start,period_end,vcpu_seconds,memory_mib_seconds) VALUES($1,$2,$3,7200,7372800)`, team, start, end)
			window := now.Add(-time.Hour)
			exec(`INSERT INTO billing_free_allowance(team_id,window_started_at,resets_at,allowance_usd,used_usd,evaluated_at) VALUES($1,$2,$3,1.23,0.5,$2)`, team, window, window.Add(24*time.Hour))
			if mode != "unpaid" {
				exec(`UPDATE billing_pro_upgrade SET state='credited',payment_evidence='{"verified":true}',grant_id=$2 WHERE id=$1`, u.ID, "cred_"+uuid.NewString())
			}
			if mode == "touched" {
				exec(`UPDATE team_billing_period SET status='validating' WHERE team_id=$1`, team)
			}
			if mode == "leased" {
				exec(`INSERT INTO billing_export_work(team_id,lease_token,lease_until) VALUES($1,$2,now()+interval '1 minute')`, team, uuid.New())
			}
			exec(`INSERT INTO billing_period_close_work(team_id,period_start,period_end) VALUES($1,$2,$3)`, team, start, end)
			if mode == "close_attempted" {
				exec(`UPDATE billing_period_close_work SET attempts=1,last_attempt_at=clock_timestamp() WHERE team_id=$1`, team)
			}
			if mode == "close_leased" {
				exec(`UPDATE billing_period_close_work SET lease_token=$2,lease_until=now()+interval '1 minute' WHERE team_id=$1`, team, uuid.New())
			}
			snapshot := func(query string) string {
				t.Helper()
				var out string
				if err := h.Pool.QueryRow(t.Context(), query, team).Scan(&out); err != nil {
					t.Fatal(err)
				}
				return out
			}
			usageQuery := `SELECT jsonb_build_object('usage',(SELECT to_jsonb(x) FROM team_billing_usage x WHERE team_id=$1),'allowance',(SELECT to_jsonb(x) FROM billing_free_allowance x WHERE team_id=$1))::text`
			periodQuery := `SELECT to_jsonb(x)::text FROM team_billing_period x WHERE team_id=$1`
			closeQuery := `SELECT to_jsonb(x)::text FROM billing_period_close_work x WHERE team_id=$1`
			originalUsage, originalPeriod, originalClose := snapshot(usageQuery), snapshot(periodQuery), snapshot(closeQuery)
			transition := func(commit bool) error {
				tx, e := h.Pool.Begin(t.Context())
				if e != nil {
					return e
				}
				defer tx.Rollback(t.Context())
				if _, e = h.DB.WithTx(tx).LockTeamBillingAccount(t.Context(), team); e != nil {
					return e
				}
				if e = retireFirstProCycleReportingPeriods(t.Context(), tx, u); e != nil {
					return e
				}
				if commit {
					return tx.Commit(t.Context())
				}
				return nil
			}
			err = transition(mode != "rollback")
			if mode == "unpaid" || mode == "touched" || mode == "leased" || mode == "close_attempted" || mode == "close_leased" {
				if err == nil {
					t.Fatal("retired unpaid, touched, or leased Free period")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if snapshot(usageQuery) != originalUsage {
				t.Fatal("rewrote pre-upgrade usage or daily allowance history")
			}
			if mode != "paid" {
				if snapshot(periodQuery) != originalPeriod || snapshot(closeQuery) != originalClose {
					t.Fatal("failed or rolled-back transition changed reporting period")
				}
				return
			}
			var safe bool
			if err = h.Pool.QueryRow(t.Context(), `SELECT status='finalized' AND blocked_reason='reporting_only_calendar_period' AND finalized_at IS NOT NULL AND period_start=$2 AND period_end=$3 AND gross_charges_usd=0 AND net_invoice_amount_usd=0 FROM team_billing_period WHERE team_id=$1`, team, start, end).Scan(&safe); err != nil || !safe {
				t.Fatalf("reporting-only retirement: %v %v", safe, err)
			}
			if err = h.Pool.QueryRow(t.Context(), `SELECT completed_at IS NOT NULL AND stage='reporting_only_calendar_period' AND attempts=0 AND lease_token IS NULL FROM billing_period_close_work WHERE team_id=$1`, team).Scan(&safe); err != nil || !safe {
				t.Fatalf("unstarted Free close work left claimable after retirement: %v %v", safe, err)
			}
			retiredClose := snapshot(closeQuery)
			retired := snapshot(periodQuery)
			if err = transition(true); err != nil || snapshot(periodQuery) != retired || snapshot(closeQuery) != retiredClose {
				t.Fatalf("transition replay changed historical report: %v", err)
			}
			if _, err = h.DB.UpsertTeamBillingPeriod(t.Context(), db.UpsertTeamBillingPeriodParams{TeamID: team, PeriodStart: u.Start, PeriodEnd: u.End, Status: "open"}); err != nil {
				t.Fatal(err)
			}
			var priorCopied bool
			if err = h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM team_billing_usage WHERE team_id=$1 AND period_start=$2 AND period_end=$3)`, team, u.Start, u.End).Scan(&priorCopied); err != nil || priorCopied {
				t.Fatalf("copied Free reporting usage into paid month: %v %v", priorCopied, err)
			}
		})
	}
}

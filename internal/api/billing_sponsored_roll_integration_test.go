//go:build integration

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/superserve-ai/sandbox/internal/billing"
)

// sponsoredRollFixture is a sponsored team without a payment method whose
// first period ended rolledAgo ago. That period's grant was issued, the next
// one is not, and its funding account is observed with no credit left.
type sponsoredRollFixture struct {
	*entitlementFixture
	t         *testing.T
	operation uuid.UUID
	prior     time.Time
}

func newSponsoredRollFixture(t *testing.T, rolledAgo time.Duration) *sponsoredRollFixture {
	t.Helper()
	at := time.Now().UTC().Add(-rolledAgo).Truncate(time.Second)
	f := &sponsoredRollFixture{entitlementFixture: newEntitlementFixtureAt(t, at, false, false, false, false, true, false, true), t: t}
	ctx := t.Context()
	f.prior = f.start.AddDate(0, -1, 0)
	if err := f.h.Pool.QueryRow(ctx, `SELECT id FROM billing_sponsored_operation WHERE contract_id=$1`, f.c.ID).Scan(&f.operation); err != nil {
		t.Fatal(err)
	}
	conn, err := f.h.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// The first period's issuance happened before this test's clock.
	if _, err = conn.Exec(ctx, `SET session_replication_role=replica`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `RESET session_replication_role`)
	grant := "credgr_prior_" + f.c.ID.String()
	for _, step := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO billing_usage_entitlement(team_id,contract_id,sponsorship_id,kind,period_start,period_end,amount_cents,currency,state,first_attempt_at,request_payload,provider_invoice_start,provider_invoice_end,stripe_credit_grant_id,issuance_evidence,issued_at)
 VALUES($1,$2,$3,'metered_usage',$4,$5,3000000,'usd','issued',$4,'{}',$4,$5,$6,'{"fixture":true}',$4)`, []any{f.c.TeamID, f.c.ID, f.operation, f.prior, f.start, grant}},
		{`INSERT INTO team_billing_period(team_id,period_start,period_end,status,finalized_at,gross_charges_usd,credits_applied_usd,net_invoice_amount_usd) VALUES($1,$2,$3,'finalized',$3,0,0,0)`, []any{f.c.TeamID, f.prior, f.start}},
		{`INSERT INTO billing_invoice_calendar(team_id,period_start,period_end,customer_id,subscription_id,invoice_start,invoice_end,billing_cycle_anchor) VALUES($1,$2,$3,$4,$5,$2,$3,$6)`, []any{f.c.TeamID, f.prior, f.start, f.c.CustomerID, f.c.SubscriptionID, f.prior.Unix()}},
	} {
		if _, err = conn.Exec(ctx, step.q, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	var budget int32
	if err = f.h.Pool.QueryRow(ctx, `SELECT max_observed_teams FROM billing_funding_policy WHERE singleton`).Scan(&budget); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE billing_funding_policy SET max_observed_teams=greatest(max_observed_teams,100) WHERE singleton`)
	t.Cleanup(func() {
		_, _ = f.h.Pool.Exec(context.Background(), `UPDATE billing_funding_account SET observe=false,enforce=false WHERE team_id=$1`, f.c.TeamID)
		_, _ = f.h.Pool.Exec(context.Background(), `UPDATE billing_funding_policy SET max_observed_teams=$1 WHERE singleton`, budget)
	})
	f.exec(`UPDATE billing_funding_account SET observe=true,enforce=true WHERE team_id=$1`, f.c.TeamID)
	f.observe(0)
	return f
}

func (f *sponsoredRollFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.h.Pool.Exec(f.t.Context(), q, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *sponsoredRollFixture) observe(creditUSD int) {
	f.t.Helper()
	f.exec(`UPDATE billing_funding_account SET observed_generation=generation,customer_id=$2,subscription_id=$3,credit_usd=$4,credit_valid_until=now()+interval '1 minute',payment_method_ready=false,collection_ready=true,provider_valid_until=now()+interval '1 minute' WHERE team_id=$1`, f.c.TeamID, f.c.CustomerID, f.c.SubscriptionID, creditUSD)
}

// eligible reports whether the pause sweep would leave the team running.
func (f *sponsoredRollFixture) eligible() bool {
	f.t.Helper()
	ok, err := f.h.DB.IsTeamSandboxBillingEligible(f.t.Context(), f.c.TeamID)
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

func (f *sponsoredRollFixture) absentRecorded() bool {
	f.t.Helper()
	var ok bool
	if err := f.h.Pool.QueryRow(f.t.Context(), `SELECT EXISTS(SELECT 1 FROM billing_sponsored_absent_close WHERE contract_id=$1 AND period_end=$2)`, f.c.ID, f.start).Scan(&ok); err != nil {
		f.t.Fatal(err)
	}
	return ok
}

func (f *sponsoredRollFixture) invoice(id, reason, status string) map[string]any {
	return map[string]any{"id": id, "customer": f.c.CustomerID, "billing_reason": reason, "status": status, "period_start": f.prior.Unix(), "period_end": f.start.Unix()}
}

func TestSponsoredRollWithoutRenewalInvoice(t *testing.T) {
	for _, shape := range []string{"none", "create_and_update", "void_cycle"} {
		t.Run(shape, func(t *testing.T) {
			f := newSponsoredRollFixture(t, 2*time.Hour)
			switch shape {
			case "create_and_update":
				// Activation and a later change bill outside the cycle; neither closes a period.
				f.subscriptionInvoices = []map[string]any{f.invoice("in_create", "subscription_create", "paid"), f.invoice("in_update", "subscription_update", "paid")}
			case "void_cycle":
				f.subscriptionInvoices = []map[string]any{f.invoice("in_void", "subscription_cycle", "void")}
			}
			if !f.eligible() {
				t.Fatal("sponsored team paused at its period roll")
			}
			if _, err := f.h.reconcileUsageEntitlement(t.Context(), f.c.ID); err != nil {
				t.Fatal("next grant blocked on a renewal invoice that does not exist:", err)
			}
			if f.grants.Load() != 1 || !f.absentRecorded() {
				t.Fatal("absent renewal invoice not recorded with one grant", f.grants.Load(), f.absentRecorded())
			}
			// Issuing the grant moved the funding generation; the funding worker
			// has not observed it yet.
			if !f.eligible() {
				t.Fatal("team paused before the issued grant was observed")
			}
			// The absent period must not hold the next period's invoice close.
			if err := f.h.requireEarlierInvoicePeriodsReleased(t.Context(), billing.ExportPeriod{TeamID: f.c.TeamID, Start: f.start, End: f.end}, f.prior); err != nil {
				t.Fatal("recorded absent period still holds later closes:", err)
			}
			// A cycle invoice that turns up later changes nothing already decided.
			f.subscriptionInvoices = append(f.subscriptionInvoices, f.invoice("in_late", "subscription_cycle", "draft"))
			if _, err := f.h.reconcileUsageEntitlement(t.Context(), f.c.ID); err != nil || f.grants.Load() != 1 || !f.absentRecorded() {
				t.Fatal("late renewal invoice reopened the period", f.grants.Load(), err)
			}
			f.observe(30000)
			if !f.eligible() {
				t.Fatal("issued next grant left the team paused")
			}
		})
	}
}

func TestSponsoredRollWaitsForDraftRenewalInvoice(t *testing.T) {
	f := newSponsoredRollFixture(t, 2*time.Hour)
	f.subscriptionInvoices = []map[string]any{f.invoice("in_cycle", "subscription_cycle", "draft")}
	_, err := f.h.reconcileUsageEntitlement(t.Context(), f.c.ID)
	if err == nil || !strings.Contains(err.Error(), "prior closing invoice is not reconciled and released") || f.grants.Load() != 0 || f.absentRecorded() {
		t.Fatal("existing renewal draft was skipped instead of closed", err, f.grants.Load())
	}
	if !f.eligible() {
		t.Fatal("team paused while its renewal invoice closes")
	}
	invoice := "in_prior_" + f.c.TeamID.String()
	f.exec(`INSERT INTO billing_invoice_close(team_id,period_start,period_end,invoice_id,state,verified_at,plan,finalized_evidence) VALUES($1,$2,$3,$4,'released',clock_timestamp(),jsonb_build_object('customer',$5::text,'subscription',$6::text,'expected_cents',0,'credit_before_cents',0,'resources',jsonb_build_array(jsonb_build_object('resource','cpu','expected_cents',0,'snapshot',billing_meter_accounting_snapshot($1,$2,$3,'cpu')))), '{"credit_applied_cents":0,"credit_remaining_cents":0,"net_cents":0}')`, f.c.TeamID, f.prior, f.start, invoice, f.c.CustomerID, f.c.SubscriptionID)
	if _, err = f.h.reconcileUsageEntitlement(t.Context(), f.c.ID); err != nil || f.grants.Load() != 1 || f.absentRecorded() {
		t.Fatal("released $0 renewal invoice did not issue the next grant", err, f.grants.Load())
	}
}

func TestSponsoredRollRecentlyEndedCycleIsNotAbsent(t *testing.T) {
	// Stripe may still be creating the renewal draft.
	f := newSponsoredRollFixture(t, 10*time.Minute)
	if _, err := f.h.reconcileUsageEntitlement(t.Context(), f.c.ID); err == nil || f.grants.Load() != 0 || f.absentRecorded() {
		t.Fatal("renewal invoice treated as absent before Stripe had time to create it", err)
	}
	if !f.eligible() {
		t.Fatal("team paused while its renewal invoice is created")
	}
}

func TestSponsoredRollGrace(t *testing.T) {
	cases := []struct {
		name      string
		rolledAgo time.Duration
		change    func(f *sponsoredRollFixture)
		eligible  bool
	}{
		{"platform failure keeps it", 2 * time.Hour, func(f *sponsoredRollFixture) {
			f.exec(`INSERT INTO billing_entitlement_withheld(contract_id,period_start,period_end,evidence,platform_failure_at) VALUES($1,$2,$3,'{}',clock_timestamp())`, f.c.ID, f.start, f.end)
		}, true},
		{"unpaid invoice ends it", 2 * time.Hour, func(f *sponsoredRollFixture) {
			f.exec(`INSERT INTO billing_entitlement_withheld(contract_id,period_start,period_end,evidence) VALUES($1,$2,$3,'{"unpaid_invoice":"in_unpaid"}')`, f.c.ID, f.start, f.end)
		}, false},
		{"debt ends it", 2 * time.Hour, func(f *sponsoredRollFixture) {
			f.exec(`INSERT INTO billing_entitlement_debt(contract_id,period_start,invoice_id) VALUES($1,$2,'in_unpaid')`, f.c.ID, f.start)
		}, false},
		{"revocation ends it", 2 * time.Hour, func(f *sponsoredRollFixture) {
			f.exec(`UPDATE billing_sponsored_operation SET revoked_at=clock_timestamp(),revoked_by=$2,state='revoking' WHERE id=$1`, f.operation, uuid.New())
		}, false},
		{"six hours ends it", 7 * time.Hour, func(*sponsoredRollFixture) {}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSponsoredRollFixture(t, tc.rolledAgo)
			tc.change(f)
			f.observe(0)
			if got := f.eligible(); got != tc.eligible {
				t.Fatalf("eligible = %v, want %v", got, tc.eligible)
			}
		})
	}
}

func TestSponsoredRollGraceEndsWithContract(t *testing.T) {
	f := newSponsoredRollFixture(t, 2*time.Hour)
	conn, err := f.h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(t.Context(), `SET session_replication_role=replica`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.WithoutCancel(t.Context()), `RESET session_replication_role`)
	// The sponsorship's last period was the one that just ended.
	if _, err = conn.Exec(t.Context(), `UPDATE billing_commercial_contract SET ends_at=$2 WHERE id=$1`, f.c.ID, f.start); err != nil {
		t.Fatal(err)
	}
	if f.eligible() {
		t.Fatal("grace extended past the end of the sponsorship")
	}
}

func TestSponsoredRollGraceWaitsForSuccessfulCreditRead(t *testing.T) {
	f := newSponsoredRollFixture(t, 2*time.Hour)
	ctx := t.Context()
	if _, err := f.h.reconcileUsageEntitlement(ctx, f.c.ID); err != nil || f.grants.Load() != 1 {
		t.Fatal("next grant not issued", err)
	}
	publish := func(credit *fundingCreditObservation, workErr error) {
		t.Helper()
		j, s := claimFundingTestJob(t, f.h, f.c.TeamID)
		if err := f.h.publishFundingObservation(ctx, j, s, credit, nil, workErr); err != nil {
			t.Fatal(err)
		}
	}
	// A failed read advances the observed generation without reading credit.
	publish(nil, errors.New("provider temporarily unavailable"))
	if !f.eligible() {
		t.Fatal("failed credit read ended the grace")
	}
	publish(&fundingCreditObservation{SpendableUSD: "30000", ValidUntil: time.Now().Add(time.Minute), Evidence: []byte(`{"fixture":"credit"}`)}, nil)
	if !f.eligible() {
		t.Fatal("observed next grant left the team paused")
	}
	// After a successful read, real exhaustion pauses the team as before.
	publish(&fundingCreditObservation{SpendableUSD: "0", ValidUntil: time.Now().Add(time.Minute), Evidence: []byte(`{"fixture":"credit"}`)}, nil)
	if f.eligible() {
		t.Fatal("grace outlived a successful read showing no credit")
	}
}

func TestSponsoredRollAbsenceIsSerializedWithInvoiceClose(t *testing.T) {
	closeKey := func(f *sponsoredRollFixture) string { return "invoice:" + f.c.TeamID.String() }
	t.Run("close found during listing", func(t *testing.T) {
		f := newSponsoredRollFixture(t, 2*time.Hour)
		// The invoice close finds a draft and commits under its lock while the
		// entitlement worker is still listing invoices.
		f.onSubscriptionInvoices = func() {
			f.onSubscriptionInvoices = nil
			tx, err := f.h.Pool.Begin(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			defer tx.Rollback(context.Background())
			if _, err = tx.Exec(context.Background(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, closeKey(f)); err != nil {
				t.Error(err)
				return
			}
			if _, err = tx.Exec(context.Background(), `INSERT INTO billing_invoice_close(team_id,period_start,period_end,invoice_id,plan) VALUES($1,$2,$3,$4,'{}')`, f.c.TeamID, f.prior, f.start, "in_found_"+f.c.TeamID.String()); err != nil {
				t.Error(err)
				return
			}
			if err = tx.Commit(context.Background()); err != nil {
				t.Error(err)
			}
		}
		if _, err := f.h.reconcileUsageEntitlement(t.Context(), f.c.ID); err == nil || f.grants.Load() != 0 || f.absentRecorded() {
			t.Fatal("period recorded absent although its close was created", err, f.grants.Load())
		}
	})
	t.Run("close running during decision", func(t *testing.T) {
		f := newSponsoredRollFixture(t, 2*time.Hour)
		tx, err := f.h.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.WithoutCancel(t.Context()))
		if _, err = tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, closeKey(f)); err != nil {
			t.Fatal(err)
		}
		if _, err = f.h.reconcileUsageEntitlement(t.Context(), f.c.ID); err == nil || f.grants.Load() != 0 || f.absentRecorded() {
			t.Fatal("absence decided while the invoice close held its lock", err)
		}
	})
	t.Run("close after absence", func(t *testing.T) {
		f := newSponsoredRollFixture(t, 2*time.Hour)
		if _, err := f.h.reconcileUsageEntitlement(t.Context(), f.c.ID); err != nil || !f.absentRecorded() {
			t.Fatal(err)
		}
		f.subscriptionInvoices = []map[string]any{f.invoice("in_late", "subscription_cycle", "draft")}
		if err := f.h.reconcileInvoicePeriod(t.Context(), billing.ExportPeriod{TeamID: f.c.TeamID, Start: f.prior, End: f.start}); err != nil {
			t.Fatal(err)
		}
		var closing bool
		if err := f.h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM billing_invoice_close WHERE team_id=$1 AND period_end=$2)`, f.c.TeamID, f.start).Scan(&closing); err != nil || closing {
			t.Fatal("absent period was also closed", closing, err)
		}
	})
}

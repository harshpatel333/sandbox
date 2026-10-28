//go:build integration

package api

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/superserve-ai/sandbox/internal/db"
)

type countingCreditWarningSender struct {
	mu        sync.Mutex
	remaining []float64
}

func (s *countingCreditWarningSender) SendTrialCreditWarning(_ context.Context, _ uuid.UUID, remaining float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remaining = append(s.remaining, remaining)
	return nil
}

func (s *countingCreditWarningSender) sent() []float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.remaining)
}

// Fixture values are arbitrary; the threshold comes from the plan's frozen
// top-off terms.
func TestPrepaidLowCreditWarningOnlyWithAutoTopoffOff(t *testing.T) {
	f, contract := fundedSubscriptionFixture(t, &planTopoff{AmountCents: 1500, ThresholdCents: 1000, DefaultEnabled: true})
	f.issueIncludedGrant(contract)
	sender := &countingCreditWarningSender{}
	f.h.TrialWarningSender = sender
	discovered := func() bool {
		t.Helper()
		teams, err := f.h.DB.ListTrialCreditWarningTeams(t.Context(), db.ListTrialCreditWarningTeamsParams{BatchLimit: 100000})
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(teams, f.team)
	}
	process := func() {
		t.Helper()
		for range 2 {
			f.h.processTrialCreditWarning(t.Context(), f.team)
		}
	}

	if err := f.applyTopoff("cs_"+uuid.NewString(), true); err != nil {
		t.Fatal(err)
	}
	f.observe(500, true)
	process()
	if discovered() || len(sender.sent()) != 0 {
		t.Fatal("warned a team whose Auto Topoff is on")
	}
	b, err := f.h.DB.GetTeamTrialBalance(t.Context(), f.team)
	if err != nil || b.Kind != creditWarningPrepaid || b.State != "auto_topoff" || b.Eligible {
		t.Fatalf("balance with Auto Topoff on = %+v, %v", b, err)
	}

	f.exec(`UPDATE billing_prepaid_auto_recharge SET enabled=false,disabled_reason='card declined',disabled_at=clock_timestamp() WHERE team_id=$1`, f.team)
	f.observe(1000, true)
	process()
	if discovered() || len(sender.sent()) != 0 {
		t.Fatal("warned at the top-off threshold")
	}
	f.observe(999, true)
	if !discovered() {
		t.Fatal("low balance with Auto Topoff off was not discovered")
	}
	process()
	if got := sender.sent(); len(got) != 1 || got[0] != 9.99 {
		t.Fatalf("low-credit warnings = %v, want one quoting the balance", got)
	}
	if discovered() {
		t.Fatal("a warned balance stays in discovery")
	}

	f.rawExec(`INSERT INTO billing_prepaid_purchase(team_id,customer_id,source,amount_cents,payment_state,payment_idempotency_key,payment_first_attempt_at,
  paid_at,payment_evidence,credit_state,credit_idempotency_key,credit_request,credit_first_attempt_at,stripe_credit_grant_id,credit_evidence,credited_at)
 VALUES($1,$2,'auto_recharge',1500,'succeeded',gen_random_uuid()::text,clock_timestamp(),clock_timestamp(),'{}','issued',gen_random_uuid()::text,'{}',clock_timestamp(),
  'cg_'||gen_random_uuid(),'{}',clock_timestamp())`, f.team, f.customer)
	f.observe(800, true)
	process()
	if got := sender.sent(); len(got) != 2 || got[1] != 8 {
		t.Fatalf("warnings after a top-off = %v, want the warning re-armed once", got)
	}

	f.rawExec(`UPDATE billing_usage_entitlement SET period_end=clock_timestamp() WHERE team_id=$1`, f.team)
	f.rawExec(`INSERT INTO billing_usage_entitlement(team_id,contract_id,payment_id,kind,period_start,period_end,amount_cents,currency,state,
  first_attempt_at,request_payload,provider_invoice_start,provider_invoice_end,stripe_credit_grant_id,issuance_evidence,issued_at)
 SELECT team_id,contract_id,gen_random_uuid(),kind,period_end,period_end+interval '30 days',amount_cents,currency,'issued',
  clock_timestamp(),'{}',period_end,period_end+interval '1 hour','cg_'||gen_random_uuid(),'{"fixture":true}',clock_timestamp()
 FROM billing_usage_entitlement WHERE team_id=$1`, f.team)
	f.observe(700, true)
	process()
	if got := sender.sent(); len(got) != 3 || got[2] != 7 {
		t.Fatalf("warnings after a new period's grant = %v, want the warning re-armed once", got)
	}
}

// rawExec writes payment, credit and grant history directly; how they are
// settled is outside what these tests exercise.
func (f *prepaidFixture) rawExec(sql string, args ...any) {
	f.t.Helper()
	conn, err := f.h.Pool.Acquire(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(f.t.Context(), `SET session_replication_role=replica`); err != nil {
		f.t.Fatal(err)
	}
	defer conn.Exec(context.WithoutCancel(f.t.Context()), `RESET session_replication_role`)
	if _, err = conn.Exec(f.t.Context(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

func TestPrepaidSummaryUnobservedBalanceIsNotLow(t *testing.T) {
	f := newPrepaidFixture(t)
	summary := func() (observed, low bool) {
		t.Helper()
		raw, err := f.h.prepaidSummary(t.Context(), f.team, 0)
		if err != nil {
			t.Fatal(err)
		}
		var s struct {
			Observed bool `json:"balance_observed"`
			Low      bool `json:"low_balance"`
		}
		if err = json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		return s.Observed, s.Low
	}
	f.observe(100, true)
	if observed, low := summary(); !observed || !low {
		t.Fatal("a fresh low balance is not reported as observed and low")
	}
	f.exec(`UPDATE billing_funding_account SET credit_valid_until=clock_timestamp()-interval '1 second' WHERE team_id=$1`, f.team)
	if observed, low := summary(); observed || low {
		t.Fatal("a stale balance is reported as observed or low")
	}
}

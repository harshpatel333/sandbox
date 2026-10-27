//go:build integration

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fundedSubscriptionFixture moves a prepaid fixture team onto a published
// prepaid-funded subscription backed by an active contract. All values are
// arbitrary fixtures, not catalog values.
func fundedSubscriptionFixture(t *testing.T, topoff *planTopoff) (*prepaidFixture, uuid.UUID) {
	t.Helper()
	f := newPrepaidFixture(t)
	family := "funded" + uuid.NewString()[:8]
	var version uuid.UUID
	var amount, threshold *int64
	var preselect *bool
	if topoff != nil {
		amount, threshold, preselect = &topoff.AmountCents, &topoff.ThresholdCents, &topoff.DefaultEnabled
	}
	if err := f.h.Pool.QueryRow(t.Context(), `INSERT INTO billing_plan_version(family,version,pricing_version,kind,display_name,monthly_fee_cents,credit_schedule,max_sandboxes,
  funding_policy,topoff_amount_cents,topoff_threshold_cents,topoff_default_enabled)
 VALUES($1,1,'v2','subscription','Funded fixture',700,'[{"start_months":0,"every_months":1,"amount_cents":700,"expires_after_months":1}]',3,'prepaid',$2,$3,$4) RETURNING id`,
		family, amount, threshold, preselect).Scan(&version); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE billing_plan_version SET state='published',published_at=clock_timestamp() WHERE id=$1`, version)
	t.Cleanup(func() {
		_, _ = f.h.Pool.Exec(context.Background(), `UPDATE billing_plan_version SET state='retired',retired_at=clock_timestamp() WHERE id=$1`, version)
	})
	terms, err := f.h.planTermsVersion(t.Context(), version, 1)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	f.exec(`UPDATE team_billing_account SET commercial_billing_anchor=$2 WHERE team_id=$1`, f.team, start)
	var contract uuid.UUID
	if err = f.h.Pool.QueryRow(t.Context(), `INSERT INTO billing_commercial_contract(team_id,offer_key,starts_at,customer_id,subscription_id,base_price_id,charge_amount_cents,charge_interval_months,entitlement_amount_cents,entitlement_anchor)
 VALUES($1,$2,$3,$4,$5,'price_fixture',700,1,700,$3) RETURNING id`, f.team, terms.OfferKey, start, f.customer, f.sub).Scan(&contract); err != nil {
		t.Fatal(err)
	}
	if _, err = assignInTx(t, f.h, planAssignment{Team: f.team, Terms: terms, Source: planSourceCheckout, Reason: "funded subscription fixture", ContractID: &contract}); err != nil {
		t.Fatal(err)
	}
	return f, contract
}

// issueIncludedGrant records the period's included grant as issued, as the
// entitlement worker would after Stripe confirms it.
func (f *prepaidFixture) issueIncludedGrant(contract uuid.UUID) {
	f.t.Helper()
	conn, err := f.h.Pool.Acquire(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	defer conn.Release()
	// The payment row is outside what this test exercises.
	if _, err = conn.Exec(f.t.Context(), `SET session_replication_role=replica`); err != nil {
		f.t.Fatal(err)
	}
	defer conn.Exec(context.WithoutCancel(f.t.Context()), `RESET session_replication_role`)
	if _, err = conn.Exec(f.t.Context(), `INSERT INTO billing_usage_entitlement(team_id,contract_id,payment_id,kind,period_start,period_end,amount_cents,currency,state,
  first_attempt_at,request_payload,provider_invoice_start,provider_invoice_end,stripe_credit_grant_id,issuance_evidence,issued_at)
 VALUES($1,$2,gen_random_uuid(),'metered_usage',clock_timestamp()-interval '1 hour',clock_timestamp()+interval '29 days',700,'usd','issued',
  clock_timestamp(),'{}',clock_timestamp()-interval '1 hour',clock_timestamp(),$3,'{"fixture":true}',clock_timestamp())`,
		f.team, contract, "cg_"+uuid.NewString()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *prepaidFixture) eligible() bool {
	f.t.Helper()
	var ok bool
	if err := f.h.Pool.QueryRow(f.t.Context(), `SELECT team_sandbox_billing_eligible($1)`, f.team).Scan(&ok); err != nil {
		f.t.Fatal(err)
	}
	return ok
}

func (f *prepaidFixture) applyTopoff(session string, enabled bool) error {
	f.t.Helper()
	tx, err := f.h.Pool.Begin(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(f.t.Context()))
	if err = applyCheckoutTopoff(f.t.Context(), tx, f.team, session, enabled, pricingPublicState{Version: "v2", EntryOpen: true}); err != nil {
		return err
	}
	return tx.Commit(f.t.Context())
}

func TestPrepaidFundedSubscriptionPausesAtZeroWithTopoffOff(t *testing.T) {
	f, contract := fundedSubscriptionFixture(t, &planTopoff{AmountCents: 1500, ThresholdCents: 1000, DefaultEnabled: true})
	f.exec(`UPDATE billing_funding_account SET observe=false WHERE team_id=$1`, f.team)
	if err := f.applyTopoff("cs_off", false); err != nil {
		t.Fatal(err)
	}
	if err := f.h.retryPrepaidEnrolment(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.observe(500, true)
	if err := f.h.startPrepaidEnforcement(t.Context()); err != nil {
		t.Fatal(err)
	}
	var prepaid bool
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT observe AND prepaid FROM billing_funding_account WHERE team_id=$1`, f.team).Scan(&prepaid); err != nil || prepaid {
		t.Fatal("enforcement began before the included grant was issued and observed", prepaid, err)
	}
	f.issueIncludedGrant(contract)
	f.observe(500, true)
	if err := f.h.startPrepaidEnforcement(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT observe AND prepaid FROM billing_funding_account WHERE team_id=$1`, f.team).Scan(&prepaid); err != nil || !prepaid {
		t.Fatal("observed included grant did not start enforcement", prepaid, err)
	}
	if !f.eligible() {
		t.Fatal("funded team with credit is not eligible")
	}
	f.observe(0, true)
	if err := f.tick(); err != nil {
		t.Fatal(err)
	}
	if f.eligible() || len(f.intentPosts) != 0 {
		t.Fatal("team with Auto Topoff off kept running or was charged at $0", len(f.intentPosts))
	}
}

func TestPrepaidFundedSubscriptionTopsUpAndPausesAfterDecline(t *testing.T) {
	f, _ := fundedSubscriptionFixture(t, &planTopoff{AmountCents: 1500, ThresholdCents: 1000, DefaultEnabled: true})
	if err := f.applyTopoff("cs_on", true); err != nil {
		t.Fatal(err)
	}
	f.observe(500, true)
	if err := f.tick(); err != nil {
		t.Fatal(err)
	}
	if r := f.rows(); len(r) != 1 || r[0].Source != "auto_recharge" || r[0].Amount != 1500 || r[0].Credit != "issued" {
		t.Fatalf("top-off did not use the frozen amount: %+v", r)
	}
	f.exec(`UPDATE billing_prepaid_purchase SET credit_voided_at=clock_timestamp() WHERE team_id=$1`, f.team)
	f.exec(`UPDATE billing_funding_account SET enforce=true,prepaid=true WHERE team_id=$1`, f.team)
	f.decline = true
	f.observe(0, true)
	if err := f.tick(); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT enabled FROM billing_prepaid_auto_recharge WHERE team_id=$1`, f.team).Scan(&enabled); err != nil || enabled {
		t.Fatal("declined top-off left Auto Topoff on", enabled, err)
	}
	if f.eligible() {
		t.Fatal("team kept running after a declined top-off at $0")
	}
}

func TestPrepaidFundedSubscriptionTopoffChoiceReplaysPerSession(t *testing.T) {
	f, _ := fundedSubscriptionFixture(t, &planTopoff{AmountCents: 1500, ThresholdCents: 1000, DefaultEnabled: true})
	state := func() (enabled bool, amount int64) {
		t.Helper()
		if err := f.h.Pool.QueryRow(t.Context(), `SELECT enabled,amount_cents FROM billing_prepaid_auto_recharge WHERE team_id=$1`, f.team).Scan(&enabled, &amount); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := f.applyTopoff("cs_first", true); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE billing_prepaid_auto_recharge SET enabled=false WHERE team_id=$1`, f.team)
	if err := f.applyTopoff("cs_first", true); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := state(); enabled {
		t.Fatal("a replayed settlement overrode the customer's later choice")
	}
	if err := f.applyTopoff("cs_second", true); err != nil {
		t.Fatal(err)
	}
	if enabled, amount := state(); !enabled || amount != 1500 {
		t.Fatal("a new checkout's choice was not applied", enabled, amount)
	}
}

func TestPrepaidFundedSubscriptionTopoffNeedsAnOffer(t *testing.T) {
	f, _ := fundedSubscriptionFixture(t, nil)
	if err := f.applyTopoff("cs_none", true); !errors.Is(err, errTopoffNotOffered) {
		t.Fatal("enabled a top-off the plan does not offer", err)
	}
	if err := f.applyTopoff("cs_none", false); err != nil {
		t.Fatal("declining an unoffered top-off must still record the checkout", err)
	}
}

func TestInvoiceCashFundedSubscriptionCollectsDespiteSettledTopoff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  string
		settled bool
		want    string
	}{
		{"funded settled", fundingPolicyPrepaid, true, "residual_allowed"},
		{"funded unsettled", fundingPolicyPrepaid, false, "prepaid_hold"},
		{"postpaid settled", "", true, "prepaid_hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInvoiceCashFixture(t, "", "")
			f.quiescePrepaidPurchases(t)
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := f.h.Pool.Exec(t.Context(), q, args...); err != nil {
					t.Fatal(err)
				}
			}
			var contract uuid.UUID
			if err := f.h.Pool.QueryRow(t.Context(), `INSERT INTO billing_commercial_contract(team_id,offer_key,starts_at,customer_id,subscription_id,base_price_id,charge_amount_cents,charge_interval_months,entitlement_amount_cents,entitlement_anchor)
 VALUES($1,'funded_monthly',$2,$3,$4,'price_base',700,1,700,$2) RETURNING id`, f.team, f.start, f.invoice.Customer, "sub_"+f.team.String()).Scan(&contract); err != nil {
				t.Fatal(err)
			}
			exec(`INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason,contract_id)
 VALUES($1,'funded_monthly','subscription',jsonb_strip_nulls(jsonb_build_object('kind','subscription','pricing_version','v2','funding_policy',NULLIF($2,''))),'{"max_concurrent":1}',$3,'staff','funded fixture',$4)`,
				f.team, tc.policy, f.start.Add(-time.Hour), contract)
			id := uuid.New()
			exec(`INSERT INTO billing_prepaid_purchase(id,team_id,customer_id,source,amount_cents,payment_idempotency_key,credit_idempotency_key,payment_request,created_at)
 VALUES($1,$2,$3,'auto_recharge',500,'','','{}',$4)`, id, f.team, f.invoice.Customer, f.start.Add(time.Hour))
			exec(`UPDATE billing_prepaid_purchase SET payment_state='succeeded',paid_at=clock_timestamp(),payment_evidence=jsonb_build_object('amount_cents',amount_cents,'customer',customer_id) WHERE id=$1`, id)
			if tc.settled {
				request, err := prepaidCreditRequest(id, f.invoice.Customer, 500, "auto_recharge")
				if err != nil {
					t.Fatal(err)
				}
				exec(`UPDATE billing_prepaid_purchase SET credit_state='pending',credit_request=$2 WHERE id=$1`, id, request)
				exec(`UPDATE billing_prepaid_purchase SET credit_state='uncertain',credit_first_attempt_at=clock_timestamp() WHERE id=$1`, id)
				exec(`UPDATE billing_prepaid_purchase SET credit_state='issued',stripe_credit_grant_id=$2,credit_evidence=credit_request || jsonb_build_object('id',$2::text),credited_at=clock_timestamp() WHERE id=$1`, id, "cg_"+id.String())
			}
			var policy string
			if err := f.h.Pool.QueryRow(t.Context(), `SELECT policy FROM resolve_invoice_cash_authority($1)`, f.invoice.ID).Scan(&policy); err != nil || policy != tc.want {
				t.Fatalf("cash authority = %q, want %q (%v)", policy, tc.want, err)
			}
		})
	}
}

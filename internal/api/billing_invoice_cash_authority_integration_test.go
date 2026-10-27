//go:build integration

package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/superserve-ai/sandbox/internal/billing"
)

type invoiceCashFixture struct {
	h          *Handlers
	team       uuid.UUID
	start, end time.Time
	invoice    StripeInvoiceAmounts
	client     *stripeHTTPClient
	pays       atomic.Int64
}

func newInvoiceCashFixture(t *testing.T, kind string, queuedState string, creditAmounts ...int64) *invoiceCashFixture {
	t.Helper()
	credit := int64(500)
	if len(creditAmounts) > 0 {
		credit = creditAmounts[0]
	}
	net := int64(600) - credit
	status := "open"
	if net == 0 {
		status = "paid"
	}
	h, team := newPlanAssignmentFixture(t)
	f := &invoiceCashFixture{h: h, team: team, start: time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second), end: time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := h.Pool.Exec(t.Context(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	customer, subscription, invoice := "cus_"+team.String(), "sub_"+team.String(), "in_"+team.String()
	exec(`INSERT INTO team_billing_account(team_id,stripe_customer_id,stripe_subscription_id,stripe_subscription_status,commercial_billing_anchor) VALUES($1,$2,$3,'active',$4)`, team, customer, subscription, f.start)
	if kind != "" {
		var contract *uuid.UUID
		if kind == planKindSubscription || kind == planKindCustom {
			// The contract's immutable commercial association is separate from today's plan.
			var id uuid.UUID
			err := h.Pool.QueryRow(t.Context(), `INSERT INTO billing_commercial_contract(team_id,offer_key,starts_at,customer_id,subscription_id,base_price_id,charge_amount_cents,charge_interval_months,entitlement_kind,entitlement_amount_cents,entitlement_cadence,entitlement_anchor)
    VALUES($1,'pro_monthly',$2,$3,$4,'price_base',25000,1,'metered_usage',25000,'monthly',$2) RETURNING id`, team, f.start, customer, subscription).Scan(&id)
			if err != nil {
				t.Fatal(err)
			}
			contract = &id
		}
		exec(`INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason,contract_id)
   VALUES($1,$2,$2,jsonb_build_object('kind',$2::text,'pricing_version','v2'),'{"max_concurrent":1}',$3,'staff','cash authority fixture',$4)`, team, kind, f.start.Add(-time.Hour), contract)
	}
	grant := uuid.New()
	exec(`INSERT INTO team_credit_grant(id,team_id,amount_usd,remaining_usd,reason) VALUES($1,$2,5,5,'unrelated retained fixture grant')`, grant, team)
	exec(`INSERT INTO team_credit_ledger(team_id,grant_id,amount_usd,reason) VALUES($1,$2,5,'fixture original grant')`, team, grant)
	exec(`INSERT INTO billing_invoice_account(team_id,customer_id,subscription_id,adjustment_price_id,adjustment_event_name,adjustment_meter_id,enrolled_at) VALUES($1,$2,$3,'price_round','round','meter_round',$4)`, team, customer, subscription, f.start.Add(-time.Hour))
	exec(`INSERT INTO team_billing_period(team_id,period_start,period_end,status,finalized_at,gross_charges_usd,credits_applied_usd,net_invoice_amount_usd) VALUES($1,$2,$3,'finalized',clock_timestamp(),6,$4::numeric/100,$5::numeric/100)`, team, f.start, f.end, credit, net)
	exec(`INSERT INTO billing_invoice_close(team_id,period_start,period_end,invoice_id,state,verified_at,plan,finalized_evidence)
  VALUES($1,$2,$3,$4,'verified',clock_timestamp(),jsonb_build_object('customer',$5::text,'subscription',$6::text,'expected_cents',600,'credit_before_cents',$7::bigint,'resources',jsonb_build_array(jsonb_build_object('resource','cpu','expected_cents',600,'snapshot',billing_meter_accounting_snapshot($1,$2,$3,'cpu')))), jsonb_build_object('credit_applied_cents',$7::bigint,'credit_remaining_cents',0,'net_cents',$8::bigint))`, team, f.start, f.end, invoice, customer, subscription, credit, net)
	raw := fmt.Sprintf(`{"id":%q,"customer":%q,"currency":"usd","status":%q,"collection_method":"charge_automatically","subtotal":600,"total":%d,"amount_due":%d,"amount_remaining":%d,"ending_balance":0,"parent":{"subscription_details":{"subscription":%q}},"total_pretax_credit_amounts":[{"type":"credit_balance_transaction","amount":%d,"credit_balance_transaction":"cbtxn_fixture"}],"lines":{"data":[{"id":"il_fixture","amount":600}]}}`, invoice, customer, status, net, net, net, subscription, credit)
	if err := json.Unmarshal([]byte(raw), &f.invoice); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/invoices/"+invoice:
			result = f.invoice
		case r.Method == http.MethodGet && r.URL.Path == "/v1/invoice_payments":
			result = map[string]any{"data": []any{}}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions/"+subscription:
			result = map[string]any{"id": subscription, "customer": customer, "status": "active", "collection_method": "charge_automatically", "default_payment_method": "pm_fixture", "items": map[string]any{"data": []any{map[string]any{"id": "si_fixture"}}}, "pause_collection": map[string]any{"behavior": "keep_as_draft"}}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/invoices/"+invoice+"/pay":
			f.pays.Add(1)
			result = map[string]any{}
		default:
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 404)
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	t.Cleanup(srv.Close)
	f.client = &stripeHTTPClient{baseURL: srv.URL, httpClient: srv.Client(), secretKey: "sk_test_fixture", apiVersion: "2026-07-29.dahlia"}
	h.Stripe = f.client
	if queuedState != "" {
		exec(`INSERT INTO billing_invoice_collection(invoice_id,customer_id,subscription_id,livemode,settlement_hash,idempotency_key,payment_authorized,state,first_attempt_at)
   VALUES($1,$2,$3,false,$4,$5,true,$6,CASE WHEN $6='unknown' THEN clock_timestamp()-interval '1 hour' END)`, invoice, customer, subscription, invoiceSettlementHash(f.invoice), "pay:"+invoice, queuedState)
	}
	return f
}

func (f *invoiceCashFixture) claim(t *testing.T) invoiceCollection {
	t.Helper()
	var j invoiceCollection
	j.Token = uuid.New()
	err := f.h.Pool.QueryRow(t.Context(), `UPDATE billing_invoice_collection SET lease_token=$2,lease_until=clock_timestamp()+interval '2 minutes'
  WHERE invoice_id=$1 RETURNING invoice_id,customer_id,subscription_id,livemode,settlement_hash,idempotency_key,payment_authorized,state,first_attempt_at`, f.invoice.ID, j.Token).Scan(&j.Invoice, &j.Customer, &j.Subscription, &j.LiveMode, &j.Hash, &j.Key, &j.Authorized, &j.State, &j.FirstAttempt)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestInvoiceCashHistoricalResidual(t *testing.T) {
	for _, kind := range []string{planKindPrepaid, "", planKindLegacyPAYG, planKindSubscription, planKindCustom} {
		for _, queued := range []string{"pending", "unknown"} {
			for _, recharge := range []string{"absent", "disabled", "enabled"} {
				// Consent cases matter only for prepaid; baseline collection controls need one each.
				if kind != planKindPrepaid && recharge != "absent" {
					continue
				}
				t.Run(kind+"/"+queued+"/"+recharge, func(t *testing.T) {
					f := newInvoiceCashFixture(t, kind, queued)
					if recharge != "absent" {
						if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO billing_prepaid_auto_recharge(team_id,enabled,threshold_cents,amount_cents) VALUES($1,$2,500,500)`, f.team, recharge == "enabled"); err != nil {
							t.Fatal(err)
						}
					}
					before := f.financialSnapshot(t)
					j := f.claim(t)
					state, reason := f.h.collectInvoice(t.Context(), f.client, &j)
					wantPays := int64(1)
					wantState := "unknown"
					if kind == planKindPrepaid {
						wantPays = 0
						wantState = "review_required"
					}
					if state != wantState || f.pays.Load() != wantPays {
						t.Fatalf("state=%s reason=%s pay calls=%d want %s/%d", state, reason, f.pays.Load(), wantState, wantPays)
					}
					if after := f.financialSnapshot(t); before != after {
						t.Fatal("collection changed retained financial rows")
					}
					var authorized bool
					if err := f.h.Pool.QueryRow(t.Context(), `SELECT payment_authorized FROM billing_invoice_collection WHERE invoice_id=$1`, f.invoice.ID).Scan(&authorized); err != nil || !authorized {
						t.Fatalf("prior immutable authorization rewritten: %v %v", authorized, err)
					}
				})
			}
		}
	}
}

func TestInvoiceCashEnqueueAndOldWriterIntent(t *testing.T) {
	for _, kind := range []string{planKindPrepaid, ""} {
		t.Run(kind, func(t *testing.T) {
			f := newInvoiceCashFixture(t, kind, "")
			p := billing.ExportPeriod{TeamID: f.team, Start: f.start, End: f.end}
			if err := f.h.enqueueInvoiceCollection(t.Context(), p, f.invoice, true); err != nil {
				t.Fatal(err)
			}
			var state string
			var authorized bool
			if err := f.h.Pool.QueryRow(t.Context(), `SELECT state,payment_authorized FROM billing_invoice_collection WHERE invoice_id=$1`, f.invoice.ID).Scan(&state, &authorized); err != nil {
				t.Fatal(err)
			}
			if (kind == planKindPrepaid && (state != "review_required" || authorized)) || (kind == "" && (state != "pending" || !authorized)) {
				t.Fatalf("enqueue state=%s authorized=%v", state, authorized)
			}
		})
	}
	// Reproduce the unchanged old writer's last SQL barrier before provider /pay.
	for _, state := range []string{"pending", "unknown"} {
		t.Run("old_writer_"+state, func(t *testing.T) {
			f := newInvoiceCashFixture(t, planKindPrepaid, state)
			_, err := f.h.Pool.Exec(t.Context(), `UPDATE billing_invoice_collection SET first_attempt_at=COALESCE(first_attempt_at,clock_timestamp()),state='unknown' WHERE invoice_id=$1`, f.invoice.ID)
			if err == nil {
				t.Fatal("old writer's payment intent escaped prepaid guard")
			}
			j := f.claim(t)
			got, _ := f.h.collectInvoice(t.Context(), f.client, &j)
			if got != "review_required" || f.pays.Load() != 0 {
				t.Fatal("old/new retry escaped hold", got, f.pays.Load())
			}
		})
	}
}

func TestInvoiceCashFrozenHistoryAndPermissions(t *testing.T) {
	f := newInvoiceCashFixture(t, planKindPrepaid, "pending")
	var policies [8]string
	var errs [8]error
	var wg sync.WaitGroup
	for i := range policies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = f.h.Pool.QueryRow(t.Context(), `SELECT policy FROM resolve_invoice_cash_authority($1)`, f.invoice.ID).Scan(&policies[i])
		}(i)
	}
	wg.Wait()
	for i := range policies {
		if errs[i] != nil || policies[i] != "prepaid_hold" {
			t.Fatal(policies[i], errs[i])
		}
	}
	var before string
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT row_to_json(a)::text FROM billing_invoice_cash_authority a WHERE invoice_id=$1`, f.invoice.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// A later normal switch closes after the historical period, preserving its clipped interval.
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE team_plan_assignment SET effective_to=clock_timestamp() WHERE team_id=$1 AND effective_to IS NULL`, f.team); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason) VALUES($1,'payg','legacy_payg','{}','{"max_concurrent":1}',clock_timestamp(),'staff','later plan')`, f.team); err != nil {
		t.Fatal(err)
	}
	allowed, err := f.h.invoiceResidualCashAllowed(t.Context(), f.invoice.ID, f.invoice.Customer, f.invoice.Parent.SubscriptionDetails.Subscription)
	if err != nil || allowed {
		t.Fatal("later plan reclassified historical prepaid", allowed, err)
	}
	var after string
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT row_to_json(a)::text FROM billing_invoice_cash_authority a WHERE invoice_id=$1`, f.invoice.ID).Scan(&after); err != nil || before != after {
		t.Fatal("frozen evidence changed", err)
	}
	for _, query := range []string{`UPDATE billing_invoice_cash_authority SET policy='residual_allowed' WHERE invoice_id=$1`, `DELETE FROM billing_invoice_cash_authority WHERE invoice_id=$1`} {
		if _, err := f.h.Pool.Exec(t.Context(), query, f.invoice.ID); err == nil {
			t.Fatal("authority mutation accepted")
		}
	}
	var publicExecute, tenantTable bool
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT has_function_privilege('authenticated','resolve_invoice_cash_authority(text)','EXECUTE'),has_table_privilege('authenticated','billing_invoice_cash_authority','INSERT')`).Scan(&publicExecute, &tenantTable); err != nil || publicExecute || tenantTable {
		t.Fatal("tenant authority write exposed", publicExecute, tenantTable, err)
	}
	// Legacy absence is frozen too: a privileged backdated assignment cannot retroactively alter it.
	legacy := newInvoiceCashFixture(t, "", "pending")
	if _, err := legacy.h.invoiceResidualCashAllowed(t.Context(), legacy.invoice.ID, legacy.invoice.Customer, legacy.invoice.Parent.SubscriptionDetails.Subscription); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.h.Pool.Exec(t.Context(), `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason) VALUES($1,'starter','prepaid','{}','{"max_concurrent":1}',$2,'staff','backdate')`, legacy.team, legacy.start); err == nil {
		t.Fatal("backdated plan rewrote frozen absence")
	}
}

// Snapshot the retained period, grant/ledger, payment, assignment and contract
// rows separately from the collection job's intentional retry state changes.
func (f *invoiceCashFixture) financialSnapshot(t *testing.T) string {
	t.Helper()
	combined := ""
	for _, table := range []string{"team_billing_period", "billing_invoice_close", "team_credit_grant", "team_credit_ledger", "billing_prepaid_purchase", "team_plan_assignment", "billing_commercial_contract", "billing_usage_entitlement", "team_billing_account"} {
		var rows string
		err := f.h.Pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]')::text FROM %s x WHERE team_id=$1`, table), f.team).Scan(&rows)
		if err != nil {
			t.Fatal(table, err)
		}
		combined += table + rows
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(combined)))
}

func TestInvoiceCashMixedPeriodsAndContractAssociation(t *testing.T) {
	for _, first := range []string{planKindPrepaid, planKindSubscription} {
		t.Run(first, func(t *testing.T) {
			f := newInvoiceCashFixture(t, first, "pending")
			midpoint := f.start.Add(12 * time.Hour)
			if _, err := f.h.Pool.Exec(t.Context(), `UPDATE team_plan_assignment SET effective_to=$2 WHERE team_id=$1 AND effective_to IS NULL`, f.team, midpoint); err != nil {
				t.Fatal(err)
			}
			next := planKindPrepaid
			var contract *uuid.UUID
			if first == planKindPrepaid {
				next = planKindSubscription
				var id uuid.UUID
				err := f.h.Pool.QueryRow(t.Context(), `INSERT INTO billing_commercial_contract(team_id,offer_key,starts_at,customer_id,subscription_id,base_price_id,charge_amount_cents,charge_interval_months,entitlement_kind,entitlement_amount_cents,entitlement_cadence,entitlement_anchor)
     VALUES($1,'pro_monthly',$2,$3,$4,'price_base',25000,1,'metered_usage',25000,'monthly',$2) RETURNING id`, f.team, f.start, f.invoice.Customer, f.invoice.Parent.SubscriptionDetails.Subscription).Scan(&id)
				if err != nil {
					t.Fatal(err)
				}
				contract = &id
			}
			if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason,contract_id) VALUES($1,$2,$2,jsonb_build_object('kind',$2::text,'pricing_version','v2'),'{"max_concurrent":1}',$3,'staff','mixed history',$4)`, f.team, next, midpoint, contract); err != nil {
				t.Fatal(err)
			}
			j := f.claim(t)
			state, _ := f.h.collectInvoice(t.Context(), f.client, &j)
			if state != "review_required" || f.pays.Load() != 0 {
				t.Fatal("mixed period acquired retroactive consent", state, f.pays.Load())
			}
		})
	}
	t.Run("wrong retained commercial association", func(t *testing.T) {
		f := newInvoiceCashFixture(t, "", "pending")
		other := newInvoiceCashFixture(t, planKindSubscription, "pending")
		if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason,contract_id)
   SELECT $1,'pro','subscription','{"kind":"subscription","pricing_version":"v2"}','{"max_concurrent":1}',$2,'staff','incorrect historical association',id FROM billing_commercial_contract WHERE team_id=$3`, f.team, f.start, other.team); err != nil {
			t.Fatal(err)
		}
		j := f.claim(t)
		state, _ := f.h.collectInvoice(t.Context(), f.client, &j)
		if state != "review_required" || f.pays.Load() != 0 {
			t.Fatal("mismatched historical contract collected", state, f.pays.Load())
		}
	})
}

func TestInvoiceCashHistoricalBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name          string
		startAt       func(*invoiceCashFixture) time.Time
		kind, pricing string
		wantCash      bool
	}{
		{"exact end belongs to next period", func(f *invoiceCashFixture) time.Time { return f.end }, planKindPrepaid, "v2", true},
		{"v2 gap requires review", func(f *invoiceCashFixture) time.Time { return f.start.Add(time.Hour) }, planKindLegacyPAYG, "v2", false},
		{"legacy bootstrap gap preserves cash", func(f *invoiceCashFixture) time.Time { return f.start.Add(time.Hour) }, planKindLegacyPAYG, "legacy", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInvoiceCashFixture(t, "", "pending")
			if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason)
    VALUES($1,$2,$2,jsonb_build_object('kind',$2::text,'pricing_version',$3::text),'{"max_concurrent":1}',$4,'staff','boundary fixture')`, f.team, tc.kind, tc.pricing, tc.startAt(f)); err != nil {
				t.Fatal(err)
			}
			j := f.claim(t)
			state, _ := f.h.collectInvoice(t.Context(), f.client, &j)
			if tc.wantCash && (state != "unknown" || f.pays.Load() != 1) {
				t.Fatal("historical allowed interval held", state, f.pays.Load())
			}
			if !tc.wantCash && (state != "review_required" || f.pays.Load() != 0) {
				t.Fatal("historical gap collected", state, f.pays.Load())
			}
		})
	}
	t.Run("resolve after later plan switch", func(t *testing.T) {
		f := newInvoiceCashFixture(t, planKindPrepaid, "pending")
		if _, err := f.h.Pool.Exec(t.Context(), `UPDATE team_plan_assignment SET effective_to=clock_timestamp() WHERE team_id=$1 AND effective_to IS NULL`, f.team); err != nil {
			t.Fatal(err)
		}
		if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason) VALUES($1,'payg','legacy_payg','{"pricing_version":"legacy"}','{"max_concurrent":1}',clock_timestamp(),'staff','later switch')`, f.team); err != nil {
			t.Fatal(err)
		}
		j := f.claim(t)
		state, _ := f.h.collectInvoice(t.Context(), f.client, &j)
		if state != "review_required" || f.pays.Load() != 0 {
			t.Fatal("current plan granted historical consent", state, f.pays.Load())
		}
	})
	for _, before := range []bool{false, true} {
		t.Run(fmt.Sprintf("unfinished prepaid request before period %v", before), func(t *testing.T) {
			f := newInvoiceCashFixture(t, "", "pending")
			at := f.start.Add(time.Hour)
			if before {
				at = f.start.Add(-time.Hour)
			}
			if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO billing_prepaid_purchase(team_id,customer_id,source,amount_cents,actor_id,payment_idempotency_key,credit_idempotency_key,plan_snapshot,created_at)
    VALUES($1,$2,'checkout',500,$3,'','','{"terms":{"kind":"prepaid"}}',$4)`, f.team, f.invoice.Customer, uuid.New(), at); err != nil {
				t.Fatal(err)
			}
			if before {
				if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason) VALUES($1,'payg','legacy_payg','{"kind":"legacy_payg","pricing_version":"legacy"}','{"max_concurrent":1}',$2,'staff','legacy gap after unfinished purchase')`, f.team, f.start.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			j := f.claim(t)
			state, _ := f.h.collectInvoice(t.Context(), f.client, &j)
			if state != "review_required" || f.pays.Load() != 0 {
				t.Fatal("unfinished prepaid activation collected", state, f.pays.Load())
			}
		})
	}
}

func TestInvoiceCashLegacyGapAfterV2History(t *testing.T) {
	f := newInvoiceCashFixture(t, "", "pending")
	if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason) VALUES($1,'free','free','{"kind":"free","pricing_version":"v2"}','{"max_concurrent":1}',$2,'staff','earlier v2 history')`, f.team, f.start.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE team_plan_assignment SET effective_to=$2 WHERE team_id=$1 AND effective_to IS NULL`, f.team, f.start.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason) VALUES($1,'payg','legacy_payg','{"kind":"legacy_payg","pricing_version":"legacy"}','{"max_concurrent":1}',$2,'staff','legacy after gap')`, f.team, f.start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	j := f.claim(t)
	state, _ := f.h.collectInvoice(t.Context(), f.client, &j)
	if state != "review_required" || f.pays.Load() != 0 {
		t.Fatal("gap following v2 history acquired cash consent", state, f.pays.Load())
	}
}

// Test only the newly introduced table's backend RLS policy. This local
// non-BYPASS role lacks access to legacy close tables; the API harness resolves
// invoices as its existing postgres role. Actual deployment role proof is separate.
func TestInvoiceCashServiceRoleTablePolicy(t *testing.T) {
	f := newInvoiceCashFixture(t, "", "pending")
	tx, err := f.h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SET LOCAL ROLE service_role`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `INSERT INTO billing_invoice_cash_authority(invoice_id,team_id,period_start,period_end,customer_id,subscription_id,policy_version,policy,evidence)
  VALUES($1,$2,$3,$4,$5,$6,1,'residual_allowed','{"fixture":"new table policy only"}')`, f.invoice.ID, f.team, f.start, f.end, f.invoice.Customer, f.invoice.Parent.SubscriptionDetails.Subscription); err != nil {
		t.Fatal("new table backend insert policy", err)
	}
	var policy string
	if err = tx.QueryRow(t.Context(), `SELECT policy FROM billing_invoice_cash_authority WHERE invoice_id=$1`, f.invoice.ID).Scan(&policy); err != nil || policy != "residual_allowed" {
		t.Fatal("new table backend select policy", policy, err)
	}
}

func TestInvoiceCashFullyCreditedStarter(t *testing.T) {
	f := newInvoiceCashFixture(t, planKindPrepaid, "", 600)
	if err := VerifyBillingInvoiceSettlement(f.invoice, 600, 600, 0); err != nil {
		t.Fatal("fully credited provider settlement invalid", err)
	}
	p := billing.ExportPeriod{TeamID: f.team, Start: f.start, End: f.end}
	if err := f.h.enqueueInvoiceCollection(t.Context(), p, f.invoice, true); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT state FROM billing_invoice_collection WHERE invoice_id=$1`, f.invoice.ID).Scan(&state); err != nil || state != "paid" {
		t.Fatal("fully credited Starter settlement held", state, err)
	}
	j := f.claim(t)
	state, reason := f.h.collectInvoice(t.Context(), f.client, &j)
	if state != "paid" || f.pays.Load() != 0 {
		t.Fatal("paid zero-cash settlement changed", state, reason, f.pays.Load())
	}
}

// These are retained historical purchases: a covering paid assignment is not
// proof that an earlier payment or credit operation has finished.
func TestInvoiceCashPreperiodUnresolvedPurchaseWithFullCoverage(t *testing.T) {
	for _, purchase := range []struct {
		name, source string
		snapshot     bool
	}{
		{"checkout", "checkout", true},
		{"legacy checkout", "checkout", false},
		{"auto recharge", "auto_recharge", false},
	} {
		for _, kind := range []string{planKindLegacyPAYG, planKindSubscription, ""} {
			if kind == "" && purchase.snapshot {
				continue
			} // Preserve the original snapshot-based no-history policy.
			for _, outcome := range []struct {
				name, payment, credit string
				hold                  bool
			}{
				{"unopened", "pending", "none", true},
				{"payment uncertain", "uncertain", "none", true},
				{"payment processing", "processing", "none", true},
				{"paid unfulfilled", "succeeded", "none", true},
				{"credit pending", "succeeded", "pending", true},
				{"credit uncertain", "succeeded", "uncertain", true},
				{"credit issued late", "succeeded", "issued", true},
				{"credit issued before period", "succeeded", "issued_before", false},
				{"checkout expired", "failed", "none", false},
				{"checkout canceled", "failed", "none", false},
			} {
				t.Run(purchase.name+"/"+kind+"/"+outcome.name, func(t *testing.T) {
					f := newInvoiceCashFixture(t, kind, "pending")
					f.quiescePrepaidPurchases(t)
					id := uuid.New()
					exec := func(query string, args ...any) {
						t.Helper()
						if _, err := f.h.Pool.Exec(t.Context(), query, args...); err != nil {
							t.Fatal(err)
						}
					}
					exec(`INSERT INTO billing_prepaid_purchase(id,team_id,customer_id,source,amount_cents,actor_id,payment_idempotency_key,credit_idempotency_key,plan_snapshot,created_at)
 VALUES($1,$2,$3,$6,500,$4,'','',CASE WHEN $7 THEN '{"terms":{"kind":"prepaid"}}'::jsonb END,$5)`, id, f.team, f.invoice.Customer, uuid.New(), f.start.Add(-2*time.Hour), purchase.source, purchase.snapshot)
					switch outcome.payment {
					case "succeeded":
						exec(`UPDATE billing_prepaid_purchase SET payment_state='succeeded',paid_at=clock_timestamp(),payment_evidence=jsonb_build_object('amount_cents',amount_cents,'customer',customer_id) WHERE id=$1`, id)
					case "failed":
						exec(`UPDATE billing_prepaid_purchase SET payment_state='failed',failure_reason=$2 WHERE id=$1`, id, outcome.name)
					case "uncertain", "processing":
						exec(`UPDATE billing_prepaid_purchase SET payment_state=$2,payment_first_attempt_at=clock_timestamp() WHERE id=$1`, id, outcome.payment)
					}
					if outcome.credit != "none" {
						request, err := prepaidCreditRequest(id, f.invoice.Customer, 500, purchase.source)
						if err != nil {
							t.Fatal(err)
						}
						exec(`UPDATE billing_prepaid_purchase SET credit_state='pending',credit_request=$2 WHERE id=$1`, id, request)
						if outcome.credit != "pending" {
							exec(`UPDATE billing_prepaid_purchase SET credit_state='uncertain',credit_first_attempt_at=clock_timestamp() WHERE id=$1`, id)
						}
						if outcome.credit == "issued" || outcome.credit == "issued_before" {
							exec(`UPDATE billing_prepaid_purchase SET credit_state='issued',stripe_credit_grant_id=$2,credit_evidence=credit_request || jsonb_build_object('id',$2::text),credited_at=clock_timestamp() WHERE id=$1`, id, "cg_"+id.String())
						}
						if outcome.credit == "issued_before" {
							// Import a valid old grant timestamp into this disposable historical
							// fixture. Production's immutable paid/grant evidence is never rewritten.
							tx, err := f.h.Pool.Begin(t.Context())
							if err != nil {
								t.Fatal(err)
							}
							defer tx.Rollback(t.Context())
							for _, query := range []string{
								`ALTER TABLE billing_prepaid_purchase DISABLE TRIGGER billing_prepaid_purchase_guard`,
								`UPDATE billing_prepaid_purchase SET paid_at=created_at,credit_first_attempt_at=created_at,credited_at=created_at WHERE id=$1`,
								`ALTER TABLE billing_prepaid_purchase ENABLE TRIGGER billing_prepaid_purchase_guard`,
							} {
								var args []any
								if query[0:6] == "UPDATE" {
									args = []any{id}
								}
								if _, err := tx.Exec(t.Context(), query, args...); err != nil {
									t.Fatal(err)
								}
							}
							if err := tx.Commit(t.Context()); err != nil {
								t.Fatal(err)
							}
						}
					}
					before := f.financialSnapshot(t)
					j := f.claim(t)
					state, reason := f.h.collectInvoice(t.Context(), f.client, &j)
					wantState, wantPays := "unknown", int64(1)
					if outcome.hold {
						wantState, wantPays = "review_required", 0
					}
					if state != wantState || f.pays.Load() != wantPays {
						t.Fatalf("pre-period purchase cash fence: state=%s reason=%s pays=%d want=%s/%d", state, reason, f.pays.Load(), wantState, wantPays)
					}
					if after := f.financialSnapshot(t); before != after {
						t.Fatal("cash decision rewrote retained financial rows")
					}
					var policy string
					var count int
					var gap bool
					if err := f.h.Pool.QueryRow(t.Context(), `SELECT policy,jsonb_array_length(evidence->'assignments'),(evidence->>'gap')::boolean FROM billing_invoice_cash_authority WHERE invoice_id=$1`, f.invoice.ID).Scan(&policy, &count, &gap); err != nil {
						t.Fatal(err)
					}
					wantCount := 1
					if kind == "" {
						wantCount = 0
					}
					if count != wantCount || gap {
						t.Fatal("fixture must cover the complete invoice interval", count, gap)
					}
					if outcome.hold {
						if policy != "prepaid_hold" {
							t.Fatal("cash authority did not freeze prepaid hold", policy)
						}
						// A retained old writer cannot bypass the collector and save a cash intent.
						if _, err := f.h.Pool.Exec(t.Context(), `UPDATE billing_invoice_collection SET state='unknown',first_attempt_at=clock_timestamp() WHERE invoice_id=$1`, f.invoice.ID); err == nil {
							t.Fatal("old writer bypassed the SQL cash fence")
						}
					} else if policy != "residual_allowed" {
						t.Fatal("settled prior purchase blocked legitimate residual cash", policy)
					}
				})
			}
		}
	}
}

func TestInvoiceCashSnapshotlessFailedPurchaseDoesNotBlockLegacyCash(t *testing.T) {
	for _, source := range []string{"checkout", "auto_recharge"} {
		for _, kind := range []string{"", planKindLegacyPAYG, planKindSubscription} {
			for _, before := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/before=%v", source, kind, before), func(t *testing.T) {
					f := newInvoiceCashFixture(t, kind, "pending")
					f.quiescePrepaidPurchases(t)
					at := f.start.Add(time.Hour)
					if before {
						at = f.start.Add(-time.Hour)
					}
					var id uuid.UUID
					if err := f.h.Pool.QueryRow(t.Context(), `INSERT INTO billing_prepaid_purchase(team_id,customer_id,source,amount_cents,actor_id,payment_idempotency_key,credit_idempotency_key,created_at) VALUES($1,$2,$3,500,$4,'','',$5) RETURNING id`, f.team, f.invoice.Customer, source, uuid.New(), at).Scan(&id); err != nil {
						t.Fatal(err)
					}
					if _, err := f.h.Pool.Exec(t.Context(), `UPDATE billing_prepaid_purchase SET payment_state='failed',failure_reason='checkout expired or charge conclusively failed' WHERE id=$1`, id); err != nil {
						t.Fatal(err)
					}
					j := f.claim(t)
					state, reason := f.h.collectInvoice(t.Context(), f.client, &j)
					if state != "unknown" || f.pays.Load() != 1 {
						t.Fatal("terminal snapshotless purchase held legitimate legacy cash", state, reason, f.pays.Load())
					}
				})
			}
		}
	}
}

// Later worker tests scan the shared disposable database. Keep these retained
// unresolved funding examples without letting another fixture fulfill them.
func (f *invoiceCashFixture) quiescePrepaidPurchases(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_, err := f.h.Pool.Exec(context.Background(), `UPDATE billing_prepaid_purchase SET next_attempt_at='9999-01-01',credit_balance_checked_at='9999-01-01',reversal_checked_at='9999-01-01',reversal_check_requested_at=NULL,reversal_retry_after='9999-01-01' WHERE team_id=$1`, f.team)
		if err != nil {
			t.Errorf("quiesce retained cash funding fixture: %v", err)
		}
	})
}

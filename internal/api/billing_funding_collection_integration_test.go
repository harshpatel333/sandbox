//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/superserve-ai/sandbox/internal/billing"
	"github.com/superserve-ai/sandbox/internal/config"
	"github.com/superserve-ai/sandbox/internal/db"
)

func TestFundingReplacementEnrollmentAndScopedCreditSettlement(t *testing.T) {
	h, team := fundingTransitionFixture(t)
	ctx := t.Context()
	poolConfig := h.Pool.Config()
	poolConfig.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h.Pool, h.DB = pool, db.New(pool)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	end := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	start, anchor := end.AddDate(0, -1, 0), end.AddDate(0, -2, 0)
	customer, oldSub, replacement := "cus_"+team.String(), "sub_old_"+team.String(), "sub_new_"+team.String()
	exec(`INSERT INTO team_billing_account(team_id,stripe_customer_id,stripe_subscription_id,stripe_subscription_status,commercial_billing_anchor) VALUES($1,$2,$3,'canceled',$4)`, team, customer, oldSub, anchor)
	contract, err := h.DB.CreateCommercialContract(ctx, db.CreateCommercialContractParams{TeamID: team, OfferKey: "pro_monthly", StartsAt: anchor, CustomerID: customer, SubscriptionID: oldSub, BasePriceID: "price_base", ChargeAmountCents: 120000, ChargeIntervalMonths: 1, EntitlementKind: "metered_usage", EntitlementAmountCents: 100000, EntitlementCadence: "monthly", EntitlementAnchor: anchor})
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE billing_signup_choice SET offer_key='pro_monthly',state='committed',generation=now(),actor_id=$3,base_price_id='price_base',request_payload='{}',subscription_id=$2 WHERE team_id=$1`, team, oldSub, uuid.New())
	exec(`INSERT INTO billing_invoice_account(team_id,customer_id,subscription_id,adjustment_price_id,adjustment_event_name,adjustment_meter_id,enrolled_at) VALUES($1,$2,$3,'price_round','round','mtr_round',$4)`, team, customer, oldSub, anchor)
	exec(`INSERT INTO team_feature_flag(team_id,key,enabled) VALUES($1,'billing_export_enabled',true) ON CONFLICT(team_id,key) DO UPDATE SET enabled=true`, team)
	planKey := "funding-replacement-" + team.String()
	exec(`INSERT INTO pricing_plan(key,name) VALUES($1,'Replacement test')`, planKey)
	exec(`INSERT INTO pricing_rate(plan_key,resource,unit,price_usd,effective_from) VALUES($1,'vcpu','second',0.00001,$2),($1,'memory_gib','second',0.00001,$2)`, planKey, anchor)
	exec(`INSERT INTO team_pricing_plan(team_id,plan_key,effective_from) VALUES($1,$2,$3)`, team, planKey, anchor)
	h.Config = &config.Config{BillingResources: []config.BillingResourceConfig{
		{ResourceKey: "vcpu", UsageUnit: "second", StripePriceID: "price_cpu", StripeEventName: "cpu", Billable: true, CheckoutEnabled: true},
		{ResourceKey: "memory_gib", UsageUnit: "second", StripePriceID: "price_memory", StripeEventName: "memory", Billable: true, CheckoutEnabled: true},
	}}
	grant := testEntitlementGrant()
	grant.ID, grant.Customer, grant.Metadata = "credgr_"+team.String(), customer, nil
	grant.Created, grant.EffectiveAt, grant.ExpiresAt = anchor.Unix(), anchor.Unix(), end.AddDate(0, 3, 0).Unix()
	grant.Applicability.Scope.Prices = grant.Applicability.Scope.Prices[:3]
	for i, price := range []string{"price_cpu", "price_memory", "price_round"} {
		grant.Applicability.Scope.Prices[i].ID = price
	}
	balance := int64(100000)
	price := func(id string) map[string]any {
		amount := "3.6"
		if id == "round" {
			amount = "1"
		}
		return map[string]any{"id": "price_" + id, "product": "prod_example", "active": true, "currency": "usd", "billing_scheme": "per_unit", "unit_amount_decimal": amount, "recurring": map[string]any{"usage_type": "metered", "interval": "month", "interval_count": 1, "meter": "mtr_" + id}}
	}
	subscription := func(id, status string) map[string]any {
		items := []any{}
		for _, resource := range []string{"cpu", "memory", "round"} {
			items = append(items, map[string]any{"id": "si_" + resource, "price": price(resource), "current_period_start": end.Unix(), "current_period_end": end.AddDate(0, 1, 0).Unix()})
		}
		return map[string]any{"id": id, "customer": customer, "status": status, "collection_method": "charge_automatically", "billing_cycle_anchor": anchor.Unix(), "pause_collection": map[string]any{"behavior": "keep_as_draft"}, "items": map[string]any{"data": items}}
	}
	invoiceID := "in_" + team.String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			t.Errorf("preserved-credit path created or altered provider object: %s %s", r.Method, r.URL)
		}
		var out any
		switch r.URL.Path {
		case "/v1/subscriptions/" + oldSub:
			out = subscription(oldSub, "canceled")
		case "/v1/subscriptions/" + replacement:
			out = subscription(replacement, "active")
		case "/v1/subscriptions":
			out = map[string]any{"data": []any{subscription(oldSub, "canceled"), subscription(replacement, "active")}}
		case "/v1/prices/price_round":
			out = price("round")
		case "/v1/billing/meters":
			meters := []any{}
			for _, id := range []string{"cpu", "memory", "round"} {
				meters = append(meters, map[string]any{"id": "mtr_" + id, "event_name": id, "default_aggregation": map[string]any{"formula": "sum"}, "customer_mapping": map[string]any{"event_payload_key": "stripe_customer_id"}, "value_settings": map[string]any{"event_payload_key": "value"}})
			}
			out = map[string]any{"data": meters}
		case "/v1/billing/credit_grants":
			out = map[string]any{"data": []any{grant}}
		case "/v1/billing/credit_grants/" + grant.ID:
			out = grant
		case "/v1/billing/credit_balance_transactions":
			out = map[string]any{"data": []any{}}
		case "/v1/billing/credit_balance_transactions/cbtxn_example":
			out = map[string]any{"id": "cbtxn_example", "credit_grant": grant.ID, "type": "debit", "debit": map[string]any{"type": "credits_applied", "amount": map[string]any{"type": "monetary", "monetary": map[string]any{"currency": "usd", "value": 100}}, "credits_applied": map[string]any{"invoice": invoiceID, "invoice_line_item": "il_cpu"}}}
		case "/v1/billing/credit_balance_summary":
			out = map[string]any{"balances": []any{map[string]any{"ledger_balance": map[string]any{"monetary": map[string]any{"currency": "usd", "value": balance}}}}}
		case "/v1/invoices":
			out = map[string]any{"data": []any{}}
		default:
			t.Errorf("unexpected provider request: %s", r.URL)
			http.Error(w, "unexpected", 404)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	client := &stripeHTTPClient{baseURL: srv.URL, httpClient: srv.Client(), apiVersion: "2026-07-29.dahlia"}
	h.Stripe = client
	if err := h.commitFreeFundingPlan(ctx, team, uuid.New()); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE team_billing_account SET stripe_subscription_id=$2,stripe_subscription_status='active' WHERE team_id=$1`, team, replacement)
	// Resume an existing held enrollment attempt from the replacement's first
	// billing period, then close that completed period with its preserved grant.
	exec(`UPDATE billing_invoice_enrollment SET started_at=$2,requested_at=$2 WHERE team_id=$1`, team, start)
	enrollmentCtx, cancelEnrollment := context.WithTimeout(ctx, 3*time.Second)
	defer cancelEnrollment()
	if err := h.enrollInvoiceAccount(enrollmentCtx, team, nil); err != nil {
		t.Fatalf("replacement enrollment rejected scoped expiring credit: %v", err)
	}
	a, enrolled, err := h.invoiceAccount(ctx, team)
	if err != nil || !enrolled || a.Subscription != replacement || a.Base.Price != "" {
		t.Fatalf("replacement not enrolled as PAYG: %+v %v", a, err)
	}
	if _, err := client.invoiceCreditLedger(ctx, customer, end.Unix()); err == nil {
		t.Fatal("legacy PAYG ledger unexpectedly accepted scoped expiring grants")
	}
	p := billing.ExportPeriod{TeamID: team, Start: start, End: end}
	plan := billing.InvoiceClosePlan{Customer: customer, Subscription: replacement, InvoiceStart: start.Unix(), InvoiceEnd: end.Unix(), ExpectedCents: 100, Resources: []billing.InvoiceCloseResource{{PriceID: "price_cpu"}, {PriceID: "price_memory"}}}
	released, err := h.prepareReleasedFundingCredits(ctx, p, a, invoiceID, &plan, client)
	if err != nil || !released || plan.CreditVersion != 1 || plan.CreditBeforeCents != 100000 || len(plan.CreditGrants) != 1 {
		t.Fatalf("replacement credit plan=%+v released=%v err=%v", plan, released, err)
	}
	raw, _ := json.Marshal(plan)
	exec(`INSERT INTO team_billing_period(team_id,period_start,period_end,status) VALUES($1,$2,$3,'exporting')`, team, start, end)
	exec(`INSERT INTO billing_invoice_close(team_id,period_start,period_end,invoice_id,plan) VALUES($1,$2,$3,$4,$5)`, team, start, end, invoiceID, raw)
	before, err := h.invoicePlanCredits(ctx, p, a, plan, nil, client)
	if err != nil || before != 100000 {
		t.Fatalf("audited inventory fallback failed before finalization: %d %v", before, err)
	}
	balance -= 100
	var invoice StripeInvoiceAmounts
	invoiceJSON := fmt.Sprintf(`{"id":%q,"customer":%q,"currency":"usd","status":"paid","subtotal":100,"total":0,"amount_due":0,"amount_remaining":0,"starting_balance":0,"ending_balance":0,"total_pretax_credit_amounts":[{"type":"credit_balance_transaction","amount":100,"credit_balance_transaction":"cbtxn_example"}],"lines":{"data":[{"id":"il_cpu","pricing":{"price_details":{"price":"price_cpu"}},"pretax_credit_amounts":[{"type":"credit_balance_transaction","amount":100,"credit_balance_transaction":"cbtxn_example"}]}]}}`, invoiceID, customer)
	if err := json.Unmarshal([]byte(invoiceJSON), &invoice); err != nil {
		t.Fatal(err)
	}
	after, err := h.invoicePlanCredits(ctx, p, a, plan, &invoice, client)
	if err != nil || after != 99900 {
		t.Fatalf("preserved grant settlement not proven: %d %v", after, err)
	}
	if err := verifyInvoicePlanSettlement(invoice, plan, before, after); err != nil {
		t.Fatalf("normal scoped grant could not settle replacement usage: %v", err)
	}
	var ended bool
	if err := h.Pool.QueryRow(ctx, `SELECT status='ended' AND NOT EXISTS(SELECT 1 FROM billing_usage_entitlement WHERE contract_id=$1) FROM billing_commercial_contract WHERE id=$1`, contract.ID).Scan(&ended); err != nil || !ended {
		t.Fatalf("inventory fallback revived old benefits: %v %v", ended, err)
	}
	// Any change after the prepared plan stays held rather than charging cash.
	grant.VoidedAt = now.Unix()
	if _, err := h.invoicePlanCredits(ctx, p, a, plan, nil, client); err == nil || !strings.Contains(err.Error(), "credits changed") {
		t.Fatalf("voided preserved credit was silently accepted: %v", err)
	}
}

func TestFundingPreservedIssuedAllowanceCannotDisappear(t *testing.T) {
	f := newEntitlementFixture(t)
	ctx := t.Context()
	if _, err := f.h.reconcileUsageEntitlement(ctx, f.c.ID); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	original, err := json.Marshal(f.grant)
	f.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Pool.Exec(ctx, `UPDATE billing_commercial_contract SET status='ended',revision=revision+1 WHERE id=$1`, f.c.ID); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"preserved", "missing-from-plan", "expired-before-invoice", "voided", "already-consumed"} {
		t.Run(scenario, func(t *testing.T) {
			var grant entitlementGrant
			if err := json.Unmarshal(original, &grant); err != nil {
				t.Fatal(err)
			}
			balance := grant.Amount.Monetary.Value
			grants := []billing.InvoiceCreditGrant{{ID: grant.ID, Before: balance, ExpiresAt: grant.ExpiresAt, Evidence: original}}
			switch scenario {
			case "missing-from-plan":
				grants = nil
			case "expired-before-invoice":
				grant.ExpiresAt = f.end.Unix()
			case "voided":
				grant.VoidedAt = time.Now().Unix()
			case "already-consumed":
				balance, grants = 0, nil
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("promise verification changed provider state")
				}
				switch r.URL.Path {
				case "/v1/billing/credit_grants/" + grant.ID:
					_ = json.NewEncoder(w).Encode(grant)
				case "/v1/billing/credit_balance_summary":
					_ = json.NewEncoder(w).Encode(map[string]any{"balances": []any{map[string]any{"ledger_balance": map[string]any{"monetary": map[string]any{"currency": "usd", "value": balance}}}}})
				default:
					t.Errorf("unexpected promise request: %s", r.URL)
					http.Error(w, "unexpected", 404)
				}
			}))
			defer srv.Close()
			client := &stripeHTTPClient{baseURL: srv.URL, httpClient: srv.Client(), apiVersion: "2026-07-29.dahlia"}
			p := billing.ExportPeriod{TeamID: f.c.TeamID, Start: f.start, End: f.end}
			err := f.h.verifyPreservedFundingCredits(ctx, p, f.c.ID, f.c.CustomerID, f.end.Unix(), []string{"price_cpu", "price_memory", "price_storage", "price_round"}, grants, client)
			valid := scenario == "preserved" || scenario == "already-consumed"
			if (err == nil) != valid {
				t.Fatalf("valid=%v err=%v", valid, err)
			}
		})
	}
	if f.grants.Load() != 1 {
		t.Fatal("ended allowance was issued again")
	}
}

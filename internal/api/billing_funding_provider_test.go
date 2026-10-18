package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func fundingTestCharge() fundingCharge {
	return fundingCharge{ID: "ch_example", Customer: "cus_example", Currency: "usd", Status: "succeeded", Paid: true, Captured: true, AmountCaptured: 100, PaymentMethod: "pm_example", PaymentIntent: "pi_example"}
}

func TestFundingRequiresCapturedUnreversedCash(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fundingCharge)
	}{
		{"zero-cash", func(c *fundingCharge) { c.AmountCaptured = 0 }},
		{"authorization-only", func(c *fundingCharge) { c.Captured = false }},
		{"unpaid", func(c *fundingCharge) { c.Paid = false }},
		{"failed", func(c *fundingCharge) { c.Status = "failed" }},
		{"partial-refund", func(c *fundingCharge) { c.AmountRefunded = 1 }},
		{"refunded", func(c *fundingCharge) { c.Refunded = true }},
		{"disputed", func(c *fundingCharge) { c.Disputed = true }},
		{"wrong-customer", func(c *fundingCharge) { c.Customer = "cus_other" }},
		{"wrong-mode", func(c *fundingCharge) { c.Live = true }},
		{"non-usd", func(c *fundingCharge) { c.Currency = "eur" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			charge := fundingTestCharge()
			if !charge.collected("cus_example", false) {
				t.Fatal("valid cash evidence rejected")
			}
			tc.mutate(&charge)
			if charge.collected("cus_example", false) {
				t.Fatal("invalid cash evidence authorized funding")
			}
		})
	}
}

type fundingTestProvider struct {
	setup      map[string]any
	method     map[string]any
	intent     map[string]any
	saved      fundingCharge
	charges    []fundingCharge
	hasMore    bool
	debtMore   bool
	debt       bool
	cursor     string
	chargeGets int
	intentGets int
}

func newFundingTestProvider() *fundingTestProvider {
	return &fundingTestProvider{
		setup:  map[string]any{"id": "seti_example", "customer": "cus_example", "payment_method": "pm_example", "status": "succeeded", "usage": "off_session", "livemode": false},
		method: map[string]any{"id": "pm_example", "customer": "cus_example", "livemode": false, "type": "card", "card": map[string]any{"exp_month": 12, "exp_year": time.Now().Year() + 1, "fingerprint": "fp_example"}},
		intent: map[string]any{"id": "pi_example", "customer": "cus_example", "payment_method": "pm_example", "latest_charge": "ch_example", "status": "succeeded", "currency": "usd", "amount_received": 100, "setup_future_usage": "off_session", "livemode": false},
		saved:  fundingTestCharge(),
	}
}

func (p *fundingTestProvider) client(t *testing.T) *stripeHTTPClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("funding observation mutated provider: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		var response any
		switch r.URL.Path {
		case "/v1/customers/cus_example":
			response = map[string]any{"id": "cus_example", "livemode": false, "invoice_settings": map[string]any{"default_payment_method": "pm_example"}}
		case "/v1/payment_methods/pm_example":
			response = p.method
		case "/v1/setup_intents":
			if r.URL.Query().Get("payment_method") != "pm_example" || r.URL.Query().Get("limit") != "100" {
				t.Error("setup discovery is not bounded and scoped to effective method")
			}
			data := []any{}
			if p.setup != nil {
				data = append(data, p.setup)
			}
			response = map[string]any{"data": data}
		case "/v1/charges/ch_example":
			p.chargeGets++
			response = p.saved
		case "/v1/charges":
			if r.URL.Query().Get("customer") != "cus_example" || r.URL.Query().Get("limit") != "100" {
				t.Error("charge discovery is not bounded and scoped to customer")
			}
			p.cursor = r.URL.Query().Get("starting_after")
			response = map[string]any{"data": p.charges, "has_more": p.hasMore}
		case "/v1/payment_intents/pi_example":
			p.intentGets++
			response = p.intent
		case "/v1/invoices":
			data := []any{}
			if p.debt {
				data = append(data, map[string]any{"id": "in_example", "customer": "cus_example", "status": r.URL.Query().Get("status")})
			}
			response = map[string]any{"data": data, "has_more": p.debtMore}
		default:
			t.Errorf("unexpected provider request %s", r.URL.String())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(srv.Close)
	return &stripeHTTPClient{baseURL: srv.URL, httpClient: srv.Client(), apiVersion: "2026-05-27.dahlia"}
}

func TestFundingMethodRequiresCurrentReusableAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fundingTestProvider)
		ready  bool
		err    bool
	}{
		{"off-session-setup", func(p *fundingTestProvider) {}, true, false},
		{"attached-only", func(p *fundingTestProvider) { p.setup = nil }, false, false},
		{"stale-method-setup", func(p *fundingTestProvider) { p.setup["payment_method"] = "pm_old" }, false, false},
		{"setup-wrong-customer", func(p *fundingTestProvider) { p.setup["customer"] = "cus_other" }, false, false},
		{"setup-wrong-mode", func(p *fundingTestProvider) { p.setup["livemode"] = true }, false, false},
		{"setup-incomplete", func(p *fundingTestProvider) { p.setup["status"] = "requires_action" }, false, false},
		{"on-session-only", func(p *fundingTestProvider) { p.setup["usage"] = "on_session" }, false, false},
		{"single-use-mandate", func(p *fundingTestProvider) { p.setup["single_use_mandate"] = "mandate_example" }, false, false},
		{"method-wrong-owner", func(p *fundingTestProvider) { p.method["customer"] = "cus_other" }, false, true},
		{"method-wrong-mode", func(p *fundingTestProvider) { p.method["livemode"] = true }, false, true},
		{"expired-card", func(p *fundingTestProvider) { p.method["card"].(map[string]any)["exp_year"] = time.Now().Year() - 1 }, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newFundingTestProvider()
			tc.mutate(p)
			out, err := p.client(t).observeFunding(t.Context(), "cus_example", "", false, "")
			if (err != nil) != tc.err || out.PaymentMethodReusable != tc.ready {
				t.Fatalf("ready=%v err=%v", out.PaymentMethodReusable, err)
			}
			if err == nil && (out.CollectionReady || out.CashCollected || len(out.Evidence) == 0 || !out.ValidUntil.After(time.Now())) {
				t.Fatalf("unexpected funding observation: %+v", out)
			}
		})
	}
}

func TestFundingPaymentIntentReuseAndCashAreSeparate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fundingTestProvider)
		ready  bool
	}{
		{"saved-for-reuse", func(p *fundingTestProvider) {}, true},
		{"one-time-payment", func(p *fundingTestProvider) { p.intent["setup_future_usage"] = "" }, false},
		{"different-method", func(p *fundingTestProvider) { p.intent["payment_method"] = "pm_old" }, false},
		{"different-charge", func(p *fundingTestProvider) { p.intent["latest_charge"] = "ch_other" }, false},
		{"different-customer", func(p *fundingTestProvider) { p.intent["customer"] = "cus_other" }, false},
		{"different-mode", func(p *fundingTestProvider) { p.intent["livemode"] = true }, false},
		{"no-cash-received", func(p *fundingTestProvider) { p.intent["amount_received"] = 0 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newFundingTestProvider()
			p.setup = nil
			p.charges = []fundingCharge{fundingTestCharge()}
			tc.mutate(p)
			out, err := p.client(t).observeFunding(t.Context(), "cus_example", "", false, "")
			if err != nil || !out.CashCollected || out.CashChargeID != "ch_example" || out.PaymentMethodReusable != tc.ready || p.intentGets != 1 {
				t.Fatalf("observation=%+v intentGets=%d err=%v", out, p.intentGets, err)
			}
		})
	}
}

func TestFundingRefreshRereadsAndWithdrawsReversedCash(t *testing.T) {
	p := newFundingTestProvider()
	c := p.client(t)
	first, err := c.observeFunding(t.Context(), "cus_example", "", false, "ch_example")
	if err != nil || !first.CashCollected {
		t.Fatalf("first observation=%+v err=%v", first, err)
	}
	p.saved.Disputed = true
	second, err := c.observeFunding(t.Context(), "cus_example", "", false, first.CashChargeID)
	if err != nil || second.CashCollected || second.CashChargeID != "" || p.chargeGets != 2 || !second.PaymentMethodReusable {
		t.Fatalf("second observation=%+v chargeGets=%d err=%v", second, p.chargeGets, err)
	}
}

func TestFundingIncompleteInventoryAndDebtFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name          string
		charges, debt bool
	}{
		{"charge-inventory", true, false}, {"debt-inventory", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newFundingTestProvider()
			p.hasMore, p.debtMore = tc.charges, tc.debt
			if _, err := p.client(t).observeFunding(t.Context(), "cus_example", "", false, ""); err == nil {
				t.Fatal("incomplete provider observation accepted")
			}
		})
	}
	p := newFundingTestProvider()
	p.debt = true
	out, err := p.client(t).observeFunding(t.Context(), "cus_example", "", false, "ch_example")
	if err != nil || out.DebtInvoiceID == "" || !out.CashCollected || !out.PaymentMethodReusable {
		t.Fatalf("debt must not erase independent funding facts: %+v %v", out, err)
	}
}

func TestFundingChargeDiscoveryPersistsProgress(t *testing.T) {
	p := newFundingTestProvider()
	charge := fundingTestCharge()
	charge.AmountCaptured = 0
	p.charges, p.hasMore = []fundingCharge{charge}, true
	out, err := p.client(t).observeFundingPage(t.Context(), "cus_example", "", false, "", "ch_previous")
	if err != nil || out.CashCollected || out.ChargeScanComplete || out.NextChargeCursor != "ch_example" || p.cursor != "ch_previous" {
		t.Fatalf("observation=%+v cursor=%s err=%v", out, p.cursor, err)
	}
}

func TestFundingCollectionRequiresLiveMeteredRoute(t *testing.T) {
	for _, tc := range []struct {
		status string
		ready  bool
	}{
		{"active", true}, {"trialing", true}, {"past_due", true}, {"canceled", false}, {"incomplete", false}, {"incomplete_expired", false}, {"unpaid", false}, {"paused", false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			sub := invoiceSubscription{ID: "sub_example", Status: tc.status, CollectionMethod: "send_invoice"}
			for _, id := range []string{"cpu", "memory"} {
				p := storagePrice{ID: "price_" + id, Product: "prod_example", Currency: "usd", BillingScheme: "per_unit", UnitAmountDecimal: "1"}
				p.Recurring.UsageType, p.Recurring.Interval, p.Recurring.IntervalCount, p.Recurring.Meter = "metered", "month", 1, "mtr_"+id
				sub.Items.Data = append(sub.Items.Data, invoiceSubscriptionItem{ID: "si_" + id, Price: p})
			}
			if got := fundingCollectionReady(sub); got != tc.ready {
				t.Fatalf("ready=%v", got)
			}
			sub.Items.Data[1].Price.Recurring.Meter = ""
			if fundingCollectionReady(sub) {
				t.Fatal("incomplete metered route accepted")
			}
		})
	}
}

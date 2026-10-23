package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestPrepaidEntryRefusedWhilePricingV2IsOff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enabled, allowed := prepaidPricingV2Enabled, prepaidPlanAllowsPurchase
	t.Cleanup(func() { prepaidPricingV2Enabled, prepaidPlanAllowsPurchase = enabled, allowed })
	for _, tc := range []struct {
		name         string
		on, plan, ok bool
	}{{"switch off", false, true, false}, {"plan cannot buy", true, false, false}, {"allowed", true, true, true}} {
		prepaidPricingV2Enabled = func(context.Context, *Handlers) (bool, error) { return tc.on, nil }
		prepaidPlanAllowsPurchase = func(context.Context, *Handlers, uuid.UUID) (bool, error) { return tc.plan, nil }
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/billing/prepaid/checkout", nil)
		if got := (&Handlers{}).prepaidEntryAllowed(c, uuid.New()); got != tc.ok || (!tc.ok && w.Code != http.StatusConflict) {
			t.Fatalf("%s: allowed=%v status=%d", tc.name, got, w.Code)
		}
	}
}

func TestPrepaidAmountsUseFiveDollarSteps(t *testing.T) {
	for amount, want := range map[int64]bool{0: false, 499: false, 500: true, 750: false, 1000: true, 100000000: true, 100000500: false} {
		if validPrepaidAmount(amount) != want {
			t.Fatalf("amount %d", amount)
		}
	}
}

func TestPrepaidGrantFormIsPaidNonExpiringMetered(t *testing.T) {
	id := uuid.New()
	raw, err := prepaidCreditRequest(id, "cus_example", 2500, "checkout")
	if err != nil {
		t.Fatal(err)
	}
	form, err := prepaidGrantForm(raw)
	if err != nil || form.Get("category") != "paid" || form.Has("expires_at") || form.Get("applicability_config[scope][price_type]") != "metered" || form.Get("metadata[prepaid_purchase_id]") != id.String() {
		t.Fatal(form, err)
	}
}

func TestInvoiceCreditLedgerPagesBeyondOneHundredGrants(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/billing/credit_balance_summary" {
			fmt.Fprint(w, `{"balances":[{"ledger_balance":{"monetary":{"currency":"usd","value":100}}}]}`)
			return
		}
		page := 0
		if r.URL.Query().Get("starting_after") != "" {
			page = 1
		}
		data := []map[string]any{}
		for i := 0; i < 100; i++ {
			data = append(data, map[string]any{"id": fmt.Sprintf("credgr_%d_%d", page, i), "applicability_config": map[string]any{"scope": map[string]any{"price_type": "metered"}}})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": page == 0})
	}))
	defer srv.Close()
	c := &stripeHTTPClient{baseURL: srv.URL, apiVersion: "2026-07-29.dahlia", httpClient: srv.Client()}
	total, err := c.invoiceCreditLedger(t.Context(), "cus_example", time.Now().Unix())
	if err != nil || total != 200*100 {
		t.Fatal(total, err)
	}
}

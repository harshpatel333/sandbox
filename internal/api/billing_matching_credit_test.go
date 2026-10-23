package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestMatchingUsageExcludesFeeDiscountsAndCredits(t *testing.T) {
	var inv StripeInvoiceAmounts
	if err := json.Unmarshal([]byte(`{"lines":{"data":[
 {"amount":120000,"currency":"usd","pricing":{"price_details":{"price":"price_base"}},"pretax_credit_amounts":[{"type":"discount","amount":20000}]},
 {"amount":50000,"currency":"usd","pricing":{"price_details":{"price":"price_cpu"}},"pretax_credit_amounts":[{"type":"credit_balance_transaction","amount":25000,"credit_balance_transaction":"cbtxn_1"},{"type":"discount","amount":3}]},
 {"amount":7,"currency":"usd","pricing":{"price_details":{"price":"price_round"}}}]}}`), &inv); err != nil {
		t.Fatal(err)
	}
	u, err := matchingInvoiceUsage(inv, []string{"price_cpu", "price_round"})
	if err != nil || u.SkipReason != "" || u.UsageCents != 50007 || u.CreditCents != 25000 || u.DiscountCents != 3 {
		t.Fatalf("unexpected matchable usage: %+v %v", u, err)
	}
	inv.Taxes = []json.RawMessage{json.RawMessage(`{"amount":1}`)}
	if u, err = matchingInvoiceUsage(inv, []string{"price_cpu"}); err != nil || u.SkipReason == "" {
		t.Fatal("taxed invoice was matched automatically")
	}
	if u, err = matchingInvoiceUsage(StripeInvoiceAmounts{}, nil); err != nil || u.SkipReason == "" {
		t.Fatal("invoice without usage price identification was matched")
	}
}

func TestMatchingGrantFormRejectsExpiringOrScopedRequests(t *testing.T) {
	valid := `{"customer":"cus_example","category":"promotional","name":"Matching credit","priority":50,"amount":{"type":"monetary","monetary":{"currency":"usd","value":100}},"applicability_config":{"scope":{"price_type":"metered"}},"metadata":{"matching_credit_id":"m","invoice_id":"in_example"}}`
	form, err := matchingGrantForm([]byte(valid))
	if err != nil || form.Get("applicability_config[scope][price_type]") != "metered" || form.Has("expires_at") {
		t.Fatal(form, err)
	}
	for _, bad := range []string{
		`{"customer":"cus_example","category":"promotional","name":"Matching credit","priority":50,"expires_at":5,"amount":{"type":"monetary","monetary":{"currency":"usd","value":100}},"applicability_config":{"scope":{"price_type":"metered"}},"metadata":{"matching_credit_id":"m","invoice_id":"in_example"}}`,
		`{"customer":"cus_example","category":"promotional","name":"Matching credit","priority":50,"amount":{"type":"monetary","monetary":{"currency":"usd","value":100}},"applicability_config":{"scope":{"prices":[{"id":"price_cpu"}]}},"metadata":{"matching_credit_id":"m","invoice_id":"in_example"}}`,
		`{"customer":"cus_example","category":"promotional","name":"Matching credit","priority":50,"amount":{"type":"monetary","monetary":{"currency":"usd","value":100}},"applicability_config":{"scope":{"price_type":"metered"}},"metadata":{"entitlement_id":"e","matching_credit_id":"m","invoice_id":"in_example"}}`,
	} {
		if _, err = matchingGrantForm([]byte(bad)); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}

func TestMatchingWritesRequireOperatorCredential(t *testing.T) {
	router := machineAdminRouter(t, nil)
	team, credit := uuid.NewString(), uuid.NewString()
	for _, path := range []string{"/billing/matching", "/billing/matching/revoke", "/billing/matching/credits/" + credit + "/acknowledge"} {
		for _, token := range []string{"", "test-infrastructure-only"} {
			response := machineAdminRequest(router, http.MethodPost, "/internal/teams/"+team+path, `{"reason":"test"}`, token, false)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("%s with token %q got %d", path, token, response.Code)
			}
		}
	}
}

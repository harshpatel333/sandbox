package api

import (
	"encoding/json"
	"testing"
)

func TestCheckoutLineDiscountAcceptsOnlyThePromotion(t *testing.T) {
	var line StripeInvoiceAmountLine
	if err := json.Unmarshal([]byte(`{"amount":25000,"discounts":["di_promo"],"pretax_credit_amounts":[{"type":"discount","amount":0},{"type":"discount","amount":5000,"discount":"di_promo"}]}`), &line); err != nil {
		t.Fatal(err)
	}
	if _, credited := checkoutLineDiscount(line, ""); !credited {
		t.Fatal("accepted a discounted fee line without a promotion")
	}
	if d, credited := checkoutLineDiscount(line, "di_promo"); credited || d != 5000 {
		t.Fatalf("promotion on fee line: %d %v", d, credited)
	}
	if _, credited := checkoutLineDiscount(line, "di_other"); !credited {
		t.Fatal("accepted another promotion's discount")
	}
	line.Discounts = append(line.Discounts, json.RawMessage(`{"id":"di_item"}`))
	if _, credited := checkoutLineDiscount(line, "di_promo"); !credited {
		t.Fatal("accepted an item discount alongside the promotion")
	}
	line.Discounts = nil
	line.PretaxCredits = append(line.PretaxCredits, StripeInvoicePretaxCredit{Type: "credit_balance_transaction", Amount: 1})
	if _, credited := checkoutLineDiscount(line, "di_promo"); !credited {
		t.Fatal("accepted credit against the licensed fee")
	}
	line.PretaxCredits = []StripeInvoicePretaxCredit{{Type: "discount", Amount: 25001, Discount: "di_promo"}}
	if _, credited := checkoutLineDiscount(line, "di_promo"); !credited {
		t.Fatal("accepted a discount larger than the fee")
	}
}

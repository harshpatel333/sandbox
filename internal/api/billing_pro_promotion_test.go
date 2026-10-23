package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

func TestCheckoutPlanAssignmentUsesFrozenTerms(t *testing.T) {
	max := int32(1000)
	team, actor, contract, version := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	offer := proOffer{Amount: 25000, Months: 1, Credit: 25000, VersionID: version, Version: 2, PricingV2: true, Family: "pro", DisplayName: "Pro", Limits: &planLimits{MaxConcurrent: &max}}
	choice := proChoice{Team: team, Actor: actor, Offer: "pro_monthly"}
	defer func(saved func(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, planTerms) error) {
		paidPlanAssigner = saved
	}(paidPlanAssigner)
	paidPlanAssigner = nil
	if err := assignCheckoutPlan(t.Context(), nil, choice, offer, contract); err == nil {
		t.Fatal("activated a pricing v2 purchase without assigning its plan")
	}
	var got planTerms
	paidPlanAssigner = func(_ context.Context, _ pgx.Tx, gotTeam, gotActor, gotContract uuid.UUID, terms planTerms) error {
		if gotTeam != team || gotActor != actor || gotContract != contract {
			t.Fatalf("assignment identity: %v %v %v", gotTeam, gotActor, gotContract)
		}
		got = terms
		return nil
	}
	if err := assignCheckoutPlan(t.Context(), nil, choice, offer, contract); err != nil {
		t.Fatal(err)
	}
	if got.OfferKey != "pro_monthly" || got.Kind != "subscription" || got.FeeCents != 25000 || got.MonthlyCreditCents != 25000 || got.VersionID != version || got.Version != 2 || got.Family != "pro" || got.PricingVersion != "v2" || *got.Limits.MaxConcurrent != 1000 || got.Retired {
		t.Fatalf("assigned terms differ from checkout: %+v", got)
	}
}

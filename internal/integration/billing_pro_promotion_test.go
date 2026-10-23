//go:build integration

package integration

import (
	"testing"
	"time"
)

func sellProWithPricingV2(t *testing.T, f *proProviderFixture) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(), `UPDATE billing_signup_choice SET request_payload=jsonb_set(request_payload||'{"AllowPromotionCodes":true}'::jsonb,'{PlanTerms}',request_payload->'PlanTerms'||'{"PricingV2":true,"Family":"pro","Version":1,"DisplayName":"Pro"}'::jsonb) WHERE team_id=$1`, f.team); err != nil {
		t.Fatal(err)
	}
}

func proPaymentDiscount(t *testing.T, f *proProviderFixture, newest bool) (paid, discounted int64) {
	t.Helper()
	order := "ASC"
	if newest {
		order = "DESC"
	}
	if err := testPool.QueryRow(t.Context(), `SELECT p.invoice_amount_paid_cents,COALESCE(d.discounted_cents,0) FROM billing_contract_payment p LEFT JOIN billing_contract_payment_discount d USING(invoice_id) WHERE p.team_id=$1 ORDER BY p.service_period_start `+order+` LIMIT 1`, f.team).Scan(&paid, &discounted); err != nil {
		t.Fatal(err)
	}
	return paid, discounted
}

func TestIntegration_ProPromotionCodeDiscountsTheCheckoutInvoice(t *testing.T) {
	for name, discount := range map[string]int64{"partial": 25000, "full": 120000} {
		t.Run(name, func(t *testing.T) {
			f, r, key := newProFixture(t, "pro_monthly")
			assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody("pro_monthly"), 200, f.session)
			sellProWithPricingV2(t, f)
			f.mu.Lock()
			f.status, f.paid, f.discount, f.subDiscount = "complete", true, discount, true
			f.mu.Unlock()
			for range 2 {
				assertProResponse(t, r, key, "/stripe/pro-checkout/recover", `{}`, 200, "active")
			}
			if paid, discounted := proPaymentDiscount(t, f, false); paid != 120000-discount || discounted != discount {
				t.Fatalf("checkout receipt paid=%d discount=%d", paid, discounted)
			}
			var code, coupon string
			var fee, credit int64
			if err := testPool.QueryRow(t.Context(), `SELECT p.promotion_code,p.coupon_id,c.charge_amount_cents,c.entitlement_amount_cents FROM billing_contract_promotion p JOIN billing_commercial_contract c ON c.id=p.contract_id WHERE p.team_id=$1`, f.team).Scan(&code, &coupon, &fee, &credit); err != nil || code != "promo_example" || coupon != "coupon_example" || fee != 120000 || credit != 100000 {
				t.Fatalf("contract promotion code=%q coupon=%q fee=%d credit=%d err=%v", code, coupon, fee, credit, err)
			}
			f.mu.Lock()
			cleared, still := f.discountCleared, f.subDiscount
			f.mu.Unlock()
			if cleared != 1 || still {
				t.Fatalf("promotion kept on the subscription: cleared=%d remaining=%v", cleared, still)
			}
			var kind, source string
			if err := testPool.QueryRow(t.Context(), `SELECT kind,source FROM team_plan_assignment WHERE team_id=$1 AND effective_to IS NULL AND contract_id IS NOT NULL`, f.team).Scan(&kind, &source); err != nil || kind != "subscription" || source != "checkout" {
				t.Fatalf("assignment kind=%q source=%q err=%v", kind, source, err)
			}
			f.mu.Lock()
			f.baseStart = f.baseEnd
			f.baseEnd = time.Unix(f.baseStart, 0).UTC().AddDate(0, 1, 0).Unix()
			f.mu.Unlock()
			assertProResponse(t, r, key, "/stripe/pro-checkout/recover", `{}`, 200, "active")
			if paid, discounted := proPaymentDiscount(t, f, true); paid != 120000 || discounted != 0 {
				t.Fatalf("renewal paid=%d discount=%d", paid, discounted)
			}
		})
	}
}

func TestIntegration_ProWithoutPricingV2RejectsDiscountedInvoices(t *testing.T) {
	f, r, key := newProFixture(t, "pro_monthly")
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody("pro_monthly"), 200, f.session)
	f.mu.Lock()
	f.status, f.paid, f.discount = "complete", true, 25000
	f.mu.Unlock()
	assertProResponse(t, r, key, "/stripe/pro-checkout/recover", `{}`, 202, "payment_confirmation_pending")
	var receipts, assignments, promotions int
	if err := testPool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM billing_contract_payment WHERE team_id=$1),(SELECT count(*) FROM team_plan_assignment WHERE team_id=$1),(SELECT count(*) FROM billing_contract_promotion WHERE team_id=$1)`, f.team).Scan(&receipts, &assignments, &promotions); err != nil || receipts != 0 || assignments != 0 || promotions != 0 {
		t.Fatalf("legacy purchase accepted a discount: receipts=%d assignments=%d promotions=%d err=%v", receipts, assignments, promotions, err)
	}
}

func TestIntegration_ProPromotionRenewalRejectsDiscounts(t *testing.T) {
	f, r, key := newProFixture(t, "pro_monthly")
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody("pro_monthly"), 200, f.session)
	sellProWithPricingV2(t, f)
	f.mu.Lock()
	f.status, f.paid, f.discount, f.subDiscount = "complete", true, 25000, true
	f.mu.Unlock()
	assertProResponse(t, r, key, "/stripe/pro-checkout/recover", `{}`, 200, "active")
	f.mu.Lock()
	f.renewalDiscount = 25000
	f.baseStart = f.baseEnd
	f.baseEnd = time.Unix(f.baseStart, 0).UTC().AddDate(0, 1, 0).Unix()
	f.mu.Unlock()
	assertProResponse(t, r, key, "/stripe/pro-checkout/recover", `{}`, 202, "payment_confirmation_pending")
	var receipts int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM billing_contract_payment WHERE team_id=$1`, f.team).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("discounted renewal was recorded: receipts=%d err=%v", receipts, err)
	}
}

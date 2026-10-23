//go:build integration

package integration

import (
	"context"
	"testing"
)

func setCatalogPlan(t *testing.T, offer string, amount, credit int64, available bool) {
	t.Helper()
	var oldAmount, oldCredit int64
	var oldAvailable bool
	if err := testPool.QueryRow(t.Context(), `SELECT charge_amount_cents,monthly_credit_cents,available FROM billing_plan WHERE offer_key=$1`, offer).Scan(&oldAmount, &oldCredit, &oldAvailable); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE billing_plan SET charge_amount_cents=$2,monthly_credit_cents=$3,available=$4 WHERE offer_key=$1`, offer, amount, credit, available); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `UPDATE billing_plan SET charge_amount_cents=$2,monthly_credit_cents=$3,available=$4 WHERE offer_key=$1`, offer, oldAmount, oldCredit, oldAvailable); err != nil {
			t.Error(err)
		}
	})
}

func TestIntegration_PlanCatalogRejectsUnavailablePlan(t *testing.T) {
	setCatalogPlan(t, "starter_monthly", 10000, 5000, false)
	_, r, key := newProFixture(t, "starter_monthly")
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody("starter_monthly"), 400, "invalid_offer")
}

func TestIntegration_PlanCatalogPurchaseKeepsCheckoutTerms(t *testing.T) {
	setCatalogPlan(t, "starter_monthly", 10000, 5000, true)
	f, r, key := newProFixture(t, "starter_monthly")
	f.amount = 10000
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody(f.offer), 200, f.session)
	// A catalog edit after checkout must not change the purchase in flight.
	if _, err := testPool.Exec(t.Context(), `UPDATE billing_plan SET monthly_credit_cents=7500,available=false WHERE offer_key='starter_monthly'`); err != nil {
		t.Fatal(err)
	}
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody(f.offer), 200, f.session)
	f.mu.Lock()
	f.status = "complete"
	f.paid = true
	f.mu.Unlock()
	assertProResponse(t, r, key, "/stripe/pro-checkout/recover", `{}`, 200, "active")
	var amount, credit int64
	var sandboxes int32
	if err := testPool.QueryRow(t.Context(), `SELECT c.charge_amount_cents,c.entitlement_amount_cents,t.max_sandboxes FROM billing_commercial_contract c JOIN team t ON t.id=c.team_id
		WHERE c.team_id=$1 AND c.offer_key='starter_monthly' AND c.status='active'`, f.team).Scan(&amount, &credit, &sandboxes); err != nil {
		t.Fatal(err)
	}
	if amount != 10000 || credit != 5000 || sandboxes != 20 {
		t.Fatalf("contract amount=%d credit=%d max_sandboxes=%d", amount, credit, sandboxes)
	}
}

func TestIntegration_PlanCatalogCompletesPurchaseStartedBeforeCatalog(t *testing.T) {
	f, r, key := newProFixture(t, "pro_monthly")
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody(f.offer), 200, f.session)
	if _, err := testPool.Exec(t.Context(), `UPDATE billing_signup_choice SET request_payload=request_payload-'PlanTerms' WHERE team_id=$1`, f.team); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status = "complete"
	f.paid = true
	f.mu.Unlock()
	assertProResponse(t, r, key, "/stripe/pro-checkout/recover", `{}`, 200, "active")
	var credit int64
	if err := testPool.QueryRow(t.Context(), `SELECT entitlement_amount_cents FROM billing_commercial_contract WHERE team_id=$1 AND status='active'`, f.team).Scan(&credit); err != nil || credit != 100000 {
		t.Fatalf("legacy purchase credit=%d err=%v", credit, err)
	}
}

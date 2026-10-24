//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/superserve-ai/sandbox/internal/db"
)

func TestProUpgradeRecurringReplayKeepsFutureContractAndLimits(t *testing.T) {
	h, team := newPlanAssignmentFixture(t)
	cfg := h.Pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h.Pool, h.DB = pool, db.New(pool)
	now := time.Now().UTC().Truncate(time.Second)
	anchor := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := anchor.AddDate(0, 1, 0)
	h.Now = func() time.Time { return now }
	terms, err := h.planTermsForPricingVersion(t.Context(), "pro_monthly", "v2")
	if err != nil {
		t.Fatal(err)
	}
	offer := proOffer{Key: terms.OfferKey, Name: terms.DisplayName, Price: "price_upgrade", Amount: terms.FeeCents, Months: 1, Credit: terms.MonthlyCreditCents, VersionID: terms.VersionID, Version: terms.Version, Family: terms.Family, Limits: &terms.Limits, Grants: terms.Grants, Allowance: terms.Allowance, PricingV2: true}
	u := proUpgrade{ID: uuid.New(), Team: team, Actor: uuid.New(), Customer: "cus_" + uuid.NewString(), Subscription: stringPtr("sub_" + uuid.NewString()), Offer: offer.Key, Anchor: anchor, Start: anchor, End: end, Quoted: now, Amount: 12500, Grant: stringPtr("grant_" + uuid.NewString())}
	initial, err := assignInTx(t, h, planAssignment{Team: team, Terms: testPlanTerms("free", "free", 2, nil), Source: "signup", Reason: "accepted Free predecessor"})
	if err != nil {
		t.Fatal(err)
	}
	u.InitialAssignment = &initial.ID
	token := uuid.New()
	if _, err = h.Pool.Exec(t.Context(), `INSERT INTO team_billing_account(team_id,stripe_customer_id,stripe_subscription_id,stripe_subscription_status,commercial_billing_anchor) VALUES($1,$2,$3,'active',$4) ON CONFLICT(team_id) DO UPDATE SET stripe_customer_id=$2,stripe_subscription_id=$3,stripe_subscription_status='active',commercial_billing_anchor=$4`, team, u.Customer, *u.Subscription, anchor); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Pool.Exec(t.Context(), `INSERT INTO billing_pro_upgrade(id,team_id,actor_id,customer_id,subscription_id,offer_key,request_payload,anchor,period_start,period_end,quoted_at,amount_cents,state,grant_id,payment_evidence) VALUES($1,$2,$3,$4,$5,$6,'{}',$7,$7,$8,$9,12500,'credited',$10,'{}')`, u.ID, team, u.Actor, u.Customer, *u.Subscription, offer.Key, anchor, end, now, *u.Grant); err != nil {
		t.Fatal(err)
	}
	if _, err = h.DB.ClaimStripeWebhookProcessingLease(t.Context(), db.ClaimStripeWebhookProcessingLeaseParams{CustomerID: u.Customer, Token: token}); err != nil {
		t.Fatal(err)
	}
	var sub invoiceSubscription
	sub.ID, sub.Customer, sub.Status, sub.DefaultPaymentMethod = *u.Subscription, u.Customer, "active", "pm_example"
	sub.BillingMode.Type = "flexible"
	sub.BillingCycleAnchor = anchor.Unix()
	if err = json.Unmarshal([]byte(`{"behavior":"keep_as_draft"}`), &sub.PauseCollection); err != nil {
		t.Fatal(err)
	}
	base := invoiceSubscriptionItem{ID: "si_base", Created: now.Unix(), Quantity: 1, Metadata: map[string]string{"pro_upgrade_id": u.ID.String()}}
	if err = json.Unmarshal([]byte(`{"id":"price_upgrade","currency":"usd","unit_amount_decimal":"25000","billing_scheme":"per_unit","recurring":{"usage_type":"licensed","interval":"month","interval_count":1}}`), &base.Price); err != nil {
		t.Fatal(err)
	}
	base.Price.UnitAmountDecimal = fmt.Sprint(offer.Amount)
	resource := invoiceSubscriptionItem{ID: "si_cpu", Start: anchor.Unix(), End: end.Unix()}
	resource.Price.ID = "price_cpu"
	sub.Items.Data = []invoiceSubscriptionItem{base, resource}
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/subscriptions/"+sub.ID {
			t.Errorf("replay mutated provider: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sub)
	}))
	defer srv.Close()
	client := &stripeHTTPClient{baseURL: srv.URL, apiVersion: "2026-07-29.dahlia", httpClient: srv.Client()}
	conn, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(t.Context(), `CREATE FUNCTION pg_temp.reject_pro_assignment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.source='checkout' THEN RAISE EXCEPTION 'injected assignment failure'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER pro_assignment_fault BEFORE INSERT ON team_plan_assignment FOR EACH ROW EXECUTE FUNCTION pg_temp.reject_pro_assignment()`); err != nil {
		t.Fatal(err)
	}
	if err = h.commitProUpgrade(t.Context(), conn, &u, sub, offer, "pm_example", client, token); err == nil {
		t.Fatal("assignment failure committed paid authority")
	}
	if _, err = conn.Exec(t.Context(), `DROP TRIGGER pro_assignment_fault ON team_plan_assignment`); err != nil {
		t.Fatal(err)
	}
	var rolledBack bool
	if err = conn.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM billing_commercial_contract WHERE team_id=$1) AND EXISTS(SELECT 1 FROM team_plan_assignment WHERE id=$2 AND effective_to IS NULL) AND (SELECT max_sandboxes=2 FROM team WHERE id=$1)`, team, initial.ID).Scan(&rolledBack); err != nil || !rolledBack {
		t.Fatalf("partial paid activation: %v %v", rolledBack, err)
	}
	for range 2 {
		if err = h.commitProUpgrade(t.Context(), conn, &u, sub, offer, "pm_example", client, token); err != nil {
			t.Fatal(err)
		}
	}
	var starts, storedAnchor time.Time
	var state string
	var assignments int
	if err = conn.QueryRow(t.Context(), `SELECT starts_at,entitlement_anchor,status FROM billing_commercial_contract WHERE id=$1`, u.Contract).Scan(&starts, &storedAnchor, &state); err != nil {
		t.Fatal(err)
	}
	if !starts.Equal(end) || !storedAnchor.Equal(anchor) || state != "pending" {
		t.Fatalf("premature or moved contract: %s %s %s", starts, storedAnchor, state)
	}
	if err = conn.QueryRow(t.Context(), `SELECT count(*) FROM team_plan_assignment WHERE team_id=$1`, team).Scan(&assignments); err != nil {
		t.Fatal(err)
	}
	if assignments != 2 || requests != 3 {
		t.Fatalf("replay duplicated assignment or mutation: assignments=%d reads=%d", assignments, requests)
	}
	var max int32
	if err = conn.QueryRow(t.Context(), `SELECT max_sandboxes FROM team WHERE id=$1`, team).Scan(&max); err != nil {
		t.Fatal(err)
	}
	if max != *terms.Limits.MaxConcurrent {
		t.Fatalf("paid limits delayed: %d", max)
	}
	tx, e := conn.Begin(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	newer, e := assignTeamPlan(t.Context(), tx, planAssignment{Team: team, Terms: testPlanTerms("free", "free", 1, nil), Source: "plan_change", Reason: "later Free"})
	if e != nil {
		tx.Rollback(t.Context())
		t.Fatal(e)
	}
	if e = tx.Commit(t.Context()); e != nil {
		t.Fatal(e)
	}
	if err = h.commitProUpgrade(t.Context(), conn, &u, sub, offer, "pm_example", client, token); err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err = conn.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM team_plan_assignment WHERE id=$1 AND effective_to IS NULL) AND (SELECT max_sandboxes=1 FROM team WHERE id=$2)`, newer.ID, team).Scan(&retained); err != nil || !retained {
		t.Fatalf("replay overwrote newer plan: %v %v", retained, err)
	}
	if _, err = conn.Exec(context.WithoutCancel(t.Context()), `SELECT 1`); err != nil {
		t.Fatal(err)
	}
}

func TestProUpgradeCreditRecoversLostAcknowledgmentWithoutExpiry(t *testing.T) {
	h, team := newPlanAssignmentFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	u := proUpgrade{ID: uuid.New(), Team: team, Actor: uuid.New(), Customer: "cus_" + uuid.NewString(), Offer: "pro_monthly", Anchor: now, Start: now, End: now.Add(24 * time.Hour), Quoted: now, Amount: 12500}
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO billing_pro_upgrade(id,team_id,actor_id,customer_id,offer_key,request_payload,anchor,period_start,period_end,quoted_at,amount_cents,state,payment_evidence) VALUES($1,$2,$3,$4,$5,'{}',$6,$6,$7,$6,$8,'paid','{}')`, u.ID, team, u.Actor, u.Customer, u.Offer, now, u.End, u.Amount); err != nil {
		t.Fatal(err)
	}
	var g entitlementGrant
	g.ID, g.Customer, g.Category, g.Priority = "cred_"+uuid.NewString(), u.Customer, "paid", 50
	g.Amount.Type, g.Amount.Monetary.Currency, g.Amount.Monetary.Value = "monetary", "usd", u.Amount
	g.Applicability.Scope.PriceType = "metered"
	g.Metadata = map[string]string{"pro_upgrade_id": u.ID.String()}
	g.EffectiveAt = now.Unix()
	creates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/invoices":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case "/v1/billing/credit_grants":
			if r.Method == http.MethodPost {
				creates++
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.Form.Has("expires_at") || r.Form.Get("category") != "paid" || r.Form.Get("amount[monetary][value]") != "12500" || r.Form.Get("applicability_config[scope][price_type]") != "metered" {
					t.Error("credit is expiring, promotional, or mismatched")
				}
				_ = json.NewEncoder(w).Encode(g)
			} else if creates == 0 {
				_, _ = w.Write([]byte(`{"data":[]}`))
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []entitlementGrant{g}})
			}
		case "/v1/billing/credit_grants/" + g.ID:
			_ = json.NewEncoder(w).Encode(g)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := &stripeHTTPClient{baseURL: srv.URL, apiVersion: "2026-07-29.dahlia", httpClient: srv.Client()}
	conn, release, err := h.proUpgradeInvoiceLock(t.Context(), team)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err = h.issueProUpgradeCredit(t.Context(), conn, &u, client); err != nil {
		t.Fatal(err)
	}
	u.Grant = nil
	if err = h.issueProUpgradeCredit(t.Context(), conn, &u, client); err != nil {
		t.Fatal(err)
	}
	if creates != 1 {
		t.Fatalf("duplicate paid credit: %d", creates)
	}
	var expires int64
	if err = conn.QueryRow(t.Context(), `SELECT expires_at FROM billing_provider_credit WHERE customer_id=$1 AND grant_id=$2`, u.Customer, g.ID).Scan(&expires); err != nil || expires != 0 {
		t.Fatalf("non-expiring credit inventory: %d %v", expires, err)
	}
}

func TestProUpgradePaymentReviewSurvivesProviderFailure(t *testing.T) {
	h, team := newPlanAssignmentFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	u := proUpgrade{ID: uuid.New(), Team: team, Actor: uuid.New(), Customer: "cus_" + uuid.NewString(), Session: stringPtr("cs_example_" + uuid.NewString()), Offer: "pro_monthly", Amount: 12500}
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO billing_pro_upgrade(id,team_id,actor_id,customer_id,offer_key,request_payload,anchor,period_start,period_end,quoted_at,amount_cents,state,payment_evidence,session_id) VALUES($1,$2,$3,$4,$5,'{}',$6,$6,$7,$6,$8,'paid','{}',$9)`, u.ID, team, u.Actor, u.Customer, u.Offer, now, now.Add(time.Hour), u.Amount, u.Session); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "temporary provider failure", 503) }))
	defer srv.Close()
	client := &stripeHTTPClient{baseURL: srv.URL, apiVersion: "2026-07-29.dahlia", httpClient: srv.Client()}
	s := StripeRetrievedCheckoutSession{ID: *u.Session, CustomerID: u.Customer, ClientReferenceID: team.String(), Mode: "payment", Currency: "usd", AmountSubtotal: 12501, AmountTotal: 12500, InvoiceID: "in_example", PaymentStatus: "paid", Metadata: map[string]string{"pro_upgrade_id": u.ID.String()}}
	conn, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, _, err = h.verifyProUpgradePayment(t.Context(), conn, client, u, s, false); err == nil {
		t.Fatal("accepted changed payment amount")
	}
	var review bool
	if err = conn.QueryRow(t.Context(), `SELECT payment_review_required FROM billing_pro_upgrade WHERE id=$1`, u.ID).Scan(&review); err != nil || !review {
		t.Fatalf("missing durable payment review: %v %v", review, err)
	}
	s.AmountSubtotal = 12500
	if _, _, err = h.verifyProUpgradePayment(t.Context(), conn, client, u, s, false); err == nil {
		t.Fatal("accepted unavailable provider evidence")
	}
	if err = conn.QueryRow(t.Context(), `SELECT payment_review_required FROM billing_pro_upgrade WHERE id=$1`, u.ID).Scan(&review); err != nil || !review {
		t.Fatalf("transport failure cleared review: %v %v", review, err)
	}
}

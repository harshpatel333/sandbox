//go:build integration

package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/superserve-ai/sandbox/internal/db"
)

func TestProviderPairBindingReplayAndIsolation(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatal("DATABASE_URL must name a migrated disposable test database")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cfg.ConnConfig.Database, "_test") {
		t.Fatal("requires a test database")
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	f := newPairFixture(t)
	verified, err := f.client(t).verifyInitialProviderPair(t.Context(), f.p)
	if err != nil {
		t.Fatal(err)
	}
	contract := uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO team(id,name,home_region) VALUES($1,$2,'use')`, f.p.Identity.TeamID, "pair-"+uuid.NewString())
	exec(`INSERT INTO team_feature_flag(team_id,key,enabled) VALUES($1,'billing_commercial_offer_assignments_enabled',true)`, f.p.Identity.TeamID)
	exec(`INSERT INTO team_billing_account(team_id,stripe_customer_id,stripe_subscription_id,commercial_billing_anchor) VALUES($1,$2,$3,$4)`, f.p.Identity.TeamID, f.p.CustomerID, f.p.UsageSubscriptionID, f.p.Anchor)
	exec(`INSERT INTO billing_commercial_offer_assignment(id,team_id,approval_id,company_identity,home_region,offer_key,offer_version,idempotency_key,rate_card_key,rate_ids,pricing_at,resource_rates,request,assigned_by,reason,reference)
VALUES($1,$2,$3,'example-company','use','yc_2026',1,'initial','payg',$4,clock_timestamp(),'[]','{}',$5,'fixture','fixture')`, f.p.Identity.AssignmentID, f.p.Identity.TeamID, uuid.New(), []uuid.UUID{uuid.New()}, uuid.New())
	exec(`INSERT INTO billing_commercial_contract(id,team_id,offer_key,starts_at,ends_at,customer_id,subscription_id,base_price_id,charge_amount_cents,charge_interval_months,entitlement_kind,entitlement_amount_cents,entitlement_cadence,entitlement_anchor)
VALUES($1,$2,'yc_2026',$3,$4,$5,$6,$7,1200000,12,'metered_usage',100000,'scheduled',$3)`, contract, f.p.Identity.TeamID, f.p.Anchor, annualBoundary(f.p.Anchor, 12), f.p.CustomerID, f.p.UsageSubscriptionID, f.p.Terms.FeePriceID)
	b, err := ConfirmContractProviderBinding(t.Context(), tx, contract, verified)
	if err != nil {
		t.Fatal(err)
	}
	// A retry must read existing immutable authority before a pending-only insert
	// guard, even after the contract has advanced beyond its initial state.
	exec(`UPDATE billing_commercial_contract SET status='ended',revision=revision+1 WHERE id=$1`, contract)
	if _, err = ConfirmContractProviderBinding(t.Context(), tx, contract, verified); err != nil {
		t.Fatalf("identical replay after transition: %v", err)
	}
	changed := verified
	changed.Input.FeeItemID = "si_changed"
	changed.seal = pairSeal(changed)
	if _, err = ConfirmContractProviderBinding(t.Context(), tx, contract, changed); err == nil {
		t.Fatal("changed binding replay accepted")
	}
	for _, query := range []string{`UPDATE billing_contract_provider_binding SET fee_item_id='si_replaced' WHERE contract_id=$1`, `DELETE FROM billing_contract_provider_binding WHERE contract_id=$1`} {
		sp, e := tx.Begin(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		_, e = sp.Exec(t.Context(), query, contract)
		_ = sp.Rollback(t.Context())
		if e == nil {
			t.Fatal("immutable binding changed")
		}
	}
	successor := b
	successor.ContractID = uuid.New()
	successor.StartsAt = *b.EndsAt
	successorEnd := annualBoundary(b.Anchor, 24)
	successor.EndsAt = &successorEnd
	successor.FeeItemID = "si_successor"
	exec(`INSERT INTO billing_commercial_contract(id,team_id,offer_key,starts_at,ends_at,customer_id,subscription_id,base_price_id,charge_amount_cents,charge_interval_months,entitlement_kind,entitlement_amount_cents,entitlement_cadence,entitlement_anchor)
VALUES($1,$2,'pro_annual',$3,$4,$5,$6,$7,1200000,12,'metered_usage',100000,'monthly',$8)`, successor.ContractID, f.p.Identity.TeamID, successor.StartsAt, successor.EndsAt, f.p.CustomerID, f.p.UsageSubscriptionID, f.p.Terms.FeePriceID, f.p.Anchor)
	insertSuccessor := `INSERT INTO billing_contract_provider_binding(contract_id,team_id,assignment_id,acceptance_id,approval_semantic_revision,merchant,live_mode,api_version,terms_digest,provider_terms_digest,customer_id,usage_subscription_id,fee_subscription_id,fee_item_id,anchor,starts_at,ends_at,binding,evidence,verified_at)
SELECT $1,team_id,assignment_id,acceptance_id,approval_semantic_revision,merchant,live_mode,api_version,terms_digest,$6,customer_id,usage_subscription_id,fee_subscription_id,'si_successor',anchor,$2,$3,$4,evidence,clock_timestamp() FROM billing_contract_provider_binding WHERE contract_id=$5`
	changedSuccessor := successor
	changedSuccessor.Terms.Resources = append([]StripeCheckoutResourceTerms{}, successor.Terms.Resources...)
	changedSuccessor.Terms.Resources[0].PriceID = "price_changed_usage"
	changedSuccessor.Identity.ProviderTermsDigest, _ = changedSuccessor.Terms.Digest()
	badPayload, _ := json.Marshal(changedSuccessor)
	sp, err := tx.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = sp.Exec(t.Context(), insertSuccessor, successor.ContractID, successor.StartsAt, successor.EndsAt, badPayload, contract, changedSuccessor.Identity.ProviderTermsDigest)
	_ = sp.Rollback(t.Context())
	if err == nil {
		t.Fatal("successor reinterpreted immutable usage price mapping")
	}
	goodPayload, _ := json.Marshal(successor)
	exec(insertSuccessor, successor.ContractID, successor.StartsAt, successor.EndsAt, goodPayload, contract, successor.Identity.ProviderTermsDigest)
	a := invoiceAccount{Customer: f.p.CustomerID, Subscription: f.p.UsageSubscriptionID, Base: invoiceBase{Price: "stale_base"}, Price: "ambient_rounding"}
	paired, err := loadInvoiceProviderPair(t.Context(), tx, f.p.Identity.TeamID, &a)
	if err != nil || !paired || a.Base.Price != "" || a.Price != f.p.Terms.Rounding.PriceID || a.Pair == nil {
		t.Fatalf("paired collector mapping: %#v %v", a, err)
	}
	other := invoiceAccount{Customer: f.p.CustomerID, Subscription: f.p.UsageSubscriptionID}
	if paired, err = loadInvoiceProviderPair(t.Context(), tx, uuid.New(), &other); err != nil || paired {
		t.Fatalf("cross-team mapping: %v %v", paired, err)
	}
	// A forged or serialized success cannot bypass fresh verification.
	for _, v := range []VerifiedProviderPair{{Input: f.p, VerifiedAt: time.Now(), Evidence: json.RawMessage(`{}`)}, verified} {
		if v.seal != ([32]byte{}) {
			v.VerifiedAt = time.Now().Add(-2 * time.Minute)
			v.seal = pairSeal(v)
		}
		if _, err = ConfirmContractProviderBinding(t.Context(), tx, b.ContractID, v); err == nil {
			t.Fatal("unverified or stale binding accepted")
		}
	}
	var count int
	if err = tx.QueryRow(t.Context(), `SELECT count(*) FROM billing_contract_provider_binding WHERE contract_id=$1`, contract).Scan(&count); err != nil || count != 1 {
		t.Fatalf("binding replay duplicated: %d %v", count, err)
	}
	exec(`UPDATE team_billing_account SET stripe_subscription_status='active' WHERE team_id=$1`, f.p.Identity.TeamID)
	exec(`INSERT INTO team_feature_flag(team_id,key,enabled) VALUES($1,'billing_export_enabled',true)`, f.p.Identity.TeamID)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	h := Handlers{Pool: pool, DB: db.New(pool), Stripe: f.client(t)}
	if err = h.enrollInvoiceAccount(t.Context(), f.p.Identity.TeamID, nil); err != nil {
		t.Fatalf("enrollment ignored pinned mapping: %v", err)
	}
	var price, event, meter string
	if err = pool.QueryRow(t.Context(), `SELECT adjustment_price_id,adjustment_event_name,adjustment_meter_id FROM billing_invoice_account WHERE team_id=$1`, f.p.Identity.TeamID).Scan(&price, &event, &meter); err != nil {
		t.Fatal(err)
	}
	if price != f.p.Terms.Rounding.PriceID || event != f.p.Terms.Rounding.EventName || meter != f.p.Terms.Rounding.MeterID {
		t.Fatal("enrollment replaced pinned rounding terms")
	}
	if err = h.enrollInvoiceAccount(t.Context(), f.p.Identity.TeamID, &invoiceAccount{Price: "price_ambient", Event: "ambient"}); err == nil {
		t.Fatal("ambient rounding replaced existing pair enrollment")
	}
}

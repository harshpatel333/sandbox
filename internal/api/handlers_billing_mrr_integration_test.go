//go:build integration

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/superserve-ai/sandbox/internal/db"
)

func TestMRRLegacyAnnualReferenceSurvivesMonthlyOnlyV2(t *testing.T) {
	h, _ := newPlanAssignmentFixture(t)
	setPricingVersion(t, h, "v2")
	if _, err := h.planTermsForPricingVersion(t.Context(), "pro_annual", "v2"); !errors.Is(err, errUnknownPlan) {
		t.Fatalf("v2 annual must remain unavailable: %v", err)
	}
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Lifecycle fixtures isolate valuation from enrollment and provider effects.
	exec(`SET LOCAL session_replication_role=replica`)
	now := time.Now().UTC()
	past := now.AddDate(0, -1, 0)
	region := "usw"
	before, err := h.loadMRROverview(t.Context(), tx, now, region, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		offer, kind string
		amount      int64
		months      int32
		annual      bool
	}{
		{"yc_2026", "upfront", 10000000, 3, false},
		{"sponsored_usage", "sponsored_entitlement", 0, 1, true},
		{"sponsored_usage", "sponsored_entitlement", 0, 1, false},
		{"pro_annual", "subscription", 1080000, 12, false},
		{"pro_monthly", "subscription", 24000, 1, false},
	} {
		team, err := db.New(tx).CreateTeam(t.Context(), "mrr-"+uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE team SET home_region=$2 WHERE id=$1`, team.ID, region)
		id := uuid.New()
		price := "price_example"
		if c.kind == "sponsored_entitlement" {
			price = ""
		}
		exec(`INSERT INTO billing_commercial_contract(id,team_id,offer_key,status,starts_at,customer_id,subscription_id,base_price_id,charge_kind,charge_amount_cents,charge_interval_months,entitlement_amount_cents,entitlement_anchor,ends_at,entitlement_cadence) VALUES($1::uuid,$2,$3,'active',$4,'cus_'||$1::text,'sub_'||$1::text,$5,$6,$7,$8,100000,$4,billing_anniversary_boundary($4,12),CASE WHEN $6='sponsored_entitlement' THEN 'scheduled' ELSE 'monthly' END)`, id, team.ID, c.offer, past, price, c.kind, c.amount, c.months)
		if c.annual {
			exec(`INSERT INTO billing_sponsored_annual_authority(team_id,sponsorship_id,contract_id,idempotency_key,request,approved_by,customer_id,subscription_id,item_id,base_price_id,base_product_id,live_mode,home_region,starts_at,waiver_ends_at,provider_evidence) VALUES($1,gen_random_uuid(),$2::uuid,'mrr','{}',gen_random_uuid(),'cus_'||$2::text,'sub_'||$2::text,'si_'||$2::text,'price_annual','prod_example',false,$3,$4,billing_anniversary_boundary($4,12),'{}')`, team.ID, id, region, past)
		}
	}
	out, err := h.loadMRROverview(t.Context(), tx, now, region, "")
	if err != nil {
		t.Fatal("v2 MRR with historical annual authority", err)
	}
	if out.MRRCents-before.MRRCents != 314000 || out.RevenueTeams-before.RevenueTeams != 4 || out.Teams-before.Teams != 5 {
		t.Fatalf("legacy valuation or frozen paid fee changed: %+v", out)
	}
	want := map[string]int64{"yc_2026": 100000, mrrSponsoredAnnualKey: 100000, "sponsored_usage": 0, "pro_annual": 90000, "pro_monthly": 24000}
	for _, p := range before.Plans {
		if _, ok := want[p.Key]; ok {
			want[p.Key] += p.MRRCents
		}
	}
	for _, p := range out.Plans {
		if _, ok := want[p.Key]; !ok {
			continue
		}
		if p.MRRCents != want[p.Key] {
			t.Errorf("%s MRR=%d want%d", p.Key, p.MRRCents, want[p.Key])
		}
	}
}

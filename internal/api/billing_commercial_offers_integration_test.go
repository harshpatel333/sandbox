//go:build integration

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/superserve-ai/sandbox/internal/config"
)

func commercialOfferFixture(t *testing.T) (*Handlers, uuid.UUID, uuid.UUID, CommercialOfferRequest) {
	t.Helper()
	h, team := fundingTransitionFixture(t)
	ensureYCTestAuthority(t, h.Pool)
	h.PromotionAuthPool = h.Pool
	h.Config = &config.Config{TeamCreationRegion: "use"}
	actor := uuid.New()
	r := CommercialOfferRequest{Key: uuid.NewString(), OfferKey: "yc_2026", Version: 1, Company: "https://www.ycombinator.com/companies/fixture-" + uuid.NewString(), Region: "use", Reason: "synthetic approval", Reference: "fixture"}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE team SET home_region='use' WHERE id=$1`, team); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT approval_id FROM reserve_yc_activation_authority($1,$2,'use',$3,'operator','fixture',NULL)`, r.Company, team, actor).Scan(&r.ApprovalID); err != nil {
		t.Fatal(err)
	}
	return h, team, actor, r
}

func TestYCCommercialOfferAssignmentAuthorityAndRetry(t *testing.T) {
	h, team, actor, r := commercialOfferFixture(t)
	ctx := t.Context()
	// The authority RPC returns company OR team matches. Neither alone authorizes another binding.
	other, err := h.DB.CreateTeam(ctx, "other-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.assignCommercialOffer(ctx, other.ID, actor, r, true); !errors.Is(err, errCommercialOfferConflict) {
		t.Fatalf("cross-team approval accepted: %v", err)
	}
	bad := r
	bad.Company = "https://www.ycombinator.com/companies/different-fixture"
	if _, err = h.assignCommercialOffer(ctx, team, actor, bad, true); !errors.Is(err, errCommercialOfferConflict) {
		t.Fatalf("cross-company approval accepted: %v", err)
	}
	if _, err = h.assignCommercialOffer(ctx, team, actor, r, false); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = h.Pool.QueryRow(ctx, `SELECT count(*) FROM billing_commercial_offer_assignment WHERE team_id=$1`, team).Scan(&count); err != nil || count != 0 {
		t.Fatalf("preflight assigned: %d %v", count, err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := h.assignCommercialOffer(ctx, team, actor, r, true); results <- err }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var before json.RawMessage
	if err = h.Pool.QueryRow(ctx, `SELECT resource_rates FROM billing_commercial_offer_assignment WHERE team_id=$1`, team).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err = h.Pool.QueryRow(ctx, `SELECT count(*) FROM billing_commercial_offer_assignment WHERE team_id=$1`, team).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate assignment: %d %v", count, err)
	}
	if err = h.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM billing_commercial_offer_period WHERE team_id=$1)+(SELECT count(*) FROM billing_commercial_contract WHERE team_id=$1)+(SELECT count(*) FROM billing_usage_entitlement WHERE team_id=$1)`, team).Scan(&count); err != nil || count != 0 {
		t.Fatalf("acceptance activated benefits: %d %v", count, err)
	}
	bad = r
	bad.Version = 2
	if _, err = h.assignCommercialOffer(ctx, team, actor, bad, true); err == nil {
		t.Fatal("changed version accepted")
	}
	bad = r
	bad.Key = uuid.NewString()
	if _, err = h.assignCommercialOffer(ctx, team, actor, bad, true); !errors.Is(err, errCommercialOfferConflict) {
		t.Fatalf("second key accepted: %v", err)
	}
	// A later catalog publication cannot replace accepted rates on retry.
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE pricing_rate SET price_usd=price_usd+0.000000000001 WHERE plan_key='payg'`); err != nil {
		t.Fatal(err)
	}
	var frozen json.RawMessage
	if err = tx.QueryRow(ctx, `SELECT resource_rates FROM billing_commercial_offer_assignment WHERE team_id=$1`, team).Scan(&frozen); err != nil || string(frozen) != string(before) {
		t.Fatalf("accepted pricing changed: %v", err)
	}
	for _, test := range []struct {
		query string
		args  []any
	}{
		{`UPDATE billing_commercial_offer_version SET charge_amount_cents=1 WHERE offer_key='yc_2026' AND version=1`, nil},
		{`UPDATE billing_commercial_offer_assignment SET offer_version=2 WHERE team_id=$1`, []any{team}},
		{`DELETE FROM billing_commercial_offer_assignment WHERE team_id=$1`, []any{team}},
	} {
		if _, err = h.Pool.Exec(ctx, test.query, test.args...); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("immutability guard failed: %v", err)
		}
	}

}

func TestYCCommercialOfferFrozenCalendar(t *testing.T) {
	for _, anchor := range []time.Time{time.Date(time.Now().UTC().Year(), 1, 31, 12, 34, 56, 0, time.UTC), time.Date(time.Now().UTC().Year(), 2, 28, 23, 59, 59, 0, time.UTC)} {
		if anchor.After(time.Now().UTC()) {
			anchor = anchor.AddDate(-1, 0, 0)
		}
		t.Run(anchor.Format("2006-01-02"), func(t *testing.T) {
			h, team, actor, r := commercialOfferFixture(t)
			ctx := t.Context()
			if _, err := h.assignCommercialOffer(ctx, team, actor, r, true); err != nil {
				t.Fatal(err)
			}
			var assignment uuid.UUID
			if err := h.Pool.QueryRow(ctx, `SELECT id FROM billing_commercial_offer_assignment WHERE team_id=$1`, team).Scan(&assignment); err != nil {
				t.Fatal(err)
			}
			adminPool := h.Pool
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := adminPool.Exec(ctx, q, args...); err != nil {
					t.Fatal(err)
				}
			}
			contract := uuid.New()
			customer, subscription := "cus_"+team.String(), "sub_"+team.String()
			end := annualBoundary(anchor, 12)
			exec(`INSERT INTO team_billing_account(team_id,stripe_customer_id,stripe_subscription_id,commercial_billing_anchor) VALUES($1,$2,$3,$4) ON CONFLICT(team_id) DO UPDATE SET stripe_customer_id=$2,stripe_subscription_id=$3,commercial_billing_anchor=$4`, team, customer, subscription, anchor)
			var resourceTerms json.RawMessage
			if err := h.Pool.QueryRow(ctx, `SELECT jsonb_build_object('ResourceTerms',jsonb_agg(jsonb_build_object('ResourceKey',r->>'resource','UnitAmountDecimal',((r->>'price_usd')::numeric*360000)::text,'PriceID','price_'||(r->>'resource'),'MeterID','meter_'||(r->>'resource'),'EventName',r->>'resource'))) FROM billing_commercial_offer_assignment a CROSS JOIN LATERAL jsonb_array_elements(a.resource_rates) r WHERE a.id=$1`, assignment).Scan(&resourceTerms); err != nil {
				t.Fatal(err)
			}
			request, _ := json.Marshal(map[string]any{"company_identity": r.Company, "home_region": "use", "merchant_account": "acct_fixture", "live_mode": false})
			exec(`INSERT INTO billing_yc_operation(id,team_id,contract_id,approved_by,evidence_reference,request,resource_terms,customer_id,subscription_id,state,starts_at,ends_at,payment_method_ready,payment_method_checked_at) VALUES($1,$2,$3,$4,'fixture',$5,$6,$7,$8,'active',$9,$10,true,clock_timestamp())`, r.ApprovalID, team, contract, actor.String(), request, resourceTerms, customer, subscription, anchor, end)
			exec(`INSERT INTO billing_commercial_contract(id,team_id,offer_key,starts_at,ends_at,customer_id,subscription_id,base_price_id,charge_kind,charge_amount_cents,charge_interval_months,entitlement_amount_cents,entitlement_cadence,entitlement_anchor) VALUES($1,$2,'yc_2026',$3,$4,$5,$6,'price_annual','subscription',1200000,12,3000000,'scheduled',$3)`, contract, team, anchor, end, customer, subscription)
			exec(`INSERT INTO billing_contract_entitlement_schedule(team_id,contract_id,month_offset,amount_cents) SELECT $1,$2,n,CASE WHEN n<3 THEN 3000000 ELSE 100000 END FROM generate_series(0,11) n`, team, contract)
			exec(`INSERT INTO billing_contract_waiver(id,team_id,contract_id,offer_key,offer_version,customer_id,subscription_id,starts_at,ends_at,collectible_base_cents) VALUES($1,$2,$3,'yc_2026',1,$4,$5,$6,$7,0)`, r.ApprovalID, team, contract, customer, subscription, anchor, end)
			if err := h.materializeCommercialOffer(ctx, team, assignment, contract, "acct_fixture", false); err == nil {
				t.Fatal("pending contract supplied activation authority")
			}
			exec(`UPDATE billing_commercial_contract SET status='active',revision=revision+1 WHERE id=$1`, contract)
			if err := h.materializeCommercialOffer(ctx, team, assignment, contract, "acct_other", false); err == nil {
				t.Fatal("wrong merchant accepted")
			}
			if err := h.materializeCommercialOffer(ctx, team, assignment, contract, "acct_fixture", true); err == nil {
				t.Fatal("wrong provider mode accepted")
			}
			// Exercise first materialization and concurrent retries with the backend's
			// restricted new-table grants, including the database trigger chain.
			restricted := *h
			restricted.Pool = commercialOfferRestrictedPool(t, h.Pool)
			h = &restricted
			results := make(chan error, 4)
			for range 4 {
				go func() {
					results <- h.materializeCommercialOffer(ctx, team, assignment, contract, "acct_fixture", false)
				}()
			}
			for range 4 {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			rows, err := h.Pool.Query(ctx, `SELECT month_offset,starts_at,ends_at,expires_at,amount_cents FROM billing_commercial_offer_period WHERE assignment_id=$1 ORDER BY month_offset`, assignment)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for rows.Next() {
				var offset int
				var start, finish, expiry time.Time
				var amount int64
				if err = rows.Scan(&offset, &start, &finish, &expiry, &amount); err != nil {
					t.Fatal(err)
				}
				expected := int64(100000)
				if offset < 3 {
					expected = 3000000
				}
				if offset != n || !start.Equal(annualBoundary(anchor, n)) || !finish.Equal(annualBoundary(anchor, n+1)) || !expiry.Equal(finish) || amount != expected {
					t.Fatalf("incorrect period %d: %s %s %s %d", offset, start, finish, expiry, amount)
				}
				n++
			}
			rows.Close()
			if rows.Err() != nil || n != 12 {
				t.Fatalf("periods=%d err=%v", n, rows.Err())
			}
			if _, err = h.Pool.Exec(ctx, `UPDATE billing_commercial_offer_period SET amount_cents=1 WHERE assignment_id=$1`, assignment); err == nil {
				t.Fatal("frozen period mutated")
			}
			exec(`UPDATE billing_commercial_contract SET status='ended',revision=revision+1 WHERE id=$1`, contract)
			if err = h.materializeCommercialOffer(ctx, team, assignment, contract, "acct_fixture", false); err != nil {
				t.Fatalf("terminal replay failed: %v", err)
			}
			if err = h.materializeCommercialOffer(ctx, team, assignment, uuid.New(), "acct_fixture", false); !errors.Is(err, errCommercialOfferConflict) {
				t.Fatalf("changed binding accepted: %v", err)
			}
		})
	}
}

func TestYCCommercialOfferHTTPAuthorization(t *testing.T) {
	h, team, _, _ := commercialOfferFixture(t)
	for _, provider := range []string{"google", "email"} {
		actor := uuid.New()
		if _, err := h.Pool.Exec(t.Context(), `INSERT INTO profile(id,email,provider,provider_id) VALUES($1,$2,$3,$4)`, actor, "fixture-"+actor.String()+"@superserve.ai", provider, actor.String()); err != nil {
			t.Fatal(err)
		}
		for _, endpoint := range []struct{ method, path string }{{"POST", ""}, {"POST", "/preflight"}, {"GET", ""}} {
			router := gin.New()
			h.registerCommercialOfferRoutes(router.Group("/internal"))
			req := httptest.NewRequest(endpoint.method, "/internal/teams/"+team.String()+"/billing/commercial-offer-assignment"+endpoint.path, strings.NewReader(`{}`))
			req.Header.Set("X-Actor-User-Id", actor.String())
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != http.StatusForbidden {
				t.Fatalf("%s %s: got %d: %s", provider, endpoint.path, response.Code, response.Body.String())
			}
		}
	}
	for _, role := range []string{"anon", "authenticated"} {
		for _, table := range []string{"billing_commercial_offer_version", "billing_commercial_offer_assignment", "billing_commercial_offer_activation", "billing_commercial_offer_period", "billing_commercial_offer_audit"} {
			var allowed bool
			if err := h.Pool.QueryRow(t.Context(), `SELECT has_table_privilege($1,$2,'INSERT') OR has_table_privilege($1,$2,'UPDATE') OR has_table_privilege($1,$2,'SELECT')`, role, table).Scan(&allowed); err != nil || allowed {
				t.Fatalf("public role has access to %s: %v %v", table, allowed, err)
			}
		}
	}
}

// Use a unique, narrowly granted synthetic role so the test cannot accidentally
// pass because its fixture connection owns the tables. Cleanup removes its ACLs
// and policies; no application role or default privileges are changed.
func commercialOfferRestrictedPool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()
	role := pgx.Identifier{"offer_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP OWNED BY "+role+"; DROP ROLE "+role); err != nil {
			t.Error(err)
		}
	})
	if _, err := admin.Exec(ctx, "GRANT USAGE ON SCHEMA public TO "+role+"; GRANT EXECUTE ON FUNCTION billing_anniversary_boundary(timestamptz,integer) TO "+role); err != nil {
		t.Fatal(err)
	}
	for _, grant := range []struct{ table, privileges string }{
		{"billing_commercial_offer_version", "SELECT"},
		{"billing_commercial_offer_assignment", "SELECT,INSERT"},
		{"billing_commercial_offer_activation", "SELECT,INSERT"},
		{"billing_commercial_offer_period", "SELECT,INSERT"},
		{"team_billing_account", "SELECT,UPDATE"},
		{"billing_commercial_contract", "SELECT,UPDATE"},
		{"billing_yc_operation", "SELECT,UPDATE"},
		{"billing_contract_waiver", "SELECT"},
		{"billing_contract_entitlement_schedule", "SELECT"},
	} {
		table := pgx.Identifier{grant.table}.Sanitize()
		if _, err := admin.Exec(ctx, "GRANT "+grant.privileges+" ON "+table+" TO "+role+"; CREATE POLICY "+role+" ON "+table+" TO "+role+" USING(true) WITH CHECK(true)"); err != nil {
			t.Fatal(err)
		}
	}
	cfg := admin.Config().Copy()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+role)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var canUpdate bool
	if err = pool.QueryRow(ctx, `SELECT has_table_privilege(current_user,'billing_commercial_offer_assignment','UPDATE')`).Scan(&canUpdate); err != nil || canUpdate {
		t.Fatalf("fixture permits assignment updates: %v %v", canUpdate, err)
	}
	return pool
}

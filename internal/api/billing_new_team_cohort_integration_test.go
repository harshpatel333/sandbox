//go:build integration

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/superserve-ai/sandbox/internal/billing"
	"github.com/superserve-ai/sandbox/internal/config"
	"github.com/superserve-ai/sandbox/internal/db"
	"github.com/superserve-ai/sandbox/internal/integrationdb"
)

type newTeamCohortFixture struct {
	h                       *Handlers
	team, old, actor, epoch uuid.UUID
}

func newTeamCohortTestFixture(t *testing.T) newTeamCohortFixture {
	t.Helper()
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := "cohort_api_" + uuid.NewString()[:8] + "_test"
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	cfg := admin.Config().Copy()
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	system, err := integrationdb.Reset(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	f := newTeamCohortFixture{h: &Handlers{Pool: pool, DB: db.New(pool), Config: &config.Config{TeamCreationRegion: "use", SystemTeamID: system.String()}}, actor: uuid.New()}
	exec := func(s string, args ...any) {
		t.Helper()
		if _, e := pool.Exec(ctx, s, args...); e != nil {
			t.Fatal(e)
		}
	}
	old, err := f.h.DB.CreateTeam(ctx, "old-cohort-control")
	if err != nil {
		t.Fatal(err)
	}
	f.old = old.ID
	exec(`INSERT INTO profile(id,email) VALUES($1,'cohort-api@example.invalid')`, f.actor)
	var free uuid.UUID
	err = pool.QueryRow(ctx, `INSERT INTO billing_plan_version(family,version,pricing_version,kind,display_name,public,allowance_period,allowance_on_exhausted,allowance_compute_seconds,allowance_compute_vcpu,allowance_compute_memory_gib,max_sandboxes,max_template_vcpu,max_template_memory_mib,max_template_disk_mib,max_templates)
 VALUES('free',(SELECT max(version)+1 FROM billing_plan_version WHERE family='free'),'v2','free','Free',true,'day_window','pause',7200,1,1,1,1,1024,4096,0) RETURNING id`).Scan(&free)
	if err != nil {
		t.Fatal(err)
	}
	exec(`SELECT publish_billing_plan_version($1,NULL)`, free)
	if err = pool.QueryRow(ctx, `SELECT billing_prepare_new_team_gate('use',$1,'test','new-team API test')`, system).Scan(&f.epoch); err != nil {
		t.Fatal(err)
	}
	sha := "1111111111111111111111111111111111111111"
	var writers []map[string]any
	for _, role := range []string{"api", "workers", "console"} {
		writers = append(writers, map[string]any{"role": role, "cohort_contract": 1, "verified": true, "source_sha": sha, "reference": "local-disposable-fixture", "digest": "local-disposable-fixture"})
	}
	evidence, _ := json.Marshal(map[string]any{"region": "use", "system_team_id": system, "epoch": f.epoch, "cohort_contract": 1, "assembled_sha": sha, "schema_sha": sha, "writers": writers})
	exec(`SELECT billing_release_new_team_gate('use',$1,$2,'test','local verified test writers',$3)`, system, f.epoch, evidence)
	if err = pool.QueryRow(ctx, `SELECT id FROM create_team_with_signup_trial('new-cohort-control',$1,'use','first_team')`, f.actor).Scan(&f.team); err != nil {
		t.Fatal(err)
	}
	for _, team := range []uuid.UUID{f.team, f.old} {
		exec(`INSERT INTO team_memberships(team_id,user_id,status) VALUES($1,$2,'active')`, team, f.actor)
		exec(`INSERT INTO user_role_assignments(user_id,role_id,scope_type,team_id) SELECT $1,id,'team',$2 FROM roles WHERE name='team_owner'`, f.actor, team)
	}
	f.h.Stripe = &stripeHTTPClient{}
	return f
}
func (f newTeamCohortFixture) close(t *testing.T) {
	t.Helper()
	if _, err := f.h.Pool.Exec(t.Context(), `SELECT billing_close_new_team_gate($1,'test','close new acceptance only')`, f.epoch); err != nil {
		t.Fatal(err)
	}
}

func TestNewTeamCohortCapabilitiesAreTeamScopedAndKeepClosedDisplay(t *testing.T) {
	f := newTeamCohortTestFixture(t)
	get := func(team uuid.UUID) map[string]any {
		t.Helper()
		r := gin.New()
		r.GET("/cap", func(c *gin.Context) {
			c.Set("team_id", team.String())
			c.Set("actor_id", f.actor)
			f.h.GetBillingCapabilities(c)
		})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cap", nil))
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("capability response: %d %s", rec.Code, rec.Body.String())
		}
		var v map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	old, newState := get(f.old), get(f.team)
	if old["pricing_version"] != "legacy" || old["fresh_entry"].(map[string]any)["pro"] != false {
		t.Fatalf("public new signup repriced old team: %+v", old)
	}
	if newState["pricing_version"] != "v2" || newState["fresh_entry"].(map[string]any)["pro"] != true {
		t.Fatal("new team missing v2 admission")
	}
	for _, v := range []map[string]any{old, newState} {
		if v["catalog"].(map[string]any)["pricing_version"] != v["pricing_version"] {
			t.Fatal("catalog/model mismatch")
		}
	}
	public, err := f.h.signupPricingState(t.Context())
	if err != nil || public.Version != "v2" || !public.EntryOpen {
		t.Fatalf("new signup catalog=%+v %v", public, err)
	}
	f.close(t)
	closed := get(f.team)
	if closed["pricing_version"] != "v2" || closed["entry_open"] != false || closed["enrollment_epoch"] != newState["enrollment_epoch"] || closed["epoch"] == newState["epoch"] {
		t.Fatal("gate closure lost immutable enrolled display")
	}
	if closed["fresh_entry"].(map[string]any)["starter"] != false || closed["actions"].(map[string]any)["recover_payment"] != true || closed["actions"].(map[string]any)["disable_auto_recharge"] != true {
		t.Fatal("closure confused fresh and retained actions")
	}
	// A query parameter cannot switch the authenticated capability to another team.
}

func TestNewTeamCohortStarterFreezesAndReplaysAcrossGateClosure(t *testing.T) {
	f := newTeamCohortTestFixture(t)
	customer := "cus_local_cohort"
	if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO team_billing_account(team_id,stripe_customer_id) VALUES($1,$2)`, f.team, customer); err != nil {
		t.Fatal(err)
	}
	purchase := uuid.New()
	prepare := func(id uuid.UUID) error {
		tx, err := f.h.Pool.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(t.Context())
		if err = f.h.preparePrepaidPurchase(t.Context(), tx, id, f.team, customer, f.actor, 500); err != nil {
			return err
		}
		return tx.Commit(t.Context())
	}
	if err := prepare(purchase); err != nil {
		t.Fatal(err)
	}
	var source string
	var epoch uuid.UUID
	var snapshot []byte
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT admission_source,admission_epoch,plan_snapshot FROM billing_prepaid_purchase WHERE id=$1`, purchase).Scan(&source, &epoch, &snapshot); err != nil {
		t.Fatal(err)
	}
	var p prepaidPurchasePlan
	if err := json.Unmarshal(snapshot, &p); err != nil {
		t.Fatal(err)
	}
	if source != "new_team" || epoch != f.epoch || p.Terms.PricingVersion != "v2" || p.Terms.Kind != "prepaid" || p.Predecessor == nil {
		t.Fatalf("unfrozen Starter admission: %s/%s/%s", source, epoch, snapshot)
	}
	f.close(t)
	if err := prepare(purchase); err != nil {
		t.Fatalf("gate close stranded accepted money: %v", err)
	}
	if err := prepare(uuid.New()); !errors.Is(err, errPricingV2Off) {
		t.Fatalf("new purchase bypassed closed gate: %v", err)
	}
	var after []byte
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT plan_snapshot FROM billing_prepaid_purchase WHERE id=$1`, purchase).Scan(&after); err != nil || string(after) != string(snapshot) {
		t.Fatal("replay changed frozen Starter terms")
	}
}

func TestNewTeamCohortFirstProUsesFullMonthAndRetainsAcceptedQuote(t *testing.T) {
	f := newTeamCohortTestFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	f.h.Now = func() time.Time { return now }
	customer := "cus_local_pro"
	if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO team_billing_account(team_id,stripe_customer_id) VALUES($1,$2)`, f.team, customer); err != nil {
		t.Fatal(err)
	}
	terms, err := f.h.planTermsForPricingVersion(t.Context(), "pro_monthly", "v2")
	if err != nil {
		t.Fatal(err)
	}
	offer := proOffer{Key: terms.OfferKey, Name: terms.DisplayName, Price: "price_local_pro", Amount: terms.FeeCents, Months: 1, Credit: terms.MonthlyCreditCents, VersionID: terms.VersionID, Version: terms.Version, Family: terms.Family, Pricing: "v2", PricingV2: true, Limits: &terms.Limits, Grants: terms.Grants}
	params := StripeCreateCheckoutSessionParams{CustomerID: customer, OperationID: uuid.New(), ClientReferenceID: f.team.String(), BasePriceID: offer.Price, OneOffProduct: "prod_local_pro", AllowPromotionCodes: true, PlanTerms: &StripeCheckoutPlanTerms{Amount: offer.Amount, Months: 1, Credit: offer.Credit, VersionID: offer.VersionID, Version: offer.Version, Family: offer.Family, PricingV2: true, Limits: offer.Limits, Grants: offer.Grants}}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "test unavailable", 503) }))
	t.Cleanup(provider.Close)
	f.h.Stripe = &stripeHTTPClient{baseURL: provider.URL, httpClient: provider.Client(), apiVersion: "2026-07-29.dahlia"}
	admission, err := f.h.teamPricingState(t.Context(), f.team)
	if err != nil {
		t.Fatal(err)
	}
	quote := func() {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/quote", nil)
		f.h.createProUpgrade(c, f.team, f.actor, offer, params, admission)
		if rec.Code != 202 {
			t.Fatalf("cohort quote %d %s", rec.Code, rec.Body.String())
		}
	}
	quote()
	accepted, err := f.h.proUpgrade(t.Context(), f.team)
	if err != nil {
		t.Fatal(err)
	}
	_, end, valid := billing.AnniversaryPeriod(now, now)
	var frozen StripeCreateCheckoutSessionParams
	_ = json.Unmarshal(accepted.Payload, &frozen)
	if !valid || accepted.Amount != 25000 || accepted.bridgeCreditAmount() != 25000 || !accepted.Anchor.Equal(now) || !accepted.End.Equal(end) || !frozen.FirstPaidProCycle {
		t.Fatalf("new-team Pro was prorated or repriced: %+v", accepted)
	}
	f.close(t)
	f.h.Now = func() time.Time { return now.Add(48 * time.Hour) }
	quote()
	replay, err := f.h.proUpgrade(t.Context(), f.team)
	if err != nil || replay.ID != accepted.ID || string(replay.Payload) != string(accepted.Payload) {
		t.Fatalf("accepted quote changed after close: %+v %v", replay, err)
	}
}

func TestNewTeamCohortStarterLeaseRetainsIdentityAfterClose(t *testing.T) {
	f := newTeamCohortTestFixture(t)
	if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO team_billing_account(team_id,stripe_customer_id) VALUES($1,'cus_cohort_lease')`, f.team); err != nil {
		t.Fatal(err)
	}
	if err := f.h.DB.PrepareStripeCheckoutIdentity(t.Context(), db.PrepareStripeCheckoutIdentityParams{TeamID: f.team, UserID: f.actor}); err != nil {
		t.Fatal(err)
	}
	req := billingCheckoutSessionRequest{SuccessURL: "https://console.example.invalid/return", CancelURL: "https://console.example.invalid/cancel"}
	first, err := f.h.beginCohortPrepaidCheckout(t.Context(), f.team, f.actor, req, "cohort-accepted-key", uuid.New(), false)
	if err != nil || !first.CheckoutInitializingAt.Valid {
		t.Fatalf("ordinary Starter admission: %+v %v", first, err)
	}
	f.close(t)
	second, err := f.h.beginCohortPrepaidCheckout(t.Context(), f.team, f.actor, req, "cohort-accepted-key", uuid.New(), false)
	if err != nil || !second.CheckoutInitializingAt.Time.Equal(first.CheckoutInitializingAt.Time) || derefString(second.CheckoutRequestKey) != derefString(first.CheckoutRequestKey) || derefString(second.StripeCustomerID) != derefString(first.StripeCustomerID) {
		t.Fatalf("accepted Starter lease changed after close: %+v %v", second, err)
	}
	_, err = f.h.beginCohortPrepaidCheckout(t.Context(), f.team, f.actor, req, "different-request", uuid.New(), false)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a different request stole the accepted Starter generation: %v", err)
	}
}

// The scheduler consumes an existing saved authorization. Closing signup and
// fresh settings admission must not invent a new consent or strand that one.
func TestNewTeamCohortRechargeSchedulerKeepsAcceptedConsentAfterClose(t *testing.T) {
	f := newTeamCohortTestFixture(t)
	terms, err := f.h.planTermsForPricingVersion(t.Context(), "starter", "v2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = assignInTx(t, f.h, planAssignment{Team: f.team, Terms: terms, Source: planSourceCreditPurchase, Reason: "local previously funded Starter fixture"}); err != nil {
		t.Fatal(err)
	}
	exec := func(s string, args ...any) {
		t.Helper()
		if _, e := f.h.Pool.Exec(t.Context(), s, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO team_billing_account(team_id,stripe_customer_id) VALUES($1,'cus_saved_consent')`, f.team)
	exec(`INSERT INTO billing_prepaid_auto_recharge(team_id,enabled,threshold_cents,amount_cents,updated_by,admission_source,admission_epoch) VALUES($1,true,500,500,$2,'new_team',$3)`, f.team, f.actor, f.epoch)
	if err = f.h.enrolPrepaidObservation(t.Context(), f.team); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE billing_funding_account SET observe=true,customer_id='cus_saved_consent',payment_method_ready=true,payment_method_id='pm_saved',credit_usd=0,credit_checked_at=clock_timestamp(),credit_valid_until=clock_timestamp()+interval '1 minute',observed_generation=generation WHERE team_id=$1`, f.team)
	f.close(t)
	if err = f.h.recordAutoRecharges(t.Context()); err != nil {
		t.Fatalf("closed gate stranded existing recharge consent: %v", err)
	}
	if err = f.h.recordAutoRecharges(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	var epoch uuid.UUID
	var source string
	if err = f.h.Pool.QueryRow(t.Context(), `SELECT count(*),max(admission_source),(array_agg(admission_epoch))[1] FROM billing_prepaid_purchase WHERE team_id=$1 AND source='auto_recharge'`, f.team).Scan(&count, &source, &epoch); err != nil {
		t.Fatal(err)
	}
	if count != 1 || source != "new_team" || epoch != f.epoch {
		t.Fatalf("saved consent duplicated or re-admitted: %d/%s/%s", count, source, epoch)
	}
}

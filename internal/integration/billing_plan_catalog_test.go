//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func currentPlanVersion(t *testing.T, family string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM billing_plan_version WHERE family=$1 AND state='published'`, family).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// publishPlanCopy publishes a new version of from's terms with the given
// overrides and returns its id.
func publishPlanCopy(t *testing.T, from uuid.UUID, set string, args ...any) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := testPool.QueryRow(ctx, `INSERT INTO billing_plan_version(family,version,public,display_name,tagline,features,sort_order,display_only,self_serve,monthly_fee_cents,annual_fee_cents,
 credit_schedule,matching_bps,cap_cents,max_sandboxes,max_template_vcpu,max_template_memory_mib,max_template_disk_mib)
 SELECT family,(SELECT MAX(version)+1 FROM billing_plan_version x WHERE x.family=v.family),public,display_name,tagline,features,sort_order,display_only,self_serve,monthly_fee_cents,annual_fee_cents,
 credit_schedule,matching_bps,cap_cents,max_sandboxes,max_template_vcpu,max_template_memory_mib,max_template_disk_mib FROM billing_plan_version v WHERE id=$1 RETURNING id`, from).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	if set != "" {
		if _, err = testPool.Exec(ctx, `UPDATE billing_plan_version SET `+set+` WHERE id=$1`, append([]any{id}, args...)...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = testPool.Exec(ctx, `SELECT publish_billing_catalog_version('billing_plan_version',$1,NULL)`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// setCatalogPlan publishes a family version for the test and restores the
// previous terms as a newer version afterwards.
func setCatalogPlan(t *testing.T, family, set string, args ...any) uuid.UUID {
	t.Helper()
	previous := currentPlanVersion(t, family)
	id := publishPlanCopy(t, previous, set, args...)
	t.Cleanup(func() { publishPlanCopy(t, previous, "") })
	return id
}

func monthlySchedule(cents int64) string {
	return fmt.Sprintf(`[{"start_months":0,"repeat":null,"every_months":1,"amount_cents":%d,"expires_after_months":1}]`, cents)
}

func TestIntegration_PlanCatalogRejectsUnavailablePlan(t *testing.T) {
	setCatalogPlan(t, "starter", "self_serve=false,credit_schedule=$2::jsonb", monthlySchedule(5000))
	_, r, key := newProFixture(t, "starter_monthly")
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody("starter_monthly"), 400, "invalid_offer")
}

func TestIntegration_PlanCatalogRetiredFamilyRejectsCheckout(t *testing.T) {
	previous := currentPlanVersion(t, "pro")
	if _, err := testPool.Exec(t.Context(), `SELECT retire_billing_plan_version($1,NULL)`, previous); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { publishPlanCopy(t, previous, "") })
	var available bool
	if err := testPool.QueryRow(t.Context(), `SELECT bool_or(available) FROM billing_plan WHERE family='pro'`).Scan(&available); err != nil || available {
		t.Fatalf("retired family stays available to older binaries: %v %v", available, err)
	}
	_, r, key := newProFixture(t, "pro_monthly")
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody("pro_monthly"), 400, "invalid_offer")
}

func TestIntegration_PlanCatalogPurchaseKeepsCheckoutTerms(t *testing.T) {
	version := setCatalogPlan(t, "starter", "self_serve=true,credit_schedule=$2::jsonb", monthlySchedule(5000))
	f, r, key := newProFixture(t, "starter_monthly")
	f.amount = 10000
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody(f.offer), 200, f.session)
	var frozen string
	if err := testPool.QueryRow(t.Context(), `SELECT request_payload->'PlanTerms'->>'VersionID' FROM billing_signup_choice WHERE team_id=$1`, f.team).Scan(&frozen); err != nil || frozen != version.String() {
		t.Fatalf("frozen version=%q err=%v want %s", frozen, err, version)
	}
	// A new version after checkout must not change the purchase in flight.
	setCatalogPlan(t, "starter", "self_serve=false,credit_schedule=$2::jsonb", monthlySchedule(7500))
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

func TestIntegration_PlanCatalogCreatesStripePriceForNewFee(t *testing.T) {
	version := setCatalogPlan(t, "pro", "monthly_fee_cents=150000")
	f, r, key := newProFixture(t, "pro_monthly")
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody(f.offer), 200, f.session)
	f.mu.Lock()
	creates, price := append([]string{}, f.priceCreates...), f.price
	f.mu.Unlock()
	if len(creates) != 1 || creates[0] != "plan-price-superserve_plan_"+version.String()+"_monthly" || price != "price_catalog_1" {
		t.Fatalf("price creates=%v price=%s", creates, price)
	}
	var base string
	var frozenAmount int64
	if err := testPool.QueryRow(t.Context(), `SELECT base_price_id,(request_payload->'PlanTerms'->>'Amount')::bigint FROM billing_signup_choice WHERE team_id=$1`, f.team).Scan(&base, &frozenAmount); err != nil || base != price || frozenAmount != 150000 {
		t.Fatalf("base=%s amount=%d err=%v", base, frozenAmount, err)
	}
	f.mu.Lock()
	f.status = "complete"
	f.paid = true
	f.mu.Unlock()
	assertProResponse(t, r, key, "/stripe/pro-checkout/recover", `{}`, 200, "active")
	var amount int64
	var contractPrice string
	if err := testPool.QueryRow(t.Context(), `SELECT charge_amount_cents,base_price_id FROM billing_commercial_contract WHERE team_id=$1 AND status='active'`, f.team).Scan(&amount, &contractPrice); err != nil || amount != 150000 || contractPrice != price {
		t.Fatalf("contract amount=%d price=%s err=%v", amount, contractPrice, err)
	}
}

func TestIntegration_PlanCatalogKeepsConfiguredPriceWhileTermsMatch(t *testing.T) {
	f, r, key := newProFixture(t, "pro_monthly")
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody(f.offer), 200, f.session)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.priceCreates) != 0 || f.price != "price_pro_monthly" {
		t.Fatalf("configured price replaced: creates=%v price=%s", f.priceCreates, f.price)
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

func TestIntegration_PlanVersionsAreImmutableOncePublished(t *testing.T) {
	ctx := t.Context()
	published := currentPlanVersion(t, "enterprise")
	for _, stmt := range []string{
		`UPDATE billing_plan_version SET display_name='Changed' WHERE id=$1`,
		`UPDATE billing_plan_version SET state='draft',published_at=NULL WHERE id=$1`,
		`DELETE FROM billing_plan_version WHERE id=$1`,
		`UPDATE billing_plan_version SET state='retired',retired_at=now(),tagline='changed' WHERE id=$1`,
	} {
		if _, err := testPool.Exec(ctx, stmt, published); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("%s: err=%v", stmt, err)
		}
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO billing_plan_version(family,version,state,display_name,display_only,published_at) VALUES('enterprise',999,'published','Sneaky',true,now())`); err == nil {
		t.Fatal("inserted a published version without publication")
	}
	var draft uuid.UUID
	if err := testPool.QueryRow(ctx, `INSERT INTO billing_plan_version(family,version,display_name,display_only) VALUES('enterprise',(SELECT MAX(version)+1 FROM billing_plan_version WHERE family='enterprise'),'Enterprise draft',true) RETURNING id`).Scan(&draft); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE billing_plan_version SET tagline='edited' WHERE id=$1`, draft); err != nil {
		t.Fatalf("drafts are editable: %v", err)
	}
	if _, err := testPool.Exec(ctx, `SELECT publish_billing_catalog_version('billing_plan_version',$1,NULL)`, draft); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { publishPlanCopy(t, published, "") })
	var oldState string
	if err := testPool.QueryRow(ctx, `SELECT state FROM billing_plan_version WHERE id=$1`, published).Scan(&oldState); err != nil || oldState != "retired" {
		t.Fatalf("previous version state=%s err=%v", oldState, err)
	}
	if _, err := testPool.Exec(ctx, `SELECT publish_billing_catalog_version('billing_plan_version',$1,NULL)`, draft); err == nil {
		t.Fatal("republished a published version")
	}
	for _, bad := range []string{
		`[{"start_months":0,"every_months":1,"amount_cents":0,"expires_after_months":1}]`,
		`[{"start_months":0,"every_months":0,"amount_cents":100,"expires_after_months":1}]`,
		`[{"start_months":0,"every_months":1,"amount_cents":100,"expires_after_months":1,"surprise":true}]`,
		`[{"start_months":0.5,"every_months":1,"amount_cents":100,"expires_after_months":1}]`,
		`{"start_months":0}`,
	} {
		if _, err := testPool.Exec(ctx, `INSERT INTO billing_plan_version(family,version,display_name,monthly_fee_cents,max_sandboxes,credit_schedule) VALUES('pro',1000,'Bad',100,1,$1::jsonb)`, bad); err == nil {
			t.Fatalf("accepted invalid schedule %s", bad)
		}
	}
}

func TestIntegration_PublicBillingPlansReturnsCardsAndSignupCredits(t *testing.T) {
	r := newRouter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/billing/plans/public", nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=300, s-maxage=300" {
		t.Fatalf("status=%d cache=%q body=%s", w.Code, w.Header().Get("Cache-Control"), w.Body)
	}
	var body struct {
		Cards []struct {
			Family      string `json:"family"`
			Name        string `json:"name"`
			DisplayOnly bool   `json:"display_only"`
			Monthly     *struct {
				OfferKey   string `json:"offer_key"`
				PriceCents int64  `json:"price_cents"`
				Available  bool   `json:"available"`
			} `json:"monthly"`
			Annual *struct {
				PriceCents             int64 `json:"price_cents"`
				MonthlyEquivalentCents int64 `json:"monthly_equivalent_cents"`
			} `json:"annual"`
			CreditSummary      string `json:"credit_summary"`
			MonthlyCreditCents *int64 `json:"monthly_credit_cents"`
			Limits             struct {
				MaxSandboxes *int32 `json:"max_sandboxes"`
			} `json:"limits"`
		} `json:"cards"`
		Plans []struct {
			Key string `json:"key"`
		} `json:"plans"`
		Signup struct {
			TrialCreditCents int64 `json:"trial_credit_cents"`
			CardBonusCents   int64 `json:"card_bonus_cents"`
		} `json:"signup"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Cards) != 3 || body.Cards[0].Family != "starter" || body.Cards[1].Family != "pro" || body.Cards[2].Family != "enterprise" {
		t.Fatalf("cards=%+v", body.Cards)
	}
	pro := body.Cards[1]
	if pro.Monthly == nil || pro.Monthly.PriceCents != 120000 || !pro.Monthly.Available || pro.Annual == nil || pro.Annual.PriceCents != 1200000 || pro.Annual.MonthlyEquivalentCents != 100000 ||
		pro.MonthlyCreditCents == nil || *pro.MonthlyCreditCents != 100000 || pro.CreditSummary != "$1,000 every month" || pro.Limits.MaxSandboxes == nil || *pro.Limits.MaxSandboxes != 1000 {
		t.Fatalf("pro card=%+v", pro)
	}
	if e := body.Cards[2]; !e.DisplayOnly || e.Monthly != nil || e.Annual != nil {
		t.Fatalf("enterprise card=%+v", e)
	}
	if len(body.Plans) != 2 || body.Plans[0].Key != "pro_monthly" || body.Plans[1].Key != "pro_annual" {
		t.Fatalf("purchasable plans=%+v", body.Plans)
	}
	if body.Signup.TrialCreditCents != 500 || body.Signup.CardBonusCents != 9500 {
		t.Fatalf("signup=%+v", body.Signup)
	}
}

func TestIntegration_BillingCatalogStaffAPI(t *testing.T) {
	r := newInternalRouter(t)
	// Catalog writes need the operator credential; reads and the internal token suffice for listing.
	do := func(method, path, actor, body string) *httptest.ResponseRecorder {
		if method == http.MethodGet {
			return doInternal(r, method, path, actor, body)
		}
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+operatorRBACToken)
		req.Header.Set("X-Actor-User-Id", actor)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	admin := seedPlatformAdminProfile(t).String()
	outsider := seedSuperserveEmailProfile(t).String()
	draftBody := `{"family":"enterprise","public":true,"display_name":"Enterprise","tagline":"Talk to us","features":["SSO"],"sort_order":3,"display_only":true,"reason":"Refresh the Enterprise card copy"}`
	for _, req := range []struct{ method, path, body string }{
		{http.MethodGet, "/internal/billing/catalog", ""},
		{http.MethodPost, "/internal/billing/catalog/plans", draftBody},
		{http.MethodPost, "/internal/billing/catalog/promotions", `{"promotion":"card_bonus","amount_cents":12000}`},
	} {
		if w := do(req.method, req.path, outsider, req.body); w.Code != http.StatusForbidden {
			t.Fatalf("%s %s by non-staff = %d %s", req.method, req.path, w.Code, w.Body)
		}
	}
	if w := doInternal(r, http.MethodPost, "/internal/billing/catalog/plans", admin, draftBody); w.Code != http.StatusUnauthorized {
		t.Fatalf("write with the internal token = %d %s", w.Code, w.Body)
	}
	previous := currentPlanVersion(t, "enterprise")
	w := do(http.MethodPost, "/internal/billing/catalog/plans", admin, draftBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("create draft = %d %s", w.Code, w.Body)
	}
	var draft struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &draft)
	if w = do(http.MethodPut, "/internal/billing/catalog/plans/"+draft.ID, admin, strings.Replace(draftBody, "Talk to us", "Custom pricing", 1)); w.Code != 200 || !strings.Contains(w.Body.String(), "Custom pricing") {
		t.Fatalf("update draft = %d %s", w.Code, w.Body)
	}
	if w = do(http.MethodPost, "/internal/billing/catalog/plans", admin, `{"family":"pro","display_name":"Pro","monthly_fee_cents":100,"limits":{"max_sandboxes":1},"credit_schedule":[{"start_months":0,"every_months":0,"amount_cents":1,"expires_after_months":1}]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid draft = %d %s", w.Code, w.Body)
	}
	if w = do(http.MethodPost, "/internal/billing/catalog/plans/"+draft.ID+"/publish", admin, `{"reason":" "}`); w.Code != http.StatusBadRequest {
		t.Fatalf("publish without a reason = %d %s", w.Code, w.Body)
	}
	if w = do(http.MethodPost, "/internal/billing/catalog/plans/"+draft.ID+"/publish", admin, `{"reason":"Approved card copy"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"published"`) {
		t.Fatalf("publish = %d %s", w.Code, w.Body)
	}
	t.Cleanup(func() { publishPlanCopy(t, previous, "") })
	if w = do(http.MethodPut, "/internal/billing/catalog/plans/"+draft.ID, admin, draftBody); w.Code != http.StatusConflict {
		t.Fatalf("edit published = %d %s", w.Code, w.Body)
	}
	if w = do(http.MethodDelete, "/internal/billing/catalog/plans/"+draft.ID, admin, `{"reason":"cleanup"}`); w.Code != http.StatusConflict {
		t.Fatalf("delete published = %d %s", w.Code, w.Body)
	}
	schedule := url.QueryEscape(`[{"start_months":0,"repeat":3,"every_months":1,"amount_cents":3000000,"expires_after_months":1},{"start_months":3,"repeat":9,"every_months":1,"amount_cents":100000,"expires_after_months":1}]`)
	if w = do(http.MethodGet, "/internal/billing/catalog/preview?credit_schedule="+schedule, admin, ""); w.Code != 200 {
		t.Fatalf("preview = %d %s", w.Code, w.Body)
	}
	var preview struct {
		Issues []struct {
			MonthOffset int32 `json:"month_offset"`
		} `json:"issues"`
		Summary string `json:"summary"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &preview)
	if len(preview.Issues) != 12 || preview.Summary != "3 × $30,000 every month, then 9 × $1,000 every month from month 4" {
		t.Fatalf("preview=%+v", preview)
	}
	var audit []string
	rows, err := testPool.Query(t.Context(), `SELECT action||':'||reason FROM billing_catalog_audit WHERE version_id=$1 AND actor_id=$2 ORDER BY created_at`, draft.ID, admin)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var entry string
		if err := rows.Scan(&entry); err != nil {
			t.Fatal(err)
		}
		audit = append(audit, entry)
	}
	if strings.Join(audit, "|") != "create_draft:Refresh the Enterprise card copy|update_draft:Refresh the Enterprise card copy|publish:Approved card copy" {
		t.Fatalf("audit=%v", audit)
	}
	if _, err := testPool.Exec(t.Context(), `DELETE FROM billing_catalog_audit WHERE version_id=$1`, draft.ID); err == nil {
		t.Fatal("audit rows were deletable")
	}
	if w = do(http.MethodGet, "/internal/billing/catalog", admin, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"promotions"`) {
		t.Fatalf("catalog = %d %s", w.Code, w.Body)
	}
}

func publishSignupPromotion(t *testing.T, promotion string, cents int64) {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := testPool.QueryRow(ctx, `INSERT INTO billing_signup_promotion_version(promotion,version,amount_cents)
		SELECT $1,MAX(version)+1,$2 FROM billing_signup_promotion_version WHERE promotion=$1 RETURNING id`, promotion, cents).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `SELECT publish_billing_catalog_version('billing_signup_promotion_version',$1,NULL)`, id); err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_SignupTrialUsesPublishedAmountAtGrantTime(t *testing.T) {
	ctx := t.Context()
	newTrial := func() float64 {
		t.Helper()
		userID := uuid.New()
		if _, err := testPool.Exec(ctx, `INSERT INTO profile (id, email) VALUES ($1, $2)`, userID, "trial-amount-"+uuid.NewString()[:8]+"@example.com"); err != nil {
			t.Fatal(err)
		}
		teamID, err := provisionTeamForUser(ctx, userID, "trial-amount-"+uuid.NewString()[:8])
		if err != nil {
			t.Fatal(err)
		}
		var amount float64
		if err := testPool.QueryRow(ctx, `SELECT amount_usd::float8 FROM team_credit_grant WHERE team_id=$1 AND reason='signup trial credit'`, teamID).Scan(&amount); err != nil {
			t.Fatal(err)
		}
		return amount
	}
	if got := newTrial(); got != 5 {
		t.Fatalf("default trial=%v", got)
	}
	publishSignupPromotion(t, "trial", 700)
	t.Cleanup(func() { publishSignupPromotion(t, "trial", 500) })
	if got := newTrial(); got != 7 {
		t.Fatalf("configured trial=%v", got)
	}
	if _, err := testPool.Exec(ctx, `UPDATE billing_signup_promotion_version SET amount_cents=1 WHERE promotion='trial' AND state='published'`); err == nil {
		t.Fatal("published promotion amount was editable")
	}
}

func TestIntegration_PlanCatalogDirectRetireStopsOlderBinariesSelling(t *testing.T) {
	previous := currentPlanVersion(t, "pro")
	if _, err := testPool.Exec(t.Context(), `UPDATE billing_plan_version SET state='retired',retired_at=now() WHERE id=$1`, previous); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { publishPlanCopy(t, previous, "") })
	var available bool
	if err := testPool.QueryRow(t.Context(), `SELECT bool_or(available) FROM billing_plan WHERE family='pro'`).Scan(&available); err != nil || available {
		t.Fatalf("retiring outside retire_billing_plan_version left pro available: %v %v", available, err)
	}
}

func TestIntegration_PlanCatalogConcurrentPublishAndRetireSerialize(t *testing.T) {
	ctx := t.Context()
	original := currentPlanVersion(t, "enterprise")
	t.Cleanup(func() { publishPlanCopy(t, original, "") })
	for i := 0; i < 10; i++ {
		current := currentPlanVersion(t, "enterprise")
		var draft uuid.UUID
		if err := testPool.QueryRow(ctx, `INSERT INTO billing_plan_version(family,version,display_name,display_only)
			SELECT 'enterprise',MAX(version)+1,'Enterprise',true FROM billing_plan_version WHERE family='enterprise' RETURNING id`).Scan(&draft); err != nil {
			t.Fatal(err)
		}
		errs := make(chan error, 2)
		go func() {
			_, err := testPool.Exec(ctx, `SELECT publish_billing_catalog_version('billing_plan_version',$1,NULL)`, draft)
			errs <- err
		}()
		go func() {
			_, err := testPool.Exec(ctx, `SELECT retire_billing_plan_version($1,NULL)`, current)
			errs <- err
		}()
		for j := 0; j < 2; j++ {
			if err := <-errs; err != nil && !strings.Contains(err.Error(), "only the published version can be retired") {
				t.Fatalf("round %d: %v", i, err)
			}
		}
		var published int
		if err := testPool.QueryRow(ctx, `SELECT count(*) FROM billing_plan_version WHERE family='enterprise' AND state='published'`).Scan(&published); err != nil || published > 1 {
			t.Fatalf("round %d: published=%d err=%v", i, published, err)
		}
		if published == 0 {
			publishPlanCopy(t, original, "")
		}
	}
}

func TestIntegration_PlanCatalogDoesNotSellUndeliveredMatching(t *testing.T) {
	setCatalogPlan(t, "pro", "matching_bps=5000,cap_cents=1000000")
	var available bool
	if err := testPool.QueryRow(t.Context(), `SELECT bool_or(available) FROM billing_plan WHERE family='pro'`).Scan(&available); err != nil || available {
		t.Fatalf("matching plan offered to older binaries: %v %v", available, err)
	}
	_, r, key := newProFixture(t, "pro_monthly")
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody("pro_monthly"), 400, "invalid_offer")
}

func TestIntegration_PlanCatalogPendingCheckoutResumesAfterRetire(t *testing.T) {
	f, r, key := newProFixture(t, "pro_monthly")
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody(f.offer), 200, f.session)
	previous := currentPlanVersion(t, "pro")
	if _, err := testPool.Exec(t.Context(), `SELECT retire_billing_plan_version($1,NULL)`, previous); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { publishPlanCopy(t, previous, "") })
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody(f.offer), 200, f.session)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.priceCreates) != 0 || f.creates != 2 || f.keys[0] != f.keys[1] {
		t.Fatalf("resume changed the frozen request: creates=%d keys=%v prices=%v", f.creates, f.keys, f.priceCreates)
	}
}

func TestIntegration_PlanCatalogTransientPriceReadCreatesNothing(t *testing.T) {
	f, r, key := newProFixture(t, "pro_monthly")
	f.failBasePriceRead = true
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody(f.offer), 503, "service_unavailable")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.priceCreates) != 0 {
		t.Fatalf("transient read created prices: %v", f.priceCreates)
	}
}

func TestIntegration_PlanCatalogPublishesNewFamily(t *testing.T) {
	ctx := t.Context()
	family := "scale" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	family = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return 'a' + (r - '0')
		}
		return r
	}, family)
	var id uuid.UUID
	if err := testPool.QueryRow(ctx, `INSERT INTO billing_plan_version(family,version,display_name,self_serve,monthly_fee_cents,credit_schedule,max_sandboxes)
		VALUES($1,1,'Scale',true,50000,$2::jsonb,200) RETURNING id`, family, monthlySchedule(40000)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `SELECT publish_billing_catalog_version('billing_plan_version',$1,NULL)`, id); err != nil {
		t.Fatalf("publish new family: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `SELECT retire_billing_plan_version($1,NULL)`, id); err != nil {
			t.Error(err)
		}
	})
	var available bool
	var sandboxes int32
	if err := testPool.QueryRow(ctx, `SELECT p.available,l.max_sandboxes FROM billing_plan p JOIN plan_limits l USING(offer_key) WHERE p.offer_key=$1`, family+"_monthly").Scan(&available, &sandboxes); err != nil || available || sandboxes != 200 {
		t.Fatalf("new family offer available=%v sandboxes=%d err=%v", available, sandboxes, err)
	}
	// A customer-specific family is neither a public card nor sold through checkout.
	w := httptest.NewRecorder()
	newRouter(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/billing/plans/public", nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), family) {
		t.Fatalf("non-public family listed: %d %s", w.Code, w.Body)
	}
}

func TestIntegration_SignupPromotionsCannotBeRetiredOrTruncated(t *testing.T) {
	ctx := t.Context()
	if _, err := testPool.Exec(ctx, `UPDATE billing_signup_promotion_version SET state='retired',retired_at=now() WHERE promotion='trial' AND state='published'`); err == nil || !strings.Contains(err.Error(), "needs a published amount") {
		t.Fatalf("retired the trial without a replacement: %v", err)
	}
	for _, table := range []string{"billing_plan_version", "billing_signup_promotion_version"} {
		if _, err := testPool.Exec(ctx, `TRUNCATE `+table+` CASCADE`); err == nil || !strings.Contains(err.Error(), "cannot be truncated") {
			t.Fatalf("truncated %s: %v", table, err)
		}
	}
}

func TestIntegration_BillingCatalogWritesAreIdempotentPerVersionID(t *testing.T) {
	r := newInternalRouter(t)
	admin := seedPlatformAdminProfile(t).String()
	operator := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+operatorRBACToken)
		req.Header.Set("X-Actor-User-Id", admin)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	previous := currentPlanVersion(t, "enterprise")
	var next int32
	if err := testPool.QueryRow(t.Context(), `SELECT MAX(version)+1 FROM billing_plan_version WHERE family='enterprise'`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	body := fmt.Sprintf(`{"id":"%s","version":%d,"family":"enterprise","public":true,"display_name":"Enterprise","tagline":"Fan-out","sort_order":3,"display_only":true,"reason":"Sync every region"}`, id, next)
	for attempt := 0; attempt < 2; attempt++ {
		if w := operator(http.MethodPost, "/internal/billing/catalog/plans", body); (attempt == 0 && w.Code != http.StatusCreated) || (attempt == 1 && w.Code != http.StatusOK) || !strings.Contains(w.Body.String(), id.String()) {
			t.Fatalf("create attempt %d = %d %s", attempt, w.Code, w.Body)
		}
	}
	if w := operator(http.MethodPost, "/internal/billing/catalog/plans", strings.Replace(body, "Fan-out", "Different", 1)); w.Code != http.StatusConflict {
		t.Fatalf("same id, different terms = %d %s", w.Code, w.Body)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if w := operator(http.MethodPost, "/internal/billing/catalog/plans/"+id.String()+"/publish", `{"reason":"Sync every region"}`); w.Code != 200 {
			t.Fatalf("publish attempt %d = %d %s", attempt, w.Code, w.Body)
		}
	}
	t.Cleanup(func() { publishPlanCopy(t, previous, "") })
	for attempt := 0; attempt < 2; attempt++ {
		if w := operator(http.MethodPost, "/internal/billing/catalog/plans/"+id.String()+"/retire", `{"reason":"Sync every region"}`); w.Code != 200 {
			t.Fatalf("retire attempt %d = %d %s", attempt, w.Code, w.Body)
		}
	}
	if w := operator(http.MethodDelete, "/internal/billing/catalog/plans/"+uuid.NewString(), `{"reason":"Sync every region"}`); w.Code != http.StatusNoContent {
		t.Fatalf("delete of a missing draft = %d %s", w.Code, w.Body)
	}
	var actions string
	if err := testPool.QueryRow(t.Context(), `SELECT string_agg(action, ',' ORDER BY created_at) FROM billing_catalog_audit WHERE version_id=$1`, id).Scan(&actions); err != nil || actions != "create_draft,publish,retire" {
		t.Fatalf("audit actions=%q err=%v", actions, err)
	}

	promotion := uuid.New()
	var nextBonus int32
	if err := testPool.QueryRow(t.Context(), `SELECT MAX(version)+1 FROM billing_signup_promotion_version WHERE promotion='card_bonus'`).Scan(&nextBonus); err != nil {
		t.Fatal(err)
	}
	promo := fmt.Sprintf(`{"id":"%s","version":%d,"promotion":"card_bonus","amount_cents":9500,"reason":"Sync every region"}`, promotion, nextBonus)
	for attempt := 0; attempt < 2; attempt++ {
		if w := operator(http.MethodPost, "/internal/billing/catalog/promotions", promo); w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("promotion attempt %d = %d %s", attempt, w.Code, w.Body)
		}
	}
	if w := operator(http.MethodPost, "/internal/billing/catalog/promotions", strings.Replace(promo, "9500", "12000", 1)); w.Code != http.StatusConflict {
		t.Fatalf("same promotion id, different amount = %d %s", w.Code, w.Body)
	}
	var published int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM billing_signup_promotion_version WHERE id=$1 AND state='published'`, promotion).Scan(&published); err != nil || published != 1 {
		t.Fatalf("promotion published=%d err=%v", published, err)
	}
}

func TestIntegration_PlanCatalogNeverPublishesOverANewerVersion(t *testing.T) {
	ctx := t.Context()
	previous := currentPlanVersion(t, "enterprise")
	var older, newer uuid.UUID
	for _, id := range []*uuid.UUID{&older, &newer} {
		if err := testPool.QueryRow(ctx, `INSERT INTO billing_plan_version(family,version,display_name,display_only)
			SELECT 'enterprise',MAX(version)+1,'Enterprise',true FROM billing_plan_version WHERE family='enterprise' RETURNING id`).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := testPool.Exec(ctx, `SELECT publish_billing_catalog_version('billing_plan_version',$1,NULL)`, newer); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { publishPlanCopy(t, previous, "") })
	if _, err := testPool.Exec(ctx, `SELECT publish_billing_catalog_version('billing_plan_version',$1,NULL)`, older); err == nil || !strings.Contains(err.Error(), "newer version") {
		t.Fatalf("older draft replaced the newer version: %v", err)
	}
	if current := currentPlanVersion(t, "enterprise"); current != newer {
		t.Fatalf("current=%s want %s", current, newer)
	}
	var seeded int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM billing_plan_version WHERE version=1 AND id=md5('billing-plan-version:'||family||':1')::uuid`).Scan(&seeded); err != nil || seeded != 3 {
		t.Fatalf("seeded versions with region-independent ids=%d err=%v", seeded, err)
	}
}

// Without catalog edits customers see exactly what they did before the
// catalog: a $5 trial, a $95 card bonus and Pro checkout on the configured
// Stripe prices.
func TestIntegration_PlanCatalogDefaultsMatchPreviousBehaviour(t *testing.T) {
	ctx := t.Context()
	userID := uuid.New()
	if _, err := testPool.Exec(ctx, `INSERT INTO profile (id, email) VALUES ($1, $2)`, userID, "defaults-"+uuid.NewString()[:8]+"@example.com"); err != nil {
		t.Fatal(err)
	}
	trialTeam, err := provisionTeamForUser(ctx, userID, "defaults-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatal(err)
	}
	var trial float64
	if err := testPool.QueryRow(ctx, `SELECT amount_usd::float8 FROM team_credit_grant WHERE team_id=$1 AND reason='signup trial credit'`, trialTeam).Scan(&trial); err != nil || trial != 5 {
		t.Fatalf("trial=%v err=%v", trial, err)
	}

	teamID, _, owner := seedTeamAndKeyWithRole(t, "team_owner")
	customerID, subscriptionID := "cus_"+teamID.String(), "sub_"+teamID.String()
	if _, err := testPool.Exec(ctx, `INSERT INTO team_billing_account (team_id, stripe_customer_id, stripe_subscription_id, stripe_subscription_status) VALUES ($1,$2,$3,'incomplete')`, teamID, customerID, subscriptionID); err != nil {
		t.Fatal(err)
	}
	stripe := &fakeStripeClient{}
	created := time.Now().UTC().Truncate(time.Second)
	if w := sendStripeActivationWebhook(t, newBillingRouter(t, stripe), "evt_defaults_"+teamID.String(), teamID, owner, created); w.Code != http.StatusOK {
		t.Fatalf("activation: %d %s", w.Code, w.Body)
	}
	var recorded int64
	var ledger float64
	if err := testPool.QueryRow(ctx, `SELECT a.stripe_activation_credit_amount_cents, g.amount_usd::float8 FROM team_billing_account a JOIN team_credit_grant g USING(team_id)
		WHERE a.team_id=$1 AND g.reason='stripe promotional credit'`, teamID).Scan(&recorded, &ledger); err != nil {
		t.Fatal(err)
	}
	if len(stripe.creditGrantCalls) != 1 || stripe.creditGrantCalls[0].AmountCents != 9500 || recorded != 9500 || ledger != 95 {
		t.Fatalf("card bonus calls=%+v recorded=%d ledger=%v", stripe.creditGrantCalls, recorded, ledger)
	}

	for _, offer := range []string{"pro_monthly", "pro_annual"} {
		f, r, key := newProFixture(t, offer)
		assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody(f.offer), 200, f.session)
		f.mu.Lock()
		creates, price := len(f.priceCreates), f.price
		f.mu.Unlock()
		var base string
		if err := testPool.QueryRow(ctx, `SELECT base_price_id FROM billing_signup_choice WHERE team_id=$1`, f.team).Scan(&base); err != nil || base != "price_"+offer || price != "price_"+offer || creates != 0 {
			t.Fatalf("%s: base=%s price=%s creates=%d err=%v", offer, base, price, creates, err)
		}
	}
}

func TestIntegration_PlanCatalogContractsKeepTheLimitsTheyWereSold(t *testing.T) {
	setCatalogPlan(t, "starter", "self_serve=true,credit_schedule=$2::jsonb", monthlySchedule(5000))
	f, r, key := newProFixture(t, "starter_monthly")
	f.amount = 10000
	assertProResponse(t, r, key, "/stripe/checkout-session", proCheckoutBody(f.offer), 200, f.session)
	setCatalogPlan(t, "starter", "self_serve=true,max_sandboxes=75,credit_schedule=$2::jsonb", monthlySchedule(5000))
	f.mu.Lock()
	f.status = "complete"
	f.paid = true
	f.mu.Unlock()
	assertProResponse(t, r, key, "/stripe/pro-checkout/recover", `{}`, 200, "active")
	ctx := t.Context()
	var contract uuid.UUID
	var frozen, sandboxes int32
	if err := testPool.QueryRow(ctx, `SELECT c.id,l.max_sandboxes,t.max_sandboxes FROM billing_commercial_contract c JOIN billing_contract_limits l ON l.contract_id=c.id JOIN team t ON t.id=c.team_id
		WHERE c.team_id=$1 AND c.status='active'`, f.team).Scan(&contract, &frozen, &sandboxes); err != nil {
		t.Fatal(err)
	}
	if frozen != 20 || sandboxes != 20 {
		t.Fatalf("frozen=%d team=%d want the 20 sold at checkout", frozen, sandboxes)
	}
	if _, err := testPool.Exec(ctx, `SELECT apply_contract_limits($1)`, contract); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT max_sandboxes FROM team WHERE id=$1`, f.team).Scan(&sandboxes); err != nil || sandboxes != 20 {
		t.Fatalf("reapplied limits=%d err=%v", sandboxes, err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE billing_contract_limits SET max_sandboxes=1 WHERE contract_id=$1`, contract); err == nil {
		t.Fatal("frozen limits were editable")
	}
}

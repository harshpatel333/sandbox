//go:build integration

package api

import (
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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/superserve-ai/sandbox/internal/db"
)

type renewalStripe struct {
	t            *testing.T
	mu           sync.Mutex
	sub          string
	periodEnd    time.Time
	status       string
	cancel       bool
	writes       []string
	failWrite    bool
	renewOnWrite bool
}

func (s *renewalStripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path != "/v1/subscriptions/"+s.sub {
		s.t.Errorf("unexpected provider request %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", 500)
		return
	}
	if r.Method == http.MethodPost {
		if s.failWrite {
			http.Error(w, `{"error":{"message":"unavailable"}}`, 503)
			return
		}
		if s.renewOnWrite {
			s.periodEnd = s.periodEnd.AddDate(0, 1, 0)
			s.renewOnWrite = false
		}
		_ = r.ParseForm()
		s.cancel = r.PostForm.Get("cancel_at_period_end") == "true"
		s.writes = append(s.writes, "cancel_at_period_end="+r.PostForm.Get("cancel_at_period_end"))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": s.sub, "customer": "cus_x", "status": s.status, "cancel_at_period_end": s.cancel,
		"items": map[string]any{"data": []map[string]any{{"id": "si_x", "current_period_start": s.periodEnd.AddDate(0, -1, 0).Unix(), "current_period_end": s.periodEnd.Unix()}}}})
}

func (s *renewalStripe) set(f func(*renewalStripe)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

type planChangeV2Fixture struct {
	h      *Handlers
	team   uuid.UUID
	stripe *renewalStripe
	actor  uuid.UUID
}

var planChangeTestKeys = map[string]string{planKindSubscription: "pro_monthly", planKindPrepaid: "starter", planKindCustom: "custom_example", planKindFree: "free"}

func newPlanChangeV2Fixture(t *testing.T, kind string) planChangeV2Fixture {
	t.Helper()
	h, team := newPlanAssignmentFixture(t)
	f := planChangeV2Fixture{h: h, team: team, actor: uuid.New(), stripe: &renewalStripe{t: t, sub: "sub_" + uuid.NewString(), status: "active"}}
	srv := httptest.NewServer(f.stripe)
	t.Cleanup(srv.Close)
	h.Stripe = &stripeHTTPClient{baseURL: srv.URL, apiVersion: "2026-07-29.dahlia", httpClient: srv.Client()}
	anchor := time.Now().UTC().AddDate(0, -2, -3).Truncate(time.Second)
	customer := "cus_" + uuid.NewString()
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO team_billing_account(team_id,stripe_customer_id,stripe_subscription_id,stripe_subscription_status,commercial_billing_anchor) VALUES($1,$2,$3,'active',$4)`,
		team, customer, f.stripe.sub, anchor); err != nil {
		t.Fatal(err)
	}
	_, end, err := h.currentBillingPeriod(t.Context(), team, pgtype.Timestamptz{Time: anchor, Valid: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.stripe.periodEnd = end
	a := planAssignment{Team: team, Terms: testPlanTerms(planChangeTestKeys[kind], kind, 1000, nil), Source: planSourceCheckout, Reason: "bought"}
	if kind == planKindSubscription {
		contract := uuid.New()
		conn, err := h.Pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err = conn.Exec(t.Context(), `SET session_replication_role=replica`); err != nil {
			t.Fatal(err)
		}
		_, err = conn.Exec(t.Context(), `INSERT INTO billing_commercial_contract(id,team_id,offer_key,status,starts_at,customer_id,subscription_id,base_price_id,charge_amount_cents,charge_interval_months,entitlement_amount_cents,entitlement_anchor)
            VALUES($1,$2,'pro_monthly','active',$3,$4,$5,'price_pro',25000,1,25000,$3)`, contract, team, anchor, customer, f.stripe.sub)
		_, _ = conn.Exec(t.Context(), `RESET session_replication_role`)
		if err != nil {
			t.Fatal(err)
		}
		a.ContractID = &contract
	}
	setPricingVersion(t, h, "v2")
	if _, err = assignInTx(t, h, a); err != nil {
		t.Fatal(err)
	}
	setPricingVersion(t, h, "legacy")
	return f
}

func (f planChangeV2Fixture) request(t *testing.T, target, key string) (teamPlanChange, error) {
	t.Helper()
	kind := map[string]string{"pro_monthly": planKindSubscription, "starter": planKindPrepaid, "free": planKindFree}[target]
	return f.h.scheduleTeamPlanChangeTo(t.Context(), teamPlanChangeRequest{Team: f.team, Target: target, Key: key, Actor: f.actor, Via: "customer", Reason: "requested by the team"},
		testPlanTerms(target, kind, 1, nil))
}

func (f planChangeV2Fixture) tick(t *testing.T) {
	t.Helper()
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE team_plan_change SET next_check_at=clock_timestamp() WHERE team_id=$1 AND next_check_at<>'infinity'`, f.team); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.runDueTeamPlanChange(t.Context(), f.team); err != nil {
		t.Fatal(err)
	}
}

func (f planChangeV2Fixture) reload(t *testing.T, id uuid.UUID) teamPlanChange {
	t.Helper()
	c, err := getTeamPlanChange(t.Context(), f.h.Pool, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f planChangeV2Fixture) plan(t *testing.T) teamPlan {
	t.Helper()
	p, _, err := currentTeamPlan(t.Context(), f.h.Pool, f.team)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// reachEffective moves a change's effective time into the past, as if the
// period had ended.
func (f planChangeV2Fixture) reachEffective(t *testing.T, id uuid.UUID) {
	t.Helper()
	conn, err := f.h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(t.Context(), `SET session_replication_role=replica`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(t.Context(), `RESET session_replication_role`)
	if _, err = conn.Exec(t.Context(), `UPDATE team_plan_change SET effective_at=clock_timestamp()-interval '1 second',next_check_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

func TestTeamPlanChangeStarterToFreeAppliesNow(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindPrepaid)
	c, err := f.request(t, "free", "k1")
	if err != nil || !c.PausesRunning || c.Subscription != nil || time.Since(c.EffectiveAt) > time.Minute {
		t.Fatalf("starter to free: %+v %v", c, err)
	}
	f.tick(t)
	c = f.reload(t, c.ID)
	var pausesAvailable bool
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT to_regprocedure('request_team_pause_all(uuid,text)') IS NOT NULL`).Scan(&pausesAvailable); err != nil {
		t.Fatal(err)
	}
	if !pausesAvailable {
		if c.State != "scheduled" || c.LastError == nil || f.plan(t).PlanKey != "starter" {
			t.Fatalf("free applied without durable pause support: %+v", c)
		}
		return
	}
	plan := f.plan(t)
	if c.State != "applied" || plan.PlanKey != "free" || plan.Source != planSourcePlanChange || plan.ID != *c.AppliedPlan {
		t.Fatalf("free not applied: %+v %+v", c, plan)
	}
	if max, _ := limitsOf(t, f.h, f.team); max != 1 {
		t.Fatalf("free limits not applied: %d", max)
	}
	f.tick(t)
	if c = f.reload(t, c.ID); c.State != "completed" {
		t.Fatalf("change did not complete after durable pause requests settled: %+v", c)
	}
	if len(f.stripe.writes) != 0 {
		t.Fatalf("a team without a subscription touched Stripe: %v", f.stripe.writes)
	}
}

func TestTeamPlanChangeProDowngradeStopsRenewalAndUndoRestoresIt(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	c, err := f.request(t, "free", "k1")
	if err != nil || c.Subscription == nil || *c.Subscription != f.stripe.sub || !c.EffectiveAt.Equal(f.stripe.periodEnd) || !c.Undoable(time.Now()) {
		t.Fatalf("pro to free: %+v %v", c, err)
	}
	if again, err := f.request(t, "free", "k1"); err != nil || again.ID != c.ID {
		t.Fatalf("replay scheduled again: %+v %v", again, err)
	}
	if _, err = f.request(t, "free", "k2"); !errors.Is(err, errTeamPlanChangeConflict) {
		t.Fatalf("second pending change accepted: %v", err)
	}
	f.tick(t)
	if c = f.reload(t, c.ID); c.CancelRequestedAt == nil || c.CancelPreexisting || c.ProviderCancelAt == nil || !f.stripe.cancel || c.State != "scheduled" {
		t.Fatalf("renewal not stopped: %+v %v", c, f.stripe.writes)
	}
	if c, err = f.h.undoTeamPlanChange(t.Context(), f.team, c.ID, f.actor, "changed my mind"); err != nil || c.State != "undone" {
		t.Fatalf("undo: %+v %v", c, err)
	}
	if _, err = f.request(t, "free", "replacement"); !errors.Is(err, errTeamPlanChangeConflict) {
		t.Fatalf("replacement raced renewal restoration: %v", err)
	}
	f.tick(t)
	if c = f.reload(t, c.ID); c.needsRenewal() || c.ProviderRenewedAt == nil || f.stripe.cancel {
		t.Fatalf("renewal not restored after undo: %+v %v", c, f.stripe.writes)
	}
	if again, err := f.h.undoTeamPlanChange(t.Context(), f.team, c.ID, f.actor, "changed my mind"); err != nil || again.State != "undone" {
		t.Fatalf("repeated undo: %v", err)
	}
	if plan := f.plan(t); plan.PlanKey != "pro_monthly" {
		t.Fatalf("undone change moved the team: %+v", plan)
	}
	c, err = f.request(t, "starter", "k3")
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	f.reachEffective(t, c.ID)
	f.stripe.set(func(s *renewalStripe) { s.status = "canceled" })
	if _, err = f.h.undoTeamPlanChange(t.Context(), f.team, c.ID, f.actor, "changed my mind"); !errors.Is(err, errTeamPlanChangeConflict) {
		t.Fatalf("undo after the cutoff: %v", err)
	}
	f.tick(t)
	c = f.reload(t, c.ID)
	if plan := f.plan(t); c.State != "completed" || plan.PlanKey != "starter" || plan.Kind != planKindPrepaid {
		t.Fatalf("starter not applied at the period end: %+v %+v", c, plan)
	}
}

func TestTeamPlanChangeUndoWaitsForAnInFlightStripeWrite(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	c, err := f.request(t, "free", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.h.Pool.Exec(t.Context(), `UPDATE team_plan_change SET lease_token=gen_random_uuid(),lease_until=clock_timestamp()+interval '1 minute' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.h.undoTeamPlanChange(t.Context(), f.team, c.ID, f.actor, "changed my mind"); !errors.Is(err, errTeamPlanChangeConflict) || !strings.Contains(err.Error(), "try again") {
		t.Fatalf("undo raced the worker: %v", err)
	}
	if c = f.reload(t, c.ID); c.State != "scheduled" {
		t.Fatalf("change undone while the worker held it: %+v", c)
	}
}

func TestTeamPlanChangeUndoAfterUnconfirmedRequestRestoresRenewal(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	c, err := f.request(t, "free", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.h.Pool.Exec(t.Context(), `UPDATE team_plan_change SET provider_cancel_requested_at=clock_timestamp() WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	f.stripe.set(func(s *renewalStripe) { s.cancel = true })
	if _, err = f.h.undoTeamPlanChange(t.Context(), f.team, c.ID, f.actor, "changed my mind"); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if c = f.reload(t, c.ID); c.ProviderRenewedAt == nil || f.stripe.cancel {
		t.Fatalf("a cancellation whose response was lost stayed in place after undo: %+v", c)
	}
}

func TestTeamPlanChangeLeavesTheCustomersOwnCancellation(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	f.stripe.set(func(s *renewalStripe) { s.cancel = true })
	c, err := f.request(t, "free", "k1")
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if c = f.reload(t, c.ID); !c.CancelPreexisting || c.ProviderCancelAt == nil || len(f.stripe.writes) != 0 {
		t.Fatalf("existing cancellation not recognised: %+v %v", c, f.stripe.writes)
	}
	if _, err = f.h.undoTeamPlanChange(t.Context(), f.team, c.ID, f.actor, "changed my mind"); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if !f.stripe.cancel || len(f.stripe.writes) != 0 {
		t.Fatalf("undo turned renewal on against the customer's own cancellation: %v", f.stripe.writes)
	}
}

func TestTeamPlanChangeNotAppliedWhileStripeWouldRenew(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	c, err := f.request(t, "free", "k1")
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	f.stripe.set(func(s *renewalStripe) { s.cancel = false })
	f.reachEffective(t, c.ID)
	f.tick(t)
	if c = f.reload(t, c.ID); c.State != "scheduled" || c.LastError == nil || f.plan(t).PlanKey != "pro_monthly" {
		t.Fatalf("team moved off Pro while the subscription still renews: %+v", c)
	}
	g := newPlanChangeV2Fixture(t, planKindSubscription)
	g.stripe.set(func(s *renewalStripe) { s.failWrite = true })
	c, err = g.request(t, "free", "k1")
	if err != nil {
		t.Fatal(err)
	}
	g.tick(t)
	g.reachEffective(t, c.ID)
	g.tick(t)
	if c = g.reload(t, c.ID); c.State != "scheduled" || c.LastError == nil {
		t.Fatalf("change applied although Stripe was never told not to renew: %+v", c)
	}
}

func TestTeamPlanChangeDefersWhenThePeriodAlreadyRenewed(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	c, err := f.request(t, "starter", "k1")
	if err != nil {
		t.Fatal(err)
	}
	f.stripe.set(func(s *renewalStripe) { s.periodEnd = s.periodEnd.AddDate(0, 1, 0) })
	f.tick(t)
	c = f.reload(t, c.ID)
	if !c.EffectiveAt.Equal(f.stripe.periodEnd) || c.ProviderCancelAt == nil || !f.stripe.cancel {
		t.Fatalf("change not deferred to the renewed period end: %+v", c)
	}
}

func TestTeamPlanChangeTreatsAnEndedSubscriptionAsStopped(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	c, err := f.request(t, "free", "k1")
	if err != nil {
		t.Fatal(err)
	}
	f.stripe.set(func(s *renewalStripe) { s.status = "canceled" })
	f.tick(t)
	if c = f.reload(t, c.ID); c.ProviderCancelAt == nil || !c.CancelPreexisting || len(f.stripe.writes) != 0 {
		t.Fatalf("ended subscription left the change stuck: %+v %v", c, f.stripe.writes)
	}
}

func TestTeamPlanChangeRefusals(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindPrepaid)
	for target, want := range map[string]string{"starter": "already", "pro_monthly": "purchased"} {
		if _, err := f.request(t, target, "k-"+target); !errors.Is(err, errTeamPlanChangeConflict) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", target, err)
		}
	}
	h, team := newPlanAssignmentFixture(t)
	if _, err := h.scheduleTeamPlanChangeTo(t.Context(), teamPlanChangeRequest{Team: team, Target: "free", Key: "k", Actor: uuid.New(), Via: "customer", Reason: "r"}, testPlanTerms("free", planKindFree, 1, nil)); !errors.Is(err, errTeamPlanChangeConflict) {
		t.Fatalf("team without a plan scheduled a change: %v", err)
	}
	g := newPlanChangeV2Fixture(t, planKindSubscription)
	g.stripe.set(func(s *renewalStripe) { s.status = "canceled" })
	if _, err := g.request(t, "free", "k1"); !errors.Is(err, errTeamPlanChangeConflict) {
		t.Fatalf("downgrade scheduled on an ended subscription: %v", err)
	}
}

func TestTeamPlanChangeWithdrawnWhenTheTeamMovedFirst(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindCustom)
	c, err := f.request(t, "free", "k1")
	if err != nil {
		t.Fatal(err)
	}
	eight := int32(8)
	if _, err = assignInTx(t, f.h, planAssignment{Team: f.team, Terms: testPlanTerms("custom_other", planKindCustom, 500, &eight), Source: planSourceStaff, Reason: "new deal"}); err != nil {
		t.Fatal(err)
	}
	f.reachEffective(t, c.ID)
	f.tick(t)
	if c = f.reload(t, c.ID); c.State != "undone" || f.plan(t).PlanKey != "custom_other" {
		t.Fatalf("stale change applied over the team's new plan: %+v", c)
	}
}

func TestTeamPlanChangeHistoryIsGuarded(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindCustom)
	c, err := f.request(t, "free", "k1")
	if err != nil {
		t.Fatal(err)
	}
	for name, stmt := range map[string]string{
		"apply early":     `UPDATE team_plan_change SET state='applied',applied_at=clock_timestamp(),applied_assignment_id=from_assignment_id WHERE id=$1`,
		"edit target":     `UPDATE team_plan_change SET target_plan_key='starter' WHERE id=$1`,
		"skip to done":    `UPDATE team_plan_change SET state='completed',completed_at=clock_timestamp() WHERE id=$1`,
		"move earlier":    `UPDATE team_plan_change SET effective_at=effective_at-interval '1 day' WHERE id=$1`,
		"confirm unasked": `UPDATE team_plan_change SET provider_cancel_at=clock_timestamp() WHERE id=$1`,
		"renew unasked":   `UPDATE team_plan_change SET provider_renewed_at=clock_timestamp() WHERE id=$1`,
		"delete":          `DELETE FROM team_plan_change WHERE id=$1`,
		"second pending":  `INSERT INTO team_plan_change(team_id,from_assignment_id,target_plan_key,target_kind,target_terms,target_limits,effective_at,pauses_running,requested_by,requested_via,reason,idempotency_key) SELECT team_id,from_assignment_id,'starter','prepaid',target_terms,target_limits,effective_at,false,requested_by,'staff','r','k2' FROM team_plan_change WHERE id=$1`,
	} {
		tag, err := f.h.Pool.Exec(t.Context(), stmt, c.ID)
		if err == nil && tag.RowsAffected() > 0 {
			t.Errorf("%s was allowed", name)
		}
	}
}

func TestTeamPlanChangeExitAndUndoContinueWhilePricingV2IsOff(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	on, err := f.h.pricingV2Enabled(t.Context())
	if err != nil || on {
		t.Fatalf("fixture must have entry switched off: %v %v", on, err)
	}
	c, err := f.h.scheduleTeamPlanChange(t.Context(), teamPlanChangeRequest{Team: f.team, Target: "free", Key: "exit", Actor: f.actor, Via: "customer", Reason: "leave plan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE team_plan_change SET next_check_at='infinity' WHERE team_id<>$1`, f.team); err != nil {
		t.Fatal(err)
	}
	if worked, err := f.h.teamPlanChangeTick(t.Context()); err != nil || !worked {
		t.Fatalf("exit worker stalled with entry off: %v %v", worked, err)
	}
	if c = f.reload(t, c.ID); c.ProviderCancelAt == nil || !f.stripe.cancel {
		t.Fatalf("exit did not stop renewal: %+v", c)
	}
	if _, err = f.h.undoTeamPlanChange(t.Context(), f.team, c.ID, f.actor, "keep existing plan"); err != nil {
		t.Fatal(err)
	}
	if worked, err := f.h.teamPlanChangeTick(t.Context()); err != nil || !worked {
		t.Fatalf("undo worker stalled with entry off: %v %v", worked, err)
	}
	if c = f.reload(t, c.ID); c.ProviderRenewedAt == nil || f.stripe.cancel || c.UndoReason == nil || *c.UndoReason != "keep existing plan" {
		t.Fatalf("undo did not restore existing commitment: %+v", c)
	}
}

func TestTeamPlanChangeCancellationCrossingRenewalKeepsPaidPeriod(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	c, err := f.request(t, "starter", "rollover")
	if err != nil {
		t.Fatal(err)
	}
	f.stripe.set(func(s *renewalStripe) { s.renewOnWrite = true })
	f.tick(t)
	updated := f.reload(t, c.ID)
	if updated.ProviderCancelAt == nil || !updated.EffectiveAt.Equal(f.stripe.periodEnd) || !updated.EffectiveAt.After(c.EffectiveAt) {
		t.Fatalf("renewed paid period was cut short: %+v", updated)
	}
	f.stripe.set(func(s *renewalStripe) { s.status = "canceled" })
	if replay, err := f.request(t, "starter", "rollover"); err != nil || replay.ID != c.ID {
		t.Fatalf("durable replay depended on ended subscription: %+v %v", replay, err)
	}
}

func TestTeamPlanChangeReassignmentBeforeCancellationDoesNotTouchStripe(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	c, err := f.request(t, "free", "stale")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = assignInTx(t, f.h, planAssignment{Team: f.team, Terms: testPlanTerms("custom_other", planKindCustom, 900, nil), Source: planSourceStaff, Reason: "new agreement"}); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if updated := f.reload(t, c.ID); updated.State != "undone" || len(f.stripe.writes) != 0 {
		t.Fatalf("stale change canceled subscription: %+v %v", updated, f.stripe.writes)
	}
}

func TestTeamPlanChangeSingleConnectionAndUndoAfterReassignment(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	cfg := f.h.Pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.h = &Handlers{Pool: pool, DB: db.New(pool), Stripe: f.h.Stripe}
	c, err := f.request(t, "free", "single-connection")
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if !f.stripe.cancel {
		t.Fatal("single-connection worker did not cancel renewal")
	}
	if _, err = f.h.undoTeamPlanChange(t.Context(), f.team, c.ID, f.actor, "keep plan"); err != nil {
		t.Fatal(err)
	}
	if _, err = assignInTx(t, f.h, planAssignment{Team: f.team, Terms: testPlanTerms("custom_other", planKindCustom, 900, nil), Source: planSourceStaff, Reason: "replacement agreement"}); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if len(f.stripe.writes) != 1 || !f.stripe.cancel || f.reload(t, c.ID).ProviderRenewedAt == nil {
		t.Fatalf("undo resurrected replaced subscription: %v", f.stripe.writes)
	}
}

func TestTeamPlanChangeStaffViewReportsPendingAndUndo(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO profile(id,email,provider) VALUES($1,'staff@superserve.ai','google')`, f.actor); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO user_role_assignments(user_id,role_id,scope_type) SELECT $1,id,'platform' FROM roles WHERE name='platform_admin'`, f.actor); err != nil {
		t.Fatal(err)
	}
	view := func() *customerPendingPlanChange {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Request.Header.Set(internalActorHeader, f.actor.String())
		c.Params = gin.Params{{Key: "team_id", Value: f.team.String()}}
		f.h.GetTeamPlanAssignments(c)
		var response struct {
			Pending json.RawMessage `json:"pending_change"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Pending) == 0 {
			t.Fatalf("staff view missing pending change status: %d %s", w.Code, w.Body.String())
		}
		var pending *customerPendingPlanChange
		if err := json.Unmarshal(response.Pending, &pending); err != nil {
			t.Fatal(err)
		}
		return pending
	}
	if pending := view(); pending != nil {
		t.Fatalf("unexpected pending change: %+v", pending)
	}
	change, err := f.request(t, "free", "staff-view")
	if err != nil {
		t.Fatal(err)
	}
	if pending := view(); pending == nil || pending.ID != change.ID || pending.TargetPlanKey != "free" || !pending.Undoable || !pending.PausesRunning || !pending.EffectiveAt.Equal(change.EffectiveAt) {
		t.Fatalf("staff cannot see scheduled downgrade and undo: %+v", pending)
	}
	if _, err := f.h.undoTeamPlanChange(t.Context(), f.team, change.ID, f.actor, "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if pending := view(); pending != nil {
		t.Fatalf("undone change still pending: %+v", pending)
	}
}

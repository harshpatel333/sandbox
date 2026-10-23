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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

type renewalStripe struct {
	t         *testing.T
	mu        sync.Mutex
	sub       string
	periodEnd time.Time
	status    string
	cancel    bool
	writes    []string
	failWrite bool
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
		_ = r.ParseForm()
		s.cancel = r.PostForm.Get("cancel_at_period_end") == "true"
		s.writes = append(s.writes, r.Header.Get("Idempotency-Key")+"="+r.PostForm.Get("cancel_at_period_end"))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": s.sub, "customer": "cus_x", "status": s.status, "cancel_at_period_end": s.cancel,
		"items": map[string]any{"data": []map[string]any{{"id": "si_x", "current_period_start": s.periodEnd.AddDate(0, -1, 0).Unix(), "current_period_end": s.periodEnd.Unix()}}}})
}

type planChangeV2Fixture struct {
	h      *Handlers
	team   uuid.UUID
	stripe *renewalStripe
	actor  uuid.UUID
}

func newPlanChangeV2Fixture(t *testing.T, kind string, withSubscription bool) planChangeV2Fixture {
	t.Helper()
	h, team := newPlanAssignmentFixture(t)
	f := planChangeV2Fixture{h: h, team: team, actor: uuid.New(), stripe: &renewalStripe{t: t, sub: "sub_" + uuid.NewString(), status: "active"}}
	srv := httptest.NewServer(f.stripe)
	t.Cleanup(srv.Close)
	h.Stripe = &stripeHTTPClient{baseURL: srv.URL, apiVersion: "2026-07-29.dahlia", httpClient: srv.Client()}
	anchor := time.Now().UTC().AddDate(0, -2, -3).Truncate(time.Second)
	var sub *string
	if withSubscription {
		sub = &f.stripe.sub
	}
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO team_billing_account(team_id,stripe_customer_id,stripe_subscription_id,stripe_subscription_status,commercial_billing_anchor) VALUES($1,$4,$2,'active',$3)`, team, sub, anchor, "cus_"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	_, end, err := h.currentBillingPeriod(t.Context(), team, pgtypeTime(anchor), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.stripe.periodEnd = end
	if _, err = assignInTx(t, h, planAssignment{Team: team, Terms: testPlanTerms(map[string]string{planKindSubscription: "pro_monthly", planKindPrepaid: "starter", planKindCustom: "custom_example"}[kind], 1000, nil), Kind: kind, Source: planSourceCheckout, Reason: "bought"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f planChangeV2Fixture) request(t *testing.T, target, key string) (teamPlanChange, error) {
	t.Helper()
	return f.h.scheduleTeamPlanChangeTo(t.Context(), teamPlanChangeRequest{Team: f.team, Target: target, Key: key, Actor: f.actor, Via: "customer", Reason: "requested by the team"},
		testPlanTerms(target, 1, nil))
}

func (f planChangeV2Fixture) tick(t *testing.T) {
	t.Helper()
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE team_plan_change SET next_check_at=clock_timestamp() WHERE team_id=$1 AND state IN ('scheduled','applied')`, f.team); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.runDueTeamPlanChange(t.Context(), f.team); err != nil {
		t.Fatal(err)
	}
}

func (f planChangeV2Fixture) reload(t *testing.T, id uuid.UUID) teamPlanChange {
	t.Helper()
	c, err := scanTeamPlanChange(f.h.Pool.QueryRow(t.Context(), `SELECT `+teamPlanChangeColumns+` FROM team_plan_change WHERE id=$1`, id))
	if err != nil {
		t.Fatal(err)
	}
	return c
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
	f := newPlanChangeV2Fixture(t, planKindPrepaid, false)
	c, err := f.request(t, "free", "k1")
	if err != nil || !c.PausesRunning || c.Subscription != nil || time.Since(c.EffectiveAt) > time.Minute {
		t.Fatalf("starter to free: %+v %v", c, err)
	}
	f.tick(t)
	c = f.reload(t, c.ID)
	plan, _, _ := currentTeamPlan(t.Context(), f.h.Pool, f.team)
	if c.State != "applied" || plan.PlanKey != "free" || plan.Source != planSourcePlanChange || plan.ID != *c.AppliedPlan {
		t.Fatalf("free not applied: %+v %+v", c, plan)
	}
	if max, _ := teamLimits(t, f.h, f.team); max != 1 {
		t.Fatalf("free limits not applied: %d", max)
	}
	f.tick(t)
	if c = f.reload(t, c.ID); c.State != "applied" || c.LastError == nil {
		t.Fatalf("change completed without pausing the team: %+v", c)
	}
	if len(f.stripe.writes) != 0 {
		t.Fatalf("a team without a subscription touched Stripe: %v", f.stripe.writes)
	}
}

func TestTeamPlanChangeProDowngradeStopsRenewalAndUndoes(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription, true)
	c, err := f.request(t, "free", "k1")
	if err != nil || c.Subscription == nil || !c.EffectiveAt.Equal(f.stripe.periodEnd) || !c.Undoable(time.Now()) {
		t.Fatalf("pro to free: %+v %v", c, err)
	}
	if again, err := f.request(t, "free", "k1"); err != nil || again.ID != c.ID {
		t.Fatalf("replay scheduled again: %+v %v", again, err)
	}
	if _, err = f.request(t, "free", "k2"); !errors.Is(err, errTeamPlanChangeConflict) {
		t.Fatalf("second pending change accepted: %v", err)
	}
	f.tick(t)
	if c = f.reload(t, c.ID); c.ProviderCancelAt == nil || !f.stripe.cancel || c.State != "scheduled" {
		t.Fatalf("renewal not stopped: %+v %v", c, f.stripe.writes)
	}
	if c, err = f.h.undoTeamPlanChange(t.Context(), f.team, c.ID, f.actor); err != nil || c.State != "undone" || f.stripe.cancel {
		t.Fatalf("undo: %+v %v %v", c, err, f.stripe.writes)
	}
	if again, err := f.h.undoTeamPlanChange(t.Context(), f.team, c.ID, f.actor); err != nil || again.State != "undone" {
		t.Fatalf("repeated undo: %v", err)
	}
	plan, _, _ := currentTeamPlan(t.Context(), f.h.Pool, f.team)
	if plan.PlanKey != "pro_monthly" {
		t.Fatalf("undone change moved the team: %+v", plan)
	}
	c, err = f.request(t, "starter", "k3")
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	f.reachEffective(t, c.ID)
	if _, err = f.h.undoTeamPlanChange(t.Context(), f.team, c.ID, f.actor); !errors.Is(err, errTeamPlanChangeConflict) {
		t.Fatalf("undo after the cutoff: %v", err)
	}
	f.tick(t)
	c = f.reload(t, c.ID)
	plan, _, _ = currentTeamPlan(t.Context(), f.h.Pool, f.team)
	if c.State != "completed" || plan.PlanKey != "starter" || plan.Kind != planKindPrepaid {
		t.Fatalf("starter not applied at the period end: %+v %+v", c, plan)
	}
}

func TestTeamPlanChangeDefersWhenThePeriodAlreadyRenewed(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription, true)
	c, err := f.request(t, "starter", "k1")
	if err != nil {
		t.Fatal(err)
	}
	f.stripe.periodEnd = f.stripe.periodEnd.AddDate(0, 1, 0)
	f.tick(t)
	c = f.reload(t, c.ID)
	if !c.EffectiveAt.Equal(f.stripe.periodEnd) || c.ProviderCancelAt == nil || !f.stripe.cancel {
		t.Fatalf("change not deferred to the renewed period end: %+v", c)
	}
}

func TestTeamPlanChangeRefusals(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindPrepaid, false)
	for target, want := range map[string]string{"starter": "already", "pro_monthly": "purchased"} {
		if _, err := f.request(t, target, "k-"+target); !errors.Is(err, errTeamPlanChangeConflict) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", target, err)
		}
	}
	h, team := newPlanAssignmentFixture(t)
	if _, err := h.scheduleTeamPlanChangeTo(t.Context(), teamPlanChangeRequest{Team: team, Target: "free", Key: "k", Actor: uuid.New(), Via: "customer", Reason: "r"}, testPlanTerms("free", 1, nil)); !errors.Is(err, errTeamPlanChangeConflict) {
		t.Fatalf("team without a plan scheduled a change: %v", err)
	}
	g := newPlanChangeV2Fixture(t, planKindSubscription, true)
	g.stripe.failWrite = true
	c, err := g.request(t, "free", "k1")
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

func TestTeamPlanChangeWithdrawnWhenTheTeamMovedFirst(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindCustom, false)
	c, err := f.request(t, "free", "k1")
	if err != nil {
		t.Fatal(err)
	}
	eight := int32(8)
	if _, err = assignInTx(t, f.h, planAssignment{Team: f.team, Terms: testPlanTerms("custom_other", 500, &eight), Kind: planKindCustom, Source: planSourceStaff, Reason: "new deal"}); err != nil {
		t.Fatal(err)
	}
	f.reachEffective(t, c.ID)
	f.tick(t)
	plan, _, _ := currentTeamPlan(t.Context(), f.h.Pool, f.team)
	if c = f.reload(t, c.ID); c.State != "undone" || plan.PlanKey != "custom_other" {
		t.Fatalf("stale change applied over the team's new plan: %+v %+v", c, plan)
	}
}

func TestTeamPlanChangeHistoryIsGuarded(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindCustom, false)
	c, err := f.request(t, "free", "k1")
	if err != nil {
		t.Fatal(err)
	}
	for name, stmt := range map[string]string{
		"apply early":    `UPDATE team_plan_change SET state='applied',applied_at=clock_timestamp(),applied_assignment_id=from_assignment_id WHERE id=$1`,
		"edit target":    `UPDATE team_plan_change SET target_plan_key='starter' WHERE id=$1`,
		"skip to done":   `UPDATE team_plan_change SET state='completed',completed_at=clock_timestamp() WHERE id=$1`,
		"move earlier":   `UPDATE team_plan_change SET effective_at=effective_at-interval '1 day' WHERE id=$1`,
		"delete":         `DELETE FROM team_plan_change WHERE id=$1`,
		"second pending": `INSERT INTO team_plan_change(team_id,from_assignment_id,target_plan_key,target_kind,target_terms,target_limits,effective_at,pauses_running,requested_by,requested_via,reason,idempotency_key) SELECT team_id,from_assignment_id,'starter','prepaid',target_terms,target_limits,effective_at,false,requested_by,'staff','r','k2' FROM team_plan_change WHERE id=$1`,
	} {
		tag, err := f.h.Pool.Exec(t.Context(), stmt, c.ID)
		if err == nil && tag.RowsAffected() > 0 {
			t.Errorf("%s was allowed", name)
		}
	}
}

func pgtypeTime(at time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: at, Valid: true} }

func TestTeamPlanChangeRefusedAndIdleWhilePricingV2IsOff(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindPrepaid, false)
	c, err := f.request(t, "free", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.h.scheduleTeamPlanChange(t.Context(), teamPlanChangeRequest{Team: f.team, Target: "free", Key: "k2", Actor: f.actor, Via: "customer", Reason: "r"}); !errors.Is(err, errTeamPlanChangeConflict) {
		t.Fatalf("plan change scheduled while pricing v2 is off: %v", err)
	}
	if worked, err := f.h.teamPlanChangeTick(t.Context()); err != nil || worked {
		t.Fatalf("worker advanced a change while pricing v2 is off: %v %v", worked, err)
	}
	if c = f.reload(t, c.ID); c.State != "scheduled" {
		t.Fatalf("change moved while pricing v2 is off: %+v", c)
	}
}

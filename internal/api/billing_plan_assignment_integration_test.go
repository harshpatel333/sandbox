//go:build integration

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/superserve-ai/sandbox/internal/db"
)

func newPlanAssignmentFixture(t *testing.T) (*Handlers, uuid.UUID) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil || !strings.HasSuffix(cfg.ConnConfig.Database, "_test") {
		t.Fatal("DATABASE_URL must name a migrated disposable test database")
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h := &Handlers{Pool: pool, DB: db.New(pool)}
	team, err := h.DB.CreateTeam(t.Context(), "plan-assignment-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	return h, team.ID
}

func testPlanTerms(key string, maxSandboxes int32, vcpu *int32) planTerms {
	return planTerms{OfferKey: key, DisplayName: key, Limits: planLimits{MaxSandboxes: &maxSandboxes, MaxTemplateVCPU: vcpu}}
}

func assignInTx(t *testing.T, h *Handlers, a planAssignment) (teamPlan, error) {
	t.Helper()
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(t.Context()))
	p, err := assignTeamPlan(t.Context(), tx, a)
	if err != nil {
		return p, err
	}
	return p, tx.Commit(t.Context())
}

func teamLimits(t *testing.T, h *Handlers, team uuid.UUID) (int32, *int32) {
	t.Helper()
	var max int32
	var vcpu *int32
	if err := h.Pool.QueryRow(t.Context(), `SELECT max_sandboxes,max_template_vcpu FROM team WHERE id=$1`, team).Scan(&max, &vcpu); err != nil {
		t.Fatal(err)
	}
	return max, vcpu
}

func TestPlanAssignmentMovesTeamAndAppliesLimits(t *testing.T) {
	h, team := newPlanAssignmentFixture(t)
	if _, ok, err := currentTeamPlan(t.Context(), h.Pool, team); err != nil || ok {
		t.Fatalf("new team already has a plan: %v %v", ok, err)
	}
	actor := uuid.New()
	free, err := assignInTx(t, h, planAssignment{Team: team, Terms: testPlanTerms("free", 1, nil), Kind: planKindFree, Source: planSourceSignup, Reason: "signed up"})
	if err != nil {
		t.Fatal(err)
	}
	if max, vcpu := teamLimits(t, h, team); max != 1 || vcpu != nil {
		t.Fatalf("free limits not applied: %d %v", max, vcpu)
	}
	eight := int32(8)
	pro, err := assignInTx(t, h, planAssignment{Team: team, Terms: testPlanTerms("pro_monthly", 1000, &eight), Kind: planKindSubscription, Source: planSourceCheckout, Actor: &actor, Reason: "paid"})
	if err != nil {
		t.Fatal(err)
	}
	if max, vcpu := teamLimits(t, h, team); max != 1000 || vcpu == nil || *vcpu != 8 {
		t.Fatalf("pro limits not applied: %d %v", max, vcpu)
	}
	current, ok, err := currentTeamPlan(t.Context(), h.Pool, team)
	if err != nil || !ok || current.ID != pro.ID || current.Kind != planKindSubscription || *current.AssignedBy != actor {
		t.Fatalf("current plan is not the latest assignment: %+v %v", current, err)
	}
	history, err := teamPlanHistory(t.Context(), h.Pool, team, 10)
	if err != nil || len(history) != 2 || history[1].ID != free.ID || history[1].EffectiveTo == nil || !history[1].EffectiveTo.Equal(pro.EffectiveFrom) {
		t.Fatalf("previous plan was not closed where the next began: %+v %v", history, err)
	}
}

func TestPlanAssignmentKeepsCustomizedLimits(t *testing.T) {
	h, team := newPlanAssignmentFixture(t)
	if _, err := h.Pool.Exec(t.Context(), `UPDATE team SET max_sandboxes=77 WHERE id=$1`, team); err != nil {
		t.Fatal(err)
	}
	if _, err := assignInTx(t, h, planAssignment{Team: team, Terms: testPlanTerms("free", 1, nil), Kind: planKindFree, Source: planSourceMigration, Reason: "migration"}); err != nil {
		t.Fatal(err)
	}
	if max, _ := teamLimits(t, h, team); max != 77 {
		t.Fatalf("hand-set limits were overwritten: %d", max)
	}
}

func TestPlanAssignmentRefusesRetiredAndReusedKeys(t *testing.T) {
	h, team := newPlanAssignmentFixture(t)
	retired := testPlanTerms("custom_example", 50, nil)
	retired.Retired = true
	if _, err := assignInTx(t, h, planAssignment{Team: team, Terms: retired, Kind: planKindCustom, Source: planSourceStaff, Reason: "r"}); !errors.Is(err, errPlanRetired) {
		t.Fatalf("retired plan assigned: %v", err)
	}
	a := planAssignment{Team: team, Terms: testPlanTerms("custom_example", 50, nil), Kind: planKindCustom, Source: planSourceStaff, Reason: "r", Key: "assign-1"}
	first, err := assignInTx(t, h, a)
	if err != nil {
		t.Fatal(err)
	}
	again, err := assignInTx(t, h, a)
	if err != nil || again.ID != first.ID {
		t.Fatalf("repeated request assigned again: %+v %v", again, err)
	}
	other := a
	other.Terms = testPlanTerms("free", 1, nil)
	other.Kind = planKindFree
	if _, err = assignInTx(t, h, other); !errors.Is(err, errPlanAssignmentReused) {
		t.Fatalf("idempotency key reused for another plan: %v", err)
	}
}

func TestPlanAssignmentHistoryIsImmutable(t *testing.T) {
	h, team := newPlanAssignmentFixture(t)
	p, err := assignInTx(t, h, planAssignment{Team: team, Terms: testPlanTerms("free", 1, nil), Kind: planKindFree, Source: planSourceSignup, Reason: "signed up"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.DB.CreateTeam(t.Context(), "plan-assignment-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Pool.Exec(t.Context(), `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason) VALUES($1,'free','free','{}','{"max_sandboxes":1}',clock_timestamp()+interval '1 day','staff','r')`, other.ID); err == nil {
		t.Error("future assignment was allowed")
	}
	for name, stmt := range map[string]string{
		"edit terms":     `UPDATE team_plan_assignment SET plan_key='pro_monthly' WHERE id=$1`,
		"delete":         `DELETE FROM team_plan_assignment WHERE id=$1`,
		"second open":    `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason) SELECT team_id,'pro_monthly','subscription',terms,limits,clock_timestamp(),'staff','r' FROM team_plan_assignment WHERE id=$1`,
		"backdated":      `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason) SELECT team_id,'pro_monthly','subscription',terms,limits,effective_from-interval '1 day','staff','r' FROM team_plan_assignment WHERE id=$1`,
		"missing limits": `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason) SELECT team_id,'free','free',terms,'{}',clock_timestamp(),'staff','r' FROM team_plan_assignment WHERE id=$1`,
	} {
		if _, err := h.Pool.Exec(t.Context(), stmt, p.ID); err == nil {
			t.Errorf("%s was allowed", name)
		}
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE team_plan_assignment SET effective_to=clock_timestamp() WHERE id=$1`, p.ID); err != nil {
		t.Fatalf("closing an assignment was refused: %v", err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE team_plan_assignment SET effective_to=clock_timestamp() WHERE id=$1`, p.ID); err == nil {
		t.Fatal("a closed assignment was closed again")
	}
}

func TestPlanAssignmentStaffViewRequiresStaff(t *testing.T) {
	h, team := newPlanAssignmentFixture(t)
	for _, actor := range []struct {
		header string
		want   int
	}{{"", http.StatusUnauthorized}, {uuid.NewString(), http.StatusForbidden}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		if actor.header != "" {
			c.Request.Header.Set(internalActorHeader, actor.header)
		}
		c.Params = gin.Params{{Key: "team_id", Value: team.String()}}
		h.GetTeamPlanAssignments(c)
		if w.Code != actor.want {
			t.Fatalf("plan assignments with actor %q: got %d want %d", actor.header, w.Code, actor.want)
		}
	}
}

func TestPlanAssignmentInitialBackfill(t *testing.T) {
	h, assigned := newPlanAssignmentFixture(t)
	if _, err := assignInTx(t, h, planAssignment{Team: assigned, Terms: testPlanTerms("free", 1, nil), Kind: planKindFree, Source: planSourceSignup, Reason: "signed up"}); err != nil {
		t.Fatal(err)
	}
	var fresh []uuid.UUID
	for range 3 {
		team, err := h.DB.CreateTeam(t.Context(), "plan-backfill-"+uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		fresh = append(fresh, team.ID)
	}
	busy := fresh[2]
	locker, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = locker.Exec(t.Context(), `SELECT 1 FROM team WHERE id=$1 FOR UPDATE`, busy); err != nil {
		t.Fatal(err)
	}
	batch := []planAssignment{
		{Team: assigned, Terms: testPlanTerms("legacy_payg", 20, nil), Kind: planKindLegacyPAYG, Source: planSourceMigration, Reason: "pricing v2"},
		{Team: fresh[0], Terms: testPlanTerms("legacy_payg", 20, nil), Kind: planKindLegacyPAYG, Source: planSourceMigration, Reason: "pricing v2"},
		{Team: fresh[1], Terms: testPlanTerms("free", 1, nil), Kind: planKindFree, Source: planSourceMigration, Reason: "pricing v2"},
		{Team: busy, Terms: testPlanTerms("free", 1, nil), Kind: planKindFree, Source: planSourceMigration, Reason: "pricing v2"},
	}
	run := func() []uuid.UUID {
		tx, err := h.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.WithoutCancel(t.Context()))
		done, err := assignInitialTeamPlans(t.Context(), tx, batch)
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		return done
	}
	done := run()
	if len(done) != 2 || !slices.Contains(done, fresh[0]) || !slices.Contains(done, fresh[1]) {
		t.Fatalf("first pass assigned %v", done)
	}
	if p, _, _ := currentTeamPlan(t.Context(), h.Pool, assigned); p.Kind != planKindFree {
		t.Fatalf("backfill replaced an existing plan: %+v", p)
	}
	if max, _ := teamLimits(t, h, fresh[1]); max != 1 {
		t.Fatalf("backfilled limits not applied: %d", max)
	}
	if err = locker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if done = run(); len(done) != 1 || done[0] != busy {
		t.Fatalf("second pass should pick up only the busy team: %v", done)
	}
	if done = run(); len(done) != 0 {
		t.Fatalf("re-run assigned again: %v", done)
	}
}

func TestStaffPlanAssignmentRefusedWhilePricingV2IsOff(t *testing.T) {
	h, team := newPlanAssignmentFixture(t)
	if _, err := h.assignStaffPlan(t.Context(), team, uuid.New(), staffPlanAssignmentBody{Key: "k", VersionID: uuid.New(), Reason: "enterprise approval"}); !errors.Is(err, errPricingV2Off) {
		t.Fatalf("plan assigned with pricing v2 off: %v", err)
	}
	if _, ok, _ := currentTeamPlan(t.Context(), h.Pool, team); ok {
		t.Fatal("refused assignment left a plan behind")
	}
	for _, actor := range []struct {
		header string
		want   int
	}{{"", http.StatusUnauthorized}, {uuid.NewString(), http.StatusForbidden}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"idempotency_key":"k","version_id":"`+uuid.NewString()+`","reason":"r"}`))
		if actor.header != "" {
			c.Request.Header.Set(internalActorHeader, actor.header)
		}
		c.Params = gin.Params{{Key: "team_id", Value: team.String()}}
		h.AssignTeamPlan(c)
		if w.Code != actor.want {
			t.Fatalf("staff assign with actor %q: got %d want %d", actor.header, w.Code, actor.want)
		}
	}
}

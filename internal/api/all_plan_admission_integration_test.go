//go:build integration

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/superserve-ai/sandbox/internal/config"
)

// The SQL trigger is shared by raw, template and snapshot creation, as well as
// resume. Exercise persisted rows so a handler's preflight cannot hide a bypass.
func TestAllPlanSandboxShapeAdmission(t *testing.T) {
	for _, kind := range []string{planKindPrepaid, planKindSubscription, planKindCustom, "legacy"} {
		t.Run(kind, func(t *testing.T) {
			h, team := newPlanAssignmentFixture(t)
			if kind != "legacy" {
				terms := testPlanTerms("shape-"+kind, kind, 100, int32p(2))
				terms.Limits.MaxMemoryMiB, terms.Limits.MaxDiskMiB = int32p(2048), int32p(4096)
				if _, err := assignInTx(t, h, planAssignment{Team: team, Terms: terms, Source: planSourceStaff, Reason: "shape fixture"}); err != nil {
					t.Fatal(err)
				}
			}
			financial := func() string {
				var v string
				if err := h.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object('accounts',(SELECT coalesce(jsonb_agg(to_jsonb(a)),'[]'::jsonb) FROM billing_funding_account a WHERE team_id=$1),'upgrades',(SELECT count(*) FROM billing_pro_upgrade WHERE team_id=$1))::text`, team).Scan(&v); err != nil {
					t.Fatal(err)
				}
				return v
			}
			beforeMoney := financial()
			caps, err := h.teamEffectiveLimits(t.Context(), team)
			if err != nil {
				t.Fatal(err)
			}
			for _, dimension := range []string{"cpu", "memory", "disk"} {
				cpu, mem, disk := caps.MaxVCPU, caps.MaxMemoryMiB, caps.MaxDiskMiB
				switch dimension {
				case "cpu":
					cpu++
				case "memory":
					mem++
				case "disk":
					disk++
				}
				_, err = h.Pool.Exec(t.Context(), `INSERT INTO sandbox(id,team_id,name,host_id,status,vcpu_count,memory_mib,disk_mib) VALUES($1,$2,'oversized','default','starting',$3,$4,$5)`, uuid.New(), team, cpu, mem, disk)
				var pe *pgconn.PgError
				if !errors.As(err, &pe) || pe.Code != "SS002" {
					t.Fatalf("%s insert=%v", dimension, err)
				}
				id := uuid.New()
				if _, err = h.Pool.Exec(t.Context(), `INSERT INTO sandbox(id,team_id,name,host_id,status,vcpu_count,memory_mib,disk_mib) VALUES($1,$2,'paused-oversized','default','paused',$3,$4,$5)`, id, team, cpu, mem, disk); err != nil {
					t.Fatal(err)
				}
				_, err = h.Pool.Exec(t.Context(), `UPDATE sandbox SET status='resuming' WHERE id=$1`, id)
				if !errors.As(err, &pe) || pe.Code != "SS002" {
					t.Fatalf("%s resume=%v", dimension, err)
				}
			}
			if _, err = h.Pool.Exec(t.Context(), `INSERT INTO sandbox(id,team_id,name,host_id,status,vcpu_count,memory_mib,disk_mib) VALUES($1,$2,'boundary','default','starting',$3,$4,$5)`, uuid.New(), team, caps.MaxVCPU, caps.MaxMemoryMiB, caps.MaxDiskMiB); err != nil {
				t.Fatal(err)
			}
			if after := financial(); after != beforeMoney {
				t.Fatalf("financial side effects before=%s after=%s", beforeMoney, after)
			}
		})
	}
}

func templateAdmissionRouter(t *testing.T, h *Handlers, team uuid.UUID) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	actor := uuid.New()
	h.Config = &config.Config{}
	for _, q := range []string{
		`INSERT INTO profile(id,email,provider,provider_id) VALUES($1,$1::uuid::text||'@example.com','google',$1::uuid::text)`,
		`INSERT INTO team_memberships(team_id,user_id,status) VALUES($2,$1,'active')`,
		`INSERT INTO user_role_assignments(user_id,role_id,scope_type,team_id) SELECT $1,id,'team',$2 FROM roles WHERE name='team_owner'`,
	} { // The profile statement has one parameter; the other statements have two.
		var err error
		if strings.Contains(q, "INSERT INTO profile") {
			_, err = h.Pool.Exec(t.Context(), q, actor)
		} else {
			_, err = h.Pool.Exec(t.Context(), q, actor, team)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("team_id", team.String()); c.Set("actor_id", actor) })
	r.POST("/templates", h.CreateTemplate)
	r.POST("/templates/:template_id/builds", h.CreateTemplateBuild)
	return r
}
func templateAdmissionRequest(r http.Handler, name string, cpu int32) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/templates", strings.NewReader(fmt.Sprintf(`{"name":%q,"vcpu":%d,"memory_mib":1024,"disk_mib":4096,"build_spec":{"from":"python:3.11"}}`, name, cpu))))
	return w
}
func setAdmissionCaps(t *testing.T, h *Handlers, team uuid.UUID, cpu, templates int32) {
	t.Helper()
	if _, err := h.Pool.Exec(t.Context(), `UPDATE team SET max_template_vcpu=$2,max_template_memory_mib=2048,max_template_disk_mib=4096,max_templates=$3 WHERE id=$1`, team, cpu, templates); err != nil {
		t.Fatal(err)
	}
}
func TestTemplateAdmissionPlanContentionAndFreshLimits(t *testing.T) {
	h, team := newPlanAssignmentFixture(t)
	setAdmissionCaps(t, h, team, 8, 10)
	r := templateAdmissionRouter(t, h, team)
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(t.Context(), `UPDATE team SET max_template_vcpu=2 WHERE id=$1`, team); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	w := templateAdmissionRequest(r, "contended", 4)
	if w.Code != 503 || time.Since(start) > time.Second {
		t.Fatalf("contended=%d %s elapsed=%s", w.Code, w.Body, time.Since(start))
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if w = templateAdmissionRequest(r, "too-large", 4); w.Code != 400 {
		t.Fatalf("shape=%d %s", w.Code, w.Body)
	}
	if w = templateAdmissionRequest(r, "boundary", 2); w.Code != 202 {
		t.Fatalf("boundary=%d %s", w.Code, w.Body)
	}
	setAdmissionCaps(t, h, team, 2, 1)
	if w = templateAdmissionRequest(r, "count-at-limit", 1); w.Code != 429 {
		t.Fatalf("template count=%d %s", w.Code, w.Body)
	}
	// Remove the explicit staff override before checking the plan's zero cap.
	if _, err = h.Pool.Exec(t.Context(), `UPDATE team SET max_templates=NULL WHERE id=$1`, team); err != nil {
		t.Fatal(err)
	}
	terms := testPlanTerms("free", planKindFree, 1, int32p(2))
	terms.Limits.MaxMemoryMiB, terms.Limits.MaxDiskMiB, terms.Limits.MaxTemplates = int32p(2048), int32p(4096), int32p(0)
	if _, err = assignInTx(t, h, planAssignment{Team: team, Terms: terms, Source: planSourceStaff, Reason: "free template limit"}); err != nil {
		t.Fatal(err)
	}
	if w = templateAdmissionRequest(r, "no-templates", 1); w.Code != 429 {
		t.Fatalf("zero templates=%d %s", w.Code, w.Body)
	}
	var id uuid.UUID
	if err = h.Pool.QueryRow(t.Context(), `SELECT id FROM template WHERE team_id=$1 AND name='boundary'`, team).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// An already inflight join is idempotent and consumes no new capacity.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/templates/"+id.String()+"/builds", nil))
	if w.Code != 200 {
		t.Fatalf("join=%d %s", w.Code, w.Body)
	}
	if _, err = h.Pool.Exec(t.Context(), `UPDATE template_build SET status='failed', finalized_at=clock_timestamp() WHERE template_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/templates/"+id.String()+"/builds", nil))
	if w.Code != 429 {
		t.Fatalf("new free build=%d %s", w.Code, w.Body)
	}
}

func TestTemplateAdmissionSerializesDowngrade(t *testing.T) {
	h, team := newPlanAssignmentFixture(t)
	setAdmissionCaps(t, h, team, 8, 10)
	r := templateAdmissionRouter(t, h, team)
	blocker, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	if _, err = blocker.Exec(t.Context(), `SELECT pg_advisory_lock(hashtext($1))`, team.String()); err != nil {
		t.Fatal(err)
	}
	defer blocker.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, team.String())
	admitted := make(chan *httptest.ResponseRecorder, 1)
	go func() { admitted <- templateAdmissionRequest(r, "before-downgrade", 4) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting bool
		err = h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker.Conn().PgConn().PID()).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("creation never waited on build lock")
		}
		time.Sleep(time.Millisecond * 5)
	}
	downgrade, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer downgrade.Release()
	lowered := make(chan error, 1)
	go func() {
		_, err := downgrade.Exec(t.Context(), `UPDATE team SET max_template_vcpu=2 WHERE id=$1`, team)
		lowered <- err
	}()
	for {
		var n int
		err = h.Pool.QueryRow(t.Context(), `SELECT cardinality(pg_blocking_pids($1))`, downgrade.Conn().PgConn().PID()).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("downgrade did not wait for creation's team lock")
		}
		time.Sleep(time.Millisecond * 5)
	}
	if _, err = blocker.Exec(t.Context(), `SELECT pg_advisory_unlock(hashtext($1))`, team.String()); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-admitted:
		if w.Code != 202 {
			t.Fatalf("admitted=%d %s", w.Code, w.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("creation stuck")
	}
	select {
	case err = <-lowered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("downgrade stuck")
	}
	if w := templateAdmissionRequest(r, "after-downgrade", 4); w.Code != 400 {
		t.Fatalf("post-downgrade=%d %s", w.Code, w.Body)
	}
}

func TestAllPlanAdmissionPreservesFreeFrozenNullFallback(t *testing.T) {
	h, team := newPlanAssignmentFixture(t)
	terms := testPlanTerms("free", planKindFree, 1, int32p(1))
	terms.Limits.MaxMemoryMiB, terms.Limits.MaxDiskMiB, terms.Limits.MaxTemplates = int32p(1024), int32p(4096), int32p(0)
	if _, err := assignInTx(t, h, planAssignment{Team: team, Terms: terms, Source: planSourceStaff, Reason: "frozen Free limits"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE team SET limits_customized=true,max_template_vcpu=NULL WHERE id=$1`, team); err != nil {
		t.Fatal(err)
	}
	limits, err := h.teamEffectiveLimits(t.Context(), team)
	if err != nil || limits.MaxVCPU <= 1 {
		t.Fatalf("legacy fallback fixture=%+v %v", limits, err)
	}
	_, err = h.Pool.Exec(t.Context(), `INSERT INTO sandbox(id,team_id,name,host_id,status,vcpu_count,memory_mib,disk_mib) VALUES($1,$2,'frozen-free-cap','default','starting',2,1024,4096)`, uuid.New(), team)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "SS002" {
		t.Fatalf("Free frozen cap relaxed: %v", err)
	}
}

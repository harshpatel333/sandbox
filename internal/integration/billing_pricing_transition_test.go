//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/superserve-ai/sandbox/internal/api"
	"github.com/superserve-ai/sandbox/internal/config"
	"github.com/superserve-ai/sandbox/internal/db"
)

type pricingTransitionHTTPFixture struct {
	owner, service             *pgxpool.Pool
	router                     *gin.Engine
	actor, outsider, nonGoogle uuid.UUID
}

func newPricingTransitionHTTPFixture(t *testing.T) pricingTransitionHTTPFixture {
	t.Helper()
	owner := teamCreationDatabase(t, func(pool *pgxpool.Pool) {
		rolloutExec(t, pool, `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO anon,authenticated,service_role;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO anon,authenticated,service_role;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT EXECUTE ON FUNCTIONS TO anon,authenticated,service_role`)
	})
	service := teamCreationServiceRolePool(t, owner)
	f := pricingTransitionHTTPFixture{owner: owner, service: service, actor: uuid.New(), outsider: uuid.New(), nonGoogle: uuid.New()}
	for _, id := range []uuid.UUID{f.actor, f.outsider, f.nonGoogle} {
		provider := "google"
		if id == f.nonGoogle {
			provider = "github"
		}
		rolloutExec(t, owner, `INSERT INTO profile(id,email,provider,provider_id) VALUES($1,$2,$3,$2)`, id, id.String()+"@superserve.ai", provider)
		if id != f.outsider {
			rolloutExec(t, owner, `INSERT INTO user_role_assignments(user_id,role_id,scope_type) SELECT $1,id,'platform' FROM roles WHERE name='platform_admin'`, id)
		}
	}
	f.router = f.routerFor(t, service)
	return f
}

func (f pricingTransitionHTTPFixture) routerFor(t *testing.T, pool *pgxpool.Pool) *gin.Engine {
	t.Helper()
	t.Setenv("INTERNAL_API_TOKEN", internalRBACToken)
	t.Setenv("OPERATOR_API_TOKEN", operatorRBACToken)
	h := api.NewHandlers(&stubVMD{}, db.New(pool), &config.Config{SystemTeamID: testSystemTeamID.String()})
	h.Pool = pool
	return api.SetupRouter(t.Context(), h, pool)
}

func TestIntegration_PricingTransitionMissingRPCFailsClosed(t *testing.T) {
	f := newPricingTransitionHTTPFixture(t)
	var installed bool
	if err := f.owner.QueryRow(t.Context(), `SELECT to_regprocedure('public.billing_transition_pricing_version(text,uuid,text)') IS NOT NULL`).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if installed {
		rolloutExec(t, f.owner, `ALTER FUNCTION public.billing_transition_pricing_version(text,uuid,text) RENAME TO pricing_transition_unavailable`)
	}
	w := operatorRequest(f.router, http.MethodPost, "/internal/billing/pricing-version", f.actor.String(), `{"pricing_version":"v2","reason":"Transition unavailable"}`)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "billing_transition") {
		t.Fatalf("missing RPC=%d %s", w.Code, w.Body)
	}
	var model string
	var audits int
	if err := f.owner.QueryRow(t.Context(), `SELECT pricing_version,(SELECT count(*) FROM billing_pricing_switch_audit) FROM billing_pricing_switch`).Scan(&model, &audits); err != nil || model != "legacy" || audits != 0 {
		t.Fatalf("state=%s audits=%d err=%v", model, audits, err)
	}
}

type pricingTransitionCommitTrace struct {
	mu             sync.Mutex
	transitionConn *pgx.Conn
	rpcCalls       int
	afterCommit    func()
	beforeRPC      func()
}

func (tr *pricingTransitionCommitTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "FROM public.billing_transition_pricing_version(") {
		tr.mu.Lock()
		tr.transitionConn = conn
		tr.rpcCalls++
		callback := tr.beforeRPC
		tr.beforeRPC = nil
		tr.mu.Unlock()
		if callback != nil {
			callback()
		}
	}
	return ctx
}
func (tr *pricingTransitionCommitTrace) TraceQueryEnd(_ context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	tr.mu.Lock()
	var callback func()
	if data.Err == nil && data.CommandTag.String() == "COMMIT" && conn == tr.transitionConn {
		callback = tr.afterCommit
		tr.afterCommit = nil
		tr.transitionConn = nil
	}
	tr.mu.Unlock()
	if callback != nil {
		callback()
	}
}

func TestIntegration_PricingTransitionOperatorHTTP(t *testing.T) {
	f := newPricingTransitionHTTPFixture(t)
	var installed bool
	if err := f.owner.QueryRow(t.Context(), `SELECT to_regprocedure('public.billing_transition_pricing_version(text,uuid,text)') IS NOT NULL`).Scan(&installed); err != nil || !installed {
		t.Fatalf("owning W5 transition migration required: installed=%v err=%v", installed, err)
	}
	body := `{"pricing_version":"legacy","reason":"Authorized retry"}`
	for _, id := range []uuid.UUID{f.outsider, f.nonGoogle} {
		if w := operatorRequest(f.router, http.MethodPost, "/internal/billing/pricing-version", id.String(), body); w.Code != http.StatusForbidden {
			t.Fatalf("ineligible actor=%d %s", w.Code, w.Body)
		}
	}
	if w := doInternal(f.router, http.MethodPost, "/internal/billing/pricing-version", f.actor.String(), body); w.Code != http.StatusUnauthorized {
		t.Fatalf("internal token=%d %s", w.Code, w.Body)
	}
	if w := operatorRequest(f.router, http.MethodPost, "/internal/billing/pricing-version", "", body); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing actor=%d %s", w.Code, w.Body)
	}
	for _, bad := range []string{`{"pricing_version":"invalid","reason":"Rejected"}`, `{"pricing_version":"legacy","reason":" "}`, `{"pricing_version":"legacy","reason":"\u0000"}`} {
		if w := operatorRequest(f.router, http.MethodPost, "/internal/billing/pricing-version", f.actor.String(), bad); w.Code != 400 {
			t.Fatalf("invalid body=%d %s", w.Code, w.Body)
		}
	}
	w := operatorRequest(f.router, http.MethodPost, "/internal/billing/pricing-version", f.actor.String(), body)
	var initial map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &initial); w.Code != 200 || err != nil || len(initial) != 4 || initial["changed_by"] != nil || initial["reason"] != nil {
		t.Fatalf("initial no-op=%d %s err=%v", w.Code, w.Body, err)
	}
	if stamp, ok := initial["changed_at"].(string); !ok || len(stamp) != 20 || !strings.HasSuffix(stamp, "Z") {
		t.Fatalf("timestamp=%v", initial["changed_at"])
	}
	var epoch uuid.UUID
	if err := f.owner.QueryRow(t.Context(), `SELECT epoch FROM billing_pricing_adoption`).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	trace := &pricingTransitionCommitTrace{}
	cfg := f.service.Config().Copy()
	cfg.ConnConfig.Tracer = trace
	cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "serializable"
	traced, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	tracedRouter := f.routerFor(t, traced)
	trace.afterCommit = func() {
		rolloutExec(t, f.owner, `SELECT * FROM public.billing_transition_pricing_version('legacy',$1,'Concurrent rollback')`, f.actor)
	}
	w = operatorRequest(tracedRouter, http.MethodPost, "/internal/billing/pricing-version", f.actor.String(), `{"pricing_version":"v2","reason":"  Authorized preparing transition  ","actor_id":"00000000-0000-0000-0000-000000000000"}`)
	var changed map[string]any
	if err = json.Unmarshal(w.Body.Bytes(), &changed); w.Code != 200 || err != nil || len(changed) != 4 || changed["pricing_version"] != "v2" || changed["changed_by"] != f.actor.String() || changed["reason"] != "Authorized preparing transition" {
		t.Fatalf("captured response=%d %s err=%v", w.Code, w.Body, err)
	}
	var model, state string
	var latest uuid.UUID
	var audits, events int
	if err = f.owner.QueryRow(t.Context(), `SELECT s.pricing_version,a.epoch,a.state,(SELECT count(*) FROM billing_pricing_switch_audit),(SELECT count(*) FROM billing_pricing_adoption_event WHERE action='switch_reset') FROM billing_pricing_switch s CROSS JOIN billing_pricing_adoption a`).Scan(&model, &latest, &state, &audits, &events); err != nil || model != "legacy" || latest == epoch || state != "preparing" || audits != 2 || events != 2 {
		t.Fatalf("committed state=%s epoch=%s state=%s audits=%d events=%d err=%v", model, latest, state, audits, events, err)
	}
	trace.mu.Lock()
	calls := trace.rpcCalls
	trace.mu.Unlock()
	if calls != 1 {
		t.Fatalf("RPC calls=%d", calls)
	}
	rolloutExec(t, f.owner, `CREATE FUNCTION pricing_transition_commit_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 RAISE EXCEPTION 'private deferred failure' USING ERRCODE='42501',CONSTRAINT='billing_pricing_transition_actor'; END $$;
 CREATE CONSTRAINT TRIGGER pricing_transition_commit_fault AFTER INSERT ON billing_pricing_switch_audit DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION pricing_transition_commit_fault()`)
	w = operatorRequest(tracedRouter, http.MethodPost, "/internal/billing/pricing-version", f.actor.String(), `{"pricing_version":"v2","reason":"Commit fault"}`)
	if w.Code != 503 || strings.Contains(w.Body.String(), "private deferred failure") {
		t.Fatalf("commit failure=%d %s", w.Code, w.Body)
	}
	var after uuid.UUID
	if err = f.owner.QueryRow(t.Context(), `SELECT s.pricing_version,a.epoch,(SELECT count(*) FROM billing_pricing_switch_audit),(SELECT count(*) FROM billing_pricing_adoption_event WHERE action='switch_reset') FROM billing_pricing_switch s CROSS JOIN billing_pricing_adoption a`).Scan(&model, &after, &audits, &events); err != nil || model != "legacy" || after != latest || audits != 2 || events != 2 {
		t.Fatalf("failed commit state=%s epoch=%s audits=%d events=%d err=%v", model, after, audits, events, err)
	}
	trace.mu.Lock()
	calls = trace.rpcCalls
	trace.mu.Unlock()
	if calls != 2 {
		t.Fatalf("commit failure retried RPC: calls=%d", calls)
	}
	rolloutExec(t, f.owner, `DROP TRIGGER pricing_transition_commit_fault ON billing_pricing_switch_audit`)
	trace.beforeRPC = func() {
		rolloutExec(t, f.owner, `UPDATE user_role_assignments SET revoked_at=clock_timestamp() WHERE user_id=$1`, f.actor)
	}
	w = operatorRequest(tracedRouter, http.MethodPost, "/internal/billing/pricing-version", f.actor.String(), body)
	if w.Code != 403 {
		t.Fatalf("RPC lock-time revoked authority=%d %s", w.Code, w.Body)
	}

}

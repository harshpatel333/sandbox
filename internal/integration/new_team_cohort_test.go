//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func cohortFixture(t *testing.T) (*pgxpool.Pool, uuid.UUID, uuid.UUID) {
	t.Helper()
	// Roles are cluster-wide, as in the existing promotion test harness. These
	// databases are isolated and never name a provider or deployment database.
	rolloutExec(t, testPool, `DO $$ DECLARE r text; BEGIN FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN EXECUTE format('CREATE ROLE %I NOLOGIN',r); END IF; END LOOP; END $$`)
	pool := promotionIsolatedDatabase(t, true)
	system, actor := uuid.New(), uuid.New()
	rolloutExec(t, pool, `INSERT INTO team(id,name) VALUES($1,'cohort-system')`, system)
	rolloutExec(t, pool, `INSERT INTO profile(id,email) VALUES($1,'cohort@example.invalid')`, actor)
	var free uuid.UUID
	err := pool.QueryRow(t.Context(), `INSERT INTO billing_plan_version(family,version,pricing_version,kind,display_name,public,allowance_period,allowance_on_exhausted,allowance_compute_seconds,allowance_compute_vcpu,allowance_compute_memory_gib,max_sandboxes,max_template_vcpu,max_template_memory_mib,max_template_disk_mib,max_templates)
 VALUES('free',(SELECT max(version)+1 FROM billing_plan_version WHERE family='free'),'v2','free','Free',true,'day_window','pause',7200,1,1,1,1,1024,4096,0) RETURNING id`).Scan(&free)
	if err != nil {
		t.Fatal(err)
	}
	rolloutExec(t, pool, `SELECT publish_billing_plan_version($1,NULL)`, free)
	for _, family := range []string{"developer", "pro"} {
		cohortPublishSubscription(t, pool, family)
	}
	return pool, system, actor
}

func cohortPublishSubscription(t *testing.T, pool *pgxpool.Pool, family string) {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(), `INSERT INTO billing_plan_version(family,version,pricing_version,kind,public,self_serve,display_name,monthly_fee_cents,credit_schedule,max_sandboxes,max_template_vcpu,max_template_memory_mib,max_template_disk_mib,max_templates)
 VALUES($1,(SELECT COALESCE(max(version),0)+1 FROM billing_plan_version WHERE family=$1),'v2','subscription',true,true,$1,3100,
  jsonb_build_array(jsonb_build_object('start_months',0,'repeat',NULL,'every_months',1,'amount_cents',3100,'expires_after_months',1)),7,3,3072,9216,4) RETURNING id`, family).Scan(&id); err != nil {
		t.Fatal(err)
	}
	rolloutExec(t, pool, `SELECT publish_billing_plan_version($1,NULL)`, id)
}

func cohortRelease(t *testing.T, pool *pgxpool.Pool, system uuid.UUID) uuid.UUID {
	t.Helper()
	epoch, err := cohortTryRelease(t, pool, system)
	if err != nil {
		t.Fatal(err)
	}
	return epoch
}

func cohortTryRelease(t *testing.T, pool *pgxpool.Pool, system uuid.UUID) (uuid.UUID, error) {
	t.Helper()
	var epoch uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT billing_prepare_new_team_gate('use',$1,'local-test','genuine new teams only')`, system).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	evidence, _ := json.Marshal(map[string]any{"region": "use", "system_team_id": system, "epoch": epoch, "cohort_contract": 1, "assembled_sha": "1111111111111111111111111111111111111111", "schema_sha": "1111111111111111111111111111111111111111", "writers": []map[string]any{
		{"role": "api", "cohort_contract": 1, "verified": true, "source_sha": "1111111111111111111111111111111111111111", "reference": "disposable-test-api", "digest": "local-test"},
		{"role": "workers", "cohort_contract": 1, "verified": true, "source_sha": "1111111111111111111111111111111111111111", "reference": "disposable-test-worker", "digest": "local-test"},
		{"role": "console", "cohort_contract": 1, "verified": true, "source_sha": "2222222222222222222222222222222222222222", "reference": "disposable-test-ui", "digest": "local-test"}}})
	err := pool.QueryRow(t.Context(), `SELECT billing_release_new_team_gate('use',$1,$2,'local-test','verified test-only writers',$3)`, system, epoch, evidence).Scan(&epoch)
	return epoch, err
}

func cohortCreate(t *testing.T, pool *pgxpool.Pool, actor uuid.UUID) uuid.UUID {
	t.Helper()
	var team uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM create_team_with_signup_trial($1,$2,'use','first_team')`, "cohort-"+uuid.NewString(), actor).Scan(&team); err != nil {
		t.Fatal(err)
	}
	return team
}

func TestIntegration_NewTeamCohortPreservesOldAndRotatesAdmission(t *testing.T) {
	pool, system, actor := cohortFixture(t)
	old := uuid.New()
	rolloutExec(t, pool, `INSERT INTO team(id,name) VALUES($1,'old-legacy')`, old)
	rolloutExec(t, pool, `INSERT INTO team_billing_account(team_id,checkout_may_exist) VALUES($1,true)`, old)
	var before []byte
	if err := pool.QueryRow(t.Context(), `SELECT jsonb_build_object('team',to_jsonb(t),'account',to_jsonb(a)) FROM team t JOIN team_billing_account a ON a.team_id=t.id WHERE t.id=$1`, old).Scan(&before); err != nil {
		t.Fatal(err)
	}
	epoch := cohortRelease(t, pool, system)
	team := cohortCreate(t, pool, actor)
	var model, source string
	var open bool
	var current, enrolled *uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT * FROM billing_team_pricing_state($1)`, team).Scan(&model, &source, &open, &current, &enrolled); err != nil {
		t.Fatal(err)
	}
	if model != "v2" || source != "new_team" || !open || current == nil || *current != epoch || enrolled == nil || *enrolled != epoch {
		t.Fatalf("wrong new state %s %s %v %v %v", model, source, open, current, enrolled)
	}
	var reason string
	var plans, count int
	var pending, eligible bool
	if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM team_plan_assignment WHERE team_id=$1),o.reason,billing_team_pending_payment($1),team_sandbox_billing_eligible($1) FROM team_signup_promotion_outcome o WHERE o.team_id=$1`, team).Scan(&plans, &reason, &pending, &eligible); err != nil {
		t.Fatal(err)
	}
	if plans != 0 || reason != "pricing_v2" || !pending || eligible {
		t.Fatalf("new team was not pending without compute: plans=%d reason=%s pending=%v eligible=%v", plans, reason, pending, eligible)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM team_credit_grant WHERE team_id=$1`, team).Scan(&count); err != nil || count != 0 {
		t.Fatalf("signup granted legacy credit: %d %v", count, err)
	}
	rolloutExec(t, pool, `SELECT billing_close_new_team_gate($1,'local-test','hold fresh entry')`, epoch)
	if err := pool.QueryRow(t.Context(), `SELECT * FROM billing_team_pricing_state($1)`, team).Scan(&model, &source, &open, &current, &enrolled); err != nil {
		t.Fatal(err)
	}
	if model != "v2" || open || *current == epoch || *enrolled != epoch {
		t.Fatalf("closed state lost enrollment: %s/%v/%v/%v", model, open, current, enrolled)
	}
	second := cohortRelease(t, pool, system)
	if err := pool.QueryRow(t.Context(), `SELECT * FROM billing_team_pricing_state($1)`, team).Scan(&model, &source, &open, &current, &enrolled); err != nil {
		t.Fatal(err)
	}
	if !open || *current != second || *enrolled != epoch {
		t.Fatal("reopen orphaned/rewrote original cohort")
	}
	if err := pool.QueryRow(t.Context(), `SELECT * FROM billing_team_pricing_state($1)`, old).Scan(&model, &source, &open, &current, &enrolled); err != nil {
		t.Fatal(err)
	}
	if model != "legacy" || source != "legacy" || !open || current != nil || enrolled != nil {
		t.Fatal("public gate adopted an old team")
	}
	var after []byte
	if err := pool.QueryRow(t.Context(), `SELECT jsonb_build_object('team',to_jsonb(t),'account',to_jsonb(a)) FROM team t JOIN team_billing_account a ON a.team_id=t.id WHERE t.id=$1`, old).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("old access or checkout history changed")
	}
}

func TestIntegration_NewTeamCohortRejectsUntrustedCreationAndOldWriters(t *testing.T) {
	pool, system, actor := cohortFixture(t)
	epoch := cohortRelease(t, pool, system)
	team := cohortCreate(t, pool, actor)
	reject := func(sql, code string, args ...any) {
		t.Helper()
		_, err := pool.Exec(t.Context(), sql, args...)
		if pgCode(err) != code {
			t.Fatalf("wanted %s, got %v for %s", code, err, sql)
		}
	}
	reject(`INSERT INTO team(id,name,created_at) VALUES($1,'backdated-import','2020-01-01')`, "55000", uuid.New())
	reject(`SELECT id FROM create_team_with_signup_trial('foreign-home',$1,'usw','first_team')`, "55000", actor)
	reject(`UPDATE billing_new_team_enrollment SET enrollment_epoch=gen_random_uuid() WHERE team_id=$1`, "P0001", team)
	reject(`DELETE FROM billing_new_team_enrollment WHERE team_id=$1`, "P0001", team)
	// Even a prior API replica cannot durably accept a legacy metered lease.
	rolloutExec(t, pool, `INSERT INTO team_billing_account(team_id) VALUES($1)`, team)
	reject(`UPDATE team_billing_account SET checkout_initializing_at=clock_timestamp(),checkout_pending_attempt_ids=ARRAY[$2::uuid] WHERE team_id=$1`, "55000", team, uuid.New())
	// New consent is stamped, unchanged replay survives close, disable survives,
	// and a later re-enable is fresh even when its amount/actor are unchanged.
	rolloutExec(t, pool, `INSERT INTO billing_prepaid_auto_recharge(team_id,enabled,threshold_cents,amount_cents,updated_by,admission_source,admission_epoch) VALUES($1,true,500,500,$2,'new_team',$3)`, team, actor, epoch)
	rolloutExec(t, pool, `SELECT billing_close_new_team_gate($1,'local-test','test closed consent')`, epoch)
	rolloutExec(t, pool, `UPDATE billing_prepaid_auto_recharge SET enabled=true WHERE team_id=$1`, team)
	rolloutExec(t, pool, `UPDATE billing_prepaid_auto_recharge SET enabled=false WHERE team_id=$1`, team)
	reject(`UPDATE billing_prepaid_auto_recharge SET enabled=true WHERE team_id=$1`, "55000", team)
	var forbidden bool
	if err := pool.QueryRow(t.Context(), `SELECT has_table_privilege('service_role','billing_new_team_enrollment','INSERT') OR has_function_privilege('service_role','billing_insert_signup_team(uuid,text,uuid,text,text)','EXECUTE') OR has_function_privilege('authenticated','billing_team_pricing_state(uuid)','EXECUTE')`).Scan(&forbidden); err != nil || forbidden {
		t.Fatalf("cohort authority ACL leaked: %v/%v", forbidden, err)
	}
}

func TestIntegration_NewTeamCohortCreationHoldsGateAndRollsBackAtomically(t *testing.T) {
	pool, system, actor := cohortFixture(t)
	epoch := cohortRelease(t, pool, system)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var team uuid.UUID
	if err = tx.QueryRow(t.Context(), `SELECT id FROM create_team_with_signup_trial('cohort-rollback',$1,'use','first_team')`, actor).Scan(&team); err != nil {
		t.Fatal(err)
	}
	closeTx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer closeTx.Rollback(context.Background())
	rolloutExec(t, closeTx, `SET LOCAL lock_timeout='100ms'`)
	if _, err = closeTx.Exec(t.Context(), `SELECT billing_close_new_team_gate($1,'local-test','race')`, epoch); pgCode(err) != "55P03" {
		t.Fatalf("gate close escaped in-flight enrollment: %v", err)
	}
	_ = closeTx.Rollback(t.Context())
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM team WHERE id=$1)+(SELECT count(*) FROM billing_new_team_enrollment WHERE team_id=$1)+(SELECT count(*) FROM team_plan_assignment WHERE team_id=$1)`, team).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial signup committed: %d %v", count, err)
	}
	// A direct historical import is fenced by the same gate lock, not its date.
	importTx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer importTx.Rollback(context.Background())
	id := uuid.New()
	rolloutExec(t, importTx, `SELECT set_config('superserve.migrating_team_id',$1,true)`, id.String())
	if _, err = importTx.Exec(t.Context(), `INSERT INTO team(id,name) VALUES($1,'import')`, id); pgCode(err) != "55000" {
		t.Fatalf("historical import enrolled: %v", err)
	}
}

func TestIntegration_NewTeamCohortServiceRPCAndDeferredIntegrity(t *testing.T) {
	pool, system, actor := cohortFixture(t)
	cohortRelease(t, pool, system)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	rolloutExec(t, tx, `SET LOCAL ROLE service_role`)
	var team uuid.UUID
	if err = tx.QueryRow(t.Context(), `SELECT id FROM create_team_with_signup_trial('service-created',$1,'use','first_team')`, actor).Scan(&team); err != nil {
		t.Fatalf("trusted RPC cannot invoke protected helper: %v", err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatalf("actual enrollment failed deferred validation: %v", err)
	}
	bad, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Rollback(context.Background())
	rolloutExec(t, bad, `INSERT INTO billing_new_team_enrollment(team_id,region,enrollment_epoch,creator_user_id,source)
 SELECT gen_random_uuid(),'use',enrollment_epoch,creator_user_id,'trusted_signup' FROM billing_new_team_enrollment WHERE team_id=$1`, team)
	if err = bad.Commit(t.Context()); pgCode(err) != "23503" && pgCode(err) != "23514" {
		t.Fatalf("orphan enrollment committed: %v", err)
	}
}

func TestIntegration_NewTeamCohortPaidFirstNeedsSellableSubscriptions(t *testing.T) {
	pool, system, actor := cohortFixture(t)
	retire := `SELECT retire_billing_plan_version((SELECT id FROM billing_plan_version WHERE family=$1 AND pricing_version='v2' AND state='published'),NULL)`
	rolloutExec(t, pool, retire, "developer")
	if _, err := cohortTryRelease(t, pool, system); pgCode(err) != "55000" {
		t.Fatalf("gate opened without a sellable Developer plan: %v", err)
	}
	cohortPublishSubscription(t, pool, "developer")
	cohortRelease(t, pool, system)
	rolloutExec(t, pool, retire, "developer")
	onFree := cohortCreate(t, pool, actor)
	var kind string
	var pending bool
	if err := pool.QueryRow(t.Context(), `SELECT a.kind,billing_team_pending_payment($1) FROM team_plan_assignment a WHERE a.team_id=$1 AND a.effective_to IS NULL`, onFree).Scan(&kind, &pending); err != nil || kind != "free" || pending {
		t.Fatalf("signup without a sellable Developer plan was not kept on Free: %s %v %v", kind, pending, err)
	}
	if _, err := pool.Exec(t.Context(), retire, "free"); pgCode(err) != "55000" {
		t.Fatalf("Free retired while signups still fall back to it: %v", err)
	}
	cohortPublishSubscription(t, pool, "developer")
	rolloutExec(t, pool, retire, "free")
	if !cohortPending(t, pool, cohortCreate(t, pool, actor)) {
		t.Fatal("signup with sellable plans and no Free was not pending")
	}
}

func cohortPending(t *testing.T, pool *pgxpool.Pool, team uuid.UUID) bool {
	t.Helper()
	var pending bool
	if err := pool.QueryRow(t.Context(), `SELECT billing_team_pending_payment($1)`, team).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	return pending
}

//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/superserve-ai/sandbox/internal/api"
	"github.com/superserve-ai/sandbox/internal/db"
	"github.com/superserve-ai/sandbox/internal/integrationdb"
)

func isolatedFreeTemplatePolicy(t *testing.T) {
	t.Helper()
	admin, oldQueries, oldSystem := testPool, testQueries, testSystemTeamID
	name := "free_policy_" + uuid.NewString()[:8] + "_test"
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	cfg := admin.Config().Copy()
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	system, err := integrationdb.Reset(t.Context(), pool)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	testPool, testQueries, testSystemTeamID = pool, db.New(pool), system
	t.Cleanup(func() {
		testPool, testQueries, testSystemTeamID = admin, oldQueries, oldSystem
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
}

func prepareFreeTemplatePolicy(t *testing.T) {
	t.Helper()
	setPricingSwitch(t, "v2")
	a := api.NewPricingV2Activation(testPool, nil, "use", testSystemTeamID.String())
	if _, err := a.PrepareAdoption(t.Context(), "operator@example.com", "disposable policy test"); err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_FreeTemplatePolicy_PreservesFrozenUsageAndHistory(t *testing.T) {
	isolatedFreeTemplatePolicy(t)
	team, _ := seedTeamAndKey(t)
	assignFreePlan(t, team, time.Now().Add(-5*time.Hour))
	var predecessor uuid.UUID
	var frozen []byte
	if err := testPool.QueryRow(t.Context(), `SELECT id,to_jsonb(a)-'effective_to' FROM team_plan_assignment a WHERE team_id=$1 AND effective_to IS NULL`, team).Scan(&predecessor, &frozen); err != nil {
		t.Fatal(err)
	}
	sandbox := seedActiveSandbox(t, team, "retained-custom")
	ended := time.Now().Add(-time.Hour)
	seedComputeInterval(t, team, sandbox, 1, 1024, time.Now().Add(-4*time.Hour), &ended)
	if blocked, err := testQueries.RefreshFreeAllowance(t.Context(), team); err != nil || !blocked {
		t.Fatalf("initial allowance cap=%v: %v", blocked, err)
	}
	for _, entry := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO team_credit_grant(team_id,amount_usd,remaining_usd,reason) VALUES($1,25,5,'retained credit')`, []any{team}},
		{`INSERT INTO team_billing_account(team_id,stripe_customer_id) VALUES($1,$2)`, []any{team, "cus_" + team.String()}},
		{`INSERT INTO billing_pricing_adoption_credit_resolution(team_id,assignment_id,source) SELECT $1,$2,billing_pricing_adoption_credit_source($1)`, []any{team, predecessor}},
	} {
		if _, err := testPool.Exec(t.Context(), entry.sql, entry.args...); err != nil {
			t.Fatal(err)
		}
	}
	var allowance, credit []byte
	if err := testPool.QueryRow(t.Context(), `SELECT to_jsonb(a) FROM billing_free_allowance a WHERE team_id=$1`, team).Scan(&allowance); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM team_credit_grant c WHERE team_id=$1`, team).Scan(&credit); err != nil {
		t.Fatal(err)
	}
	prepareFreeTemplatePolicy(t)
	var successor uuid.UUID
	if err := testPool.QueryRow(t.Context(), `SELECT billing_migrate_free_template_assignment('use',$3,$1,$2,'operator@example.com','no custom templates')`, team, predecessor, testSystemTeamID).Scan(&successor); err != nil {
		t.Fatal(err)
	}
	if successor == predecessor {
		t.Fatal("did not append a successor")
	}
	var oldAfter, allowanceAfter, creditAfter []byte
	var origin, oldStart time.Time
	var matching, paired bool
	if err := testPool.QueryRow(t.Context(), `SELECT to_jsonb(a)-'effective_to',effective_from FROM team_plan_assignment a WHERE id=$1`, predecessor).Scan(&oldAfter, &oldStart); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frozen, oldAfter) {
		t.Fatal("historical frozen assignment changed")
	}
	if err := testPool.QueryRow(t.Context(), `SELECT free_allowance_origin_at,limits->'max_templates'='0'::jsonb AND (terms-'version_id'-'version')=(SELECT terms-'version_id'-'version' FROM team_plan_assignment WHERE id=$2),effective_from=(SELECT effective_to FROM team_plan_assignment WHERE id=$2) FROM team_plan_assignment WHERE id=$1`, successor, predecessor).Scan(&origin, &matching, &paired); err != nil {
		t.Fatal(err)
	}
	if !origin.Equal(oldStart) || !matching || !paired {
		t.Fatalf("origin=%s old=%s frozen=%v boundary=%v", origin, oldStart, matching, paired)
	}
	if err := testPool.QueryRow(t.Context(), `SELECT to_jsonb(a) FROM billing_free_allowance a WHERE team_id=$1`, team).Scan(&allowanceAfter); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM team_credit_grant c WHERE team_id=$1`, team).Scan(&creditAfter); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(allowance, allowanceAfter) || !bytes.Equal(credit, creditAfter) {
		t.Fatal("stored allowance/credit changed during policy transition")
	}
	var blocked bool
	if err := testPool.QueryRow(t.Context(), `SELECT billing_free_allowance_blocked_until($1) IS NOT NULL`, team).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("lost exhausted window: %v %v", blocked, err)
	}
	var resolution uuid.UUID
	if err := testPool.QueryRow(t.Context(), `SELECT assignment_id FROM billing_pricing_adoption_credit_resolution WHERE team_id=$1`, team).Scan(&resolution); err != nil || resolution != successor {
		t.Fatalf("credit authority link=%s: %v", resolution, err)
	}
	if _, err := testQueries.GetSandbox(t.Context(), db.GetSandboxParams{ID: sandbox, TeamID: team}); err != nil {
		t.Fatalf("retained sandbox lost: %v", err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE billing_pricing_switch SET pricing_version='legacy'`); err != nil {
		t.Fatal(err)
	}
	var replay uuid.UUID
	if err := testPool.QueryRow(t.Context(), `SELECT billing_migrate_free_template_assignment('use',$3,$1,$2,'operator@example.com','replay')`, team, predecessor, testSystemTeamID).Scan(&replay); err != nil || replay != successor {
		t.Fatalf("OFF replay=%s %v", replay, err)
	}
}

func TestIntegration_FreeTemplatePolicy_FencesOwnershipAndRollsBackFaults(t *testing.T) {
	isolatedFreeTemplatePolicy(t)
	team, _ := seedTeamAndKey(t)
	assignFreePlan(t, team, time.Now().Add(-time.Hour))
	var predecessor uuid.UUID
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM team_plan_assignment WHERE team_id=$1 AND effective_to IS NULL`, team).Scan(&predecessor); err != nil {
		t.Fatal(err)
	}
	prepareFreeTemplatePolicy(t)
	apply := `SELECT billing_migrate_free_template_assignment('use','` + testSystemTeamID.String() + `',$1,$2,'operator@example.com','policy')`
	for _, scope := range []struct {
		region string
		system uuid.UUID
	}{{"usw", testSystemTeamID}, {"use", uuid.New()}} {
		_, err := testPool.Exec(t.Context(), `SELECT billing_migrate_free_template_assignment($1,$2,$3,$4,'operator@example.com','wrong context')`, scope.region, scope.system, team, predecessor)
		if pgCode(err) != "55000" {
			t.Fatalf("mismatching region/system accepted: %v", err)
		}
	}

	if _, err := testPool.Exec(t.Context(), apply, team, uuid.New()); pgCode(err) != "55000" {
		t.Fatalf("stale identity: %v", err)
	}
	sandbox := seedActiveSandbox(t, team, "unsettled-policy")
	if _, err := testQueries.RequestTeamPauseAll(t.Context(), db.RequestTeamPauseAllParams{TeamID: team, Reason: "plan_switch_free"}); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), apply, team, predecessor); pgCode(err) != "55000" {
		t.Fatalf("unsettled pause crossed assignment boundary: %v", err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE sandbox SET status='paused' WHERE id=$1`, sandbox); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE billing_team_pause_target SET done_at=clock_timestamp() WHERE team_id=$1`, team); err != nil {
		t.Fatal(err)
	}
	holder, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(t.Context(), `SELECT id FROM team WHERE id=$1 FOR UPDATE`, team); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), apply, team, predecessor); pgCode(err) != "55P03" {
		t.Fatalf("transition waited on team ownership: %v", err)
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `CREATE FUNCTION free_policy_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'disposable audit fault'; END $$; CREATE TRIGGER free_policy_fault BEFORE INSERT ON billing_free_template_policy_transition FOR EACH ROW EXECUTE FUNCTION free_policy_fault()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DROP TRIGGER IF EXISTS free_policy_fault ON billing_free_template_policy_transition; DROP FUNCTION IF EXISTS free_policy_fault()`)
	})
	if _, err := testPool.Exec(t.Context(), apply, team, predecessor); pgCode(err) != "P0001" {
		t.Fatalf("audit failure: %v", err)
	}
	var current uuid.UUID
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM team_plan_assignment WHERE team_id=$1 AND effective_to IS NULL`, team).Scan(&current); err != nil || current != predecessor {
		t.Fatalf("partial history mutation=%s: %v", current, err)
	}
	denied, err := testPool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer denied.Rollback(context.Background())
	if _, err := denied.Exec(t.Context(), `SET LOCAL ROLE service_role`); err != nil {
		t.Fatal(err)
	}
	if _, err := denied.Exec(t.Context(), apply, team, predecessor); pgCode(err) != "42501" {
		t.Fatalf("service role gained human transition: %v", err)
	}
}

func TestIntegration_FreeTemplatePolicy_ReleaseRejectsCommittedOldFreeTarget(t *testing.T) {
	isolatedFreeTemplatePolicy(t)
	team, _ := seedTeamAndKey(t)
	assignFreePlan(t, team, time.Now().Add(-time.Hour))
	var predecessor uuid.UUID
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM team_plan_assignment WHERE team_id=$1 AND effective_to IS NULL`, team).Scan(&predecessor); err != nil {
		t.Fatal(err)
	}
	prepareFreeTemplatePolicy(t)
	var epoch uuid.UUID
	if err := testPool.QueryRow(t.Context(), `SELECT epoch FROM billing_pricing_adoption WHERE singleton`).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	evidence, err := json.Marshal(map[string]any{"epoch": epoch.String(), "region": "use", "system_team_id": testSystemTeamID.String(), "assembled_sha": "1111111111111111111111111111111111111111", "schema_sha": "1111111111111111111111111111111111111111", "worker_sha": "1111111111111111111111111111111111111111", "verified_worker": true, "deployment_reference": "local-fixture", "worker_reference": "local-fixture", "free_template_policy": map[string]any{"existing_assignments_verified": true, "custom_policy_reference": "local-fixture", "runtime_reference": "local-fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	release := `SELECT billing_release_pricing_adoption('use',$1,$2,'operator@example.com','local fixture',$3::jsonb)`
	if _, err := testPool.Exec(t.Context(), release, testSystemTeamID, epoch, evidence); pgCode(err) != "55000" {
		t.Fatalf("all assigned concealed old Free10: %v", err)
	}
	if _, err := testPool.Exec(t.Context(), `SELECT billing_migrate_free_template_assignment('use',$1,$2,$3,'operator@example.com','policy')`, testSystemTeamID, team, predecessor); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `CREATE TABLE team_plan_change(team_id uuid,state text,target_kind text,target_limits jsonb)`); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO team_plan_change VALUES($1,'scheduled','free','{"max_concurrent":1,"max_templates":10}')`, team); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), release, testSystemTeamID, epoch, evidence); pgCode(err) != "55000" {
		t.Fatalf("old committed target escaped current-Free0 scan: %v", err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE team_plan_change SET target_limits='{"max_concurrent":1,"max_templates":0}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), release, testSystemTeamID, epoch, evidence); err != nil {
		t.Fatalf("resolved Free0 fixture still blocked: %v", err)
	}
}

func TestIntegration_FreeTemplatePolicy_CLIContextAndReplay(t *testing.T) {
	isolatedFreeTemplatePolicy(t)
	team, _ := seedTeamAndKey(t)
	assignFreePlan(t, team, time.Now().Add(-time.Hour))
	var predecessor uuid.UUID
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM team_plan_assignment WHERE team_id=$1 AND effective_to IS NULL`, team).Scan(&predecessor); err != nil {
		t.Fatal(err)
	}
	prepareFreeTemplatePolicy(t)
	database := testPool.Config().ConnConfig.Database
	connection, err := url.Parse(testPool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	connection.Path = "/" + database
	if _, err := testPool.Exec(t.Context(), `ALTER DATABASE `+pgx.Identifier{database}.Sanitize()+` SET default_transaction_isolation='serializable'`); err != nil {
		t.Fatal(err)
	}
	cli := func(region, system string, fails bool, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "go", append([]string{"run", "../../cmd/pricing-v2-migrate"}, args...)...)
		cmd.Env = append(os.Environ(), "DATABASE_URL="+connection.String(), "TEAM_CREATION_REGION="+region, "SYSTEM_TEAM_ID="+system, "STRIPE_SECRET_KEY=")
		var output, stderr bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &stderr
		err := cmd.Run()
		if (err != nil) != fails {
			t.Fatalf("CLI failure=%t: %v stdout=%s stderr=%s", fails, err, output.String(), stderr.String())
		}
		return output.Bytes()
	}
	args := []string{"policy-apply", "--confirm", "--actor", "operator@example.com", "--reason", "local policy fixture", "--team", team.String(), "--expected-assignment", predecessor.String()}
	cli("usw", testSystemTeamID.String(), true, args...)
	cli("use", uuid.NewString(), true, args...)
	var current uuid.UUID
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM team_plan_assignment WHERE team_id=$1 AND effective_to IS NULL`, team).Scan(&current); err != nil || current != predecessor {
		t.Fatalf("wrong-context command mutated assignment=%s %v", current, err)
	}
	var inventory struct {
		Assignments []struct {
			NeedsSuccessor bool `json:"needs_successor"`
		} `json:"assignments"`
	}
	if err := json.Unmarshal(cli("use", testSystemTeamID.String(), false, "policy-report"), &inventory); err != nil || len(inventory.Assignments) != 1 || !inventory.Assignments[0].NeedsSuccessor {
		t.Fatalf("policy inventory=%+v %v", inventory, err)
	}
	var result struct {
		AssignmentID uuid.UUID `json:"assignment_id"`
	}
	if err := json.Unmarshal(cli("use", testSystemTeamID.String(), false, args...), &result); err != nil || result.AssignmentID == uuid.Nil || result.AssignmentID == predecessor {
		t.Fatalf("CLI transition=%s %v", result.AssignmentID, err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE billing_pricing_switch SET pricing_version='legacy'`); err != nil {
		t.Fatal(err)
	}
	var replay struct {
		AssignmentID uuid.UUID `json:"assignment_id"`
	}
	if err := json.Unmarshal(cli("use", testSystemTeamID.String(), false, args...), &replay); err != nil || replay.AssignmentID != result.AssignmentID {
		t.Fatalf("OFF CLI replay=%s %v", replay.AssignmentID, err)
	}
}

func TestIntegration_FreeTemplatePolicy_PartialMigrationCannotRelease(t *testing.T) {
	isolatedFreeTemplatePolicy(t)
	prepareFreeTemplatePolicy(t)
	migration, err := os.ReadFile(filepath.Join("..", "..", "supabase", "migrations", "20261025130000_free_template_policy.sql"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, name := range []string{"guard_free_template_policy_release", "billing_pricing_adoption_evidence_valid"} {
		start := strings.Index(string(migration), "CREATE OR REPLACE FUNCTION "+name+"(")
		if start < 0 {
			t.Fatalf("missing migration function %s", name)
		}
		definition := string(migration)[start:]
		end := strings.Index(definition, "$$;")
		if end < 0 {
			t.Fatalf("unterminated migration function %s", name)
		}
		if _, err := tx.Exec(t.Context(), definition[:end+3]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(t.Context(), `ALTER TABLE sandbox DROP COLUMN system_template; ALTER TABLE sandbox_snapshot DROP COLUMN system_template`); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := tx.QueryRow(t.Context(), `SELECT billing_pricing_adoption_evidence_valid(region,system_team_id,epoch,'{}'::jsonb) FROM billing_pricing_adoption WHERE singleton`).Scan(&valid); err != nil || valid {
		t.Fatalf("partial evidence valid=%v err=%v", valid, err)
	}
	_, err = tx.Exec(t.Context(), `UPDATE billing_pricing_adoption SET state='released' WHERE singleton`)
	if err == nil || !strings.Contains(err.Error(), "Free template enforcement migration must complete before release") || !strings.Contains(err.Error(), "55000") {
		t.Fatalf("partial migration release: %v", err)
	}
}

//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Every curated template ships at 2 vCPU / 4096 MiB / 8192 MiB, so the plans
// that sat below that shape moved up to it. The catalog rows are addressed by
// their deterministic ids so later catalog tests cannot disturb the check.
func TestFreeShapeTwoVCPUCatalog(t *testing.T) {
	ctx := t.Context()
	var vcpu, mem, disk, templates, allowance int
	var previous string
	if err := testPool.QueryRow(ctx, `SELECT v.max_template_vcpu,v.max_template_memory_mib,v.max_template_disk_mib,v.max_templates,v.allowance_compute_seconds,p.state
	 FROM billing_plan_version v JOIN billing_plan_version p ON p.id=md5('billing-plan-version:v2:free:1')::uuid
	 WHERE v.id=md5('billing-plan-version:v2:free:2')::uuid`).Scan(&vcpu, &mem, &disk, &templates, &allowance, &previous); err != nil {
		t.Fatal(err)
	}
	if vcpu != 2 || mem != 4096 || disk != 8192 {
		t.Fatalf("v2 Free version 2 caps = %d/%d/%d, want 2/4096/8192", vcpu, mem, disk)
	}
	if templates != 10 || allowance != 7200 || previous != "retired" {
		t.Fatalf("v2 Free version 2 must copy its predecessor's terms and retire it: templates=%d allowance=%ds previous=%s", templates, allowance, previous)
	}
	var promisesHours bool
	if err := testPool.QueryRow(ctx, `SELECT '2 hours of compute every day'=ANY(features) FROM billing_plan_version WHERE id=md5('billing-plan-version:v2:free:2')::uuid`).Scan(&promisesHours); err != nil || promisesHours {
		t.Fatalf("the allowance no longer lasts two hours at the new shape; the card must not promise it (err=%v)", err)
	}
	if err := testPool.QueryRow(ctx, `SELECT v.max_template_vcpu,v.max_template_memory_mib,v.max_template_disk_mib,p.state
	 FROM billing_plan_version v JOIN billing_plan_version p ON p.id=md5('billing-plan-version:legacy:payg:1')::uuid
	 WHERE v.id=md5('billing-plan-version:legacy:payg:2')::uuid`).Scan(&vcpu, &mem, &disk, &previous); err != nil {
		t.Fatal(err)
	}
	if vcpu != 2 || mem != 4096 || disk != 8192 || previous != "retired" {
		t.Fatalf("legacy payg version 2 caps = %d/%d/%d previous=%s, want 2/4096/8192 with version 1 retired", vcpu, mem, disk, previous)
	}
}

// A team that signed up before the shape change carries the old caps frozen
// in its open Free assignment. Re-running the migration moves it: the old
// assignment closes, a replacement opens on the published Free version with
// the new caps and the same allowance floor, and admission accepts the
// curated template shape. A second run changes nothing.
func TestFreeShapeTwoVCPUTransition(t *testing.T) {
	ctx := t.Context()
	migration, err := os.ReadFile("../../supabase/migrations/20261028003000_free_shape_two_vcpu.sql")
	if err != nil {
		t.Fatal(err)
	}
	team := uuid.New()
	if _, err := testPool.Exec(ctx, `INSERT INTO team(id,name) VALUES($1,$2)`, team, "free-shape-"+team.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM team WHERE id=$1`, team) })
	if _, err := testPool.Exec(ctx, `UPDATE team_plan_assignment SET effective_to=clock_timestamp() WHERE team_id=$1 AND effective_to IS NULL`, team); err != nil {
		t.Fatal(err)
	}
	signedUp := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond) // timestamptz precision
	var old uuid.UUID
	if err := testPool.QueryRow(ctx, `INSERT INTO team_plan_assignment(team_id,plan_key,kind,plan_version_id,terms,limits,effective_from,source,reason)
	 VALUES($1,'free','free',md5('billing-plan-version:v2:free:1')::uuid,
	  '{"pricing_version":"v2","version":1,"allowance":{"period":"day_window","on_exhausted":"pause","amount_cents":0,"compute":{"seconds":7200,"vcpu":1,"memory_gib":1}}}',
	  '{"max_concurrent":1,"max_vcpu":1,"max_memory_mib":1024,"max_disk_mib":4096,"max_templates":10}',
	  $2,'signup','new team') RETURNING id`, team, signedUp).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if code := admitShape(t, team, 2, 4096, 8192); code != "SS002" {
		t.Fatalf("old Free caps must reject the curated shape, got %q", code)
	}

	if _, err := testPool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}

	var closed bool
	var current uuid.UUID
	var vcpu, mem, disk, concurrent int
	var floor time.Time
	var key string
	if err := testPool.QueryRow(ctx, `SELECT o.effective_to IS NOT NULL,n.plan_version_id,
	  (n.limits->>'max_vcpu')::int,(n.limits->>'max_memory_mib')::int,(n.limits->>'max_disk_mib')::int,(n.limits->>'max_concurrent')::int,
	  n.free_allowance_since,n.idempotency_key
	 FROM team_plan_assignment o JOIN team_plan_assignment n ON n.team_id=o.team_id AND n.effective_to IS NULL
	 WHERE o.id=$1`, old).Scan(&closed, &current, &vcpu, &mem, &disk, &concurrent, &floor, &key); err != nil {
		t.Fatal(err)
	}
	var published uuid.UUID
	if err := testPool.QueryRow(ctx, `SELECT id FROM billing_plan_version WHERE family='free' AND pricing_version='v2' AND state='published'`).Scan(&published); err != nil {
		t.Fatal(err)
	}
	if !closed || current != published || vcpu < 2 || mem < 4096 || disk < 8192 || concurrent != 1 {
		t.Fatalf("replacement assignment: closed=%v version=%s (published %s) caps=%d/%d/%d concurrent=%d", closed, current, published, vcpu, mem, disk, concurrent)
	}
	if !floor.Equal(signedUp) || key != "free-shape-2-4-8:"+old.String() {
		t.Fatalf("replacement must keep the allowance floor %s and be idempotent: floor=%s key=%s", signedUp, floor, key)
	}
	var teamVCPU, teamMem, teamDisk int
	if err := testPool.QueryRow(ctx, `SELECT max_template_vcpu,max_template_memory_mib,max_template_disk_mib FROM team WHERE id=$1`, team).Scan(&teamVCPU, &teamMem, &teamDisk); err != nil {
		t.Fatal(err)
	}
	if teamVCPU < 2 || teamMem < 4096 || teamDisk < 8192 {
		t.Fatalf("team row caps = %d/%d/%d, want at least 2/4096/8192", teamVCPU, teamMem, teamDisk)
	}
	if code := admitShape(t, team, 2, 4096, 8192); code != "" {
		t.Fatalf("curated shape must be admitted on the new caps, got %q", code)
	}
	if code := admitShape(t, team, 4, 4096, 8192); code != "SS002" {
		t.Fatalf("caps still apply above the curated shape, got %q", code)
	}

	if _, err := testPool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	var assignments int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM team_plan_assignment WHERE team_id=$1`, team).Scan(&assignments); err != nil {
		t.Fatal(err)
	}
	if assignments != 2 {
		t.Fatalf("second run must be a no-op, team has %d assignments", assignments)
	}
}

// A cap the frozen limits leave out is no cap, so the raise neither selects
// such an assignment nor writes a floor over the missing key, and it never
// lowers a cap already above the shape.
func TestFreeShapeTwoVCPUKeepsMissingCaps(t *testing.T) {
	ctx := t.Context()
	migration, err := os.ReadFile("../../supabase/migrations/20261028003000_free_shape_two_vcpu.sql")
	if err != nil {
		t.Fatal(err)
	}
	assign := func(limits string) uuid.UUID {
		t.Helper()
		team := uuid.New()
		if _, err := testPool.Exec(ctx, `INSERT INTO team(id,name) VALUES($1,$2)`, team, "free-shape-"+team.String()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM team WHERE id=$1`, team) })
		if _, err := testPool.Exec(ctx, `UPDATE team_plan_assignment SET effective_to=clock_timestamp() WHERE team_id=$1 AND effective_to IS NULL`, team); err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(ctx, `INSERT INTO team_plan_assignment(team_id,plan_key,kind,plan_version_id,terms,limits,effective_from,source,reason)
	 VALUES($1,'free','free',md5('billing-plan-version:v2:free:1')::uuid,'{"pricing_version":"v2","version":1}',$2::jsonb,clock_timestamp()-interval '1 hour','signup','new team')`, team, limits); err != nil {
			t.Fatal(err)
		}
		return team
	}
	uncapped := assign(`{"max_concurrent":1,"max_vcpu":null,"max_templates":0}`)
	partial := assign(`{"max_concurrent":1,"max_vcpu":1,"max_disk_mib":20000,"max_templates":0}`)
	if _, err := testPool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	var replaced bool
	if err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM team_plan_assignment WHERE team_id=$1 AND source='migration')`, uncapped).Scan(&replaced); err != nil || replaced {
		t.Fatalf("an uncapped assignment was replaced: %v %v", replaced, err)
	}
	var limits string
	if err := testPool.QueryRow(ctx, `SELECT limits::text FROM team_plan_assignment WHERE team_id=$1 AND effective_to IS NULL`, partial).Scan(&limits); err != nil {
		t.Fatal(err)
	}
	// Raise-only: a cap below the shape is raised, one above it is kept.
	if limits != `{"max_vcpu": 2, "max_disk_mib": 20000, "max_templates": 0, "max_concurrent": 1}` {
		t.Fatalf("raise lowered a cap or wrote floors over missing caps: %s", limits)
	}
}

// admitShape attempts a sandbox insert at the given shape inside a rolled-back
// transaction and returns the SQLSTATE that rejected it, or "" when admitted.
func admitShape(t *testing.T, team uuid.UUID, vcpu, memoryMiB, diskMiB int) string {
	t.Helper()
	ctx := t.Context()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `INSERT INTO sandbox(team_id,name,status,host_id,vcpu_count,memory_mib,disk_mib) VALUES($1,$2,'starting',$3,$4,$5,$6)`,
		team, "free-shape-"+uuid.NewString(), testDefaultHostID, vcpu, memoryMiB, diskMiB)
	if err == nil {
		return ""
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		t.Fatalf("sandbox insert failed without a SQLSTATE: %v", err)
	}
	return pe.Code
}

// A team mid-purchase keeps its assignment: the checkout snapshot names the
// open assignment and would refuse to activate against a replacement.
func TestFreeShapeTwoVCPUKeepsInFlightCheckout(t *testing.T) {
	ctx := t.Context()
	migration, err := os.ReadFile("../../supabase/migrations/20261028003000_free_shape_two_vcpu.sql")
	if err != nil {
		t.Fatal(err)
	}
	team := uuid.New()
	if _, err := testPool.Exec(ctx, `INSERT INTO team(id,name) VALUES($1,$2)`, team, "free-shape-checkout-"+team.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM team WHERE id=$1`, team) })
	if _, err := testPool.Exec(ctx, `UPDATE team_plan_assignment SET effective_to=clock_timestamp() WHERE team_id=$1 AND effective_to IS NULL`, team); err != nil {
		t.Fatal(err)
	}
	var old uuid.UUID
	if err := testPool.QueryRow(ctx, `INSERT INTO team_plan_assignment(team_id,plan_key,kind,plan_version_id,terms,limits,effective_from,source,reason)
	 VALUES($1,'free','free',md5('billing-plan-version:v2:free:1')::uuid,'{"pricing_version":"v2","version":1}',
	  '{"max_concurrent":1,"max_vcpu":1,"max_memory_mib":1024,"max_disk_mib":4096,"max_templates":10}',
	  clock_timestamp()-interval '1 hour','signup','new team') RETURNING id`, team).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO team_billing_account(team_id,checkout_may_exist) VALUES($1,true)`, team); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	var assignments int
	var stillOpen bool
	if err := testPool.QueryRow(ctx, `SELECT count(*),bool_or(id=$2 AND effective_to IS NULL) FROM team_plan_assignment WHERE team_id=$1`, team, old).Scan(&assignments, &stillOpen); err != nil {
		t.Fatal(err)
	}
	if assignments != 1 || !stillOpen {
		t.Fatalf("in-flight checkout team must keep its assignment: assignments=%d open=%v", assignments, stillOpen)
	}
	deferred := func() bool {
		t.Helper()
		var ok bool
		if err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_plan_shape_deferred WHERE team_id=$1)`, team).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !deferred() {
		t.Fatal("skipped team was not recorded for the sweep")
	}
	// Once the purchase settles, the sweep's call moves the team.
	if _, err := testPool.Exec(ctx, `UPDATE team_billing_account SET checkout_may_exist=false WHERE team_id=$1`, team); err != nil {
		t.Fatal(err)
	}
	var moved int
	if err := testPool.QueryRow(ctx, `SELECT moved FROM billing_raise_plan_shape(true)`).Scan(&moved); err != nil {
		t.Fatal(err)
	}
	var mem int
	if err := testPool.QueryRow(ctx, `SELECT (limits->>'max_memory_mib')::int FROM team_plan_assignment WHERE team_id=$1 AND effective_to IS NULL`, team).Scan(&mem); err != nil {
		t.Fatal(err)
	}
	if moved < 1 || mem < 4096 || deferred() {
		t.Fatalf("settled team must move on the sweep's next call: moved=%d memory=%d deferred=%v", moved, mem, deferred())
	}
}

// With Free retired (paid-first live), the raise keeps an assignment's frozen
// version keys instead of nulling them, and the migration still applies.
func TestFreeShapeTwoVCPUAfterFreeRetired(t *testing.T) {
	ctx := t.Context()
	migration, err := os.ReadFile("../../supabase/migrations/20261028003000_free_shape_two_vcpu.sql")
	if err != nil {
		t.Fatal(err)
	}
	pool, system, _ := cohortFixture(t)
	team := uuid.New()
	rolloutExec(t, pool, `INSERT INTO team(id,name) VALUES($1,$2)`, team, "free-shape-"+team.String())
	rolloutExec(t, pool, `UPDATE team_plan_assignment SET effective_to=clock_timestamp() WHERE team_id=$1 AND effective_to IS NULL`, team)
	rolloutExec(t, pool, `INSERT INTO team_plan_assignment(team_id,plan_key,kind,plan_version_id,terms,limits,effective_from,source,reason)
	 SELECT $1,'free','free',v.id,jsonb_build_object('pricing_version','v2','version_id',v.id,'version',v.version),'{"max_concurrent":1,"max_vcpu":1,"max_templates":0}',clock_timestamp()-interval '1 hour','signup','new team'
	 FROM billing_plan_version v WHERE v.family='free' AND v.pricing_version='v2' AND v.state='published'`, team)
	cohortRelease(t, pool, system)
	rolloutExec(t, pool, `UPDATE billing_trial_policy SET max_memory_mib=3072 WHERE singleton`)
	rolloutExec(t, pool, `SELECT retire_billing_plan_version((SELECT id FROM billing_plan_version WHERE family='free' AND pricing_version='v2' AND state='published'),NULL)`)
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal("migration refused a valid paid-first catalog without Free:", err)
	}
	var frozen bool
	if err := pool.QueryRow(ctx, `SELECT n.terms->'version_id'=o.terms->'version_id' AND n.terms->'version'=o.terms->'version' AND n.terms->>'version_id' IS NOT NULL
	 FROM team_plan_assignment n JOIN team_plan_assignment o ON o.team_id=n.team_id AND o.effective_to IS NOT NULL AND o.source='signup'
	 WHERE n.team_id=$1 AND n.effective_to IS NULL AND n.source='migration'`, team).Scan(&frozen); err != nil || !frozen {
		t.Fatal("raise without a published Free lost the frozen version keys", frozen, err)
	}
	var trialMemory int
	if err := pool.QueryRow(ctx, `SELECT max_memory_mib FROM billing_trial_policy WHERE singleton`).Scan(&trialMemory); err != nil || trialMemory != 4096 {
		t.Fatal("an undersized trial on a released cell was not raised to the shape", trialMemory, err)
	}
}

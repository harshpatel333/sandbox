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
}

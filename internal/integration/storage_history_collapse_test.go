//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The per-row artifact pipeline that 20261012000001 collapses for simple
// owners. Recreating it from its original migration keeps one copy of the
// expected semantics instead of a hand-maintained duplicate.
const perRowStorageMigration = "../../supabase/migrations/20261003010022_reconcile_retained_delta_upgrade.sql"

func TestIntegration_StorageHistoryCollapseMatchesPerRowPipeline(t *testing.T) {
	raw, err := os.ReadFile(perRowStorageMigration)
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	start := strings.Index(sql, "CREATE OR REPLACE FUNCTION storage_mib_seconds_segmented(")
	if start < 0 {
		t.Fatal("per-row storage function not found in its migration")
	}
	end := start + strings.Index(sql[start:], "$$;") + len("$$;")
	storageExec(t, strings.Replace(sql[start:end], "storage_mib_seconds_segmented(", "per_row_storage_mib_seconds(", 1))
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DROP FUNCTION IF EXISTS per_row_storage_mib_seconds(uuid,timestamptz,timestamptz,boolean)`)
	})

	now := time.Now().UTC()
	hostA, hostB := "collapse-a-"+uuid.NewString(), "collapse-b-"+uuid.NewString()
	measured, _ := seedTeamAndKey(t)
	cutover, _ := seedTeamAndKey(t)
	unmeasured, _ := seedTeamAndKey(t)
	storageExec(t, `INSERT INTO retained_storage_cutover(host_id,team_id,started_at) VALUES($1,$2,$3)`, hostA, cutover, now.Add(-6*24*time.Hour))

	template := func(team uuid.UUID, measure bool) uuid.UUID {
		id := uuid.New()
		storageExec(t, `INSERT INTO template(id,team_id,name,status,build_spec,vcpu,memory_mib,disk_mib,rootfs_path)
			VALUES($1,$2,'collapse','ready','{}',1,1024,1024,'/templates/'||$1::uuid::text||'/base.ext4')`, id, team)
		if measure {
			storageExec(t, `INSERT INTO artifact_manifest(template_id,file_name,path,size_bytes,allocated_bytes,sha256)
				VALUES($1,'base.ext4','/templates/'||$1::uuid::text||'/base.ext4',2147483648,1073741824,repeat('0',64)),
				      ($1,'delta.ext4','/templates/'||$1::uuid::text||'/delta.ext4',536870912,268435456,repeat('1',64))`, id)
		}
		return id
	}
	type span struct {
		host   string
		from   time.Duration // before now
		to     time.Duration // before now; 0 leaves the interval open
		reason string
	}
	sandbox := func(team, tpl uuid.UUID, paths bool, created, destroyed time.Duration, spans ...span) {
		id := uuid.New()
		var destroyedAt *time.Time
		status := "paused"
		if destroyed > 0 {
			at := now.Add(-destroyed)
			destroyedAt, status = &at, "deleted"
		}
		base, delta := "/templates/"+tpl.String()+"/base.ext4", "/templates/"+tpl.String()+"/delta.ext4"
		if !paths {
			base, delta = "", ""
		}
		storageExec(t, `INSERT INTO sandbox(id,team_id,name,status,vcpu_count,memory_mib,host_id,template_id,base_path,delta_path,created_at,destroyed_at)
			VALUES($1,$2,'collapse',$3::sandbox_status,1,1024,$4,$5,NULLIF($6,''),NULLIF($7,''),$8,$9)`,
			id, team, status, spans[len(spans)-1].host, tpl, base, delta, now.Add(-created), destroyedAt)
		for i, s := range spans {
			var endedAt *time.Time
			var reason *string
			if s.to > 0 {
				at := now.Add(-s.to)
				endedAt, reason = &at, &spans[i].reason
			}
			storageExec(t, `INSERT INTO sandbox_storage_interval(sandbox_id,team_id,host_id,disk_mib,started_at,ended_at,end_reason)
				VALUES($1,$2,$3,$4,$5,$6,$7)`, id, team, s.host, 256+64*i, now.Add(-s.from), endedAt, reason)
		}
	}
	day := 24 * time.Hour

	tpl := template(measured, true)
	// Live with a re-measured history: the rows the collapse folds together.
	sandbox(measured, tpl, true, 9*day, 0,
		span{hostA, 9 * day, 7 * day, "measurement"}, span{hostA, 7 * day, 3 * day, "measurement"},
		span{hostA, 3 * day, 2 * time.Hour, "measurement"}, span{hostA, 2 * time.Hour, 0, ""})
	// Destroyed inside most windows after one re-measurement.
	sandbox(measured, tpl, true, 5*day, 26*time.Hour,
		span{hostA, 5 * day, 2 * day, "measurement"}, span{hostA, 2 * day, 26 * time.Hour, "deleted"})
	// Moved hosts: stays on the per-row pipeline.
	sandbox(measured, tpl, true, 8*day, 0,
		span{hostA, 8 * day, 4 * day, "reassigned"}, span{hostB, 4 * day, 30 * time.Hour, "measurement"}, span{hostB, 30 * time.Hour, 0, ""})
	// Destroyed while its last interval was never closed.
	sandbox(measured, tpl, true, 4*day, 3*day, span{hostB, 4 * day, 0, ""})
	// Neither base nor delta: storage falls back to the template rootfs.
	sandbox(measured, tpl, false, 6*day, 0,
		span{hostB, 6 * day, day, "measurement"}, span{hostB, day, 0, ""})

	cutoverTpl := template(cutover, true)
	sandbox(cutover, cutoverTpl, true, 9*day, 0,
		span{hostA, 9 * day, 4 * day, "measurement"}, span{hostA, 4 * day, 0, ""})
	sandbox(cutover, cutoverTpl, true, 8*day, 0,
		span{hostB, 8 * day, 5 * day, "measurement"}, span{hostB, 5 * day, 0, ""})

	sandbox(unmeasured, template(unmeasured, false), true, 3*day, 0,
		span{hostA, 3 * day, day, "measurement"}, span{hostA, day, 0, ""})

	windows := [][2]time.Duration{
		{2 * time.Hour, time.Hour}, {27 * time.Hour, 25 * time.Hour}, {day, 0}, {182 * time.Hour, 0},
		{10 * day, 0}, {10 * day, 5 * day}, {6 * day, 4 * day}, {3 * time.Hour, -5 * time.Hour}, {-time.Hour, -2 * time.Hour},
	}
	for _, team := range []uuid.UUID{measured, cutover, unmeasured} {
		for _, w := range windows {
			for _, floor := range []bool{true, false} {
				var same bool
				var collapsed, perRow string
				if err := testPool.QueryRow(t.Context(), `SELECT c IS NOT DISTINCT FROM p, COALESCE(c::text,'NULL'), COALESCE(p::text,'NULL')
					FROM (SELECT storage_mib_seconds_segmented($1,$2,$3,$4) c, per_row_storage_mib_seconds($1,$2,$3,$4) p) v`,
					team, now.Add(-w[0]), now.Add(-w[1]), floor).Scan(&same, &collapsed, &perRow); err != nil {
					t.Fatal(err)
				}
				if !same {
					t.Fatalf("team %s window -%s..-%s floor=%t: collapsed=%s per-row=%s", team, w[0], w[1], floor, collapsed, perRow)
				}
			}
		}
	}
}

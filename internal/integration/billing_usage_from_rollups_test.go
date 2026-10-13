//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/superserve-ai/sandbox/internal/billing"
	"github.com/superserve-ai/sandbox/internal/db"
)

// Billing pages read usage from saved rollups. Compute must match the live
// interval calculation exactly, and storage must never present an
// unmeasured settled hour as a number.
func TestIntegration_BillingUsageFromRollupsMatchesIntervals(t *testing.T) {
	ctx := t.Context()
	team, _ := seedTeamAndKey(t)
	enableBillingHourlyRollups(t, ctx, team)
	now := time.Now().UTC()
	base := now.Truncate(time.Hour).Add(-6 * time.Hour)
	host := "rollup-read-" + uuid.NewString()

	sandbox := func(vcpu, memory, disk int, from, to time.Duration) {
		id := uuid.New()
		var endedAt *time.Time
		reason := ""
		if to > 0 {
			at := base.Add(to)
			endedAt, reason = &at, "deleted"
		}
		storageExec(t, `INSERT INTO sandbox(id,team_id,name,status,vcpu_count,memory_mib,host_id,created_at,destroyed_at)
			VALUES($1,$2,'rollup-read',$3::sandbox_status,$4,$5,$6,$7,$8)`,
			id, team, map[bool]string{true: "deleted", false: "paused"}[endedAt != nil], vcpu, memory, host, base.Add(from), endedAt)
		storageExec(t, `INSERT INTO sandbox_compute_billing_interval(sandbox_id,team_id,vcpu_count,memory_mib,started_at,ended_at,end_reason)
			VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''))`, id, team, vcpu, memory, base.Add(from), endedAt, reason)
		storageExec(t, `INSERT INTO sandbox_storage_interval(sandbox_id,team_id,host_id,disk_mib,started_at,ended_at,end_reason)
			VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''))`, id, team, host, disk, base.Add(from), endedAt, reason)
	}
	sandbox(1, 1024, 512, 10*time.Minute, 2*time.Hour+20*time.Minute)
	sandbox(2, 2048, 256, 3*time.Hour, 0) // still running into the current hour

	// Roll up every full past hour through the real worker on a private queue.
	poolConfig := testPool.Config()
	poolConfig.MaxConns = 1
	poolConfig.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `CREATE TEMP TABLE billing_rollup_job (LIKE public.billing_rollup_job INCLUDING ALL)`); err != nil {
		t.Fatal(err)
	}
	// Rollups and comparisons share one instant so the open interval and the
	// hour in progress compare exactly.
	if _, err := pool.Exec(ctx, `SELECT set_config('superserve.billing_now', $1, false)`, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	// Rollup hours are UTC hours; a fractional-offset session must still match them.
	if _, err := pool.Exec(ctx, `SET TimeZone = 'Asia/Kolkata'`); err != nil {
		t.Fatal(err)
	}
	for hour := base; !hour.After(now.Truncate(time.Hour)); hour = hour.Add(time.Hour) {
		if _, err := pool.Exec(ctx, `INSERT INTO billing_rollup_job(team_id,hour_start,hour_end,status) VALUES($1,$2,$3,'pending')`,
			team, hour, hour.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	cfg := billing.HourlyRollupConfig{BatchSize: 100, MaxAttempts: 5, LockDuration: time.Minute}
	billing.ProcessJobsForTest(ctx, pool, db.New(pool), cfg, "rollup-read-worker")
	var open int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM billing_rollup_job WHERE status <> 'completed'`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("rollups left %d jobs open: %v", open, err)
	}

	type bucket struct {
		vcpuSame, memorySame, storageSame, direct bool
		rolled, live                              *string
	}
	compare := func(starts, ends []time.Time) []bucket {
		t.Helper()
		rows, err := pool.Query(ctx, `
WITH b AS (SELECT * FROM unnest($2::timestamptz[], $3::timestamptz[]) AS b(bs, be)),
r AS (SELECT * FROM billing_usage_from_rollups($1, $2, $3)),
l AS (
 SELECT b.bs,
  COALESCE((SELECT SUM(EXTRACT(EPOCH FROM (LEAST(COALESCE(i.ended_at, billing_request_now()), b.be) - GREATEST(i.started_at, b.bs))) * i.vcpu_count)
            FROM sandbox_compute_billing_interval i WHERE i.team_id = $1
              AND i.started_at < LEAST(billing_request_now(), b.be) AND COALESCE(i.ended_at, LEAST(billing_request_now(), b.be)) > b.bs), 0) vcpu,
  COALESCE((SELECT SUM(EXTRACT(EPOCH FROM (LEAST(COALESCE(i.ended_at, billing_request_now()), b.be) - GREATEST(i.started_at, b.bs))) * i.memory_mib)
            FROM sandbox_compute_billing_interval i WHERE i.team_id = $1
              AND i.started_at < LEAST(billing_request_now(), b.be) AND COALESCE(i.ended_at, LEAST(billing_request_now(), b.be)) > b.bs), 0) memory,
  storage_mib_seconds($1, b.bs, b.be, false) storage
 FROM b)
SELECT r.vcpu_seconds = l.vcpu, r.memory_mib_seconds = l.memory,
 r.storage_mib_seconds IS NOT DISTINCT FROM l.storage, r.storage_direct, r.storage_mib_seconds::text, l.storage::text
FROM l JOIN r ON r.bucket_start = l.bs ORDER BY l.bs`, team, starts, ends)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []bucket
		for rows.Next() {
			var b bucket
			if err := rows.Scan(&b.vcpuSame, &b.memorySame, &b.storageSame, &b.direct, &b.rolled, &b.live); err != nil {
				t.Fatal(err)
			}
			out = append(out, b)
		}
		if err := rows.Err(); err != nil || len(out) != len(starts) {
			t.Fatalf("compared %d of %d buckets: %v", len(out), len(starts), err)
		}
		return out
	}
	text := func(s *string) string {
		if s == nil {
			return "NULL"
		}
		return *s
	}

	// Hour-aligned buckets, including the live current hour and a future one.
	var starts, ends []time.Time
	for hour := base; !hour.After(now.Truncate(time.Hour).Add(time.Hour)); hour = hour.Add(time.Hour) {
		starts, ends = append(starts, hour), append(ends, hour.Add(time.Hour))
	}
	for i, b := range compare(starts, ends) {
		if !b.vcpuSame || !b.memorySame || b.direct {
			t.Fatalf("hour %s: compute same=%t/%t direct=%t; want exact compute from saved hours", starts[i], b.vcpuSame, b.memorySame, b.direct)
		}
		if !b.storageSame {
			t.Fatalf("hour %s: storage %s, intervals %s", starts[i], text(b.rolled), text(b.live))
		}
	}
	// A period spanning the hour in progress includes its latest rollup.
	if b := compare([]time.Time{base}, []time.Time{base.Add(24 * time.Hour)})[0]; !b.vcpuSame || !b.memorySame || !b.storageSame || b.direct {
		t.Fatalf("spanning period: compute same=%t/%t storage %s vs %s direct=%t", b.vcpuSame, b.memorySame, text(b.rolled), text(b.live), b.direct)
	}
	// Edges cutting hours, and a range reaching before rollup coverage, leave
	// storage to the direct calculation.
	for i, b := range compare(
		[]time.Time{base.Add(15 * time.Minute), base.Add(-24 * time.Hour)},
		[]time.Time{now, base.Add(24 * time.Hour)}) {
		if !b.vcpuSame || !b.memorySame || !b.direct || b.rolled != nil {
			t.Fatalf("range %d: compute same=%t/%t direct=%t storage=%s; want exact compute and storage left to the direct path",
				i, b.vcpuSame, b.memorySame, b.direct, text(b.rolled))
		}
	}

	// A settled hour without measured storage keeps compute but not storage.
	storageExec(t, `UPDATE team_billing_usage_hourly SET storage_mib_seconds=NULL WHERE team_id=$1 AND hour_start=$2`, team, base.Add(time.Hour))
	// A settled hour still awaiting its rollup is computed from intervals.
	storageExec(t, `DELETE FROM team_billing_usage_hourly WHERE team_id=$1 AND hour_start=$2`, team, base.Add(2*time.Hour))
	if _, err := pool.Exec(ctx, `UPDATE billing_rollup_job SET status='pending' WHERE team_id=$1 AND hour_start=$2`, team, base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// An hour with usage but neither a row nor a job (rollups switched off or
	// an enqueue that failed) must not read as empty.
	storageExec(t, `DELETE FROM team_billing_usage_hourly WHERE team_id=$1 AND hour_start=$2`, team, base.Add(3*time.Hour))
	if _, err := pool.Exec(ctx, `DELETE FROM billing_rollup_job WHERE team_id=$1 AND hour_start=$2`, team, base.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A row captured before its hour ended holds a partial hour.
	storageExec(t, `UPDATE team_billing_usage_hourly SET vcpu_seconds=vcpu_seconds/2, memory_mib_seconds=memory_mib_seconds/2,
		updated_at=hour_end-interval '2 minutes' WHERE team_id=$1 AND hour_start=$2`, team, base.Add(4*time.Hour))
	// A storage measurement landing after the hour must not vouch for that compute.
	if err := billing.MeasureUnbilledStorageForTest(ctx, pool, team, base.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got := compare([]time.Time{base, base.Add(time.Hour), base.Add(2 * time.Hour), base.Add(3 * time.Hour), base.Add(4 * time.Hour)},
		[]time.Time{base.Add(time.Hour), base.Add(2 * time.Hour), base.Add(3 * time.Hour), base.Add(4 * time.Hour), base.Add(5 * time.Hour)})
	for i, b := range got[1:] {
		if !b.vcpuSame || !b.memorySame || b.rolled != nil {
			t.Fatalf("degraded hour %d: compute same=%t/%t storage=%s; want exact compute and unknown storage",
				i, b.vcpuSame, b.memorySame, text(b.rolled))
		}
	}
	if !got[0].storageSame {
		t.Fatalf("measured hour storage %s, intervals %s", text(got[0].rolled), text(got[0].live))
	}
}

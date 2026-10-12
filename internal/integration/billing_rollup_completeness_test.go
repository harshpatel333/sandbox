//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/superserve-ai/sandbox/internal/billing"
	"github.com/superserve-ai/sandbox/internal/db"
)

func TestIntegration_BillingRollupCompletenessDoesNotExhaustAttempts(t *testing.T) {
	const maxAttempts = 3
	for _, admissionBusy := range []bool{false, true} {
		for _, priorFailures := range []int{0, maxAttempts - 1} {
			t.Run(fmt.Sprintf("admission_busy_%t/prior_failures_%d", admissionBusy, priorFailures), func(t *testing.T) {
				ctx := t.Context()
				fixture := newStorageReportFixture(t, "active", false)
				teamID := sandboxTeamID(t, fixture.sandboxID)
				if _, err := testPool.Exec(ctx, `
				INSERT INTO team_feature_flag(team_id, key, enabled)
				VALUES ($1, 'billing_hourly_rollups', true)
				ON CONFLICT (team_id, key) DO UPDATE SET enabled=true`, teamID); err != nil {
					t.Fatalf("enable hourly rollups: %v", err)
				}
				// Only billable storage holds an hour for its reports.
				seedStorageActivation(t, teamID, time.Now().Add(-48*time.Hour))

				// Give this worker a private queue while exercising the real report
				// completeness gate and hourly usage query against the fixture.
				poolConfig := testPool.Config()
				poolConfig.MaxConns = 1
				poolConfig.MinConns = 0
				pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
				if err != nil {
					t.Fatalf("create isolated rollup pool: %v", err)
				}
				t.Cleanup(pool.Close)
				if _, err := pool.Exec(ctx, `CREATE TEMP TABLE billing_rollup_job
				(LIKE public.billing_rollup_job INCLUDING ALL)`); err != nil {
					t.Fatalf("create private rollup queue: %v", err)
				}

				reportID := uuid.New()
				var receivedAt time.Time
				var releaseReceipt func()
				if admissionBusy {
					tx, err := testPool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(context.Background())
					if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,0))`, fixture.hostID); err != nil {
						t.Fatal(err)
					}
					if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&receivedAt); err != nil {
						t.Fatal(err)
					}
					releaseReceipt = func() {
						if err := tx.Commit(ctx); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					if err := testPool.QueryRow(ctx, `
				INSERT INTO host_storage_report(host_id, incarnation_id, report_id, ingest_seq, payload, next_attempt_at)
				VALUES ($1, $2::uuid, $3, 1, '[]'::jsonb, now()+interval '1 hour')
				RETURNING received_at`, fixture.hostID, fixture.incarnation, reportID).Scan(&receivedAt); err != nil {
						t.Fatalf("insert unresolved report: %v", err)
					}
					releaseReceipt = func() {
						if _, err := testPool.Exec(ctx, `UPDATE host_storage_report
						SET state='processed', payload=NULL, processed_at=now() WHERE report_id=$1`, reportID); err != nil {
							t.Fatalf("resolve storage report: %v", err)
						}
					}
				}
				hourStart := receivedAt.UTC().Truncate(time.Hour)
				var jobID uuid.UUID
				if err := pool.QueryRow(ctx, `
				INSERT INTO billing_rollup_job(team_id, hour_start, hour_end, status, attempt_count)
				VALUES ($1, $2, $3, 'failed', $4) RETURNING id`,
					teamID, hourStart, hourStart.Add(time.Hour), priorFailures).Scan(&jobID); err != nil {
					t.Fatalf("insert rollup job: %v", err)
				}
				cfg := billing.HourlyRollupConfig{BatchSize: 1, MaxAttempts: maxAttempts, LockDuration: time.Minute}
				q := db.New(pool)
				for i := 0; i < maxAttempts*2; i++ {
					if _, err := pool.Exec(ctx, `UPDATE billing_rollup_job SET next_attempt_at=now() WHERE id=$1`, jobID); err != nil {
						t.Fatalf("make deferred job due: %v", err)
					}
					billing.ProcessJobsForTest(ctx, pool, q, cfg, "completeness-worker")
					var status, lastError string
					var attempts int
					if err := pool.QueryRow(ctx, `SELECT status, attempt_count, COALESCE(last_error,'')
					FROM billing_rollup_job WHERE id=$1`, jobID).Scan(&status, &attempts, &lastError); err != nil {
						t.Fatalf("read deferred rollup: %v", err)
					}
					if status != "pending" || attempts != priorFailures || lastError != "storage reports incomplete" {
						t.Fatalf("deferral %d: status=%s attempts=%d error=%q; want pending with %d prior failures",
							i+1, status, attempts, lastError, priorFailures)
					}
				}

				releaseReceipt()
				if _, err := pool.Exec(ctx, `UPDATE billing_rollup_job SET next_attempt_at=now() WHERE id=$1`, jobID); err != nil {
					t.Fatalf("make recovered job due: %v", err)
				}
				billing.ProcessJobsForTest(ctx, pool, q, cfg, "completeness-worker")
				var status string
				var attempts int
				if err := pool.QueryRow(ctx, `SELECT status, attempt_count FROM billing_rollup_job WHERE id=$1`, jobID).Scan(&status, &attempts); err != nil {
					t.Fatalf("read recovered rollup: %v", err)
				}
				if status != "completed" || attempts != priorFailures+1 {
					t.Fatalf("recovered rollup status=%s attempts=%d; want completed with %d attempts", status, attempts, priorFailures+1)
				}
				var rolledUp bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM team_billing_usage_hourly WHERE team_id=$1 AND hour_start=$2)`, teamID, hourStart).Scan(&rolledUp); err != nil {
					t.Fatalf("read recovered hourly usage: %v", err)
				}
				if !rolledUp {
					t.Fatal("recovered job did not persist hourly usage")
				}
			})
		}
	}
}

func TestIntegration_BillingRollupUnknownStorageDoesNotStarveQueue(t *testing.T) {
	ctx := t.Context()
	team, _ := seedTeamAndKey(t)
	enableBillingHourlyRollups(t, ctx, team)
	hostID := "rollup-unknown-" + uuid.NewString()
	start := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)
	// Only billable storage holds an hour for an unknown size.
	seedStorageActivation(t, team, start.Add(-time.Hour))
	// A sandbox on a manifest-less template build has no recorded size, so
	// its hour can never roll up until the artifact is reconciled.
	template, sandbox := uuid.New(), uuid.New()
	storageExec(t, `INSERT INTO template(id,team_id,name,status,build_spec,vcpu,memory_mib,disk_mib,rootfs_path)
		VALUES($1,$2,'unknown-template','ready','{}',1,1024,1024,'/templates/'||$1::uuid::text||'/base.ext4')`, template, team)
	storageExec(t, `INSERT INTO sandbox(id,team_id,name,status,vcpu_count,memory_mib,host_id,template_id,base_path,created_at,destroyed_at)
		SELECT $1,$2,'unknown-sandbox','deleted',1,1024,$4,id,rootfs_path,$5,$6 FROM template WHERE id=$3`,
		sandbox, team, template, hostID, start, start.Add(time.Hour))
	storageExec(t, `INSERT INTO sandbox_storage_interval(sandbox_id,team_id,host_id,disk_mib,started_at,ended_at,end_reason)
		VALUES($1,$2,$3,0,$4,$5,'deleted')`, sandbox, team, hostID, start, start.Add(time.Hour))

	poolConfig := testPool.Config()
	poolConfig.MaxConns = 1
	poolConfig.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `CREATE TEMP TABLE billing_rollup_job
		(LIKE public.billing_rollup_job INCLUDING ALL)`); err != nil {
		t.Fatal(err)
	}
	var unknownJob, newerJob uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO billing_rollup_job(team_id,hour_start,hour_end,status,next_attempt_at)
		VALUES($1,$2,$3,'pending',now()-interval '2 hours') RETURNING id`, team, start, start.Add(time.Hour)).Scan(&unknownJob); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO billing_rollup_job(team_id,hour_start,hour_end,status,next_attempt_at)
		VALUES($1,$2,$3,'pending',now()-interval '1 hour') RETURNING id`, team, start.Add(time.Hour), start.Add(2*time.Hour)).Scan(&newerJob); err != nil {
		t.Fatal(err)
	}

	cfg := billing.HourlyRollupConfig{BatchSize: 1, MaxAttempts: 5, LockDuration: time.Minute}
	billing.ProcessJobsForTest(ctx, pool, db.New(pool), cfg, "unknown-worker")
	var status, lastError string
	var attempts int
	var backedOff bool
	if err := pool.QueryRow(ctx, `SELECT status, attempt_count, COALESCE(last_error,''), next_attempt_at > now()+interval '10 minutes'
		FROM billing_rollup_job WHERE id=$1`, unknownJob).Scan(&status, &attempts, &lastError, &backedOff); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != 0 || lastError != "storage usage unknown" || !backedOff {
		t.Fatalf("unknown job status=%s attempts=%d error=%q backedOff=%t; want pending, no attempt spent, labeled and backed off",
			status, attempts, lastError, backedOff)
	}

	// Once its backoff lapses the unresolved old hour must queue behind work
	// that has waited longer, not reclaim the head of the queue.
	if _, err := pool.Exec(ctx, `UPDATE billing_rollup_job SET next_attempt_at=now() WHERE id=$1`, unknownJob); err != nil {
		t.Fatal(err)
	}
	jobs, err := billing.ClaimJobsForTest(ctx, pool, cfg, "unknown-worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != newerJob {
		t.Fatalf("claimed %+v; want the newer job %s that has waited longer", jobs, newerJob)
	}
}

func TestIntegration_BillingRollupUnbilledStorageDoesNotHoldCompute(t *testing.T) {
	ctx := t.Context()
	fixture := newStorageReportFixture(t, "active", false)
	teamID := sandboxTeamID(t, fixture.sandboxID)
	enableBillingHourlyRollups(t, ctx, teamID)

	poolConfig := testPool.Config()
	poolConfig.MaxConns = 1
	poolConfig.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `CREATE TEMP TABLE billing_rollup_job
		(LIKE public.billing_rollup_job INCLUDING ALL)`); err != nil {
		t.Fatal(err)
	}

	// An unresolved report keeps the team's storage incomplete for the hour.
	reportID := uuid.New()
	var receivedAt time.Time
	if err := testPool.QueryRow(ctx, `
		INSERT INTO host_storage_report(host_id, incarnation_id, report_id, ingest_seq, payload, next_attempt_at)
		VALUES ($1, $2::uuid, $3, 1, '[]'::jsonb, now()+interval '1 hour')
		RETURNING received_at`, fixture.hostID, fixture.incarnation, reportID).Scan(&receivedAt); err != nil {
		t.Fatal(err)
	}
	hourStart := receivedAt.UTC().Truncate(time.Hour)
	var jobID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO billing_rollup_job(team_id,hour_start,hour_end,status)
		VALUES($1,$2,$3,'pending') RETURNING id`, teamID, hourStart, hourStart.Add(time.Hour)).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	cfg := billing.HourlyRollupConfig{BatchSize: 1, MaxAttempts: 5, LockDuration: time.Minute}
	process := func() (string, bool, bool) {
		billing.ProcessJobsForTest(ctx, pool, db.New(pool), cfg, "unbilled-worker")
		var status string
		var recorded, measured bool
		if err := pool.QueryRow(ctx, `SELECT j.status, h.team_id IS NOT NULL, h.storage_mib_seconds IS NOT NULL
			FROM billing_rollup_job j LEFT JOIN team_billing_usage_hourly h ON h.team_id=j.team_id AND h.hour_start=j.hour_start
			WHERE j.id=$1`, jobID).Scan(&status, &recorded, &measured); err != nil {
			t.Fatal(err)
		}
		return status, recorded, measured
	}

	if status, recorded, measured := process(); status != "pending" || !recorded || measured {
		t.Fatalf("pending reports: status=%s recorded=%t measured=%t; want compute recorded, storage unmeasured (NULL, not 0) and the job deferred",
			status, recorded, measured)
	}
	// Unmeasured, unbillable storage must not null out the hour's spend.
	var priced bool
	if err := testPool.QueryRow(ctx, `SELECT spend_usd IS NOT NULL FROM analytics.team_hourly_spend
		WHERE team_name=(SELECT name FROM team WHERE id=$1) AND hour_start=$2`, teamID, hourStart).Scan(&priced); err != nil {
		t.Fatal(err)
	}
	if !priced {
		t.Fatal("hourly spend is NULL for an hour with unmeasured, unbillable storage")
	}

	// Once the reports land, the deferred job itself fills in the storage.
	if _, err := testPool.Exec(ctx, `UPDATE host_storage_report SET state='processed', payload=NULL, processed_at=now() WHERE report_id=$1`, reportID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE billing_rollup_job SET next_attempt_at=now() WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if status, recorded, measured := process(); status != "completed" || !recorded || !measured {
		t.Fatalf("after reports: status=%s recorded=%t measured=%t; want storage measured", status, recorded, measured)
	}
}

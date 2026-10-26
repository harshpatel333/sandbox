//go:build integration

package api

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/superserve-ai/sandbox/internal/db"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestResumeUsesCurrentNonFreePlanShape(t *testing.T) {
	for _, tc := range []struct {
		name            string
		cpu, mem, disk  int32
		allowed, legacy bool
	}{
		{"cpu after downgrade", 3, 2048, 4096, false, false},
		{"memory after downgrade", 2, 2049, 4096, false, false},
		{"disk after downgrade", 2, 2048, 4097, false, false},
		{"exact boundary", 2, 2048, 4096, true, false},
		{"legacy fallback", 0, 0, 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, team := newPlanAssignmentFixture(t)
			pool := h.Pool
			if !tc.legacy {
				for _, caps := range []struct{ cpu, mem, disk int32 }{{8, 8192, 16384}} {
					terms := testPlanTerms("starter", planKindPrepaid, 100, &caps.cpu)
					terms.Limits.MaxMemoryMiB, terms.Limits.MaxDiskMiB = &caps.mem, &caps.disk
					if _, err := assignInTx(t, h, planAssignment{Team: team, Terms: terms, Source: planSourceCheckout, Reason: "shape change"}); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				limits, err := h.teamEffectiveLimits(t.Context(), team)
				if err != nil {
					t.Fatal(err)
				}
				tc.cpu, tc.mem, tc.disk = limits.MaxVCPU, limits.MaxMemoryMiB, limits.MaxDiskMiB
			}
			id, snap := uuid.New(), uuid.New()
			if _, err := pool.Exec(t.Context(), `INSERT INTO sandbox(id,team_id,name,host_id,status,vcpu_count,memory_mib,disk_mib,created_at) VALUES($1,$2,'shape-resume','default','paused',$3,$4,$5,clock_timestamp())`, id, team, tc.cpu, tc.mem, tc.disk); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `INSERT INTO snapshot(id,sandbox_id,team_id,path,trigger) VALUES($1,$2,$3,'/fixture/vmstate.snap','pause');`, snap, id, team); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE sandbox SET snapshot_id=$1 WHERE id=$2`, snap, id); err != nil {
				t.Fatal(err)
			}
			if !tc.legacy {
				terms := testPlanTerms("starter", planKindPrepaid, 100, int32p(2))
				terms.Limits.MaxMemoryMiB, terms.Limits.MaxDiskMiB = int32p(2048), int32p(4096)
				if _, err := assignInTx(t, h, planAssignment{Team: team, Terms: terms, Source: planSourceCheckout, Reason: "downgrade after pause"}); err != nil {
					t.Fatal(err)
				}
			}
			reachedBoot := false
			h.VMD = &stubVMD{resumeFn: func(context.Context, string, string, string, []byte) (string, error) {
				reachedBoot = true
				return "", status.Error(codes.InvalidArgument, "stop after real claim")
			}}
			// Isolate the real limits reader/SQL claim from the separately covered
			// funding and authorization admission layers. Boot stops at the test stub.
			h.Pool = nil
			w := httptest.NewRecorder()
			setupTestRouter(h, team.String()).ServeHTTP(w, resumeRequest(id.String()))
			if reachedBoot != tc.allowed {
				t.Fatalf("boot=%v want=%v status=%d body=%s", reachedBoot, tc.allowed, w.Code, w.Body)
			}
			if !tc.allowed && (w.Code != 402 || !strings.Contains(w.Body.String(), "sandbox_exceeds_plan_limits")) {
				t.Fatalf("oversized resume=%d %s", w.Code, w.Body)
			}
			sb, err := db.New(pool).GetSandbox(t.Context(), db.GetSandboxParams{ID: id, TeamID: team})
			if err != nil || sb.Status != db.SandboxStatusPaused {
				t.Fatalf("claim/revert state=%s err=%v", sb.Status, err)
			}
		})
	}
}

// resumeLimitPool observes the real limits read inside the real transaction.
type resumeLimitPool struct {
	db.DBTX
	begin     func(context.Context, pgx.TxOptions) (pgx.Tx, error)
	afterRead func()
}

func (p resumeLimitPool) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := p.begin(ctx, opts)
	if err != nil {
		return nil, err
	}
	return resumeLimitTx{Tx: tx, afterRead: p.afterRead}, nil
}

type resumeLimitTx struct {
	pgx.Tx
	afterRead func()
}

func (t resumeLimitTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := t.Tx.QueryRow(ctx, sql, args...)
	if strings.Contains(sql, "-- resume shape limits") {
		return resumeLimitRow{Row: row, afterRead: t.afterRead}
	}
	return row
}

type resumeLimitRow struct {
	pgx.Row
	afterRead func()
}

func (r resumeLimitRow) Scan(dest ...any) error {
	if err := r.Row.Scan(dest...); err != nil {
		return err
	}
	r.afterRead()
	return nil
}

func TestResumeSerializesConcurrentLimitReduction(t *testing.T) {
	h, team := newPlanAssignmentFixture(t)
	pool := h.Pool
	_, err := pool.Exec(t.Context(), `UPDATE team SET max_template_vcpu=8,max_template_memory_mib=8192,max_template_disk_mib=16384 WHERE id=$1`, team)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err = pool.Exec(t.Context(), `INSERT INTO sandbox(id,team_id,name,host_id,status,vcpu_count,memory_mib,disk_mib,created_at) VALUES($1,$2,'serialized-resume','default','paused',3,2048,4096,clock_timestamp())`, id, team); err != nil {
		t.Fatal(err)
	}
	updater, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer updater.Release()
	var pid int32
	if err = updater.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	updated := make(chan error, 1)
	query := db.New(resumeLimitPool{DBTX: pool, begin: pool.BeginTx, afterRead: func() {
		go func() {
			_, e := updater.Exec(t.Context(), `UPDATE team SET max_template_vcpu=2 WHERE id=$1`, team)
			updated <- e
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var blocked bool
			if e := pool.QueryRow(t.Context(), `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked); e != nil {
				t.Fatal(e)
			}
			if blocked {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("cap reduction did not block behind the resume limits lock")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}})
	_, caps, err := query.ClaimResumeWithCurrentLimits(t.Context(), db.ClaimResumeParams{ID: id, TeamID: team, LockKey: id.String()})
	if err != nil || (caps.MaxVcpu == nil || *caps.MaxVcpu != 8) {
		t.Fatalf("serialized claim caps=%+v err=%v", caps, err)
	}
	select {
	case err = <-updated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("claim returned while holding team lock")
	}
	if _, err = pool.Exec(t.Context(), `UPDATE sandbox SET status='paused' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	_, caps, err = db.New(pool).ClaimResumeWithCurrentLimits(t.Context(), db.ClaimResumeParams{ID: id, TeamID: team, LockKey: id.String()})
	if !errors.Is(err, pgx.ErrNoRows) || (caps.MaxVcpu == nil || *caps.MaxVcpu != 2) {
		t.Fatalf("post-reduction claim caps=%+v err=%v", caps, err)
	}
}

func TestResumePreservesFreeOptionalShapeCaps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cpu     int32
		allowed bool
	}{{"absent memory and disk", 2, true}, {"present CPU cap", 3, false}} {
		t.Run(tc.name, func(t *testing.T) {
			h, team := newPlanAssignmentFixture(t)
			pool := h.Pool
			if _, err := pool.Exec(t.Context(), `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason) VALUES($1,'free','free','{"allowance":{"amount_cents":13,"period":"day_window","on_exhausted":"pause","compute":{"seconds":7200,"vcpu":1,"memory_gib":1}}}','{"max_concurrent":1,"max_vcpu":2}',clock_timestamp(),'migration','optional shape fixture')`, team); err != nil {
				t.Fatal(err)
			}
			admission, err := h.DB.GetTeamSandboxAdmission(t.Context(), team)
			if err != nil || !admission.ShapeLimited || admission.MaxVcpu != 2 || admission.MaxMemoryMib != 0 || admission.MaxDiskMib != 0 || admission.AllowanceBlocked {
				t.Fatalf("Free authority=%+v %v", admission, err)
			}
			id, snap := uuid.New(), uuid.New()
			if _, err = pool.Exec(t.Context(), `INSERT INTO sandbox(id,team_id,name,host_id,status,vcpu_count,memory_mib,disk_mib) VALUES($1,$2,'optional-free-shape','default','paused',$3,8192,65536)`, id, team, tc.cpu); err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(t.Context(), `INSERT INTO snapshot(id,sandbox_id,team_id,path,trigger) VALUES($1,$2,$3,'/fixture/vmstate.snap','pause')`, snap, id, team); err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(t.Context(), `UPDATE sandbox SET snapshot_id=$1 WHERE id=$2`, snap, id); err != nil {
				t.Fatal(err)
			}
			// Check the typed claim contract before testing its handler classification.
			if tc.allowed {
				_, caps, err := h.DB.ClaimResumeWithCurrentLimits(t.Context(), db.ClaimResumeParams{ID: id, TeamID: team, LockKey: id.String()})
				if err != nil || !caps.Free || caps.MaxVcpu == nil || *caps.MaxVcpu != 2 || caps.MaxMemoryMib != nil || caps.MaxDiskMib != nil {
					t.Fatalf("typed Free caps=%+v %v", caps, err)
				}
				if _, err = pool.Exec(t.Context(), `UPDATE sandbox SET status='paused' WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			boot := false
			h.VMD = &stubVMD{resumeFn: func(context.Context, string, string, string, []byte) (string, error) {
				boot = true
				return "", status.Error(codes.InvalidArgument, "stop after authoritative claim")
			}}
			// Real DB admission/claim; existing focused seam excludes independent
			// account-funding and RBAC layers so they cannot mask shape decisions.
			h.Pool = nil
			w := httptest.NewRecorder()
			setupTestRouter(h, team.String()).ServeHTTP(w, resumeRequest(id.String()))
			if boot != tc.allowed {
				t.Fatalf("boot=%v want%v HTTP%d %s", boot, tc.allowed, w.Code, w.Body)
			}
			if !tc.allowed && (w.Code != 402 || !strings.Contains(w.Body.String(), "sandbox_exceeds_plan_limits")) {
				t.Fatalf("present cap=%d %s", w.Code, w.Body)
			}
		})
	}
}

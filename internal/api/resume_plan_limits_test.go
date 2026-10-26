package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/superserve-ai/sandbox/internal/db"
)

func TestResumeFailsClosedWithoutValidShapeLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits teamLimits
		err    error
	}{
		{name: "missing", err: pgx.ErrNoRows}, {name: "read failure", err: errors.New("database unavailable")},
		{name: "zero cpu", limits: teamLimits{MaxVCPU: 0, MaxMemoryMiB: 2048, MaxDiskMiB: 4096}},
		{name: "zero memory", limits: teamLimits{MaxVCPU: 2, MaxMemoryMiB: 0, MaxDiskMiB: 4096}},
		{name: "zero disk", limits: teamLimits{MaxVCPU: 2, MaxMemoryMiB: 2048, MaxDiskMiB: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockDBTX{resumeLimits: &tc.limits, resumeLimitsErr: tc.err, queryRowFn: func(context.Context, string, ...any) pgx.Row {
				t.Fatal("resume claim ran without authoritative limits")
				return nil
			}}
			h := &Handlers{DB: db.New(mock)}
			w := httptest.NewRecorder()
			setupTestRouter(h, uuid.NewString()).ServeHTTP(w, resumeRequest(uuid.NewString()))
			if w.Code != 500 {
				t.Fatalf("invalid limits response=%d %s", w.Code, w.Body)
			}
		})
	}
}

func TestResumeReloadsLimitsBeforeRetriedClaim(t *testing.T) {
	for _, tc := range []struct {
		name    string
		after   int
		failure bool
	}{
		{"pause retry downgrade", 1, false}, {"secret retry downgrade", 2, false}, {"refresh failure", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			team, id, snap := uuid.New(), uuid.New(), uuid.New()
			sb := pausedSandboxWithSnapshot(id, team, snap)
			sb.VcpuCount = 3
			claims := 0
			mock := &mockDBTX{resumeLimits: &teamLimits{MaxVCPU: 8, MaxMemoryMiB: 8192, MaxDiskMiB: 2097152}}
			mock.queryRowFn = func(_ context.Context, sql string, args ...any) pgx.Row {
				switch {
				case strings.Contains(sql, "-- name: ClaimResume :one"):
					claims++
					want := int32(8)
					if claims > tc.after {
						want = 2
					}
					if args[1] == nil || *args[1].(*int32) != want {
						t.Fatalf("claim%d CPU cap=%v want%d", claims, args[1], want)
					}
					if claims == tc.after {
						if tc.failure {
							mock.resumeLimitsErr = errors.New("limits refresh unavailable")
						} else {
							mock.resumeLimits = &teamLimits{MaxVCPU: 2, MaxMemoryMiB: 8192, MaxDiskMiB: 2097152}
						}
					}
					return notFoundRow()
				case strings.Contains(sql, "-- name: SandboxSecretWriteHeld :one"):
					return scalarBoolRow(false)
				case strings.Contains(sql, "FROM sandbox"):
					return sandboxRow(sb)
				default:
					t.Fatalf("unexpected query: %s", sql)
					return nil
				}
			}
			h := &Handlers{DB: db.New(mock)}
			w := httptest.NewRecorder()
			setupTestRouter(h, team.String()).ServeHTTP(w, resumeRequest(id.String()))
			wantStatus, wantClaims := 402, tc.after+1
			if tc.failure {
				wantStatus, wantClaims = 500, 1
			}
			if w.Code != wantStatus || claims != wantClaims {
				t.Fatalf("response=%d claims=%d want=%d/%d body=%s", w.Code, claims, wantStatus, wantClaims, w.Body)
			}
		})
	}
}

func TestResumeClaimRequiresTransactionSupport(t *testing.T) {
	mock := &mockDBTX{queryRowFn: func(context.Context, string, ...any) pgx.Row {
		t.Fatal("claim without transaction support must not read or mutate")
		return nil
	}}
	// Expose the ordinary query interface, deliberately hiding BeginTx.
	q := db.New(struct{ db.DBTX }{mock})
	_, _, err := q.ClaimResumeWithCurrentLimits(t.Context(), db.ClaimResumeParams{TeamID: uuid.New(), ID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "transaction-capable") {
		t.Fatalf("unsupported transaction error=%v", err)
	}
}

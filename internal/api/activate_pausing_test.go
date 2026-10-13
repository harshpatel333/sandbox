package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/superserve-ai/sandbox/internal/config"
	"github.com/superserve-ai/sandbox/internal/db"
)

// activatePausingFixture serves a sandbox whose status is decided per read
// by status(read), and reports whether the resume RPC ran.
func activatePausingFixture(t *testing.T, status func(read int32) db.SandboxStatus) (*Handlers, uuid.UUID, uuid.UUID, *int32, *bool) {
	t.Helper()
	sandboxID, teamID, snapshotID := uuid.New(), uuid.New(), uuid.New()
	sb := pausedSandboxWithSnapshot(sandboxID, teamID, snapshotID)
	snap := db.Snapshot{ID: snapshotID, SandboxID: sandboxID, TeamID: teamID, Path: "/snapshots/test/vmstate.snap", SizeBytes: 1024, Trigger: "pause"}
	var reads int32
	resumed := false
	vmd := &stubVMD{
		destroyFn: func(context.Context, string, bool) error { return nil },
		resumeFn: func(_ context.Context, _, _, _ string, _ []byte) (string, error) {
			resumed = true
			return "10.0.0.5", nil
		},
	}
	mock := &mockDBTX{
		queryRowFn: func(_ context.Context, sql string, _ ...any) pgx.Row {
			switch {
			case strings.Contains(sql, "'resuming'"):
				return claimResumeRow(sb, &snap, "", 0)
			case strings.Contains(sql, "FROM sandbox"):
				row := sb
				row.Status = status(atomic.AddInt32(&reads, 1))
				return sandboxRow(row)
			case strings.Contains(sql, "FROM snapshot"):
				return snapshotRow(snap)
			default:
				return activityRow()
			}
		},
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	h := &Handlers{VMD: vmd, DB: db.New(mock), Config: &config.Config{SandboxAccessTokenSeed: []byte("test-seed-for-hmac-32-bytes-min!!")}}
	return h, sandboxID, teamID, &reads, &resumed
}

// A sandbox still finalizing its pause is waited for, then resumed: the
// caller gets a running sandbox, not a conflict for a pause it was just
// told succeeded.
func TestActivateSandbox_WaitsOutPausingThenResumes(t *testing.T) {
	h, sandboxID, teamID, reads, resumed := activatePausingFixture(t, func(read int32) db.SandboxStatus {
		if read < 3 {
			return db.SandboxStatusPausing
		}
		return db.SandboxStatusPaused
	})
	w := httptest.NewRecorder()
	setupTestRouter(h, teamID.String()).ServeHTTP(w, activateRequest(sandboxID.String()))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if !*resumed || atomic.LoadInt32(reads) < 3 {
		t.Fatalf("resumed=%v reads=%d; want the pause waited out and the sandbox resumed", *resumed, atomic.LoadInt32(reads))
	}
	if body := parseJSON(t, w); body["status"] != "active" {
		t.Errorf("status = %q, want active", body["status"])
	}
}

// A pause that outlives the settle window is a conflict, as before: the
// caller is told the sandbox is busy within a bounded wait, never that the
// pause failed.
func TestActivateSandbox_PersistentPausing_409(t *testing.T) {
	prev := pausingSettleWindow
	pausingSettleWindow = 300 * time.Millisecond
	defer func() { pausingSettleWindow = prev }()
	h, sandboxID, teamID, _, resumed := activatePausingFixture(t, func(int32) db.SandboxStatus { return db.SandboxStatusPausing })
	w := httptest.NewRecorder()
	start := time.Now()
	setupTestRouter(h, teamID.String()).ServeHTTP(w, activateRequest(sandboxID.String()))
	if w.Code != http.StatusConflict || *resumed {
		t.Fatalf("status = %d resumed=%v; want 409 and no resume", w.Code, *resumed)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("activate held the request for %v; the wait must be bounded", took)
	}
}

// A failed sandbox is refused at once: callers tell a failed VM from a busy
// one by this 409, so it must not wait.
func TestActivateSandbox_Failed_409WithoutWaiting(t *testing.T) {
	h, sandboxID, teamID, reads, resumed := activatePausingFixture(t, func(int32) db.SandboxStatus { return db.SandboxStatusFailed })
	w := httptest.NewRecorder()
	setupTestRouter(h, teamID.String()).ServeHTTP(w, activateRequest(sandboxID.String()))
	if w.Code != http.StatusConflict || *resumed || atomic.LoadInt32(reads) != 1 {
		t.Fatalf("status = %d resumed=%v reads=%d; want an immediate 409", w.Code, *resumed, atomic.LoadInt32(reads))
	}
}

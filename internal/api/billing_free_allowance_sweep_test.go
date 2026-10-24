package api

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/superserve-ai/sandbox/internal/db"
)

func TestFreeAllowanceSweepRetriesFailedPrefix(t *testing.T) {
	teams := make([]uuid.UUID, freeAllowancePageSize)
	for i := range teams {
		teams[i] = uuid.New()
	}
	var fail atomic.Bool
	fail.Store(true)
	var attempts atomic.Int32
	var cursor pgtype.UUID
	saves := 0
	mock := &mockDBTX{
		queryRowFn: func(_ context.Context, sql string, args ...any) pgx.Row {
			if strings.Contains(sql, "ClaimSweepLease") {
				return &mockRow{scanFn: func(dest ...any) error { *dest[0].(*pgtype.UUID) = cursor; return nil }}
			}
			return &mockRow{scanFn: func(dest ...any) error {
				if args[0].(uuid.UUID) == teams[0] {
					attempts.Add(1)
					if fail.Load() {
						return context.DeadlineExceeded
					}
				}
				*dest[0].(*bool) = false
				return nil
			}}
		},
		queryFn: func(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
			rows := &scanRows{}
			if !args[0].(pgtype.UUID).Valid {
				for _, id := range teams {
					id := id
					rows.rows = append(rows.rows, func(dest ...any) error { *dest[0].(*uuid.UUID) = id; return nil })
				}
			}
			return rows, nil
		},
		execFn: func(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
			saves++
			cursor = args[0].(pgtype.UUID)
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	h := &Handlers{DB: db.New(mock)}
	h.refreshFreeAllowances(t.Context())
	if saves != 0 {
		t.Fatalf("refresh error advanced durable cursor %d times", saves)
	}
	fail.Store(false)
	h.refreshFreeAllowances(t.Context())
	if attempts.Load() != 2 || saves == 0 {
		t.Fatalf("failed prefix not retried: attempts=%d cursor saves=%d", attempts.Load(), saves)
	}
}

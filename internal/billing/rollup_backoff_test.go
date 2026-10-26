package billing

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestTeamBackoffLapses(t *testing.T) {
	b := &teamBackoff{until: map[uuid.UUID]time.Time{}}
	slow, other := uuid.New(), uuid.New()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	b.start(slow, now)
	if !b.active(slow, now.Add(time.Hour)) {
		t.Fatal("backoff not active inside its window")
	}
	if b.active(other, now.Add(time.Hour)) {
		t.Fatal("backoff applied to a team that never timed out")
	}
	if b.active(slow, now.Add(unbilledStorageBackoffFor)) {
		t.Fatal("backoff still active once its window passed")
	}
}

func TestStorageBudgetExceededOnlyForTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), true},
		{"server cancel", &pgconn.PgError{Code: "57014"}, true},
		{"caller cancel", context.Canceled, false},
		{"lock timeout", &pgconn.PgError{Code: "55P03"}, false},
		{"other", errors.New("boom"), false},
	} {
		if got := storageBudgetExceeded(tc.err); got != tc.want {
			t.Errorf("%s: storageBudgetExceeded = %v, want %v", tc.name, got, tc.want)
		}
	}
}

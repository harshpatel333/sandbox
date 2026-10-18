package api

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestFundingObservationFreshnessIsIndependent(t *testing.T) {
	at := time.Now().UTC()
	checked := pgtype.Timestamptz{Time: at.Add(-30 * time.Second), Valid: true}
	valid := pgtype.Timestamptz{Time: at.Add(3 * time.Minute), Valid: true}
	if !fundingObservationDue(checked, valid, at, 30*time.Second) || fundingObservationDue(checked, valid, at, 2*time.Minute) {
		t.Fatal("credit tick must not force a fresh provider proof")
	}
	valid.Time = at.Add(5 * time.Second)
	if !fundingObservationDue(checked, valid, at, 2*time.Minute) {
		t.Fatal("near-expiry proof was reused despite its earlier deadline")
	}
	if !fundingObservationDue(pgtype.Timestamptz{}, valid, at, 2*time.Minute) {
		t.Fatal("missing observation was reused")
	}
}

func TestFundingProviderModeMustBeExplicit(t *testing.T) {
	for _, tc := range []struct {
		key  string
		live bool
	}{
		{"sk_live_example", true}, {"rk_live_example", true}, {"sk_test_example", false}, {"rk_test_example", false},
	} {
		got, err := fundingStripeMode(tc.key)
		if err != nil || got != tc.live {
			t.Fatalf("mode=%v err=%v", got, err)
		}
	}
	if _, err := fundingStripeMode(""); err == nil {
		t.Fatal("missing credentials silently selected test mode")
	}
}

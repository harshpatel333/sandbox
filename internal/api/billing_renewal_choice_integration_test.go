//go:build integration

package api

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This fixture tests only journal persistence in its own disposable schema. It
// does not stand in for production authority, catalog or payment guard adapters.
func newRenewalPostgresFixture(t *testing.T) (*renewalChoiceService, *fixtureRenewalAuthority, *pgxpool.Pool) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("RENEWAL_JOURNAL_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.Host != "127.0.0.1" || cfg.ConnConfig.Database != "renewal_choice_test" {
		t.Fatal("requires dedicated local renewal_choice_test database")
	}
	admin, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	schema := "renewal_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); admin.Close() })
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err = pool.Exec(t.Context(), `CREATE TABLE team(id uuid PRIMARY KEY); CREATE TABLE team_billing_account(team_id uuid PRIMARY KEY REFERENCES team(id))`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../supabase/migrations/20261021135341_billing_renewal_choice.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), string(migration)); err != nil {
		t.Fatal(err)
	}
	_, _, a := newRenewalFixture(t)
	a.now = time.Now().UTC().Add(-time.Second)
	a.key.Boundary = a.now.AddDate(0, 1, 0).Truncate(time.Second)
	a.authority.Binding.Anchor = a.key.Boundary.AddDate(-1, 0, 0)
	if _, err = pool.Exec(t.Context(), `INSERT INTO team(id) VALUES($1)`, a.key.Team); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), `INSERT INTO team_billing_account(team_id) VALUES($1)`, a.key.Team); err != nil {
		t.Fatal(err)
	}
	s := &renewalChoiceService{Enabled: true, Store: postgresRenewalChoiceStore{Pool: pool}, Authority: a}
	if err = s.initialize(t.Context(), a.key, a.actor); err != nil {
		t.Fatal(err)
	}
	return s, a, pool
}

func TestRenewalChoicePostgresReplayConcurrencyAndRollback(t *testing.T) {
	s, a, pool := newRenewalPostgresFixture(t)
	j := submitRenewal(t, s, a, "free")
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.submit(t.Context(), a.key, a.actor, renewalChoiceRequest{ID: uuid.New(), Choice: "annual", Acceptance: a.authority.Acceptance, ApprovalRevision: 7, ChoiceRevision: j.Revision})
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, errRenewalConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatal("lost database revision fence", accepted.Load())
	}
	// Reconstruct the service to exercise persisted identities rather than memory.
	resumed := &renewalChoiceService{Enabled: true, Store: postgresRenewalChoiceStore{Pool: pool}, Authority: a}
	replay, err := resumed.submit(t.Context(), a.key, a.actor, j.Requested.Request)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Successor != j.Successor || replay.Revision != j.Revision+1 || replay.Confirmed.Request.Choice != "annual" {
		t.Fatal(replay)
	}
	marker := errors.New("rollback after save")
	err = resumed.Store.Transact(t.Context(), a.key, func(tx renewalChoiceTransaction) error {
		tx.Journal().SuccessorState = "active"
		if e := tx.Save("must_rollback"); e != nil {
			return e
		}
		return marker
	})
	if !errors.Is(err, marker) {
		t.Fatal(err)
	}
	current, err := resumed.status(t.Context(), a.key, a.actor)
	if err != nil || current.SuccessorState != "scheduled" || current.Revision != replay.Revision {
		t.Fatal(current, err)
	}
	var requests, events int
	if err = pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM billing_renewal_choice_request),(SELECT count(*) FROM billing_renewal_choice_event)`).Scan(&requests, &events); err != nil {
		t.Fatal(err)
	}
	if requests != 3 || events != 3 {
		t.Fatal("request/event transaction diverged", requests, events)
	}
	if _, err = pool.Exec(t.Context(), `UPDATE billing_renewal_choice_request SET request='{}'`); err == nil {
		t.Fatal("request history mutable")
	}
	if _, err = pool.Exec(t.Context(), `DELETE FROM billing_renewal_choice_event`); err == nil {
		t.Fatal("event history mutable")
	}
}

func TestRenewalChoicePostgresDispatchSurvivesRestart(t *testing.T) {
	s, a, pool := newRenewalPostgresFixture(t)
	j := submitRenewal(t, s, a, "free")
	// Crash after persisted intent and before the external response. Recovery is
	// observation-only even if it cannot tell whether the write was ever sent.
	if err := s.Store.Transact(t.Context(), a.key, func(tx renewalChoiceTransaction) error {
		op := tx.Journal().Requested
		now := tx.Now()
		op.State = "dispatched"
		op.DispatchedAt = &now
		return tx.Save("dispatch_intent")
	}); err != nil {
		t.Fatal(err)
	}
	p := &fixtureRenewalProvider{dispatch: func(renewalChoiceKey, renewalChoiceOperation) (renewalChoiceProof, error) {
		t.Fatal("replayed uncertain write")
		return renewalChoiceProof{}, nil
	}, observe: func(k renewalChoiceKey, op renewalChoiceOperation) (renewalChoiceProof, error) {
		if op.ID != j.Requested.ID {
			t.Fatal("operation replaced")
		}
		return fixtureRenewalProof(k, op, a.now), nil
	}}
	resumed := &renewalChoiceService{Enabled: true, Store: postgresRenewalChoiceStore{Pool: pool}, Authority: a, Provider: p}
	if err := resumed.reconcile(t.Context(), a.key); err != nil {
		t.Fatal(err)
	}
	got, err := resumed.status(t.Context(), a.key, a.actor)
	if err != nil || got.Confirmed.Request.Choice != "free" || p.writes.Load() != 0 {
		t.Fatal(got, err)
	}
}

func TestRenewalChoiceMonthlyPublicationRejectsConflictingVersion(t *testing.T) {
	_, _, pool := newRenewalPostgresFixture(t)
	// Use the upstream published catalog's columns and immutable key semantics.
	_, err := pool.Exec(t.Context(), `CREATE TABLE billing_commercial_offer_version(offer_key text,version int,rate_card_key text,plan_tier text,currency text,term_months int,charge_amount_cents bigint,charge_interval_months int,initial_fee_waived bool,payment_requirement text,default_successor_family text,allowance_source text,monthly_allowance_cents bigint[],rollover bool,proration bool,PRIMARY KEY(offer_key,version))`)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := os.ReadFile("../../supabase/migrations/20261021140415_pro_monthly_offer_version.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), string(seed)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), `UPDATE billing_commercial_offer_version SET charge_amount_cents=1`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), string(seed)); err == nil {
		t.Fatal("conflicting publication silently adopted")
	}
	var amount int64
	if err = pool.QueryRow(t.Context(), `SELECT charge_amount_cents FROM billing_commercial_offer_version`).Scan(&amount); err != nil || amount != 1 {
		t.Fatal(amount, err)
	}
}

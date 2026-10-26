//go:build integration

package integration

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/superserve-ai/sandbox/internal/db"
)

func TestIntegration_PrepaidHistoryProductionRole(t *testing.T) {
	owner := teamCreationDatabase(t, func(pool *pgxpool.Pool) {
		rolloutExec(t, pool, `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO anon,authenticated,service_role`)
	})
	service := teamCreationServiceRolePool(t, owner)
	tables := []string{"billing_prepaid_purchase", "billing_prepaid_reversal", "billing_prepaid_reversal_event", "billing_prepaid_reversal_decision"}
	expectCode := func(statement, code string) {
		t.Helper()
		_, err := service.Exec(t.Context(), statement)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Fatalf("%s: expected %s got %v", statement, code, err)
		}
	}
	for _, table := range tables {
		var allowed, destructive, guarded bool
		err := owner.QueryRow(t.Context(), `SELECT (has_table_privilege('service_role',$1,'SELECT') AND has_table_privilege('service_role',$1,'INSERT') AND has_table_privilege('service_role',$1,'UPDATE')), has_table_privilege('service_role',$1,'DELETE,TRUNCATE'), EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid=$1::regclass AND tgname='prepaid_history_no_truncate' AND tgenabled='O' AND (tgtype::int & 32)=32)`, table).Scan(&allowed, &destructive, &guarded)
		if err != nil || !allowed || destructive || !guarded {
			t.Fatalf("%s privileges allowed=%v destructive=%v truncate_guard=%v: %v", table, allowed, destructive, guarded, err)
		}
		for _, role := range []string{"anon", "authenticated"} {
			var exposed bool
			if err := owner.QueryRow(t.Context(), `SELECT has_table_privilege($1,$2,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE')`, role, table).Scan(&exposed); err != nil || exposed {
				t.Fatalf("%s retained default access to %s: %v %v", role, table, exposed, err)
			}
		}
		expectCode("TRUNCATE "+table+" CASCADE", "42501")
		expectCode("DELETE FROM "+table, "42501")
	}
	team, err := db.New(owner).CreateTeam(t.Context(), "history-role-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := service.Exec(t.Context(), `INSERT INTO billing_prepaid_purchase(id,team_id,customer_id,source,amount_cents) VALUES($1,$2,'cus_history','auto_recharge',500)`, id, team.ID); err != nil {
		t.Fatal("legitimate purchase insert", err)
	}
	if _, err := service.Exec(t.Context(), `UPDATE billing_prepaid_purchase SET next_attempt_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, id); err != nil {
		t.Fatal("legitimate worker update", err)
	}
	for _, table := range tables {
		rolloutExec(t, owner, "GRANT ALL ON "+table+" TO service_role")
	}
	for _, table := range tables {
		expectCode("TRUNCATE "+table+" CASCADE", "55000")
	}
	expectCode("DELETE FROM billing_prepaid_purchase", "P0001")
	var count int
	if err := service.QueryRow(t.Context(), `SELECT count(*) FROM billing_prepaid_purchase WHERE id=$1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("purchase history lost: %d %v", count, err)
	}
}

func TestIntegration_MatchingHistoryProductionRole(t *testing.T) {
	owner := teamCreationDatabase(t, func(pool *pgxpool.Pool) {
		rolloutExec(t, pool, `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO anon,authenticated,service_role`)
	})
	service := teamCreationServiceRolePool(t, owner)
	for _, table := range []string{"billing_contract_matching_rule", "billing_matching_credit", "billing_matching_invoice_inspection"} {
		for _, role := range []string{"anon", "authenticated", "service_role"} {
			var destructive bool
			if err := owner.QueryRow(t.Context(), `SELECT has_table_privilege($1,$2,'DELETE,TRUNCATE')`, role, table).Scan(&destructive); err != nil || destructive {
				t.Fatalf("%s destructive on %s: %v %v", role, table, destructive, err)
			}
		}
		var guarded bool
		if err := owner.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid=$1::regclass AND tgname='matching_history_no_truncate' AND tgenabled='O' AND (tgtype::int & 32)=32)`, table).Scan(&guarded); err != nil || !guarded {
			t.Fatalf("%s missing statement truncate guard: %v", table, err)
		}
		_, err := service.Exec(t.Context(), "TRUNCATE "+table+" CASCADE")
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s narrow ACL: %v", table, err)
		}
		rolloutExec(t, owner, "GRANT ALL ON "+table+" TO service_role")
	}
	for _, table := range []string{"billing_contract_matching_rule", "billing_matching_credit", "billing_matching_invoice_inspection"} {
		_, err := service.Exec(t.Context(), "TRUNCATE "+table+" CASCADE")
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55000" {
			t.Fatalf("%s regrant bypassed history guard: %v", table, err)
		}
	}
}

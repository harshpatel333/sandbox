//go:build integration

package api

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestMatchingCreditHistoryGuardsPreserveCapAndHold(t *testing.T) {
	m := newMatchingFixture(t, 10000, 100, 20)
	first := m.paidInvoice("paid", usageLine("price_cpu", 60))
	if err := m.tick(); err != nil {
		t.Fatal(err)
	}
	if row := m.row(first); row.Paid != 60 || row.State != "issued" {
		t.Fatalf("first cash match: %+v", row)
	}
	carry := int64(40)
	m.endingBalance = &carry
	held := m.paidInvoice("paid", usageLine("price_cpu", 40))
	m.paid[held] = 0
	if err := m.tick(); !errors.Is(err, errMatchingCashReconciliation) {
		t.Fatalf("carry hold: %v", err)
	}
	tables := []string{"billing_contract_matching_rule", "billing_matching_credit", "billing_matching_invoice_inspection"}
	for _, table := range tables {
		m.exec("GRANT ALL ON " + table + " TO service_role")
	}
	t.Cleanup(func() {
		for _, table := range tables {
			if _, err := m.f.h.Pool.Exec(context.Background(), "REVOKE ALL ON "+table+" FROM service_role; GRANT SELECT,INSERT,UPDATE ON "+table+" TO service_role"); err != nil {
				t.Error(err)
			}
		}
	})
	tx, err := m.f.h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), "SET LOCAL ROLE service_role"); err != nil {
		t.Fatal(err)
	}
	var super, bypass bool
	if err = tx.QueryRow(t.Context(), `SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil || super || !bypass {
		t.Fatalf("real service role super=%v bypass=%v: %v", super, bypass, err)
	}
	probe := func(sql, code string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(t.Context(), "SAVEPOINT destructive_probe"); err != nil {
			t.Fatal(err)
		}
		_, err := tx.Exec(t.Context(), sql, args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Fatalf("%s expected%s got%v", sql, code, err)
		}
		if _, err = tx.Exec(t.Context(), "ROLLBACK TO SAVEPOINT destructive_probe"); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range tables {
		probe("TRUNCATE "+table+" CASCADE", "55000")
	}
	probe("DELETE FROM billing_matching_credit WHERE invoice_id=$1", "P0001", first)
	probe("DELETE FROM billing_contract_matching_rule WHERE contract_id=$1", "P0001", m.f.c.ID)
	probe("DELETE FROM billing_matching_invoice_inspection WHERE invoice_id=$1", "55000", held)
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var consumed int64
	var holds int
	if err = m.f.h.Pool.QueryRow(t.Context(), `SELECT COALESCE(sum(amount_cents),0) FROM billing_matching_credit WHERE contract_id=$1`, m.f.c.ID).Scan(&consumed); err != nil || consumed != 60 {
		t.Fatalf("consumed cap lost: %d %v", consumed, err)
	}
	if err = m.f.h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM billing_matching_invoice_inspection WHERE invoice_id=$1 AND last_error=$2`, held, errMatchingCashReconciliation.Error()).Scan(&holds); err != nil || holds != 1 {
		t.Fatalf("sticky hold lost: %d %v", holds, err)
	}
	m.endingBalance = nil
	next := m.paidInvoice("paid", usageLine("price_cpu", 100))
	if err = m.tick(); err != nil {
		t.Fatal(err)
	}
	if row := m.row(next); row.Paid != 20 || row.State != "issued" {
		t.Fatalf("prior cap not preserved: %+v", row)
	}
	later := m.paidInvoice("paid", usageLine("price_cpu", 100))
	if err = m.tick(); err != nil {
		t.Fatal(err)
	}
	if row := m.row(later); row.Paid != 0 || row.State != "skipped" || len(m.posts) != 2 {
		t.Fatalf("destructive probe reopened cap or duplicate grant: %+v grants=%d", row, len(m.posts))
	}
	// Even later provider proof cannot silently erase the unresolved carry hold.
	m.mu.Lock()
	inv := m.invoices[held]
	zero := int64(0)
	inv.EndingBalance = &zero
	m.invoices[held] = inv
	m.paid[held] = 40
	m.mu.Unlock()
	m.exec(`UPDATE billing_matching_invoice_inspection SET next_attempt_at=clock_timestamp() WHERE invoice_id=$1`, held)
	if err = m.tick(); !errors.Is(err, errMatchingCashReconciliation) {
		t.Fatalf("hold cleared after attempted removal: %v", err)
	}
	if len(m.posts) != 2 {
		t.Fatal("held invoice received an extra provider grant")
	}
}

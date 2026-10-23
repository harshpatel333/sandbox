//go:build integration

package api

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPlanTermsFeeLessFamily(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil || !strings.HasSuffix(cfg.ConnConfig.Database, "_test") {
		t.Fatal("requires migrated disposable test database")
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h := &Handlers{Pool: pool}
	family := "custom_" + strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return 'a' + r - '0'
		}
		return r
	}, strings.ReplaceAll(uuid.NewString()[:8], "-", ""))
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(), `INSERT INTO billing_plan_version(family,version,kind,display_name,credit_schedule,matching_bps,cap_cents)
		VALUES($1,1,'custom','Example match','[{"start_months":0,"repeat":1,"every_months":1,"amount_cents":1000000,"expires_after_months":3}]',10000,8000000) RETURNING id`, family).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `SELECT publish_billing_plan_version($1,NULL)`, id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(t.Context(), `SELECT retire_billing_plan_version($1,NULL)`, id) })

	terms, err := h.planTerms(t.Context(), family)
	if err != nil {
		t.Fatal(err)
	}
	if terms.VersionID != id || terms.FeeCents != 0 || terms.IntervalMonths != 0 || terms.Available || terms.Matching == nil ||
		terms.Matching.MatchingBps != 10000 || terms.Matching.CapCents != 8000000 || len(terms.Grants) != 1 || terms.Grants[0].ExpiresAfterMonths != 3 {
		t.Fatalf("fee-less terms = %+v", terms)
	}
	for _, key := range []string{family + "_monthly", family + "_annual"} {
		if _, err := h.planTerms(t.Context(), key); !errors.Is(err, errUnknownPlan) {
			t.Fatalf("%s: err=%v", key, err)
		}
	}
	if _, err := h.planTerms(t.Context(), "pro"); !errors.Is(err, errUnknownPlan) {
		t.Fatalf("bare fee-bearing family: err=%v", err)
	}
	if pro, err := h.planTerms(t.Context(), "pro_monthly"); err != nil || pro.FeeCents != 120000 || pro.IntervalMonths != 1 || !pro.Available {
		t.Fatalf("pro_monthly = %+v err=%v", pro, err)
	}
	if _, err := pool.Exec(t.Context(), `SELECT retire_billing_plan_version($1,NULL)`, id); err != nil {
		t.Fatal(err)
	}
	retired, err := h.planTerms(t.Context(), family)
	if err != nil || !retired.Retired || retired.Available || retired.VersionID != id {
		t.Fatalf("retired family terms=%+v err=%v", retired, err)
	}
	if exact, err := h.planTermsVersion(t.Context(), id, 0); err != nil || exact.VersionID != id || !exact.Retired {
		t.Fatalf("exact version=%+v err=%v", exact, err)
	}
	if _, err := h.planTermsVersion(t.Context(), id, 1); !errors.Is(err, errUnknownPlan) {
		t.Fatalf("fee interval on a fee-less version: %v", err)
	}
	if _, err := h.planTerms(t.Context(), "unknownfamily_monthly"); !errors.Is(err, errUnknownPlan) {
		t.Fatalf("unknown key: %v", err)
	}
	var offers int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM billing_plan WHERE family=$1`, family).Scan(&offers); err != nil || offers != 0 {
		t.Fatalf("fee-less family created checkout offers: %d %v", offers, err)
	}
}

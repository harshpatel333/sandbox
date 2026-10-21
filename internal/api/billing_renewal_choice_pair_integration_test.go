//go:build integration

package api

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRenewalChoicePublishedCatalogMapping(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("migrated composition database not configured")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || (cfg.ConnConfig.Host != "127.0.0.1" && cfg.ConnConfig.Host != "localhost") || !strings.HasSuffix(cfg.ConnConfig.Database, "_test") {
		t.Fatal("requires loopback migrated test database")
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	mapping := newPairFixture(t).p.Terms
	annual, err := loadRenewalChoiceTerms(t.Context(), pool, "annual", 1, mapping)
	if err != nil {
		t.Fatal(err)
	}
	if annual.Offer != "pro_annual" || annual.Amount != 1200000 || len(annual.Allowances) != 12 {
		t.Fatal(annual)
	}
	mapping.FeeAmount = 120000
	mapping.FeeMonths = 1
	mapping.FeePriceID = "price_monthly_fixture"
	monthly, err := loadRenewalChoiceTerms(t.Context(), pool, "monthly", 1, mapping)
	if err != nil {
		t.Fatal(err)
	}
	if monthly.Amount != 120000 || len(monthly.Allowances) != 1 || monthly.Allowances[0] != 100000 || monthly.Price != mapping.FeePriceID {
		t.Fatal(monthly)
	}
	mapping.FeeAmount++
	if _, err = loadRenewalChoiceTerms(t.Context(), pool, "monthly", 1, mapping); err == nil {
		t.Fatal("provider price economics differed from published version")
	}
	mapping.FeeAmount--
	if _, err = loadRenewalChoiceTerms(t.Context(), pool, "monthly", 2, mapping); err == nil {
		t.Fatal("unpublished version resolved")
	}
}

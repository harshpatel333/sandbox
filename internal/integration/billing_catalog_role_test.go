//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/superserve-ai/sandbox/internal/api"
	"github.com/superserve-ai/sandbox/internal/config"
	"github.com/superserve-ai/sandbox/internal/db"
)

func TestIntegration_BillingCatalogProductionRole(t *testing.T) {
	owner := teamCreationDatabase(t, func(pool *pgxpool.Pool) {
		rolloutExec(t, pool, `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO anon,authenticated,service_role;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO anon,authenticated,service_role;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT EXECUTE ON FUNCTIONS TO anon,authenticated,service_role`)
	})
	service := teamCreationServiceRolePool(t, owner)
	var role string
	var super bool
	if err := service.QueryRow(t.Context(), `SELECT current_user,rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&role, &super); err != nil || role != "service_role" || super {
		t.Fatalf("role=%s super=%v err=%v", role, super, err)
	}
	actor, outsider := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{actor, outsider} {
		rolloutExec(t, owner, `INSERT INTO profile(id,email,provider,provider_id) VALUES($1,$2,'google',$2)`, id, id.String()+"@superserve.ai")
	}
	rolloutExec(t, owner, `INSERT INTO user_role_assignments(user_id,role_id,scope_type) SELECT $1,id,'platform' FROM roles WHERE name='platform_admin'`, actor)
	t.Setenv("INTERNAL_API_TOKEN", internalRBACToken)
	t.Setenv("OPERATOR_API_TOKEN", operatorRBACToken)
	h := api.NewHandlers(&stubVMD{}, db.New(service), &config.Config{SystemTeamID: testSystemTeamID.String()})
	h.Pool = service
	router := api.SetupRouter(t.Context(), h, service)
	body := `{"family":"example","kind":"custom","display_name":"Example","limits":{"max_concurrent":12},"reason":"Catalog maintenance"}`
	if w := operatorRequest(router, http.MethodPost, "/internal/billing/catalog/plans", outsider.String(), body); w.Code != http.StatusForbidden {
		t.Fatalf("unauthorized create=%d %s", w.Code, w.Body)
	}
	if w := doInternal(router, http.MethodPost, "/internal/billing/catalog/plans", actor.String(), body); w.Code != http.StatusUnauthorized {
		t.Fatalf("internal credential create=%d %s", w.Code, w.Body)
	}
	t.Run("create", func(t *testing.T) {
		w := operatorRequest(router, http.MethodPost, "/internal/billing/catalog/plans", actor.String(), body)
		if w.Code != http.StatusCreated {
			t.Fatalf("create=%d %s", w.Code, w.Body)
		}
	})
	t.Run("zero custom templates", func(t *testing.T) {
		var retained, unchanged, zero bool
		if err := owner.QueryRow(t.Context(), `SELECT old.state='retired' AND old.max_templates=10,
 (to_jsonb(old)-ARRAY['id','version','state','max_templates','created_by','created_at','updated_at','published_by','published_at','retired_by','retired_at']) =
 (to_jsonb(current)-ARRAY['id','version','state','max_templates','created_by','created_at','updated_at','published_by','published_at','retired_by','retired_at']),
 current.max_templates=0 AND current.id<>old.id
 FROM billing_plan_version old JOIN billing_plan_version current ON current.family=old.family AND current.pricing_version=old.pricing_version AND current.state='published'
 WHERE old.id=md5('billing-plan-version:v2:free:1')::uuid`).Scan(&retained, &unchanged, &zero); err != nil || !retained || !unchanged || !zero {
			t.Fatalf("retained=%v unchanged=%v zero=%v err=%v", retained, unchanged, zero, err)
		}
		if w := doInternal(router, http.MethodGet, "/internal/billing/catalog", actor.String(), ""); w.Code != http.StatusOK {
			t.Fatalf("catalog=%d %s", w.Code, w.Body)
		} else {
			var catalog struct {
				Plans []struct {
					PricingVersion string `json:"pricing_version"`
					Family         string `json:"family"`
					State          string `json:"state"`
					Limits         struct {
						MaxTemplates *int `json:"max_templates"`
					} `json:"limits"`
				} `json:"plans"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, p := range catalog.Plans {
				if p.PricingVersion == "v2" && p.Family == "free" && p.State == "published" {
					found = p.Limits.MaxTemplates != nil && *p.Limits.MaxTemplates == 0
				}
			}
			if !found {
				t.Fatal("published Free lost zero custom templates in catalog response")
			}
		}
		zeroBody := strings.Replace(body, `"family":"example"`, `"family":"examplezero"`, 1)
		zeroBody = strings.Replace(zeroBody, `"max_concurrent":12`, `"max_concurrent":12,"max_templates":0`, 1)
		if w := operatorRequest(router, http.MethodPost, "/internal/billing/catalog/plans", actor.String(), zeroBody); w.Code != http.StatusCreated {
			t.Fatalf("create zero=%d %s", w.Code, w.Body)
		}
		if w := operatorRequest(router, http.MethodPost, "/internal/billing/catalog/plans", actor.String(), strings.Replace(zeroBody, `"max_templates":0`, `"max_templates":-1`, 1)); w.Code != http.StatusBadRequest {
			t.Fatalf("negative templates=%d %s", w.Code, w.Body)
		}
		rolloutExec(t, owner, `INSERT INTO plan_limits(offer_key,max_sandboxes,max_templates) VALUES('example_zero',1,0)`)
		if _, err := owner.Exec(t.Context(), `UPDATE plan_limits SET max_templates=-1 WHERE offer_key='example_zero'`); err == nil {
			t.Fatal("negative plan template limit accepted")
		}
		var before, after string
		readFree := `SELECT jsonb_agg(to_jsonb(v) ORDER BY version)::text FROM billing_plan_version v WHERE family='free' AND pricing_version='v2'`
		if err := owner.QueryRow(t.Context(), readFree).Scan(&before); err != nil {
			t.Fatal(err)
		}
		dir, err := findMigrationsDir()
		if err != nil {
			t.Fatal(err)
		}
		migration, err := os.ReadFile(filepath.Join(dir, "20261025060000_free_custom_template_limit.sql"))
		if err != nil {
			t.Fatal(err)
		}
		rolloutExec(t, owner, string(migration))
		if err := owner.QueryRow(t.Context(), readFree).Scan(&after); err != nil || before != after {
			t.Fatalf("migration retry changed Free history: %v", err)
		}
		for _, hold := range []string{
			`SELECT 1 FROM billing_plan_version LIMIT 1`,
			`SELECT pg_advisory_xact_lock(hashtext('billing-catalog:free:v2')::bigint)`,
		} {
			tx, err := owner.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(t.Context(), hold); err != nil {
				_ = tx.Rollback(t.Context())
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			_, err = owner.Exec(ctx, string(migration))
			cancel()
			_ = tx.Rollback(t.Context())
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "55P03" {
				t.Fatalf("migration must fail fast for %s: %v", hold, err)
			}
		}
	})
	for _, action := range []string{"update", "delete", "publish", "retire"} {
		t.Run(action, func(t *testing.T) {
			id := uuid.New()
			family := "example" + strings.ReplaceAll(id.String(), "-", "")
			rolloutExec(t, owner, `INSERT INTO billing_plan_version(id,family,version,pricing_version,kind,display_name,max_sandboxes) VALUES($1,$2,1,'legacy','custom','Example',12)`, id, family)
			method, path, payload := http.MethodPost, "/internal/billing/catalog/plans/"+id.String(), `{"reason":"Catalog maintenance"}`
			switch action {
			case "update":
				method = http.MethodPut
				payload = strings.Replace(strings.Replace(body, `"example"`, fmt.Sprintf("%q", family), 1), `"Example"`, `"Updated"`, 1)
			case "delete":
				method = http.MethodDelete
			case "publish":
				rolloutExec(t, owner, `UPDATE billing_plan_version SET kind='subscription',monthly_fee_cents=120000 WHERE id=$1`, id)
				path += "/publish"
			case "retire":
				rolloutExec(t, owner, `SELECT publish_billing_plan_version($1,$2)`, id, actor)
				path += "/retire"
			}
			w := operatorRequest(router, method, path, actor.String(), payload)
			if w.Code != http.StatusOK && !(action == "delete" && w.Code == http.StatusNoContent) {
				t.Fatalf("%s=%d %s", action, w.Code, w.Body)
			}
			if action == "publish" {
				var fee int64
				var cap int32
				if err := service.QueryRow(t.Context(), `SELECT p.charge_amount_cents,l.max_sandboxes FROM billing_plan p JOIN plan_limits l USING(offer_key) WHERE p.offer_key=$1`, family+"_monthly").Scan(&fee, &cap); err != nil || fee != 120000 || cap != 12 {
					t.Fatalf("synced fee=%d cap=%d err=%v", fee, cap, err)
				}
			}
			var count int
			if err := owner.QueryRow(t.Context(), `SELECT count(*) FROM billing_catalog_audit WHERE version_id=$1`, id).Scan(&count); err != nil || count != 1 {
				t.Fatalf("audit count=%d err=%v", count, err)
			}
		})
	}
	t.Run("frozen contract insert", func(t *testing.T) {
		q := db.New(owner)
		team, err := q.CreateTeam(t.Context(), "catalog-role")
		if err != nil {
			t.Fatal(err)
		}
		anchor := time.Now().UTC().Add(-time.Hour)
		customer, subscription := "cus_"+uuid.NewString(), "sub_"+uuid.NewString()
		rolloutExec(t, owner, `INSERT INTO team_billing_account(team_id,stripe_customer_id,stripe_subscription_id,commercial_billing_anchor) VALUES($1,$2,$3,$4)`, team.ID, customer, subscription, anchor)
		c, err := q.CreateCommercialContract(t.Context(), db.CreateCommercialContractParams{TeamID: team.ID, OfferKey: "pro_monthly", StartsAt: anchor, CustomerID: customer, SubscriptionID: subscription, BasePriceID: "price_example", ChargeAmountCents: 120000, ChargeIntervalMonths: 1, EntitlementKind: "metered_usage", EntitlementAmountCents: 100000, EntitlementCadence: "monthly", EntitlementAnchor: anchor})
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err = service.Exec(t.Context(), `INSERT INTO billing_contract_limits(contract_id,max_sandboxes,max_template_vcpu,max_template_memory_mib,max_template_disk_mib,max_templates) VALUES($1,12,4,4096,8192,0) ON CONFLICT DO NOTHING`, c.ID); err != nil {
				t.Fatal(err)
			}
		}
		var limit int
		if err = service.QueryRow(t.Context(), `SELECT max_sandboxes FROM billing_contract_limits WHERE contract_id=$1`, c.ID).Scan(&limit); err != nil || limit != 12 {
			t.Fatalf("limit=%d err=%v", limit, err)
		}
	})
	t.Run("denied authority", func(t *testing.T) {
		for _, sql := range []string{`UPDATE billing_pricing_switch SET pricing_version='v2'`, `UPDATE billing_plan_version SET state='retired'`, `UPDATE billing_catalog_audit SET reason='changed'`, `DELETE FROM billing_catalog_audit`, `TRUNCATE billing_catalog_audit`, `UPDATE billing_contract_limits SET max_sandboxes=1`, `DELETE FROM billing_contract_limits`, `TRUNCATE billing_contract_limits`} {
			_, err := service.Exec(t.Context(), sql)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "42501" {
				t.Errorf("%s: %v", sql, err)
			}
		}
		for _, role := range []string{"anon", "authenticated"} {
			for _, sql := range []string{`INSERT INTO billing_plan_version(family,version,kind,display_name) VALUES('denied',1,'custom','Denied')`, `SELECT publish_billing_plan_version(NULL,NULL)`, `SELECT retire_billing_plan_version(NULL,NULL)`} {
				tx, err := owner.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if _, err = tx.Exec(t.Context(), "SET LOCAL ROLE "+role); err != nil {
					t.Fatal(err)
				}
				_, err = tx.Exec(t.Context(), sql)
				_ = tx.Rollback(t.Context())
				var pgerr *pgconn.PgError
				if !errors.As(err, &pgerr) || pgerr.Code != "42501" {
					t.Errorf("%s %s: %v", role, sql, err)
				}
			}
		}
	})
	t.Run("successor failure rolls back constraints", func(t *testing.T) {
		rolloutExec(t, owner, `ALTER TABLE plan_limits DROP CONSTRAINT plan_limits_max_templates_check,
 ADD CONSTRAINT plan_limits_max_templates_check CHECK (max_templates BETWEEN 0 AND 99)`)
		rolloutExec(t, owner, `SELECT retire_billing_plan_version(id,NULL) FROM billing_plan_version WHERE family='free' AND pricing_version='v2' AND state='published'`)
		query := `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='plan_limits'::regclass AND conname='plan_limits_max_templates_check'`
		var before, after string
		if err := owner.QueryRow(t.Context(), query).Scan(&before); err != nil {
			t.Fatal(err)
		}
		dir, err := findMigrationsDir()
		if err != nil {
			t.Fatal(err)
		}
		migration, err := os.ReadFile(filepath.Join(dir, "20261025060000_free_custom_template_limit.sql"))
		if err != nil {
			t.Fatal(err)
		}
		prelude, successor, ok := strings.Cut(string(migration), "DO $$\nDECLARE")
		if !ok {
			t.Fatal("successor statement not found")
		}
		conn, err := owner.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err = conn.Exec(t.Context(), prelude); err != nil {
			t.Fatal(err)
		}
		_, err = conn.Exec(t.Context(), "DO $$\nDECLARE"+successor)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "P0002" {
			t.Fatalf("missing Free must abort successor: %v", err)
		}
		if _, err = conn.Exec(t.Context(), "ROLLBACK"); err != nil {
			t.Fatal(err)
		}
		if err = conn.QueryRow(t.Context(), query).Scan(&after); err != nil || before != after {
			t.Fatalf("failed successor committed constraint changes: before=%s after=%s err=%v", before, after, err)
		}
	})
}

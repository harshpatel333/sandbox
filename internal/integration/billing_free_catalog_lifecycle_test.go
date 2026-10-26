//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func freeCatalogSnapshot(t *testing.T) string {
	t.Helper()
	var snapshot string
	if err := testPool.QueryRow(t.Context(), `SELECT jsonb_build_object(
 'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY version) FROM billing_plan_version v WHERE family='free' AND pricing_version='v2'),
 'audit',(SELECT COALESCE(jsonb_agg(to_jsonb(a) ORDER BY id),'[]'::jsonb) FROM billing_catalog_audit a))::text`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func signupFreeVersion(t *testing.T, expected uuid.UUID, zeroTemplates bool) uuid.UUID {
	t.Helper()
	team, err := testQueries.CreateTeam(t.Context(), "free-catalog-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	var version uuid.UUID
	var kind string
	var templates *int32
	var allowance string
	if err = testPool.QueryRow(t.Context(), `SELECT plan_version_id,kind,(limits->>'max_templates')::integer,(terms->'allowance')::text FROM team_plan_assignment WHERE team_id=$1 AND effective_to IS NULL`, team.ID).Scan(&version, &kind, &templates, &allowance); err != nil {
		t.Fatal(err)
	}
	if version != expected || kind != "free" || allowance == "null" || (zeroTemplates && (templates == nil || *templates != 0)) {
		t.Fatalf("signup version=%s kind=%s templates=%v allowance=%s", version, kind, templates, allowance)
	}
	if zeroTemplates {
		var effective int32
		if err := testPool.QueryRow(t.Context(), `SELECT max_templates FROM team_effective_limits($1)`, team.ID).Scan(&effective); err != nil || effective != 0 {
			t.Fatalf("zero template cap fell back: effective=%d err=%v", effective, err)
		}
	}
	var positive bool
	if err = testPool.QueryRow(t.Context(), `SELECT allowance_usd>0 FROM billing_free_allowance_eval($1)`, team.ID).Scan(&positive); err != nil || !positive {
		t.Fatalf("signup allowance positive=%v err=%v", positive, err)
	}
	return team.ID
}

func TestIntegration_PlanCatalogActiveFreeCannotBeRetired(t *testing.T) {
	setPricingSwitch(t, "v2")
	r := newInternalRouter(t)
	admin := seedPlatformAdminProfile(t).String()
	var current uuid.UUID
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM billing_plan_version WHERE family='free' AND pricing_version='v2' AND state='published'`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	before := freeCatalogSnapshot(t)
	w := operatorRequest(r, http.MethodPost, "/internal/billing/catalog/plans/"+current.String()+"/retire", admin, `{"reason":"withdraw default"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("retire=%d %s", w.Code, w.Body)
	}
	if after := freeCatalogSnapshot(t); after != before {
		t.Fatal("rejected retirement changed catalog or audit")
	}
	signupFreeVersion(t, current, false)
}

func TestIntegration_PlanCatalogInvalidFreeReplacementIsAtomic(t *testing.T) {
	setPricingSwitch(t, "v2")
	r := newInternalRouter(t)
	admin := seedPlatformAdminProfile(t).String()
	var current uuid.UUID
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM billing_plan_version WHERE family='free' AND pricing_version='v2' AND state='published'`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	makeDraft := func(set string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := testPool.QueryRow(t.Context(), `INSERT INTO billing_plan_version(family,version,`+catalogCopyColumns+`)
   SELECT family,(SELECT MAX(version)+1 FROM billing_plan_version x WHERE x.family=v.family),`+catalogCopyColumns+` FROM billing_plan_version v WHERE id=$1 RETURNING id`, current).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(t.Context(), `UPDATE billing_plan_version SET `+set+` WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	for _, set := range []string{
		"allowance_period=NULL,allowance_on_exhausted=NULL,allowance_cents=NULL,allowance_compute_seconds=NULL,allowance_compute_vcpu=NULL,allowance_compute_memory_gib=NULL",
		"kind='prepaid'", "allowance_period='month'", "allowance_on_exhausted='payg'", "max_template_vcpu=NULL",
	} {
		t.Run(set, func(t *testing.T) {
			id := makeDraft(set)
			before := freeCatalogSnapshot(t)
			w := operatorRequest(r, http.MethodPost, "/internal/billing/catalog/plans/"+id.String()+"/publish", admin, `{"reason":"replace default"}`)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("invalid publish=%d %s", w.Code, w.Body)
			}
			if _, err := testPool.Exec(t.Context(), `SELECT publish_billing_plan_version($1,NULL)`, id); err == nil {
				t.Fatal("SQL bypass published unusable Free")
			}
			if after := freeCatalogSnapshot(t); after != before {
				t.Fatal("rejected replacement changed predecessor, draft or audit")
			}
			signupFreeVersion(t, current, false)
		})
	}
	// Publishing a usable replacement and retiring its predecessor serialize on
	// the existing family lock; neither ordering may leave signup without Free.
	replacement := makeDraft("max_templates=0,allowance_cents=1,allowance_compute_seconds=NULL,allowance_compute_vcpu=NULL,allowance_compute_memory_gib=NULL")
	t.Cleanup(func() { publishPlanCopy(t, current, "") })
	errs := make(chan error, 2)
	go func() {
		_, err := testPool.Exec(t.Context(), `SELECT publish_billing_plan_version($1,NULL)`, replacement)
		errs <- err
	}()
	go func() {
		_, err := testPool.Exec(t.Context(), `SELECT retire_billing_plan_version($1,NULL)`, current)
		errs <- err
	}()
	for range 2 {
		if err := <-errs; err != nil && !strings.Contains(err.Error(), "active v2 Free must be replaced") && !strings.Contains(err.Error(), "only the published version can be retired") {
			t.Fatal(err)
		}
	}
	var published uuid.UUID
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM billing_plan_version WHERE family='free' AND pricing_version='v2' AND state='published'`).Scan(&published); err != nil || published != replacement {
		t.Fatalf("replacement=%s err=%v", published, err)
	}
	signupFreeVersion(t, replacement, true)
	var beforeState string
	if err := testPool.QueryRow(t.Context(), `SELECT state FROM billing_plan_version WHERE id=$1`, current).Scan(&beforeState); err != nil || beforeState != "retired" {
		t.Fatal(fmt.Sprint(beforeState, err))
	}
}

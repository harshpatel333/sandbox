//go:build integration

package api

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
)

// Exercise the real Go snapshot serializer, including the compute allowance's
// resolved display value being removed before terms are frozen.
func TestPlanAssignmentFreeZeroTemplatesGoSnapshot(t *testing.T) {
	h, _ := newPlanAssignmentFixture(t)
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := tx.Exec(t.Context(), q, args...); e != nil {
			t.Fatal(e)
		}
	}
	helpers, err := os.ReadFile("../../scripts/billing/free-zero-templates.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(helpers))
	exec(`UPDATE billing_pricing_switch SET pricing_version='v2'`)
	terms, err := h.planTermsForPricingVersion(t.Context(), "free", "v2", tx)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := json.Marshal(freezePlanTerms(terms))
	if err != nil {
		t.Fatal(err)
	}
	limits, err := json.Marshal(terms.Limits)
	if err != nil {
		t.Fatal(err)
	}
	team, actor := uuid.New(), uuid.New()
	exec(`INSERT INTO profile(id,email,provider,provider_id) VALUES($1::uuid,$1::uuid::text||'@example.com','google',$1::uuid::text)`, actor)
	exec(`INSERT INTO team(id,name) VALUES($1,'Go frozen Free')`, team)
	var current uuid.UUID
	if err = tx.QueryRow(t.Context(), `WITH closed AS (UPDATE team_plan_assignment SET effective_to=clock_timestamp() WHERE team_id=$1 AND effective_to IS NULL RETURNING effective_to) INSERT INTO team_plan_assignment(team_id,plan_key,kind,plan_version_id,terms,limits,effective_from,source,reason) SELECT $1,'free','free',$2,$3,$4,effective_to,'migration','Go serializer fixture' FROM closed RETURNING id`, team, terms.VersionID, frozen, limits).Scan(&current); err != nil {
		t.Fatal(err)
	}
	replacement := uuid.New()
	exec(`SELECT pg_temp.publish_free_zero_templates($1,$2,$3,'Go snapshot acceptance',true)`, terms.VersionID, replacement, actor)
	exec(`SELECT pg_temp.transition_free_zero_templates($1,$2,$3,$4,'Go snapshot acceptance')`, team, current, replacement, actor)
}

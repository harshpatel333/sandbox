//go:build integration

package api

import (
	"testing"

	"github.com/google/uuid"
)

func TestPlanAssignmentAppliesCatalogSnapshotCap(t *testing.T) {
	h, team := newPlanAssignmentFixture(t)
	setPricingVersion(t, h, "v2")
	var version uuid.UUID
	if err := h.Pool.QueryRow(t.Context(), `INSERT INTO billing_plan_version(family,version,pricing_version,kind,display_name,max_sandboxes,max_snapshots)
 VALUES($1,1,'v2','custom','Snapshot fixture',2,7) RETURNING id`, "snapcap_"+uuid.NewString()[:8]).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE billing_plan_version SET state='published',published_at=clock_timestamp() WHERE id=$1`, version); err != nil {
		t.Fatal(err)
	}
	terms, err := h.planTermsVersion(t.Context(), version, 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := func() int32 {
		t.Helper()
		var n int32
		if err := h.Pool.QueryRow(t.Context(), `SELECT max_snapshots FROM team WHERE id=$1`, team).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := snapshots()
	if _, err = assignInTx(t, h, planAssignment{Team: team, Terms: testPlanTerms("uncapped", planKindCustom, 1, nil), Source: planSourceStaff, Reason: "plan without a snapshot cap"}); err != nil {
		t.Fatal(err)
	}
	if got := snapshots(); got != before {
		t.Fatal("a plan without a snapshot cap changed the team's cap", before, got)
	}
	if _, err = assignInTx(t, h, planAssignment{Team: team, Terms: terms, Source: planSourceStaff, Reason: "catalog snapshot cap"}); err != nil {
		t.Fatal(err)
	}
	if got := snapshots(); got != 7 {
		t.Fatal("catalog snapshot cap not applied to the team", got)
	}
	if _, err = h.Pool.Exec(t.Context(), `UPDATE team SET limits_customized=true,max_snapshots=11 WHERE id=$1`, team); err != nil {
		t.Fatal(err)
	}
	if _, err = assignInTx(t, h, planAssignment{Team: team, Terms: terms, Source: planSourceStaff, Reason: "customized limits stay"}); err != nil {
		t.Fatal(err)
	}
	if got := snapshots(); got != 11 {
		t.Fatal("assignment overrode a hand-set snapshot cap", got)
	}
}

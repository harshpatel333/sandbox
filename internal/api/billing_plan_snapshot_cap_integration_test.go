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
	// A plan assigned in SQL without listing the cap still freezes and applies it.
	if _, err = h.Pool.Exec(t.Context(), `UPDATE team_plan_assignment SET effective_to=clock_timestamp() WHERE team_id=$1 AND effective_to IS NULL`, team); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Pool.Exec(t.Context(), `UPDATE team SET max_snapshots=100 WHERE id=$1`, team); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Pool.Exec(t.Context(), `UPDATE team SET limits_customized=false WHERE id=$1`, team); err != nil {
		t.Fatal(err)
	}
	var frozen *int32
	if err = h.Pool.QueryRow(t.Context(), `INSERT INTO team_plan_assignment(team_id,plan_key,kind,plan_version_id,terms,limits,effective_from,source,reason)
 VALUES($1,'snapshot_fixture','custom',$2,'{}','{"max_concurrent":2}',clock_timestamp(),'staff','SQL-written assignment') RETURNING (limits->>'max_snapshots')::integer`, team, version).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if frozen == nil || *frozen != 7 || snapshots() != 7 {
		t.Fatal("an SQL-written assignment did not freeze and apply the catalog cap", frozen, snapshots())
	}
	if _, err = h.Pool.Exec(t.Context(), `UPDATE team SET max_snapshots=11 WHERE id=$1`, team); err != nil {
		t.Fatal(err)
	}
	if _, err = assignInTx(t, h, planAssignment{Team: team, Terms: terms, Source: planSourceStaff, Reason: "customized limits stay"}); err != nil {
		t.Fatal(err)
	}
	if got := snapshots(); got != 11 {
		t.Fatal("assignment overrode a hand-set snapshot cap", got)
	}
}

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestIntegration_BillingPlanWithoutAssignment(t *testing.T) {
	teamID, key := seedTeamAndKey(t)
	r := newRouter(t)
	w := do(r, "GET", "/billing/plan", key, "")
	var body struct {
		Plan   *json.RawMessage `json:"plan"`
		Period struct {
			Start time.Time `json:"start"`
			End   time.Time `json:"end"`
		} `json:"period"`
		Pending *json.RawMessage `json:"pending_change"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("billing plan: %d %s", w.Code, w.Body.String())
	}
	if body.Plan != nil || body.Pending != nil || !body.Period.End.After(body.Period.Start) {
		t.Fatalf("team %s without an assignment: %s", teamID, w.Body.String())
	}
}

func TestIntegration_BillingPlanIgnoresAssignmentsWhilePricingV2IsOff(t *testing.T) {
	ctx := context.Background()
	teamID, key := seedTeamAndKey(t)
	if _, err := testPool.Exec(ctx, `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason)
        VALUES($1,'starter','prepaid','{"display_name":"Starter"}','{"max_sandboxes":100}',clock_timestamp(),'credit_purchase','first purchase')`, teamID); err != nil {
		t.Fatal(err)
	}
	r := newRouter(t)
	w := do(r, "GET", "/billing/plan", key, "")
	var body struct {
		Plan *json.RawMessage `json:"plan"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Plan != nil {
		t.Fatalf("assignment shown while pricing v2 is off: %d %s", w.Code, w.Body.String())
	}
	w = do(r, "GET", "/billing/plan-change-preview?target_plan=pro_monthly", key, "")
	var preview struct {
		Blocked string `json:"blocked"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &preview) != nil || preview.Blocked == "" {
		t.Fatalf("plan change previewed while pricing v2 is off: %d %s", w.Code, w.Body.String())
	}
}


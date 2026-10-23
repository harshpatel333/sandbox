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

func TestIntegration_BillingPlanShowsAssignment(t *testing.T) {
	ctx := context.Background()
	teamID, key := seedTeamAndKey(t)
	viewerKey := seedKeyForExistingTeamWithRole(t, teamID, "viewer")
	if _, err := testPool.Exec(ctx, `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason)
        VALUES($1,'starter','prepaid','{"display_name":"Starter"}','{"max_sandboxes":100,"max_template_vcpu":2,"max_template_memory_mib":4096,"max_template_disk_mib":51200}',clock_timestamp(),'credit_purchase','first purchase')`, teamID); err != nil {
		t.Fatal(err)
	}
	r := newRouter(t)
	for _, k := range []string{key, viewerKey} {
		w := do(r, "GET", "/billing/plan", k, "")
		var body struct {
			Plan *struct {
				PlanKey     string `json:"plan_key"`
				Kind        string `json:"kind"`
				DisplayName string `json:"display_name"`
				Limits      struct {
					MaxConcurrent int32  `json:"max_concurrent"`
					MaxVCPU       *int32 `json:"max_vcpu"`
					MaxMemoryMiB  *int32 `json:"max_memory_mib"`
					MaxDiskMiB    *int32 `json:"max_disk_mib"`
				} `json:"limits"`
			} `json:"plan"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Plan == nil {
			t.Fatalf("billing plan: %d %s", w.Code, w.Body.String())
		}
		p := body.Plan
		if p.PlanKey != "starter" || p.Kind != "prepaid" || p.DisplayName != "Starter" || p.Limits.MaxConcurrent != 100 ||
			p.Limits.MaxVCPU == nil || *p.Limits.MaxVCPU != 2 || p.Limits.MaxMemoryMiB == nil || *p.Limits.MaxMemoryMiB != 4096 || p.Limits.MaxDiskMiB == nil || *p.Limits.MaxDiskMiB != 51200 {
			t.Fatalf("plan not reported as assigned: %s", w.Body.String())
		}
	}
}

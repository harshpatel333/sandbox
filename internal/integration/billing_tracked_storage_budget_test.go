//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/superserve-ai/sandbox/internal/api"
	"github.com/superserve-ai/sandbox/internal/config"
	"github.com/superserve-ai/sandbox/internal/db"
)

// Tracked storage can outlast a page request for large teams. Past its budget
// the billing pages must still load with charges and report storage unknown.
func TestIntegration_BillingPagesReportTrackedStorageUnknownPastBudget(t *testing.T) {
	team, key, _ := seedTeamAndKeyWithRole(t, "viewer")
	start := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)
	end := start.Add(3 * time.Hour)
	plan := "tracked-budget-" + uuid.NewString()
	insertPricingPlanForTest(t, t.Context(), plan, true)
	storageExec(t, `INSERT INTO pricing_rate(plan_key,resource,unit,price_usd,effective_from)
		VALUES($1,'vcpu','second',0.000011,$2),($1,'memory_gib','second',0.0000045,$2),($1,'storage_gib','second',0.00000003,$2)`, plan, start.Add(-time.Hour))
	storageExec(t, `INSERT INTO team_pricing_plan(team_id,plan_key,effective_from) VALUES($1,$2,$3)`, team, plan, start.Add(-time.Hour))
	storageExec(t, `INSERT INTO team_billing_account(team_id) VALUES($1)`, team)
	storageExec(t, `INSERT INTO team_billing_period(team_id,period_start,period_end,status) VALUES($1,$2,$3,'open')`, team, start, end)
	sandbox := uuid.New()
	storageExec(t, `INSERT INTO sandbox(id,team_id,name,status,vcpu_count,memory_mib,host_id,created_at,destroyed_at)
		VALUES($1,$2,'budget-sandbox','deleted',1,1024,'default',$3,$4)`, sandbox, team, start, start.Add(time.Hour))
	storageExec(t, `INSERT INTO sandbox_storage_interval(sandbox_id,team_id,host_id,disk_mib,started_at,ended_at,end_reason)
		VALUES($1,$2,'default',1024,$3,$4,'deleted')`, sandbox, team, start, start.Add(time.Hour))
	storageExec(t, `INSERT INTO sandbox_compute_billing_interval(sandbox_id,team_id,vcpu_count,memory_mib,started_at,ended_at,end_reason)
		VALUES($1,$2,1,1024,$3,$4,'paused')`, sandbox, team, start, start.Add(time.Hour))

	t.Setenv("INTERNAL_API_TOKEN", internalRBACToken)
	h := api.NewHandlers(&stubVMD{}, db.New(testPool), &config.Config{
		Port: "0", VMDAddress: "localhost:0", SystemTeamID: testSystemTeamID.String(),
	})
	h.Pool = testPool
	h.TrackedStorageBudget = time.Nanosecond
	router := api.SetupRouter(t.Context(), h, testPool)

	w := do(router, http.MethodGet, "/billing/summary", key, "")
	if w.Code != http.StatusOK {
		t.Fatalf("summary: %d %s", w.Code, w.Body.String())
	}
	var summary struct {
		Resources map[string]struct {
			Usage *float64 `json:"usage"`
		} `json:"resources_by_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if cpu := summary.Resources["vcpu"].Usage; cpu == nil || *cpu != 3600 {
		t.Fatalf("summary vcpu usage = %v, want 3600", cpu)
	}
	if storage := summary.Resources["storage_gib"].Usage; storage != nil {
		t.Fatalf("summary storage usage = %v, want unknown past the budget", *storage)
	}

	w = do(router, http.MethodGet, "/billing/usage-series?start="+start.Format(time.RFC3339)+"&end="+end.Format(time.RFC3339)+"&granularity=hour&timezone=UTC", key, "")
	if w.Code != http.StatusOK {
		t.Fatalf("usage series: %d %s", w.Code, w.Body.String())
	}
	var series struct {
		Buckets []struct {
			CPU     struct{ Usage *float64 } `json:"cpu"`
			Storage struct{ Usage *float64 } `json:"storage"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &series); err != nil {
		t.Fatal(err)
	}
	if len(series.Buckets) != 3 || series.Buckets[0].CPU.Usage == nil || *series.Buckets[0].CPU.Usage != 3600 {
		t.Fatalf("usage series compute missing: %s", w.Body.String())
	}
	for i, bucket := range series.Buckets {
		if bucket.Storage.Usage != nil {
			t.Fatalf("bucket %d storage usage = %v, want unknown past the budget", i, *bucket.Storage.Usage)
		}
	}
}

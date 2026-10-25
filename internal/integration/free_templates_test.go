//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/superserve-ai/sandbox/internal/api"
	"github.com/superserve-ai/sandbox/internal/db"
)

func TestIntegration_FreeTemplates_RejectNewBuildsRetainArtifactsAndReplay(t *testing.T) {
	team, key := seedTeamAndKey(t)
	router := newRouter(t)
	created := do(router, "POST", "/templates", key, `{"name":"retained-custom","build_spec":{"from":"debian:12-slim"}}`)
	if created.Code != http.StatusAccepted {
		t.Fatalf("legacy template: %d %s", created.Code, created.Body.String())
	}
	var template struct {
		ID      string `json:"id"`
		BuildID string `json:"build_id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &template); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO team_plan_assignment
     (team_id,plan_key,kind,terms,limits,effective_from,source,reason)
     VALUES($1,'free','free','{}','{"max_concurrent":1,"max_vcpu":1,"max_memory_mib":1024,"max_disk_mib":4096,"max_templates":10}',now()-interval '1 hour','migration','retained Free terms')`, team); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE team SET max_templates=100, limits_customized=true WHERE id=$1`, team); err != nil {
		t.Fatal(err)
	}

	rejected := do(router, "POST", "/templates", key, `{"name":"new-custom","build_spec":{"from":"debian:12-slim"}}`)
	if rejected.Code != http.StatusForbidden || !strings.Contains(rejected.Body.String(), "custom_templates_unavailable") {
		t.Fatalf("new Free template: %d %s", rejected.Code, rejected.Body.String())
	}
	buildPath := fmt.Sprintf("/templates/%s/builds", template.ID)
	replay := do(router, "POST", buildPath, key, "")
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), template.BuildID) {
		t.Fatalf("accepted build replay: %d %s", replay.Code, replay.Body.String())
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE template_build SET status='failed' WHERE id=$1`, template.BuildID); err != nil {
		t.Fatal(err)
	}
	rebuilt := do(router, "POST", buildPath, key, "")
	if rebuilt.Code != http.StatusForbidden || !strings.Contains(rebuilt.Body.String(), "custom_templates_unavailable") {
		t.Fatalf("new Free rebuild: %d %s", rebuilt.Code, rebuilt.Body.String())
	}
	retained := do(router, "GET", "/templates/"+template.ID, key, "")
	if retained.Code != http.StatusOK {
		t.Fatalf("retained custom template: %d %s", retained.Code, retained.Body.String())
	}
	var templates, builds int
	if err := testPool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM template WHERE team_id=$1),(SELECT count(*) FROM template_build WHERE team_id=$1)`, team).Scan(&templates, &builds); err != nil {
		t.Fatal(err)
	}
	if templates != 1 || builds != 1 {
		t.Fatalf("unexpected mutations: templates=%d builds=%d", templates, builds)
	}
}

func TestIntegration_FreeTemplates_CachedAdmissionCannotCreateFromCustom(t *testing.T) {
	for _, system := range []bool{false, true} {
		t.Run(fmt.Sprint(system), func(t *testing.T) {
			team, key := seedTeamAndKey(t)
			if _, err := testPool.Exec(t.Context(), `INSERT INTO team_credit_grant(team_id,amount_usd,remaining_usd,reason) VALUES($1,5,5,'signup trial credit')`, team); err != nil {
				t.Fatal(err)
			}
			if err := testQueries.RefreshTeamTrialEligibility(t.Context(), team); err != nil {
				t.Fatal(err)
			}
			owner := team
			if system {
				owner = testSystemTeamID
			}
			tpl, err := testQueries.CreateTemplate(t.Context(), db.CreateTemplateParams{TeamID: owner, Name: "policy-" + uuid.NewString(), BuildSpec: []byte(`{"from":"example/image:stable"}`), Vcpu: 1, MemoryMib: 1024, DiskMib: 4096})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := testPool.Exec(t.Context(), `UPDATE template SET status='ready',snapshot_path='/snapshots/policy.snap',mem_path='/snapshots/policy.mem',base_path='/snapshots/policy.ext4' WHERE id=$1`, tpl.ID); err != nil {
				t.Fatal(err)
			}
			h := pauseHandlers(t, &stubVMD{})
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			if !api.RequireBillingEligibleForTest(h, c, team) {
				t.Fatalf("warm legacy admission: %s", w.Body.String())
			}
			assignFreePlan(t, team, time.Now())
			setOwnerHeartbeat(t, testDefaultHostID, time.Now())
			r := api.SetupRouter(t.Context(), h, testPool)
			result := do(r, http.MethodPost, "/sandboxes", key, `{"name":"new-from-template","from_template":"`+tpl.ID.String()+`"}`)
			if !system {
				if result.Code != http.StatusForbidden || !strings.Contains(result.Body.String(), "custom_templates_unavailable") {
					t.Fatalf("custom create: %d %s", result.Code, result.Body.String())
				}
				var n int
				if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM sandbox WHERE team_id=$1`, team).Scan(&n); err != nil || n != 0 {
					t.Fatalf("rejected create persisted %d rows: %v", n, err)
				}
				return
			}
			if result.Code != http.StatusCreated {
				t.Fatalf("system create: %d %s", result.Code, result.Body.String())
			}
			var inherited bool
			if err := testPool.QueryRow(t.Context(), `SELECT system_template FROM sandbox WHERE team_id=$1`, team).Scan(&inherited); err != nil || !inherited {
				t.Fatalf("system provenance=%v: %v", inherited, err)
			}
		})
	}
}

func TestIntegration_FreeTemplates_SnapshotForkPreservesProvenance(t *testing.T) {
	for _, system := range []bool{false, true} {
		t.Run(fmt.Sprint(system), func(t *testing.T) {
			team, _ := seedTeamAndKey(t)
			source := seedPausedSandbox(t, team)
			if _, err := testPool.Exec(t.Context(), `UPDATE sandbox SET system_template=$2,base_path='/base.ext4',vcpu_count=1,memory_mib=1024,disk_mib=4096 WHERE id=$1`, source, system); err != nil {
				t.Fatal(err)
			}
			snapshot, err := testQueries.CreateSandboxSnapshot(t.Context(), db.CreateSandboxSnapshotParams{ID: uuid.New(), TeamID: team, SandboxID: source, Kind: "mem+fs", SweepAfter: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.SystemTemplate != system {
				t.Fatalf("snapshot provenance=%v want %v", snapshot.SystemTemplate, system)
			}
			if _, err := testPool.Exec(t.Context(), `UPDATE sandbox_snapshot SET status='ready',snapshot_path='/saved/snap',mem_path='/saved/mem',overlay_path='/saved/overlay' WHERE id=$1`, snapshot.ID); err != nil {
				t.Fatal(err)
			}
			assignFreePlan(t, team, time.Now())
			fork, err := testQueries.CreateSandboxFromSnapshot(t.Context(), db.CreateSandboxFromSnapshotParams{ID: uuid.New(), TeamID: team, SnapshotID: snapshot.ID, Name: "fork", Status: db.SandboxStatusStarting, Metadata: []byte(`{}`), PreviewAccess: "public", SecretIds: []uuid.UUID{}})
			if system {
				if err != nil || !fork.SystemTemplate {
					t.Fatalf("system fork: %+v %v", fork, err)
				}
				return
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "SS004" {
				t.Fatalf("custom fork: %v", err)
			}
			if _, err := testQueries.GetSandboxSnapshot(context.Background(), db.GetSandboxSnapshotParams{ID: snapshot.ID, TeamID: team}); err != nil {
				t.Fatalf("retained snapshot: %v", err)
			}
		})
	}
}

func TestIntegration_FreeTemplates_MigrationBackfillsRetainedOrigins(t *testing.T) {
	ctx := t.Context()
	team, _ := seedTeamAndKey(t)
	source := seedPausedSandbox(t, team)
	custom, err := testQueries.CreateTemplate(ctx, db.CreateTemplateParams{TeamID: team, Name: "custom-" + uuid.NewString(), BuildSpec: []byte(`{"from":"example/image:stable"}`), Vcpu: 1, MemoryMib: 1024, DiskMib: 4096})
	if err != nil {
		t.Fatal(err)
	}
	system, err := testQueries.CreateTemplate(ctx, db.CreateTemplateParams{TeamID: testSystemTeamID, Name: "system-" + uuid.NewString(), BuildSpec: []byte(`{"from":"example/image:stable"}`), Vcpu: 1, MemoryMib: 1024, DiskMib: 4096})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `UPDATE billing_pricing_adoption SET region='use',system_team_id=$1 WHERE singleton`, testSystemTeamID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`DROP TRIGGER billing_free_template_policy_release ON billing_pricing_adoption`, `DROP TABLE billing_free_template_policy_transition`, `ALTER TABLE team_plan_assignment DROP COLUMN free_allowance_origin_at`, `ALTER TABLE sandbox DROP COLUMN system_template`, `ALTER TABLE sandbox_snapshot DROP COLUMN system_template`} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	old := []struct {
		id, template uuid.UUID
		system       bool
	}{{source, custom.ID, false}, {uuid.New(), system.ID, true}}
	if _, err := tx.Exec(ctx, `UPDATE sandbox SET template_id=$2,base_path='/base.ext4' WHERE id=$1`, source, custom.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sandbox(id,team_id,name,status,host_id,vcpu_count,memory_mib,disk_mib,template_id,base_path) VALUES($1,$2,'old-system','paused','default',1,1024,4096,$3,'/base.ext4')`, old[1].id, team, system.ID); err != nil {
		t.Fatal(err)
	}
	for _, row := range old {
		if _, err := tx.Exec(ctx, `INSERT INTO sandbox_snapshot(team_id,sandbox_id,template_id,kind,host_id,vcpu_count,memory_mib,disk_mib,base_path) VALUES($1,$2,$3,'fs','default',1,1024,4096,'/base.ext4')`, team, row.id, row.template); err != nil {
			t.Fatal(err)
		}
	}
	prefix, err := os.ReadFile(filepath.Join("..", "..", "supabase", "migrations", "20261025140000_free_system_templates.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(prefix)); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile(filepath.Join("..", "..", "supabase", "migrations", "20261025150000_free_template_policy.sql"))
	if err != nil {
		t.Fatal(err)
	}
	migration = []byte(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(string(migration)), "BEGIN;"), "COMMIT;"))
	if _, err := tx.Exec(ctx, `SAVEPOINT unsupported_prefix; ALTER TABLE sandbox ALTER COLUMN system_template SET DEFAULT true`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(migration)); err == nil || !strings.Contains(err.Error(), "unsupported Free enforcement prefix") {
		t.Fatalf("unsupported prefix: %v", err)
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT unsupported_prefix`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sandbox SET system_template=true WHERE id=$1`, source); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	for _, row := range old {
		var sandboxOrigin, snapshotOrigin bool
		if err := tx.QueryRow(ctx, `SELECT s.system_template,ss.system_template FROM sandbox s JOIN sandbox_snapshot ss ON ss.sandbox_id=s.id WHERE s.id=$1`, row.id).Scan(&sandboxOrigin, &snapshotOrigin); err != nil {
			t.Fatal(err)
		}
		if sandboxOrigin != row.system || snapshotOrigin != row.system {
			t.Fatalf("retained provenance %v/%v want %v", sandboxOrigin, snapshotOrigin, row.system)
		}
	}
}

func TestIntegration_FreeTemplates_DowngradeAfterPreflightRejectsBuild(t *testing.T) {
	team, key := seedTeamAndKey(t)
	router := newRouter(t)
	held, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(context.Background())
	var pid int
	if err := held.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := held.Exec(t.Context(), `SELECT pg_advisory_xact_lock(hashtext($1))`, team.String()); err != nil {
		t.Fatal(err)
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- do(router, "POST", "/templates", key, `{"name":"racing-custom","build_spec":{"from":"debian:12-slim"}}`)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting bool
		if err := testPool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("template admission never reached its serialization fence")
		}
		time.Sleep(time.Millisecond)
	}
	assignFreePlan(t, team, time.Now())
	if err := held.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-result:
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "custom_templates_unavailable") {
			t.Fatalf("downgrade race: %d %s", response.Code, response.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("template admission remained blocked")
	}
}

func TestIntegration_FreeTemplates_SecretBindingAdmissionUsesSystemOwner(t *testing.T) {
	for _, system := range []bool{false, true} {
		t.Run(fmt.Sprint(system), func(t *testing.T) {
			team, _ := seedTeamAndKey(t)
			owner := team
			if system {
				owner = testSystemTeamID
			}
			tpl, err := testQueries.CreateTemplate(t.Context(), db.CreateTemplateParams{TeamID: owner, Name: "secrets-" + uuid.NewString(), BuildSpec: []byte(`{"from":"example/image:stable"}`), Vcpu: 1, MemoryMib: 1024, DiskMib: 4096})
			if err != nil {
				t.Fatal(err)
			}
			path := "/template/snapshot"
			mem := "/template/memory"
			base := "/template/base"
			if _, err := testPool.Exec(t.Context(), `UPDATE template SET status='ready',snapshot_path=$2,mem_path=$3,base_path=$4 WHERE id=$1`, tpl.ID, path, mem, base); err != nil {
				t.Fatal(err)
			}
			assignFreePlan(t, team, time.Now())
			row, err := testQueries.CreateSandboxFromTemplateWithSecrets(t.Context(), db.CreateSandboxFromTemplateWithSecretsParams{SystemTeamID: testSystemTeamID, TemplateID: tpl.ID, TeamID: team, SnapshotPath: &path, MemPath: &mem, BasePath: &base, ID: uuid.New(), Name: "secret-admission", Status: db.SandboxStatusStarting, VcpuCount: 1, MemoryMib: 1024, HostID: testDefaultHostID, Metadata: []byte(`{}`), PreviewAccess: "public", SecretIds: []uuid.UUID{}, EnvKeys: []string{}, ProxyTokens: []string{}})
			if system {
				if err != nil || !row.SystemTemplate {
					t.Fatalf("system secret admission: %+v %v", row, err)
				}
				return
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "SS004" {
				t.Fatalf("custom secret admission: %v", err)
			}
		})
	}
}

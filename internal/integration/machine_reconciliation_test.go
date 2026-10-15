//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/superserve-ai/sandbox/internal/api"
	"github.com/superserve-ai/sandbox/internal/auth"
	"github.com/superserve-ai/sandbox/internal/db"
)

func TestMachineCredentialReconciliation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	team, _ := seedTeamAndKey(t)
	otherTeam, _ := seedTeamAndKey(t)
	principal := machineRepairPrincipal(t, testQueries, team)
	other := machineRepairPrincipal(t, testQueries, otherTeam)
	credential := machineRepairIssue(t, testQueries, principal, uuid.New(), uuid.New(), "reconciliation-fixture")
	authority := api.NewDBMachineAuthority(testQueries)
	// Issuance is deliberately disabled: reads and revocation remain available.
	if _, err := authority.ReadCredential(ctx, principal.ID, credential.ID); !errors.Is(err, auth.ErrMachineCapabilityDenied) {
		t.Fatalf("unprotected metadata read: %v", err)
	}
	if err := authority.RevokeCredentialScoped(ctx, principal.ID, credential.ID, credential.RevocationGeneration); !errors.Is(err, auth.ErrMachineCapabilityDenied) {
		t.Fatalf("unprotected revoke: %v", err)
	}
	t.Setenv("OPERATOR_API_TOKEN", "reconciliation-operator-fixture")
	router := api.SetupRouter(ctx, &api.Handlers{MachineCredentials: authority}, nil)
	request := func(method string, owner uuid.UUID, suffix, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/internal/machine-identity/principals/"+owner.String()+"/credentials/"+suffix, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer reconciliation-operator-fixture")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	read := func(id uuid.UUID, wantState string) {
		t.Helper()
		response := request("GET", principal.ID, id.String(), "")
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK {
			t.Fatalf("metadata read: %d %s %v", response.Code, response.Body, err)
		}
		if len(body) != 6 || body["principal_id"] != principal.ID.String() || body["credential_id"] != id.String() || body["state"] != wantState || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("metadata response: %v", body)
		}
		for _, key := range []string{"lineage_id", "expires_at", "revocation_generation"} {
			if _, ok := body[key]; !ok {
				t.Fatalf("missing %s", key)
			}
		}
	}
	read(credential.ID, "active")
	if response := request("GET", other.ID, credential.ID.String(), ""); response.Code != http.StatusNotFound {
		t.Fatalf("foreign principal read: %d", response.Code)
	}
	if response := request("GET", principal.ID, uuid.NewString(), ""); response.Code != http.StatusNotFound {
		t.Fatalf("absent credential read: %d", response.Code)
	}
	revoke := func(owner, id uuid.UUID, generation int64, want int) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"credential_id": id, "expected_generation": generation})
		response := request("POST", owner, "revoke", string(body))
		if response.Code != want {
			t.Fatalf("revoke status %d, want %d: %s", response.Code, want, response.Body)
		}
	}
	revoke(other.ID, credential.ID, credential.RevocationGeneration, http.StatusConflict)
	revoke(principal.ID, credential.ID, credential.RevocationGeneration+1, http.StatusConflict)
	revoke(principal.ID, uuid.New(), credential.RevocationGeneration, http.StatusConflict)
	read(credential.ID, "active")
	revoke(principal.ID, credential.ID, credential.RevocationGeneration, http.StatusNoContent)
	read(credential.ID, "revoked")
	var firstRevokedAt, replayRevokedAt pgtype.Timestamptz
	if err := testPool.QueryRow(ctx, `SELECT revoked_at FROM machine_credential WHERE id=$1`, credential.ID).Scan(&firstRevokedAt); err != nil {
		t.Fatal(err)
	}
	revoke(principal.ID, credential.ID, credential.RevocationGeneration, http.StatusNoContent)
	if err := testPool.QueryRow(ctx, `SELECT revoked_at FROM machine_credential WHERE id=$1`, credential.ID).Scan(&replayRevokedAt); err != nil || !firstRevokedAt.Valid || firstRevokedAt != replayRevokedAt {
		t.Fatalf("replay changed revoke evidence: %v", err)
	}

	if err := testQueries.DisableMachinePrincipalFenced(ctx, principal.ID, db.LifecycleOptions{ExpectedGeneration: &principal.Generation, OperationID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	disabled, err := testQueries.GetMachinePrincipal(ctx, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := testQueries.RestoreMachineCredentialFenced(ctx, principal.ID, uuid.New(), []byte("reconciliation-restored-fixture"), time.Now().Add(time.Hour), []string{"sandbox:read"}, "sandbox-api", db.LifecycleOptions{ExpectedGeneration: &disabled.Generation, OperationID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	// A delayed revoke retry cleans up only its original target across restore.
	revoke(principal.ID, credential.ID, credential.RevocationGeneration, http.StatusNoContent)
	revoke(principal.ID, credential.ID, restored.RevocationGeneration, http.StatusConflict)
	read(credential.ID, "revoked")
	read(restored.ID, "active")
}

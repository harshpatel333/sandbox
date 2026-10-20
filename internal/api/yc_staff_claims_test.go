package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/superserve-ai/sandbox/internal/ycclaims"
)

func staffAssertion(t *testing.T, key ed25519.PrivateKey, actor uuid.UUID, method, path, body string, change func(*ycStaffClaims)) string {
	t.Helper()
	digest := sha256.Sum256([]byte(body))
	claims := ycStaffClaims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "promotion-auth-adapter", Subject: actor.String(), Audience: jwt.ClaimStrings{"yc-staff"}, IssuedAt: jwt.NewNumericDate(time.Now()), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}, Operation: "read", Method: method, Path: path, BodySHA256: hex.EncodeToString(digest[:]), Provider: "google", EmailVerified: true}
	if method == http.MethodPost {
		claims.Operation = "decision"
	}
	if change != nil {
		change(&claims)
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestYCStaffAssertionBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	t.Setenv("PROMOTION_ACCOUNT_PUBLIC_KEY", base64.StdEncoding.EncodeToString(pub))
	actor := uuid.New()
	path := "/internal/yc/staff/claims/decision"
	body := `{"decision":"approve"}`
	route := gin.New()
	route.POST(path, YCStaffAuth(), func(c *gin.Context) {
		if got := actorIDFromContext(c); got == nil || *got != actor || c.GetHeader("X-Actor-User-Id") != "" {
			t.Error("actor not derived exclusively from signature")
		}
		c.Status(204)
	})
	for _, tc := range []struct {
		name   string
		change func(*ycStaffClaims)
		body   string
		absent bool
		want   int
	}{
		{name: "verified", want: 204},
		{name: "actor header only", absent: true, want: 403},
		{name: "promotion audience", change: func(c *ycStaffClaims) { c.Audience = jwt.ClaimStrings{"promotion-account"} }, want: 403},
		{name: "mixed audience", change: func(c *ycStaffClaims) { c.Audience = jwt.ClaimStrings{"yc-staff", "promotion-account"} }, want: 403},
		{name: "wrong operation", change: func(c *ycStaffClaims) { c.Operation = "read" }, want: 403},
		{name: "wrong path", change: func(c *ycStaffClaims) { c.Path = "/internal/yc/staff/claims" }, want: 403},
		{name: "wrong method", change: func(c *ycStaffClaims) { c.Method = "GET" }, want: 403},
		{name: "non Google", change: func(c *ycStaffClaims) { c.Provider = "email" }, want: 403},
		{name: "unverified", change: func(c *ycStaffClaims) { c.EmailVerified = false }, want: 403},
		{name: "expired", change: func(c *ycStaffClaims) {
			c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-2 * time.Minute))
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
		}, want: 403},
		{name: "long lived", change: func(c *ycStaffClaims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(5 * time.Minute)) }, want: 403},
		{name: "changed body", body: `{"decision":"revoke"}`, want: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := body
			if tc.body != "" {
				payload = tc.body
			}
			req := httptest.NewRequest("POST", path, strings.NewReader(payload))
			req.Header.Set("X-Actor-User-Id", uuid.NewString())
			if !tc.absent {
				req.Header.Set("X-YC-Staff-Assertion", staffAssertion(t, key, actor, "POST", path, body, tc.change))
			}
			w := httptest.NewRecorder()
			route.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func staffTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("YC_CLAIM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("isolated PostgreSQL URL required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	database := "staff_claim_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = database
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)")
		admin.Close(ctx)
	})
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE SCHEMA auth; CREATE TABLE auth.users(id uuid PRIMARY KEY,email text,email_confirmed_at timestamptz,deleted_at timestamptz,created_at timestamptz NOT NULL DEFAULT now());
 CREATE TABLE profile(id uuid PRIMARY KEY,provider text,email text);
 CREATE TABLE roles(id uuid PRIMARY KEY,scope_type text);
 CREATE TABLE permissions(id uuid PRIMARY KEY,name text);
 CREATE TABLE role_permissions(role_id uuid,permission_id uuid);
 CREATE TABLE user_role_assignments(user_id uuid,role_id uuid,scope_type text,team_id uuid,revoked_at timestamptz);
 CREATE TABLE team_memberships(team_id uuid,user_id uuid,status text);
 CREATE TABLE team(id uuid PRIMARY KEY,created_at timestamptz NOT NULL,home_region text);
 CREATE TABLE stripe_promotion_migration_fence(team_id uuid PRIMARY KEY);
 CREATE TABLE team_billing_account(team_id uuid PRIMARY KEY,stripe_customer_id text,stripe_subscription_id text,checkout_initializing_at timestamptz,stripe_activation_credit_reserved_at timestamptz,stripe_activation_credit_grant_id text);
 CREATE TABLE billing_signup_choice(team_id uuid PRIMARY KEY,state text,offer_key text);
 CREATE TABLE billing_commercial_contract(id uuid PRIMARY KEY,team_id uuid);`)
	for _, name := range []string{"20261016070000_yc_company_activation_authority.sql", "20261016174553_yc_claim_authority.sql", "20261020224623_yc_staff_claim_approval_notification.sql"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "supabase", "shared-auth-migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		exec(string(b))
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "supabase", "migrations", "20261016174554_yc_claim_team_intent.sql"))
	if err != nil {
		t.Fatal(err)
	}
	exec(string(b))
	return pool
}

func TestYCStaffDatabasePermissionsAndTransactions(t *testing.T) {
	pool := staffTestPool(t)
	ctx := context.Background()
	h := &Handlers{Pool: pool, PromotionAuthPool: pool}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	t.Setenv("PROMOTION_ACCOUNT_PUBLIC_KEY", base64.StdEncoding.EncodeToString(pub))
	reader, writer, customer, nonGoogle, spoof, unverified := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, actor := range []uuid.UUID{reader, writer, customer, nonGoogle, spoof, unverified} {
		provider, email := "google", "staff@superserve.ai"
		if actor == nonGoogle {
			provider = "email"
		}
		if actor == spoof {
			email = "staff@superserve.ai.example.test"
		}
		exec("INSERT INTO profile VALUES($1,$2,$3)", actor, provider, email)
	}
	readRole, writeRole, readPermission, writePermission := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec("INSERT INTO roles VALUES($1,'platform'),($2,'platform')", readRole, writeRole)
	exec("INSERT INTO permissions VALUES($1,'platform:billing:read'),($2,'platform:billing:write')", readPermission, writePermission)
	exec("INSERT INTO role_permissions VALUES($1,$2),($3,$4),($3,$2)", readRole, readPermission, writeRole, writePermission)
	exec("INSERT INTO user_role_assignments VALUES($1,$2,'platform',NULL,NULL)", reader, readRole)
	for _, actor := range []uuid.UUID{writer, nonGoogle, spoof, unverified} {
		exec("INSERT INTO user_role_assignments VALUES($1,$2,'platform',NULL,NULL)", actor, writeRole)
	}
	router := gin.New()
	router.GET("/internal/yc/staff/claims", YCStaffAuth(), h.ListStaffYCClaims)
	router.POST("/internal/yc/staff/claims/decision", YCStaffAuth(), h.DecideStaffYCClaim)
	request := func(actor uuid.UUID, method, path, body string) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Actor-User-Id", writer.String())
		req.Header.Set("X-YC-Staff-Assertion", staffAssertion(t, key, actor, method, path, body, func(c *ycStaffClaims) {
			if actor == unverified {
				c.EmailVerified = false
			}
		}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}
	for _, tc := range []struct {
		actor       uuid.UUID
		read, write int
	}{{reader, 200, 403}, {writer, 200, 400}, {customer, 403, 403}, {nonGoogle, 403, 403}, {spoof, 403, 403}, {unverified, 403, 403}} {
		if got := request(tc.actor, "GET", "/internal/yc/staff/claims", ""); got != tc.read {
			t.Fatalf("read status %d want %d", got, tc.read)
		}
		if got := request(tc.actor, "POST", "/internal/yc/staff/claims/decision", `{}`); got != tc.write {
			t.Fatalf("write status %d want %d", got, tc.write)
		}
	}
	exec("UPDATE user_role_assignments SET revoked_at=now() WHERE user_id=$1", writer)
	if got := request(writer, "POST", "/internal/yc/staff/claims/decision", `{}`); got != 403 {
		t.Fatal("revoked permission accepted")
	}
	rpc := func(op string, p gin.H) map[string]any {
		t.Helper()
		raw, err := h.ycClaimRPC(ctx, op, p)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err = json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	staff := func(op string, p gin.H) (map[string]any, error) {
		raw, err := h.ycStaffRPC(ctx, op, p)
		out := map[string]any{}
		if err == nil {
			err = json.Unmarshal(raw, &out)
		}
		return out, err
	}
	must := func(op string, p gin.H) map[string]any {
		t.Helper()
		out, err := staff(op, p)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		return out
	}
	create := func() gin.H {
		t.Helper()
		email := uuid.NewString() + "@example.test"
		user, team := uuid.New(), uuid.New()
		claim := rpc("submit", gin.H{"submitted_email": email, "founder_email": "founder@example.test", "company_identity": "https://www.ycombinator.com/companies/" + uuid.NewString(), "evidence": gin.H{"verified": true}, "verification_reference": ycclaims.Hash(email), "state": "manual_review", "reason": "email_mismatch"})
		exec("INSERT INTO auth.users(id,email,email_confirmed_at,created_at) VALUES($1,$2,now(),clock_timestamp())", user, email)
		return gin.H{"id": claim["id"], "user_id": user, "team_id": team, "home_region": "use", "team_created_at": time.Now().Add(time.Second), "team_eligible": true, "actor_id": writer, "request_id": uuid.New(), "expected_revision": 0, "reason": "Reviewed verified signup", "evidence_reference": "synthetic-review"}
	}
	// Exercise the authenticated handler through regional intent and shared commit.
	httpClaim := create()
	teamRole, teamPermission := uuid.New(), uuid.New()
	exec("INSERT INTO roles VALUES($1,'team')", teamRole)
	exec("INSERT INTO permissions VALUES($1,'billing:write')", teamPermission)
	exec("INSERT INTO role_permissions VALUES($1,$2)", teamRole, teamPermission)
	exec("INSERT INTO user_role_assignments VALUES($1,$2,'team',$3,NULL)", httpClaim["user_id"], teamRole, httpClaim["team_id"])
	exec("INSERT INTO team_memberships VALUES($1,$2,'active')", httpClaim["team_id"], httpClaim["user_id"])
	exec("INSERT INTO team VALUES($1,clock_timestamp(),'use')", httpClaim["team_id"])
	exec("INSERT INTO billing_signup_choice VALUES($1,'selecting',NULL)", httpClaim["team_id"])
	exec("UPDATE user_role_assignments SET revoked_at=NULL WHERE user_id=$1", writer)
	payload := gin.H{}
	for _, k := range []string{"id", "user_id", "team_id", "request_id", "expected_revision", "reason", "evidence_reference"} {
		payload[k] = httpClaim[k]
	}
	payload["decision"] = "approve"
	body, _ := json.Marshal(payload)
	for range 2 {
		if got := request(writer, "POST", "/internal/yc/staff/claims/decision", string(body)); got != 200 {
			t.Fatalf("HTTP approval/replay status %d", got)
		}
	}
	httpResult := rpc("operator-get", gin.H{"id": httpClaim["id"]})
	if httpResult["state"] != "bound" || httpResult["acceptance_required"] != true {
		t.Fatal("HTTP approval did not preserve acceptance boundary")
	}
	// Keep this completed test fixture out of the mail worker cases below.
	exec("UPDATE yc_claim_approval_mail SET sent_at=now() WHERE claim_id=$1", httpClaim["id"])
	old := create()
	exec("UPDATE auth.users SET created_at=now()-interval '1 day' WHERE id=$1", old["user_id"])
	if _, err := staff("prepare-approve", old); err == nil {
		t.Fatal("historical signup accepted")
	}
	p := create()
	must("prepare-approve", p)
	// Outbox failure must roll the approval and company reservation back together.
	exec(`CREATE FUNCTION fail_approval_mail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic crash'; END $$;
 CREATE TRIGGER fail_approval_mail BEFORE INSERT ON yc_claim_approval_mail FOR EACH ROW EXECUTE FUNCTION fail_approval_mail();`)
	if _, err := staff("approve", p); err == nil {
		t.Fatal("injected outbox failure succeeded")
	}
	before := rpc("operator-get", gin.H{"id": p["id"]})
	if before["state"] != "manual_review" || before["approval_id"] != nil || before["acceptance_required"] != false {
		t.Fatalf("partial approval: %v", before)
	}
	exec("DROP TRIGGER fail_approval_mail ON yc_claim_approval_mail")
	approved := must("approve", p)
	if approved["state"] != "bound" || approved["acceptance_required"] != true || approved["billing_contract_id"] != nil {
		t.Fatalf("approval accepted or provisioned: %v", approved)
	}
	if approved["confirmation_status"] == "confirmed" {
		t.Fatal("approval forged confirmation")
	}
	var n int
	pool.QueryRow(ctx, "SELECT count(*) FROM yc_claim_approval_mail WHERE claim_id=$1", p["id"]).Scan(&n)
	if n != 1 {
		t.Fatal("missing outbox")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := staff("approve", p); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	pool.QueryRow(ctx, "SELECT count(*) FROM yc_claim_decision WHERE claim_id=$1 AND decision='operator'", p["id"]).Scan(&n)
	if n != 1 {
		t.Fatal("duplicated decision")
	}
	altered := gin.H{}
	for k, v := range p {
		altered[k] = v
	}
	altered["team_id"] = uuid.New()
	if _, err := staff("approve", altered); err == nil {
		t.Fatal("changed retry target accepted")
	}
	// Forwarded link cannot reveal the bound claim to a different verified account.
	other := uuid.New()
	exec("INSERT INTO auth.users(id,email,email_confirmed_at) VALUES($1,'other@example.test',now())", other)
	if _, err := h.ycClaimRPC(ctx, "account-get", gin.H{"id": p["id"], "user_id": other}); err == nil {
		t.Fatal("forwarded claim transferred")
	}
	sender := &staffMailSender{fail: true}
	if found, err := h.deliverYCApprovalMail(ctx, sender, "https://console.example.test", "hello@example.test"); !found || err == nil {
		t.Fatal("failure not recorded")
	}
	if rpc("operator-get", gin.H{"id": p["id"]})["state"] != "bound" {
		t.Fatal("delivery failure lost approval")
	}
	exec("UPDATE yc_claim_approval_mail SET next_attempt_at=now() WHERE claim_id=$1", p["id"])
	sender.fail = false
	if found, err := h.deliverYCApprovalMail(ctx, sender, "https://console.example.test", "hello@example.test"); !found || err != nil {
		t.Fatalf("delivery retry %t %v", found, err)
	}
	if len(sender.messages) != 2 || sender.messages[0].IdempotencyKey != sender.messages[1].IdempotencyKey || sender.messages[0].To != sender.messages[1].To || strings.Contains(sender.messages[0].Text, "token=") {
		t.Fatal("notification identity changed")
	}
	if !strings.Contains(sender.messages[0].Text, "/yc/accept?claim=") {
		t.Fatal("resume link missing")
	}
	// An old revision cannot revoke. A valid revocation and replay cannot reapprove.
	revoke := gin.H{"id": p["id"], "actor_id": writer, "request_id": uuid.New(), "expected_revision": 0, "reason": "Review withdrawn", "evidence_reference": "synthetic-review", "home_region": "use"}
	if _, err := staff("revoke", revoke); err == nil {
		t.Fatal("stale decision accepted")
	}
	revoke["expected_revision"] = approved["revision"]
	revoked := must("revoke", revoke)
	if revoked["state"] != "revoked" {
		t.Fatal("revoke failed")
	}
	replay := must("approve", p)
	if replay["state"] != "revoked" {
		t.Fatal("replay restored approval")
	}
	retry := gin.H{}
	for k, v := range revoke {
		retry[k] = v
	}
	retry["request_id"] = uuid.New()
	retry["expected_revision"] = revoked["revision"]
	if _, err := staff("retry-approval-email", retry); err == nil {
		t.Fatal("revoked approval resent")
	}
	exec("UPDATE yc_claim_approval_mail SET sent_at=NULL,next_attempt_at=now() WHERE claim_id=$1", p["id"])
	if found, err := h.deliverYCApprovalMail(ctx, sender, "https://console.example.test", "hello@example.test"); found || err != nil {
		t.Fatal("revoked claim leased")
	}
	// Recover a worker crash using the original message identity; stale ack cannot
	// complete a new lease or reset a delivered message.
	q := create()
	must("prepare-approve", q)
	must("approve", q)
	lease1, lease2 := uuid.New(), uuid.New()
	raw, err := h.ycApprovalMailRPC(ctx, "lease", gin.H{"lease_id": lease1})
	if err != nil {
		t.Fatal(err)
	}
	var job ycMailJob
	json.Unmarshal(raw, &job)
	exec("UPDATE yc_claim_approval_mail SET lease_until=now()-interval '1 second' WHERE claim_id=$1", q["id"])
	raw, err = h.ycApprovalMailRPC(ctx, "lease", gin.H{"lease_id": lease2})
	if err != nil {
		t.Fatal(err)
	}
	var recovered ycMailJob
	json.Unmarshal(raw, &recovered)
	if recovered.MessageID != job.MessageID || recovered.ClaimID == uuid.Nil {
		t.Fatal("crash recovery changed message")
	}
	h.ycApprovalMailRPC(ctx, "finish", gin.H{"id": q["id"], "message_id": job.MessageID, "lease_id": lease1, "success": true})
	pool.QueryRow(ctx, "SELECT count(*) FROM yc_claim_approval_mail WHERE claim_id=$1 AND sent_at IS NOT NULL", q["id"]).Scan(&n)
	if n != 0 {
		t.Fatal("stale lease acknowledged")
	}
	h.ycApprovalMailRPC(ctx, "finish", gin.H{"id": q["id"], "message_id": job.MessageID, "lease_id": lease2, "success": true})
	h.ycApprovalMailRPC(ctx, "finish", gin.H{"id": q["id"], "message_id": job.MessageID, "lease_id": lease2, "success": false})
	pool.QueryRow(ctx, "SELECT count(*) FROM yc_claim_approval_mail WHERE claim_id=$1 AND sent_at IS NOT NULL", q["id"]).Scan(&n)
	if n != 1 {
		t.Fatal("stale ack reset delivery")
	}
}

type staffMailSender struct {
	fail     bool
	messages []EmailMessage
}

func (s *staffMailSender) SendEmail(_ context.Context, m EmailMessage) error {
	s.messages = append(s.messages, m)
	if s.fail {
		return errors.New("synthetic delivery failure")
	}
	return nil
}

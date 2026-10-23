//go:build integration

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/superserve-ai/sandbox/internal/db"
)

type staffCreditStripe struct {
	t        *testing.T
	mu       sync.Mutex
	customer string
	grants   []entitlementGrant
	creates  int
	keys     []string
	lose     bool
	tamper   bool
}

func (s *staffCreditStripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path != "/v1/billing/credit_grants" {
		s.t.Errorf("unexpected provider request %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", 500)
		return
	}
	if r.Method == http.MethodGet {
		if r.URL.Query().Get("customer") != s.customer {
			s.t.Errorf("grant inventory read for another customer")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": s.grants, "has_more": false})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.t.Error(err)
	}
	s.creates++
	s.keys = append(s.keys, r.Header.Get("Idempotency-Key"))
	var g entitlementGrant
	g.ID = fmt.Sprintf("credgr_staff_%d_%s", s.creates, uuid.NewString())
	g.Customer = r.PostForm.Get("customer")
	g.Category = r.PostForm.Get("category")
	g.Amount.Type = r.PostForm.Get("amount[type]")
	g.Amount.Monetary.Currency = r.PostForm.Get("amount[monetary][currency]")
	fmt.Sscan(r.PostForm.Get("amount[monetary][value]"), &g.Amount.Monetary.Value)
	fmt.Sscan(r.PostForm.Get("priority"), &g.Priority)
	fmt.Sscan(r.PostForm.Get("expires_at"), &g.ExpiresAt)
	g.Applicability.Scope.PriceType = r.PostForm.Get("applicability_config[scope][price_type]")
	g.Metadata = map[string]string{"staff_credit_grant": r.PostForm.Get("metadata[staff_credit_grant]")}
	if s.tamper {
		g.Amount.Monetary.Value++
	}
	s.grants = append(s.grants, g)
	if s.lose {
		s.lose = false
		http.Error(w, `{"error":{"message":"response lost"}}`, 503)
		return
	}
	_ = json.NewEncoder(w).Encode(g)
}

func newStaffCreditFixture(t *testing.T, withCustomer bool) (*Handlers, uuid.UUID, *staffCreditStripe) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil || !strings.HasSuffix(cfg.ConnConfig.Database, "_test") {
		t.Fatal("DATABASE_URL must name a migrated disposable test database")
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h := &Handlers{Pool: pool, DB: db.New(pool)}
	team, err := h.DB.CreateTeam(t.Context(), "staff-credit-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	s := &staffCreditStripe{t: t, customer: "cus_" + uuid.NewString()}
	if withCustomer {
		if _, err = pool.Exec(t.Context(), `INSERT INTO team_billing_account(team_id,stripe_customer_id) VALUES($1,$2)`, team.ID, s.customer); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	h.Stripe = &stripeHTTPClient{baseURL: srv.URL, apiVersion: "2026-07-29.dahlia", httpClient: srv.Client()}
	return h, team.ID, s
}

func TestStaffCreditGrantIssuesOnce(t *testing.T) {
	h, team, s := newStaffCreditFixture(t, true)
	actor := uuid.New()
	req := staffCreditRequest{Key: "goodwill-1", AmountCents: 25000, Reason: "Usage billed during an outage"}
	credit, err := h.grantStaffCredit(t.Context(), team, actor, req)
	if err != nil || credit.State != "issued" || credit.GrantID == nil || credit.RequestedBy != actor {
		t.Fatalf("credit not issued: %+v %v", credit, err)
	}
	g := s.grants[0]
	if g.Amount.Monetary.Value != 25000 || g.Applicability.Scope.PriceType != "metered" || g.Priority != 50 || g.ExpiresAt != 0 || g.Category != "promotional" {
		t.Fatalf("grant is not a metered-only credit with the requested terms: %+v", g)
	}
	if err = creditScope(g, s.customer, time.Now().Unix(), nil); err != nil {
		t.Fatalf("invoice reconciliation would not recognize the grant: %v", err)
	}
	again, err := h.grantStaffCredit(t.Context(), team, actor, req)
	if err != nil || again.ID != credit.ID || s.creates != 1 {
		t.Fatalf("repeated request granted again: %d creates %v", s.creates, err)
	}
	other := req
	other.AmountCents = 30000
	if _, err = h.grantStaffCredit(t.Context(), team, actor, other); !errors.Is(err, errStaffCreditConflict) {
		t.Fatalf("idempotency key reused for a different amount: %v", err)
	}
	if _, err = h.Pool.Exec(t.Context(), `UPDATE billing_staff_credit_grant SET amount_cents=1 WHERE id=$1`, credit.ID); err == nil {
		t.Fatal("issued credit was edited")
	}
}

func TestStaffCreditGrantAdoptsLostResponse(t *testing.T) {
	h, team, s := newStaffCreditFixture(t, true)
	s.lose = true
	req := staffCreditRequest{Key: "goodwill-2", AmountCents: 5000, Reason: "Correction"}
	credit, err := h.grantStaffCredit(t.Context(), team, uuid.New(), req)
	if err == nil || credit.State != "pending" {
		t.Fatalf("lost response treated as issued: %+v %v", credit, err)
	}
	credit, err = h.grantStaffCredit(t.Context(), team, uuid.New(), req)
	if err != nil || credit.State != "issued" || s.creates != 1 || *credit.GrantID != s.grants[0].ID {
		t.Fatalf("retry did not adopt the existing grant: creates=%d %+v %v", s.creates, credit, err)
	}
}

func TestStaffCreditGrantRejectsMismatchedProviderGrant(t *testing.T) {
	h, team, s := newStaffCreditFixture(t, true)
	s.tamper = true
	credit, err := h.grantStaffCredit(t.Context(), team, uuid.New(), staffCreditRequest{Key: "k", AmountCents: 100, Reason: "r"})
	if !errors.Is(err, errStaffCreditReview) || credit.State != "pending" {
		t.Fatalf("mismatched grant recorded as issued: %+v %v", credit, err)
	}
	s.mu.Lock()
	s.tamper = false
	s.grants[0].Amount.Monetary.Value = 100
	s.grants[0].VoidedAt = time.Now().Unix()
	s.mu.Unlock()
	if credit, err = h.grantStaffCredit(t.Context(), team, uuid.New(), staffCreditRequest{Key: "k", AmountCents: 100, Reason: "r"}); !errors.Is(err, errStaffCreditReview) || s.creates != 1 {
		t.Fatalf("a voided grant was replaced by a second grant: creates=%d %v", s.creates, err)
	}
}

func TestStaffCreditGrantFollowsCurrentCustomer(t *testing.T) {
	h, team, s := newStaffCreditFixture(t, true)
	s.lose = true
	req := staffCreditRequest{Key: "k", AmountCents: 100, Reason: "r"}
	if _, err := h.grantStaffCredit(t.Context(), team, uuid.New(), req); err == nil {
		t.Fatal("lost response treated as issued")
	}
	s.mu.Lock()
	s.grants = nil
	s.mu.Unlock()
	if _, err := h.Pool.Exec(t.Context(), `UPDATE team_billing_account SET stripe_customer_id='cus_replacement' WHERE team_id=$1`, team); err != nil {
		t.Fatal(err)
	}
	if _, err := h.grantStaffCredit(t.Context(), team, uuid.New(), req); !errors.Is(err, errStaffCreditConflict) || s.creates != 1 {
		t.Fatalf("pending credit issued to the previous customer: creates=%d %v", s.creates, err)
	}
}

func TestStaffCreditIsRecognizedByAllowanceChecks(t *testing.T) {
	h, team, s := newStaffCreditFixture(t, true)
	credit, err := h.grantStaffCredit(t.Context(), team, uuid.New(), staffCreditRequest{Key: "k", AmountCents: 100, Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	staff, err := h.issuedStaffCredits(t.Context(), h.Pool, s.customer)
	if err != nil || !staff[*credit.GrantID] {
		t.Fatalf("issued staff credit not listed: %v %v", staff, err)
	}
	if err = h.Stripe.(*stripeHTTPClient).ycGrantPreflight(t.Context(), s.customer, "", "", staff); err != nil {
		t.Fatalf("staff credit blocks YC grant preflight: %v", err)
	}
	if err = h.Stripe.(*stripeHTTPClient).ycGrantPreflight(t.Context(), s.customer, "", "", nil); err == nil {
		t.Fatal("unknown live grant no longer conflicts with YC approval")
	}
}

func TestStaffCreditGrantRequiresCustomerAndStaff(t *testing.T) {
	h, team, _ := newStaffCreditFixture(t, false)
	if _, err := h.grantStaffCredit(t.Context(), team, uuid.New(), staffCreditRequest{Key: "k", AmountCents: 100, Reason: "r"}); !errors.Is(err, errStaffCreditNoCustomer) {
		t.Fatalf("credit granted without a billing customer: %v", err)
	}
	for _, call := range []struct {
		handler func(*gin.Context)
		body    string
	}{{h.CreateStaffCreditGrant, `{"idempotency_key":"k","amount_cents":100,"reason":"r"}`}, {h.ListStaffCreditGrants, ``}} {
		for _, actor := range []struct {
			header string
			want   int
		}{{"", http.StatusUnauthorized}, {uuid.NewString(), http.StatusForbidden}} {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(call.body))
			if actor.header != "" {
				c.Request.Header.Set(internalActorHeader, actor.header)
			}
			c.Params = gin.Params{{Key: "team_id", Value: team.String()}}
			call.handler(c)
			if w.Code != actor.want {
				t.Fatalf("staff credit endpoint with actor %q: got %d want %d", actor.header, w.Code, actor.want)
			}
		}
	}
}

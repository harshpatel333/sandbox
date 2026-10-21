package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/superserve-ai/sandbox/internal/ycclaims"
)

type acceptanceCatalogFixture struct {
	disclosure          YCAcceptanceDisclosure
	assignment          uuid.UUID
	calls               int
	loseAcknowledgement bool
}

func (f *acceptanceCatalogFixture) Resolve(context.Context, YCAcceptanceAuthority) (YCAcceptanceDisclosure, error) {
	return f.disclosure, nil
}
func (f *acceptanceCatalogFixture) Assign(context.Context, YCAcceptanceRecord) (uuid.UUID, error) {
	f.calls++
	if f.assignment == uuid.Nil {
		f.assignment = uuid.New()
	}
	if f.loseAcknowledgement {
		f.loseAcknowledgement = false
		return uuid.Nil, errors.New("lost assignment acknowledgement")
	}
	return f.assignment, nil
}

type acceptanceProvisioningFixture struct {
	operation uuid.UUID
	calls     int
	status    YCAcceptanceProvisioningStatus
}

func (f *acceptanceProvisioningFixture) Enqueue(context.Context, uuid.UUID, uuid.UUID) (uuid.UUID, error) {
	f.calls++
	if f.operation == uuid.Nil {
		f.operation = uuid.New()
	}
	return f.operation, nil
}
func (f *acceptanceProvisioningFixture) Status(context.Context, uuid.UUID, uuid.UUID) (YCAcceptanceProvisioningStatus, error) {
	return f.status, nil
}

func acceptanceFixture(t *testing.T) (*YCOfferAcceptanceService, *Handlers, YCAcceptanceRecord, *acceptanceCatalogFixture, *acceptanceProvisioningFixture) {
	t.Helper()
	pool := staffTestPool(t)
	ctx := context.Background()
	h := &Handlers{Pool: pool, PromotionAuthPool: pool}
	migration, err := os.ReadFile(filepath.Join("..", "..", "supabase", "migrations", "20261021135340_yc_offer_acceptance.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	email := uuid.NewString() + "@example.test"
	user, team := uuid.New(), uuid.New()
	raw, err := h.ycClaimRPC(ctx, "submit", gin.H{"submitted_email": email, "founder_email": "founder@example.test", "company_identity": "https://www.ycombinator.com/companies/" + uuid.NewString(), "evidence": gin.H{"verified": true}, "verification_reference": ycclaims.Hash(email), "state": "manual_review", "reason": "email_mismatch"})
	if err != nil {
		t.Fatal(err)
	}
	var claim struct {
		ID uuid.UUID `json:"id"`
	}
	if err = json.Unmarshal(raw, &claim); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO auth.users(id,email,email_confirmed_at,created_at) VALUES($1,$2,now(),clock_timestamp())`, []any{user, email}},
		{`INSERT INTO team VALUES($1,clock_timestamp(),'use')`, []any{team}},
		{`INSERT INTO billing_signup_choice VALUES($1,'selecting',NULL)`, []any{team}},
	} {
		if _, err = pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	p := gin.H{"id": claim.ID, "user_id": user, "team_id": team, "home_region": "use", "actor_id": uuid.New(), "request_id": uuid.New(), "expected_revision": 0, "reason": "Reviewed synthetic signup", "evidence_reference": "synthetic-review"}
	if _, err = h.ycBindWithIntentRPC(ctx, "approve", p, h.ycStaffRPC); err != nil {
		t.Fatal(err)
	}
	terms := YCAcceptanceTerms{OfferKey: "yc_2026", OfferVersion: 1, Currency: "usd", TermMonths: 12, ChargeAmountCents: 1200000, ChargeIntervalMonths: 12, InitialFeeWaived: true, MonthlyAllowanceCents: []int64{3000000, 3000000, 3000000, 100000, 100000, 100000, 100000, 100000, 100000, 100000, 100000, 100000}, PaymentRequirement: "eligible_credit_or_verified_payment", RateCardKey: "payg", DefaultSuccessorFamily: "pro_annual"}
	for _, resource := range []string{"vcpu", "memory_gib", "storage_gib"} {
		terms.ResourceRates = append(terms.ResourceRates, YCAcceptanceRate{ID: uuid.New(), Resource: resource, Unit: "second", PriceUSD: "0.000001000000", Currency: "USD"})
	}
	renewal := terms
	renewal.OfferKey = "pro_annual"
	renewal.InitialFeeWaived = false
	renewal.PaymentRequirement = "verified_payment"
	renewal.MonthlyAllowanceCents = []int64{100000, 100000, 100000, 100000, 100000, 100000, 100000, 100000, 100000, 100000, 100000, 100000}
	catalog := &acceptanceCatalogFixture{disclosure: YCAcceptanceDisclosure{Terms: terms, RenewalTerms: renewal, PricingAt: time.Now().UTC()}}
	provisioner := &acceptanceProvisioningFixture{}
	s := &YCOfferAcceptanceService{Pool: pool, Shared: pool, Region: "use", Catalog: catalog, Provisioning: provisioner, Authorize: func(_ context.Context, u, tm uuid.UUID) error {
		if u != user || tm != team {
			return errYCAcceptanceConflict
		}
		return nil
	}}
	r, err := s.Offer(ctx, user, team, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, h, r, catalog, provisioner
}
func acceptanceRequest(r YCAcceptanceRecord) ycAcceptanceRequest {
	return ycAcceptanceRequest{ClaimID: r.ClaimID, TeamID: r.TeamID, AcceptanceID: r.ID, RequestID: uuid.New(), ApprovalRevision: r.ApprovalRevision, TermsDigest: r.TermsDigest, RenewalTermsDigest: r.RenewalTermsDigest}
}

func TestYCAcceptanceDurableHandoff(t *testing.T) {
	s, _, offered, catalog, provisioner := acceptanceFixture(t)
	ctx := context.Background()
	if work, err := s.RunOnce(ctx); err != nil || work {
		t.Fatalf("approval alone dispatched: %v %v", work, err)
	}
	in := acceptanceRequest(offered)
	bad := in
	bad.TermsDigest = ycclaims.Hash("changed")
	if _, err := s.Accept(ctx, offered.UserID, bad); !errors.Is(err, errYCAcceptanceConflict) {
		t.Fatal("changed terms accepted", err)
	}
	if _, err := s.Accept(ctx, uuid.New(), in); err == nil {
		t.Fatal("wrong account accepted")
	}
	accepted, err := s.Accept(ctx, offered.UserID, in)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.Accept(ctx, offered.UserID, in)
	if err != nil || !replay.AcceptedAt.Equal(*accepted.AcceptedAt) {
		t.Fatal("acceptance retry changed identity", err)
	}
	catalog.loseAcknowledgement = true
	if work, err := s.RunOnce(ctx); !work || err == nil {
		t.Fatal("lost assignment response not surfaced")
	}
	var state string
	if err = s.Pool.QueryRow(ctx, `SELECT state FROM yc_offer_acceptance WHERE id=$1`, offered.ID).Scan(&state); err != nil || state != "accepted" {
		t.Fatal(state, err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE yc_offer_acceptance SET next_attempt_at=now() WHERE id=$1`, offered.ID); err != nil {
		t.Fatal(err)
	}
	// Restart with the same durable stores and adapters, without any request state.
	restarted := *s
	if work, err := restarted.RunOnce(ctx); !work || err != nil {
		t.Fatal(work, err)
	}
	final, err := loadYCAcceptance(ctx, s.Pool, offered.TeamID, offered.ClaimID)
	if err != nil || final.AssignmentID == nil || *final.AssignmentID != catalog.assignment || final.OperationID == nil || *final.OperationID != provisioner.operation || catalog.calls != 2 || provisioner.calls != 1 {
		t.Fatalf("handoff did not converge: %+v %v", final, err)
	}
	if work, err := s.RunOnce(ctx); err != nil || work {
		t.Fatal("completed handoff was requeued", err)
	}
	provisioner.status = YCAcceptanceProvisioningStatus{OperationID: provisioner.operation, State: "grant_pending", Ready: true}
	status, err := s.Status(ctx, offered.UserID, offered.TeamID, offered.ClaimID)
	if err != nil || status.Ready {
		t.Fatal("pending grant reported ready", err)
	}
	var legacyAllowed bool
	if err = s.Shared.QueryRow(ctx, `SELECT public.lock_yc_activation_authority($1,$2,'use')`, offered.ApprovalID, offered.TeamID).Scan(&legacyAllowed); err != nil || legacyAllowed {
		t.Fatal("acceptance weakened legacy gate", err)
	}
}

func TestYCAcceptanceConcurrentRequests(t *testing.T) {
	s, _, r, _, _ := acceptanceFixture(t)
	in := acceptanceRequest(r)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.Accept(ctx, r.UserID, in); results <- err }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	changed := in
	changed.RequestID = uuid.New()
	if _, err := s.Accept(ctx, r.UserID, changed); err == nil {
		t.Fatal("second request replaced acceptance")
	}
	wrongRegion := *s
	wrongRegion.Region = "usw"
	if _, err := wrongRegion.Offer(ctx, r.UserID, r.TeamID, r.ClaimID); err == nil {
		t.Fatal("cross-region acceptance admitted")
	}
}

func TestYCAcceptanceGuardSerializesIdentityAndRevocation(t *testing.T) {
	s, h, r, _, _ := acceptanceFixture(t)
	ctx := context.Background()
	in := acceptanceRequest(r)
	r, err := s.Accept(ctx, r.UserID, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.rpc(ctx, s.Shared, "accept", ycAcceptanceParameters(r)); err != nil {
		t.Fatal(err)
	}
	// Mail retry increments claim revision but does not change economic approval.
	var revision int64
	if err = s.Shared.QueryRow(ctx, `SELECT revision FROM yc_claim WHERE id=$1`, r.ClaimID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	_, err = h.ycStaffRPC(ctx, "retry-approval-email", gin.H{"id": r.ClaimID, "actor_id": uuid.New(), "request_id": uuid.New(), "expected_revision": revision, "reason": "retry synthetic mail", "evidence_reference": "synthetic", "home_region": "use"})
	if err != nil {
		t.Fatal(err)
	}
	err = s.WithGuard(ctx, r, func(context.Context) error {
		for _, query := range []string{`SELECT id FROM auth.users WHERE id=$1 FOR UPDATE NOWAIT`, `SELECT approval_id FROM yc_company_activation_authority WHERE approved_by IS NOT NULL AND team_id=$1 FOR UPDATE NOWAIT`} {
			id := r.UserID
			if strings.Contains(query, "yc_company_activation_authority") {
				id = r.TeamID
			}
			tx, e := s.Shared.Begin(ctx)
			if e != nil {
				return e
			}
			_, e = tx.Exec(ctx, query, id)
			_ = tx.Rollback(ctx)
			if e == nil {
				t.Error("guard did not serialize identity or authority")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Shared.Exec(ctx, `UPDATE auth.users SET email='changed@example.test' WHERE id=$1`, r.UserID); err != nil {
		t.Fatal(err)
	}
	called := false
	if err = s.WithGuard(ctx, r, func(context.Context) error { called = true; return nil }); err == nil || called {
		t.Fatal("changed identity dispatched")
	}
	// A separate fixture exercises revoke first, without relying on the email fence.
	s2, h2, r2, _, _ := acceptanceFixture(t)
	r2, err = s2.Accept(ctx, r2.UserID, acceptanceRequest(r2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s2.rpc(ctx, s2.Shared, "accept", ycAcceptanceParameters(r2)); err != nil {
		t.Fatal(err)
	}
	if _, err = h2.ycClaimRPC(ctx, "revoke", gin.H{"id": r2.ClaimID, "actor_id": uuid.New(), "reason": "synthetic revoke", "evidence_reference": "synthetic", "home_region": "use"}); err != nil {
		t.Fatal(err)
	}
	if err = s2.WithGuard(ctx, r2, func(context.Context) error { t.Error("revoked approval dispatched"); return nil }); err == nil {
		t.Fatal("revocation ignored")
	}
}

func TestYCPreferredReviewBindsExactApprovedRecipient(t *testing.T) {
	pool := staffTestPool(t)
	h := &Handlers{Pool: pool, PromotionAuthPool: pool}
	ctx := context.Background()
	original := "original@example.test"
	preferred := "preferred@example.test"
	user, team := uuid.New(), uuid.New()
	company := "https://www.ycombinator.com/companies/example-team"
	raw, err := h.ycClaimRPC(ctx, "submit", gin.H{"submitted_email": original, "founder_email": original, "company_identity": company, "evidence": gin.H{"verified": true}, "verification_reference": ycclaims.Hash("synthetic-link"), "state": "manual_review", "reason": "review"})
	if err != nil {
		t.Fatal(err)
	}
	var claim struct {
		ID uuid.UUID `json:"id"`
	}
	if err = json.Unmarshal(raw, &claim); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO auth.users(id,email,email_confirmed_at,created_at) VALUES($1,$2,now(),clock_timestamp())`, []any{user, preferred}},
		{`INSERT INTO team VALUES($1,clock_timestamp(),'use')`, []any{team}},
		{`INSERT INTO billing_signup_choice VALUES($1,'selecting',NULL)`, []any{team}},
	} {
		if _, err = pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	p := gin.H{"id": claim.ID, "user_id": user, "team_id": team, "request_id": uuid.New(), "home_region": "use", "verification_reference": ycclaims.Hash("synthetic-link"), "company_identity": company}
	raw, err = h.ycBindWithIntentRPC(ctx, "review", p, h.ycPreferredRPC)
	if err != nil {
		t.Fatal(err)
	}
	var review struct {
		ID       uuid.UUID `json:"review_request_id"`
		Revision int64     `json:"review_revision"`
	}
	if err = json.Unmarshal(raw, &review); err != nil {
		t.Fatal(err)
	}
	if _, err = h.ycBindWithIntentRPC(ctx, "review", p, h.ycPreferredRPC); err != nil {
		t.Fatal("review replay", err)
	}
	list, err := h.ycPreferredRPC(ctx, "lookup", gin.H{"user_id": user})
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Claims []json.RawMessage `json:"claims"`
	}
	if err = json.Unmarshal(list, &listed); err != nil || len(listed.Claims) != 1 {
		t.Fatal("preferred account cannot resume", string(list), err)
	}
	decision := gin.H{"id": claim.ID, "user_id": user, "team_id": team, "actor_id": uuid.New(), "request_id": uuid.New(), "expected_revision": review.Revision, "home_region": "use", "reason": "Approve exact preferred identity", "evidence_reference": "synthetic-review", "review_request_id": review.ID, "review_revision": review.Revision}
	wrong := gin.H{}
	for k, v := range decision {
		wrong[k] = v
	}
	wrong["review_request_id"] = uuid.New()
	if _, err = h.ycBindWithIntentRPC(ctx, "approve", wrong, h.ycStaffRPC); err == nil {
		t.Fatal("different review approved")
	}
	if _, err = h.ycBindWithIntentRPC(ctx, "approve", decision, h.ycStaffRPC); err != nil {
		t.Fatal(err)
	}
	var recipient, unchanged string
	if err = pool.QueryRow(ctx, `SELECT m.approved_email,c.submitted_email FROM yc_claim_approval_mail m JOIN yc_claim c ON c.id=m.claim_id WHERE c.id=$1`, claim.ID).Scan(&recipient, &unchanged); err != nil || recipient != preferred || unchanged != original {
		t.Fatal("recipient/evidence mismatch", recipient, unchanged, err)
	}
	s := &YCOfferAcceptanceService{Shared: pool}
	if _, err = s.rpc(ctx, pool, "offer", gin.H{"claim_id": claim.ID, "team_id": team, "user_id": user, "home_region": "use"}); err != nil {
		t.Fatal("approved preferred identity cannot accept", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE auth.users SET email='changed@example.test' WHERE id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if _, err = s.rpc(ctx, pool, "offer", gin.H{"claim_id": claim.ID, "team_id": team, "user_id": user, "home_region": "use"}); err == nil {
		t.Fatal("stale preferred identity accepted")
	}
}

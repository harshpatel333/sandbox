//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func prepaidOperationRouter(t *testing.T, f *prepaidFixture, team, actor uuid.UUID) *gin.Engine {
	t.Helper()
	f.exec(`INSERT INTO profile(id,email,provider,provider_id) VALUES($1,$2,'google',$1::uuid::text) ON CONFLICT(id) DO NOTHING`, actor, actor.String()+"@example.com")
	f.exec(`INSERT INTO team_memberships(team_id,user_id,status) VALUES($1,$2,'active') ON CONFLICT DO NOTHING`, team, actor)
	f.exec(`INSERT INTO user_role_assignments(user_id,role_id,scope_type,team_id) SELECT $1,id,'team',$2 FROM roles WHERE name='team_owner'`, actor, team)
	f.h.Config.AppAllowedOrigins = []string{"https://console.example.com"}
	r := gin.New()
	r.GET("/billing/prepaid", func(c *gin.Context) {
		c.Set("team_id", team.String())
		c.Set("actor_id", actor)
		f.h.GetPrepaidCredit(c)
	})
	r.POST("/billing/prepaid/checkout", func(c *gin.Context) {
		c.Set("team_id", team.String())
		c.Set("actor_id", actor)
		f.h.CreatePrepaidCheckout(c)
	})
	return r
}

func prepaidOperationPost(r *gin.Engine, req prepaidCheckoutBody) *httptest.ResponseRecorder {
	body, _ := json.Marshal(req)
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/billing/prepaid/checkout", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, request)
	return rec
}

func TestPrepaidOperationConcurrentRetryAndBinding(t *testing.T) {
	f := newPrepaidFixture(t)
	actor := uuid.New()
	r := prepaidOperationRouter(t, f, f.team, actor)
	capability := httptest.NewRecorder()
	r.ServeHTTP(capability, httptest.NewRequest(http.MethodGet, "/billing/prepaid", nil))
	var summary struct {
		Supported bool `json:"checkout_idempotency_supported"`
	}
	if capability.Code != 200 || json.Unmarshal(capability.Body.Bytes(), &summary) != nil || !summary.Supported {
		t.Fatalf("checkout capability unavailable: %d %s", capability.Code, capability.Body)
	}
	req := prepaidCheckoutBody{OperationID: uuid.New(), AmountCents: 500, SuccessURL: "https://console.example.com/ok", CancelURL: "https://console.example.com/cancel"}
	done := make(chan *httptest.ResponseRecorder, 4)
	for range 4 {
		go func() { done <- prepaidOperationPost(r, req) }()
	}
	var session string
	for range 4 {
		rec := <-done
		var got prepaidOperationResponse
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.PurchaseID != req.OperationID || got.ID == "" || got.State != "pending" {
			t.Fatalf("checkout replay: %d %s", rec.Code, rec.Body)
		}
		if session != "" && session != got.ID {
			t.Fatalf("created two sessions %s %s", session, got.ID)
		}
		session = got.ID
	}
	var count int
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM billing_prepaid_purchase WHERE team_id=$1`, f.team).Scan(&count); err != nil || count != 1 {
		t.Fatalf("purchase count %d: %v", count, err)
	}
	f.mu.Lock()
	sessions := len(f.sessions)
	f.mu.Unlock()
	if sessions != 1 {
		t.Fatalf("provider sessions %d", sessions)
	}
	for _, change := range []func(*prepaidCheckoutBody){
		func(p *prepaidCheckoutBody) { p.AmountCents = 1000 },
		func(p *prepaidCheckoutBody) { p.SuccessURL += "?other=1" },
		func(p *prepaidCheckoutBody) { p.CancelURL += "?other=1" },
	} {
		bad := req
		change(&bad)
		if rec := prepaidOperationPost(r, bad); rec.Code != 409 {
			t.Fatalf("mismatch accepted: %d %s", rec.Code, rec.Body)
		}
	}
	other, err := f.h.DB.CreateTeam(t.Context(), "operation-other-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	otherRouter := prepaidOperationRouter(t, f, other.ID, actor)
	if rec := prepaidOperationPost(otherRouter, req); rec.Code != 409 {
		t.Fatalf("cross-team operation accepted: %d %s", rec.Code, rec.Body)
	}
	otherActorRouter := prepaidOperationRouter(t, f, f.team, uuid.New())
	if rec := prepaidOperationPost(otherActorRouter, req); rec.Code != 409 {
		t.Fatalf("cross-actor operation accepted: %d %s", rec.Code, rec.Body)
	}
	setPricingVersion(t, f.h, "legacy")
	if rec := prepaidOperationPost(r, req); rec.Code != 200 {
		t.Fatalf("accepted checkout lost after switch: %d %s", rec.Code, rec.Body)
	}
	f.mu.Lock()
	f.sessions[session]["status"] = "expired"
	f.mu.Unlock()
	rec := prepaidOperationPost(r, req)
	var closed prepaidOperationResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &closed) != nil || closed.State != "failed" || closed.URL != "" {
		t.Fatalf("expired operation: %d %s", rec.Code, rec.Body)
	}
	rec = prepaidOperationPost(r, req)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &closed) != nil || closed.State != "failed" {
		t.Fatalf("closed replay: %d %s", rec.Code, rec.Body)
	}
}

func TestPrepaidOperationRecoversLostProviderReply(t *testing.T) {
	f := newPrepaidFixture(t)
	var dropped atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/checkout/sessions" && r.Method == http.MethodPost && !dropped.Swap(true) {
			rec := httptest.NewRecorder()
			f.serve(rec, r)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		f.serve(w, r)
	}))
	defer srv.Close()
	f.client.baseURL = srv.URL
	f.client.httpClient = srv.Client()
	r := prepaidOperationRouter(t, f, f.team, uuid.New())
	req := prepaidCheckoutBody{OperationID: uuid.New(), AmountCents: 500, SuccessURL: "https://console.example.com/ok", CancelURL: "https://console.example.com/cancel"}
	first := prepaidOperationPost(r, req)
	if first.Code != http.StatusBadGateway {
		t.Fatalf("expected lost reply, got %d %s", first.Code, first.Body)
	}
	retry := prepaidOperationPost(r, req)
	var got prepaidOperationResponse
	if retry.Code != 200 || json.Unmarshal(retry.Body.Bytes(), &got) != nil || got.ID == "" || got.PurchaseID != req.OperationID {
		t.Fatalf("lost reply recovery: %d %s", retry.Code, retry.Body)
	}
	f.mu.Lock()
	count := len(f.sessions)
	f.mu.Unlock()
	if count != 1 {
		t.Fatalf("recovery duplicated provider session: %d", count)
	}
}

func TestPrepaidOperationFirstPurchaseReplayAfterSubscriptionEstablished(t *testing.T) {
	f := newPrepaidFixture(t)
	actor := uuid.New()
	r := prepaidOperationRouter(t, f, f.team, actor)
	req := prepaidCheckoutBody{OperationID: uuid.New(), AmountCents: 500, SuccessURL: "https://console.example.com/ok", CancelURL: "https://console.example.com/cancel"}
	if err := f.h.bindPrepaidCheckoutOperation(t.Context(), f.team, actor, req); err != nil {
		t.Fatal(err)
	}
	session := "cs_first_" + uuid.NewString()
	// A subscription-mode purchase has no later-payment dispatch payload.
	if err := f.h.recordCheckoutPurchase(t.Context(), req.OperationID, f.team, f.customer, actor, 500, session); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.sessions[session] = map[string]any{"id": session, "url": "https://checkout.example.com/" + session, "mode": "subscription", "status": "open", "customer": f.customer, "metadata": map[string]string{"prepaid_purchase_id": req.OperationID.String()}}
	f.mu.Unlock()
	rec := prepaidOperationPost(r, req)
	var got prepaidOperationResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.ID != session {
		t.Fatalf("first→later seam duplicated purchase: %d %s", rec.Code, rec.Body)
	}
	f.mu.Lock()
	count := len(f.sessions)
	f.mu.Unlock()
	if count != 1 {
		t.Fatalf("first purchase duplicated: %d", count)
	}
}

func TestPrepaidOperationFirstCheckoutUsesCallerPurchaseIdentity(t *testing.T) {
	f := newPrepaidFixture(t)
	actor := uuid.New()
	r := prepaidOperationRouter(t, f, f.team, actor)
	f.h.Config.StripeCheckoutPriceIDs = []string{"price_cpu", "price_memory"}
	f.exec(`UPDATE team_billing_account SET stripe_subscription_id=NULL,stripe_subscription_status=NULL WHERE team_id=$1`, f.team)
	f.exec(`INSERT INTO team_feature_flag(team_id,key,enabled) VALUES($1,'billing_export_enabled',true) ON CONFLICT(team_id,key) DO UPDATE SET enabled=true`, f.team)
	req := prepaidCheckoutBody{OperationID: uuid.New(), AmountCents: 500, SuccessURL: "https://console.example.com/ok", CancelURL: "https://console.example.com/cancel"}
	rec := prepaidOperationPost(r, req)
	var got prepaidOperationResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.ID == "" || got.PurchaseID != req.OperationID {
		t.Fatalf("first checkout identity: %d %s", rec.Code, rec.Body)
	}
	var purchase uuid.UUID
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT id FROM billing_prepaid_purchase WHERE team_id=$1 AND checkout_session_id=$2`, f.team, got.ID).Scan(&purchase); err != nil || purchase != req.OperationID {
		t.Fatalf("first purchase binding %s: %v", purchase, err)
	}
	retry := prepaidOperationPost(r, req)
	var replay prepaidOperationResponse
	if retry.Code != 200 || json.Unmarshal(retry.Body.Bytes(), &replay) != nil || replay.ID != got.ID {
		t.Fatalf("first checkout retry: %d %s", retry.Code, retry.Body)
	}
}

//go:build integration

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func agePrepaidCheckoutAttempt(t *testing.T, f *prepaidFixture, id uuid.UUID, age time.Duration) time.Time {
	t.Helper()
	tx, err := f.h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	// Advance fixture time only, restoring the production guard before any retry.
	if _, err = tx.Exec(t.Context(), `ALTER TABLE billing_prepaid_purchase DISABLE TRIGGER billing_prepaid_purchase_guard`); err != nil {
		t.Fatal(err)
	}
	var first time.Time
	if err = tx.QueryRow(t.Context(), `UPDATE billing_prepaid_purchase SET payment_first_attempt_at=clock_timestamp()-$2::interval WHERE id=$1 RETURNING payment_first_attempt_at`, id, age.String()).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `ALTER TABLE billing_prepaid_purchase ENABLE TRIGGER billing_prepaid_purchase_guard`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return first
}

func TestPrepaidOperationRejectedCheckoutCannotOutlivePurchaseDeadline(t *testing.T) {
	for _, tc := range []struct {
		age        time.Duration
		interleave bool
	}{{24 * time.Hour, false}, {24 * time.Hour, true}, {time.Hour, false}} {
		interleave := tc.interleave
		t.Run(tc.age.String()+strconv.FormatBool(interleave), func(t *testing.T) {
			f := newPrepaidFixture(t)
			actor := uuid.New()
			r := prepaidOperationRouter(t, f, f.team, actor)
			f.h.Config.StripeCheckoutPriceIDs = []string{"price_cpu", "price_memory"}
			f.exec(`UPDATE team_billing_account SET stripe_subscription_id=NULL,stripe_subscription_status=NULL WHERE team_id=$1`, f.team)
			f.exec(`INSERT INTO team_feature_flag(team_id,key,enabled) VALUES($1,'billing_export_enabled',true) ON CONFLICT(team_id,key) DO UPDATE SET enabled=true`, f.team)
			var posts atomic.Int32
			var expiry atomic.Int64
			var discovery atomic.Bool
			reached, release := make(chan struct{}), make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/v1/checkout/sessions" {
					if request.Method == http.MethodPost {
						if posts.Add(1) == 1 {
							http.Error(w, `{"error":{"message":"rejected checkout"}}`, 400)
							return
						}
						_ = request.ParseForm()
						value, _ := strconv.ParseInt(request.PostForm.Get("expires_at"), 10, 64)
						expiry.Store(value)
					}
					if request.Method == http.MethodGet && interleave && discovery.CompareAndSwap(false, true) {
						close(reached)
						<-release
					}
				}
				f.serve(w, request)
			}))
			defer srv.Close()
			f.client.baseURL, f.client.httpClient = srv.URL, srv.Client()
			req := prepaidCheckoutBody{OperationID: uuid.New(), AmountCents: 500, SuccessURL: "https://console.example.com/ok", CancelURL: "https://console.example.com/cancel"}
			if first := prepaidOperationPost(r, req); first.Code != 502 {
				t.Fatalf("initial rejection: %d %s", first.Code, first.Body)
			}
			var released bool
			if err := f.h.Pool.QueryRow(t.Context(), `SELECT checkout_initializing_at IS NULL FROM team_billing_account WHERE team_id=$1`, f.team).Scan(&released); err != nil || !released {
				t.Fatalf("4xx did not release lease: %v %v", released, err)
			}
			original := agePrepaidCheckoutAttempt(t, f, req.OperationID, tc.age)
			var retry *httptest.ResponseRecorder
			if interleave {
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() { done <- prepaidOperationPost(r, req) }()
				select {
				case <-reached:
				case <-time.After(5 * time.Second):
					t.Fatal("retry did not reach provider discovery")
				}
				err := f.h.settleCheckoutPurchase(t.Context(), f.client, req.OperationID)
				close(release)
				if err != nil {
					t.Fatal(err)
				}
				retry = <-done
			} else {
				retry = prepaidOperationPost(r, req)
			}
			if tc.age < 23*time.Hour {
				var got prepaidOperationResponse
				if retry.Code != 200 || json.Unmarshal(retry.Body.Bytes(), &got) != nil || got.URL == "" || posts.Load() != 2 {
					t.Fatalf("within-deadline retry: %d %s posts=%d", retry.Code, retry.Body, posts.Load())
				}
				if expiry.Load() != original.Add(23*time.Hour).Unix() {
					t.Fatalf("fresh lease extended original purchase deadline: got%d want%d", expiry.Load(), original.Add(23*time.Hour).Unix())
				}
				return
			}
			var result prepaidOperationResponse
			if (retry.Code != 200 && retry.Code != 202) || json.Unmarshal(retry.Body.Bytes(), &result) != nil || result.URL != "" {
				t.Fatalf("expired retry exposed payment: %d %s", retry.Code, retry.Body)
			}
			if posts.Load() != 1 {
				t.Fatalf("expired operation dispatched another session: posts=%d", posts.Load())
			}
			if err := f.h.settleCheckoutPurchase(t.Context(), f.client, req.OperationID); err != nil {
				t.Fatal(err)
			}
			var failed bool
			if err := f.h.Pool.QueryRow(t.Context(), `SELECT payment_state='failed' AND credit_state='none' AND checkout_session_id IS NULL AND payment_first_attempt_at=$2 FROM billing_prepaid_purchase WHERE id=$1`, req.OperationID, original).Scan(&failed); err != nil || !failed {
				t.Fatalf("deadline outcome changed: %v %v", failed, err)
			}
		})
	}
}

func TestPrepaidSessionlessFailureRechecksPublishedSession(t *testing.T) {
	f := newPrepaidFixture(t)
	id, actor := uuid.New(), uuid.New()
	if err := f.h.recordCheckoutPurchase(t.Context(), id, f.team, f.customer, actor, 500, ""); err != nil {
		t.Fatal(err)
	}
	agePrepaidCheckoutAttempt(t, f, id, 24*time.Hour)
	reached, release := make(chan struct{}), make(chan struct{})
	var discovery atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/checkout/sessions" && r.Method == http.MethodGet && discovery.CompareAndSwap(false, true) {
			close(reached)
			<-release
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
			return
		}
		f.serve(w, r)
	}))
	defer srv.Close()
	f.client.baseURL, f.client.httpClient = srv.URL, srv.Client()
	done := make(chan error, 1)
	go func() { done <- f.h.settleCheckoutPurchase(t.Context(), f.client, id) }()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("settlement did not reach inventory")
	}
	published := "cs_published_" + uuid.NewString()
	err := f.h.recordCheckoutPurchase(t.Context(), id, f.team, f.customer, actor, 500, published)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, errPrepaidPaymentPending) {
		t.Fatalf("stale settlement result: %v", err)
	}
	var intact bool
	if err = f.h.Pool.QueryRow(t.Context(), `SELECT checkout_session_id=$2 AND payment_state='processing' AND failure_reason IS NULL FROM billing_prepaid_purchase WHERE id=$1`, id, published).Scan(&intact); err != nil || !intact {
		t.Fatalf("published session was permanently failed: %v %v", intact, err)
	}
}

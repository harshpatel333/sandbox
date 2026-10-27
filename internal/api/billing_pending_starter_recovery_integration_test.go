//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestPrepaidPendingStarterRecovery(t *testing.T) {
	for _, scenario := range []string{"missing_link_resume", "missing_link_cancel", "missing_link_wrong_actor", "missing_link_paid", "missing_link_newer_generation", "missing_link_foreign_purchase", "missing_link_other_session", "resume", "cancel", "expired", "paid", "lost_ack", "timeout", "mismatch", "changed_generation", "wrong_actor", "wrong_request", "failed_fence", "closed_entry", "wrong_team", "expired_webhook_race", "paid_during_expire", "timeout_after_expire", "mismatch_after_expire", "wrong_generation", "legacy_resume", "legacy_cancel", "legacy_closed_entry", "expired_with_invoice", "expired_with_payment_intent", "expired_with_subscription", "established_subscription_resume"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPrepaidFixture(t)
			actor := uuid.New()
			r := prepaidOperationRouter(t, f, f.team, actor)
			f.h.Config.StripeCheckoutPriceIDs = []string{"price_cpu", "price_memory"}
			f.exec(`UPDATE team_billing_account SET stripe_subscription_id=NULL,stripe_subscription_status=NULL WHERE team_id=$1`, f.team)
			f.exec(`INSERT INTO team_feature_flag(team_id,key,enabled) VALUES($1,'billing_export_enabled',true) ON CONFLICT(team_id,key) DO UPDATE SET enabled=true`, f.team)
			req := prepaidCheckoutBody{OperationID: uuid.New(), AmountCents: 2500, SuccessURL: "https://console.example.com/ok", CancelURL: "https://console.example.com/cancel"}
			if strings.HasPrefix(scenario, "legacy_") {
				req.OperationID = uuid.Nil
			}
			missingLink := strings.HasPrefix(scenario, "missing_link_")
			trigger := "test_checkout_link_" + strings.ReplaceAll(f.team.String(), "-", "")
			if missingLink {
				f.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected association write failure'; END $$`, trigger))
				f.exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE OF checkout_session_id ON billing_prepaid_purchase FOR EACH ROW WHEN (NEW.team_id='%s'::uuid AND OLD.checkout_session_id IS NULL AND NEW.checkout_session_id IS NOT NULL) EXECUTE FUNCTION %s()`, trigger, f.team, trigger))
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if _, err := f.h.Pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON billing_prepaid_purchase`, trigger)); err != nil {
						t.Error(err)
					}
					if _, err := f.h.Pool.Exec(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, trigger)); err != nil {
						t.Error(err)
					}
				})
			}
			rec := prepaidOperationPost(r, req)
			var created prepaidOperationResponse
			if missingLink {
				if rec.Code != 500 {
					t.Fatalf("association interruption %d %s", rec.Code, rec.Body)
				}
				account, err := f.h.DB.GetTeamBillingCheckoutForRecovery(t.Context(), f.team)
				if err != nil || account.CheckoutSessionID == nil {
					t.Fatalf("retained provider session %v %v", account.CheckoutSessionID, err)
				}
				created.ID = *account.CheckoutSessionID
				f.exec(fmt.Sprintf(`DROP TRIGGER %s ON billing_prepaid_purchase`, trigger))
			} else if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.ID == "" {
				t.Fatalf("create %d %s", rec.Code, rec.Body)
			}
			p, err := f.h.loadPendingStarter(t.Context(), f.team, actor)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "failed_fence" {
				f.exec(`UPDATE billing_prepaid_purchase SET payment_state='failed',failure_reason='Prior checkout failure' WHERE id=$1`, p.Purchase)
			}
			if scenario == "closed_entry" || scenario == "legacy_closed_entry" {
				f.exec(`UPDATE team_feature_flag SET enabled=false WHERE team_id=$1 AND key='billing_export_enabled'`, f.team)
				f.h.Config.AppAllowedOrigins = []string{"https://different.example.com"}
			}
			cancelCalls, gets := 0, 0
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if strings.HasPrefix(request.URL.Path, "/v1/checkout/sessions/") {
					if strings.HasSuffix(request.URL.Path, "/expire") {
						cancelCalls++
						if scenario == "lost_ack" {
							f.mu.Lock()
							f.sessions[created.ID]["status"] = "expired"
							f.mu.Unlock()
							w.WriteHeader(502)
							return
						}
						f.mu.Lock()
						f.sessions[created.ID]["status"] = "expired"
						f.mu.Unlock()
						if scenario == "expired_webhook_race" {
							raw, _ := json.Marshal(map[string]any{"id": created.ID, "customer": f.customer, "client_reference_id": f.team.String(), "metadata": map[string]string{"checkout_generation": p.Account.CheckoutInitializingAt.Time.UTC().Format(time.RFC3339Nano)}})
							event := stripeEventEnvelope{ID: "evt_" + uuid.NewString(), Type: "checkout.session.expired", Data: stripeEventEnvelopeData{Object: raw}}
							payload, _ := json.Marshal(event)
							f.exec(`INSERT INTO stripe_webhook_event(event_id,event_type,payload) VALUES($1,$2,$3)`, event.ID, event.Type, payload)
							if err := f.h.processRecordedStripeWebhook(t.Context(), event); err != nil {
								t.Error(err)
							}
						}
						if scenario == "changed_generation" {
							f.exec(`UPDATE team_billing_account SET checkout_initializing_at=checkout_initializing_at+interval '1 second' WHERE team_id=$1`, f.team)
						}
					}
					gets++
					if scenario == "missing_link_newer_generation" && gets == 3 {
						f.exec(`UPDATE team_billing_account SET checkout_initializing_at=checkout_initializing_at+interval '1 second' WHERE team_id=$1`, f.team)
					}
					if scenario == "established_subscription_resume" {
						f.exec(`UPDATE team_billing_account SET stripe_subscription_id=$2,stripe_subscription_status='active' WHERE team_id=$1`, f.team, f.sub)
					}
					if scenario == "timeout" || (scenario == "timeout_after_expire" && cancelCalls > 0) {
						w.WriteHeader(504)
						return
					}
					f.mu.Lock()
					data := map[string]any{}
					for k, v := range f.sessions[created.ID] {
						data[k] = v
					}
					f.mu.Unlock()
					data["payment_status"] = "unpaid"
					if scenario == "expired" || strings.HasPrefix(scenario, "expired_with_") {
						data["status"] = "expired"
					}
					if scenario == "expired_with_invoice" {
						data["invoice"] = "in_pending"
					}
					if scenario == "expired_with_payment_intent" {
						data["payment_intent"] = "pi_pending"
					}
					if scenario == "expired_with_subscription" {
						data["subscription"] = "sub_pending"
					}
					if scenario == "missing_link_foreign_purchase" {
						data["metadata"] = map[string]string{"activation_user_id": actor.String(), "checkout_generation": p.Account.CheckoutInitializingAt.Time.UTC().Format(time.RFC3339Nano), "prepaid_purchase_id": uuid.NewString()}
					}
					if scenario == "missing_link_paid" || scenario == "paid" || (scenario == "paid_during_expire" && cancelCalls > 0) {
						data["status"] = "complete"
						data["payment_status"] = "paid"
						data["subscription"] = "sub_completed"
					}
					if scenario == "mismatch" || (scenario == "mismatch_after_expire" && cancelCalls > 0) {
						data["customer"] = "cus_wrong"
					}
					json.NewEncoder(w).Encode(data)
					return
				}
				f.serve(w, request)
			}))
			defer provider.Close()
			f.client.baseURL = provider.URL
			f.client.httpClient = provider.Client()
			currentActor := actor
			if scenario == "wrong_actor" || scenario == "missing_link_wrong_actor" {
				currentActor = uuid.New()
			}
			r.POST("/billing/prepaid/checkout/recover", func(c *gin.Context) {
				team := f.team
				if scenario == "wrong_team" {
					team = uuid.New()
				}
				c.Set("team_id", team.String())
				c.Set("actor_id", currentActor)
				f.h.RecoverPendingStarterCheckout(c)
			})
			// The alternate actor has billing access but cannot recover somebody else's session.
			if scenario == "wrong_actor" || scenario == "missing_link_wrong_actor" {
				prepaidOperationRouter(t, f, f.team, currentActor)
			}
			post := func(input pendingStarterRequest) *httptest.ResponseRecorder {
				b, _ := json.Marshal(input)
				out := httptest.NewRecorder()
				r.ServeHTTP(out, httptest.NewRequest("POST", "/billing/prepaid/checkout/recover", strings.NewReader(string(b))))
				return out
			}
			if strings.HasPrefix(scenario, "missing_link_") {
				f.exec(`UPDATE billing_prepaid_purchase SET checkout_session_id=NULL WHERE id=$1`, p.Purchase)
				if scenario == "missing_link_other_session" {
					f.exec(`UPDATE billing_prepaid_purchase SET checkout_session_id=$2 WHERE id=$1`, p.Purchase, "cs_other_"+uuid.NewString())
				}
			}
			discover := post(pendingStarterRequest{})
			if scenario == "missing_link_foreign_purchase" || scenario == "missing_link_other_session" {
				if discover.Code != 500 || strings.Contains(discover.Body.String(), "no_pending_starter_checkout") || strings.Contains(discover.Body.String(), "https:") || cancelCalls != 0 {
					t.Fatalf("unsafe association discovery %d %s", discover.Code, discover.Body)
				}
				return
			}
			if scenario == "wrong_team" {
				if discover.Code != 403 {
					t.Fatalf("wrong team %d %s", discover.Code, discover.Body)
				}
				return
			}
			if scenario == "wrong_actor" || scenario == "missing_link_wrong_actor" {
				if !strings.Contains(discover.Body.String(), "no_pending_starter_checkout") || gets != 0 {
					t.Fatalf("actor exposure %d %s", discover.Code, discover.Body)
				}
				return
			}
			if discover.Code != 200 || !strings.Contains(discover.Body.String(), "pending_starter_checkout") || strings.Contains(discover.Body.String(), "https:") {
				t.Fatalf("discovery %d %s", discover.Code, discover.Body)
			}
			input := pendingStarterRequest{PurchaseID: p.Purchase, Generation: &p.Account.CheckoutInitializingAt.Time, Cancel: scenario != "missing_link_resume" && scenario != "missing_link_newer_generation" && scenario != "resume" && scenario != "closed_entry" && scenario != "expired" && scenario != "legacy_resume" && scenario != "legacy_closed_entry" && scenario != "established_subscription_resume"}
			if scenario == "wrong_generation" {
				g := input.Generation.Add(time.Second)
				input.Generation = &g
			}
			if scenario == "wrong_request" {
				input.PurchaseID = uuid.New()
			}
			got := post(input)
			var fence bool
			var payment string
			if err := f.h.Pool.QueryRow(t.Context(), `SELECT a.checkout_initializing_at IS NOT NULL,p.payment_state FROM team_billing_account a JOIN billing_prepaid_purchase p USING(team_id) WHERE p.id=$1`, p.Purchase).Scan(&fence, &payment); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing_link_resume", "resume", "closed_entry", "legacy_resume", "legacy_closed_entry":
				if got.Code != 200 || !strings.Contains(got.Body.String(), "recovered") || cancelCalls != 0 || !fence {
					t.Fatalf("resume %d %s fence%v", got.Code, got.Body, fence)
				}
			case "missing_link_cancel", "cancel", "expired", "failed_fence", "expired_webhook_race", "legacy_cancel":
				if got.Code != 200 || !strings.Contains(got.Body.String(), "retryable") || fence || payment != "failed" {
					t.Fatalf("release %d %s fence%v %s", got.Code, got.Body, fence, payment)
				}
				if scenario == "expired" {
					var reason string
					if err := f.h.Pool.QueryRow(t.Context(), `SELECT failure_reason FROM billing_prepaid_purchase WHERE id=$1`, p.Purchase).Scan(&reason); err != nil || reason != "The unpaid checkout expired" {
						t.Fatalf("natural expiry attribution %q %v", reason, err)
					}
				}
				var evidence bool
				if err := f.h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM stripe_checkout_expiration_evidence WHERE team_id=$1)`, f.team).Scan(&evidence); err != nil || !evidence {
					t.Fatalf("expiration %v %v", evidence, err)
				}
			case "established_subscription_resume":
				if !fence || cancelCalls != 0 || strings.Contains(got.Body.String(), "https:") || !strings.Contains(got.Body.String(), "payment_confirmation_pending") {
					t.Fatalf("payable session exposed for established subscription: %d %s", got.Code, got.Body)
				}
			case "wrong_request", "wrong_generation":
				if got.Code != 409 || gets != 0 || !fence {
					t.Fatalf("wrong target %d %s", got.Code, got.Body)
				}
			default:
				if !fence || cancelCalls > 1 || !strings.Contains(got.Body.String(), "payment_confirmation_pending") {
					t.Fatalf("unsafe uncertain release %s %d %s fence%v", scenario, got.Code, got.Body, fence)
				}
			}
			if scenario == "missing_link_resume" || scenario == "missing_link_cancel" {
				var linked string
				if err := f.h.Pool.QueryRow(t.Context(), `SELECT checkout_session_id FROM billing_prepaid_purchase WHERE id=$1`, p.Purchase).Scan(&linked); err != nil || linked != created.ID {
					t.Fatalf("original session association %q %v", linked, err)
				}
			}
			if scenario == "missing_link_newer_generation" {
				var unlinked bool
				if err := f.h.Pool.QueryRow(t.Context(), `SELECT checkout_session_id IS NULL FROM billing_prepaid_purchase WHERE id=$1`, p.Purchase).Scan(&unlinked); err != nil || !unlinked {
					t.Fatalf("newer generation bound stale session %v %v", unlinked, err)
				}
			}

			if scenario == "lost_ack" {
				retry := post(input)
				if retry.Code != 200 || !strings.Contains(retry.Body.String(), "retryable") || cancelCalls != 1 {
					t.Fatalf("lost ack retry %d %s", retry.Code, retry.Body)
				}
			}
			if scenario == "missing_link_resume" {
				replay := post(input)
				if replay.Code != 200 || replay.Body.String() != got.Body.String() || cancelCalls != 0 {
					t.Fatalf("association recovery replay %d %s", replay.Code, replay.Body)
				}
			}
			if scenario == "cancel" || scenario == "missing_link_cancel" {
				replay := post(input)
				if replay.Code != 200 || !strings.Contains(replay.Body.String(), "no_pending_starter_checkout") || cancelCalls != 1 {
					t.Fatalf("cancel replay %d %s", replay.Code, replay.Body)
				}
			}
			if scenario == "expired_webhook_race" {
				var raw []byte
				if err := f.h.Pool.QueryRow(t.Context(), `SELECT payload FROM stripe_webhook_event WHERE payload->'data'->'object'->>'id'=$1`, created.ID).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var event stripeEventEnvelope
				_ = json.Unmarshal(raw, &event)
				if err := f.h.processRecordedStripeWebhook(t.Context(), event); err != nil {
					t.Fatal(err)
				}
			}
			var count int
			if err := f.h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM billing_prepaid_purchase WHERE team_id=$1`, f.team).Scan(&count); err != nil || count != 1 {
				t.Fatalf("duplicated purchase %d %v", count, err)
			}
		})
	}
}

func TestPrepaidPendingStarterRecoveryRejectsInvalidAction(t *testing.T) {
	f := newPrepaidFixture(t)
	actor := uuid.New()
	r := prepaidOperationRouter(t, f, f.team, actor)
	r.POST("/billing/prepaid/checkout/recover", func(c *gin.Context) {
		c.Set("team_id", f.team.String())
		c.Set("actor_id", actor)
		f.h.RecoverPendingStarterCheckout(c)
	})
	for _, body := range []string{`null`, `[]`, `{"cancel":true}`, `{"purchase_id":"` + uuid.NewString() + `"}`, `{"checkout_generation":"2026-10-26T12:00:00Z"}`, `{"unknown":true}`, `{} {}`, `{"purchase_id":"invalid"}`, `{"checkout_generation":"invalid","purchase_id":"` + uuid.NewString() + `"}`} {
		t.Run(body, func(t *testing.T) {
			out := httptest.NewRecorder()
			r.ServeHTTP(out, httptest.NewRequest("POST", "/billing/prepaid/checkout/recover", strings.NewReader(body)))
			if out.Code != 400 {
				t.Fatalf("invalid action %d %s", out.Code, out.Body)
			}
		})
	}
}

// A historical hosted Pro session shares the account fence but has no prepaid
// purchase identity. Exercise the real provider transport and accepted-Pro
// replay, not a mocked Starter response or a newly sold v2 Pro upgrade.
func TestPrepaidDiscoveryPreservesAcceptedLegacyPro(t *testing.T) {
	for _, scenario := range []string{"known", "provider_actor", "provider_generation", "provider_customer", "provider_session", "provider_offer", "provider_mode", "local_session", "frozen_actor", "frozen_prepaid", "frozen_unknown_family", "present_valid_prepaid", "present_invalid_prepaid", "present_empty_prepaid", "unknown_family"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPrepaidFixture(t)
			actor := uuid.New()
			router := prepaidOperationRouter(t, f, f.team, actor)
			generation := time.Now().Truncate(time.Microsecond)
			session, key := "cs_legacy_"+strings.ReplaceAll(uuid.NewString(), "-", ""), "pro:legacy:"+uuid.NewString()
			metadata := map[string]string{"activation_user_id": actor.String(), "checkout_generation": generation.Format(time.RFC3339Nano), "commercial_offer": "pro_monthly"}
			params := StripeCreateCheckoutSessionParams{CustomerID: f.customer, ClientReferenceID: f.team.String(), IdempotencyKey: key, SuccessURL: "https://console.example.com/ok", CancelURL: "https://console.example.com/cancel", BasePriceID: "price_legacy", Metadata: metadata}
			switch scenario {
			case "frozen_actor":
				params.Metadata["activation_user_id"] = uuid.NewString()
			case "frozen_prepaid":
				params.Metadata["prepaid_purchase_id"] = uuid.NewString()
			case "frozen_unknown_family":
				params.PlanTerms = &StripeCheckoutPlanTerms{Amount: 120000, Months: 1, Credit: 100000, Family: "unknown"}
			}
			payload, _ := json.Marshal(params)
			f.exec(`UPDATE team_billing_account SET stripe_subscription_id=NULL,stripe_subscription_status=NULL,checkout_initializing_at=$2,checkout_session_id=$3,checkout_request_key=$4,checkout_may_exist=true,checkout_pending_attempt_ids='{}',stripe_checkout_actor_id=$5,stripe_checkout_actor_claimed_at=$2 WHERE team_id=$1`, f.team, generation, session, key, actor)
			f.exec(`INSERT INTO billing_signup_choice(team_id,state,offer_key,base_price_id,generation,actor_id,request_payload,session_id) VALUES($1,'pending','pro_monthly','price_legacy',$2,$3,$4,$5) ON CONFLICT(team_id) DO UPDATE SET state=excluded.state,offer_key=excluded.offer_key,base_price_id=excluded.base_price_id,generation=excluded.generation,actor_id=excluded.actor_id,request_payload=excluded.request_payload,session_id=excluded.session_id,subscription_id=NULL`, f.team, generation, actor, payload, session)
			if scenario == "local_session" {
				f.exec(`UPDATE billing_signup_choice SET session_id=$2 WHERE team_id=$1`, f.team, "cs_different")
			}
			// Unknown metadata must not be reclassified merely because no prepaid row exists.
			providerMetadata := map[string]string{"activation_user_id": actor.String(), "checkout_generation": generation.Format(time.RFC3339Nano), "commercial_offer": "pro_monthly"}
			providerSession := StripeRetrievedCheckoutSession{ID: session, CustomerID: f.customer, ClientReferenceID: f.team.String(), Mode: "subscription", Status: "open", PaymentStatus: "unpaid", URL: "https://checkout.stripe.com/original-legacy", Metadata: providerMetadata}
			switch scenario {
			case "provider_actor":
				providerMetadata["activation_user_id"] = uuid.NewString()
			case "provider_generation":
				providerMetadata["checkout_generation"] = generation.Add(time.Second).Format(time.RFC3339Nano)
			case "provider_customer":
				providerSession.CustomerID = "cus_foreign"
			case "provider_session":
				providerSession.ID = "cs_foreign"
			case "provider_offer", "unknown_family":
				providerMetadata["commercial_offer"] = "unknown"
			case "provider_mode":
				providerSession.Mode = "setup"
			case "present_valid_prepaid":
				providerMetadata["prepaid_purchase_id"] = uuid.NewString()
			case "present_invalid_prepaid":
				providerMetadata["prepaid_purchase_id"] = "malformed"
			case "present_empty_prepaid":
				providerMetadata["prepaid_purchase_id"] = ""
			}
			writes, reads := 0, 0
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					t.Errorf("Starter/Pro recovery created or cancelled a provider object: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
					return
				}
				if r.URL.Path != "/v1/checkout/sessions/"+session {
					t.Errorf("unexpected provider GET %s", r.URL.Path)
					w.WriteHeader(500)
					return
				}
				reads++
				_ = json.NewEncoder(w).Encode(providerSession)
			}))
			defer provider.Close()
			f.h.Stripe = &stripeHTTPClient{baseURL: provider.URL, httpClient: provider.Client(), apiVersion: "2026-07-29.dahlia"}
			snapshot := func() string {
				var raw string
				err := f.h.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object('account',to_jsonb(a),'choice',to_jsonb(c),'purchases',(SELECT count(*) FROM billing_prepaid_purchase WHERE team_id=$1),'contracts',(SELECT count(*) FROM billing_commercial_contract WHERE team_id=$1))::text FROM team_billing_account a JOIN billing_signup_choice c USING(team_id) WHERE a.team_id=$1`, f.team).Scan(&raw)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			before := snapshot()
			router.POST("/billing/prepaid/checkout/recover", func(c *gin.Context) {
				c.Set("team_id", f.team.String())
				c.Set("actor_id", actor)
				f.h.RecoverPendingStarterCheckout(c)
			})
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/billing/prepaid/checkout/recover", strings.NewReader(`{}`)))
			if scenario == "known" {
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"no_pending_starter_checkout"`) {
					t.Fatalf("known legacy Pro classification: %d %s", rec.Code, rec.Body)
				}
			} else if rec.Code != 500 {
				t.Fatalf("uncertain legacy identity escaped: %d %s", rec.Code, rec.Body)
			}
			if before != snapshot() {
				t.Fatal("Starter discovery mutated the Pro fence, choice, or financial state")
			}
			if writes != 0 || reads != 1 {
				t.Fatalf("unexpected discovery transport reads=%d writes=%d", reads, writes)
			}
			if scenario != "known" {
				return
			}
			account, err := f.h.DB.GetTeamBillingCheckoutForRecovery(t.Context(), f.team)
			if err != nil {
				t.Fatal(err)
			}
			differentKey := "pro:different-original-request"
			account.CheckoutRequestKey = &differentKey
			if known, err := f.h.pendingStarterIsKnownPro(t.Context(), providerSession, pendingStarterCheckout{Account: account}, actor); err != nil || known {
				t.Fatalf("different original request classified as known Pro: %v %v", known, err)
			}
			// Even an explicit Starter cancellation cannot cancel this Pro session.
			rec = httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/billing/prepaid/checkout/recover", strings.NewReader(fmt.Sprintf(`{"purchase_id":%q,"checkout_generation":%q,"cancel":true}`, uuid.NewString(), generation.Format(time.RFC3339Nano)))))
			if rec.Code != 200 || before != snapshot() || writes != 0 {
				t.Fatalf("Starter action changed Pro authority: %d %s", rec.Code, rec.Body)
			}
			// Entry can close/change pricing while this older accepted checkout remains
			// recoverable through the original Pro status and frozen-parameter replay.
			f.exec(`UPDATE team_feature_flag SET enabled=false WHERE team_id=$1 AND key='billing_export_enabled'`, f.team)
			outcome, err := f.h.reconcileProCheckout(t.Context(), f.team, false)
			if err != nil || outcome != "payment_confirmation_pending" {
				t.Fatalf("legacy Pro status lost: %s %v", outcome, err)
			}
			rec = httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/stripe/pro-checkout", nil)
			f.h.createProCheckout(c, f.team, actor, billingCheckoutSessionRequest{Offer: "pro_monthly", SuccessURL: params.SuccessURL, CancelURL: params.CancelURL})
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), providerSession.URL) || writes != 0 {
				t.Fatalf("original Pro replay lost or created a new checkout: %d %s writes=%d", rec.Code, rec.Body, writes)
			}
		})
	}
}

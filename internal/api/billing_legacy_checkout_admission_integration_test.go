//go:build integration

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type legacyAdmissionStripe struct {
	StripeBillingClient
	mu            sync.Mutex
	requests      map[string]StripeCreateCheckoutSessionParams
	sessions      map[string]StripeCheckoutSession
	lose          bool
	customerCalls int
}

func (s *legacyAdmissionStripe) CreateCustomer(context.Context, StripeCreateCustomerParams) (StripeCustomer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.customerCalls++
	return StripeCustomer{}, errors.New("unexpected customer creation")
}
func (s *legacyAdmissionStripe) CreateCheckoutSession(_ context.Context, p StripeCreateCheckoutSessionParams) (StripeCheckoutSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.requests[p.IdempotencyKey]; ok && !reflect.DeepEqual(old, p) {
		return StripeCheckoutSession{}, errors.New("Stripe returned 400: changed accepted params")
	}
	session, ok := s.sessions[p.IdempotencyKey]
	if !ok {
		session = StripeCheckoutSession{ID: "cs_" + uuid.NewString(), URL: "https://checkout.example.com/original"}
		s.sessions[p.IdempotencyKey] = session
		s.requests[p.IdempotencyKey] = p
	}
	if s.lose {
		s.lose = false
		return StripeCheckoutSession{}, context.DeadlineExceeded
	}
	return session, nil
}

type legacyAdmissionFixtureState struct {
	*prepaidFixture
	legacy *legacyAdmissionStripe
}

func legacyAdmissionFixture(t *testing.T, kind string) (*legacyAdmissionFixtureState, uuid.UUID) {
	t.Helper()
	f := newPrepaidFixture(t)
	actor := uuid.New()
	prepaidOperationRouter(t, f, f.team, actor)
	f.h.Config.StripeCheckoutPriceIDs = []string{"price_cpu", "price_memory"}
	f.exec(`UPDATE team_billing_account SET stripe_subscription_id=NULL,stripe_subscription_status=NULL WHERE team_id=$1`, f.team)
	f.exec(`INSERT INTO team_feature_flag(team_id,key,enabled) VALUES($1,'billing_export_enabled',true) ON CONFLICT(team_id,key) DO UPDATE SET enabled=true`, f.team)
	if _, err := assignInTx(t, f.h, planAssignment{Team: f.team, Terms: testPlanTerms("fixture-plan", kind, 1, nil), Source: planSourcePlanChange, Reason: "fixture checkout admission"}); err != nil {
		t.Fatal(err)
	}
	stub := &legacyAdmissionStripe{StripeBillingClient: f.h.Stripe, requests: map[string]StripeCreateCheckoutSessionParams{}, sessions: map[string]StripeCheckoutSession{}}
	f.h.Stripe = stub
	return &legacyAdmissionFixtureState{f, stub}, actor
}

func legacyAdmissionPost(t *testing.T, f *legacyAdmissionFixtureState, actor uuid.UUID, trusted bool, req billingCheckoutSessionRequest) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	r.POST("/checkout", func(c *gin.Context) {
		c.Set("team_id", f.team.String())
		c.Set("actor_id", actor)
		if trusted {
			c.Set("promotion_account", &promotionAccountClaims{
				RegisteredClaims: jwt.RegisteredClaims{Subject: actor.String()}, Operation: "checkout", TeamID: f.team.String(), OperationID: req.OperationID.String(), HomeRegion: req.HomeRegion, Decision: req.Decision, SuccessURL: req.SuccessURL, CancelURL: req.CancelURL,
			})
		}
		f.h.CreateStripeCheckoutSession(c)
	})
	raw, _ := json.Marshal(req)
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/checkout", strings.NewReader(string(raw)))
	request.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, request)
	return rec
}

func legacyAdmissionRequest(trusted bool) billingCheckoutSessionRequest {
	req := billingCheckoutSessionRequest{SuccessURL: "https://console.example.com/ok", CancelURL: "https://console.example.com/cancel"}
	if trusted {
		req.OperationID = uuid.New()
		req.HomeRegion = "use"
		req.Decision = "publication_failed"
	}
	return req
}

func TestLegacyCheckoutRejectsAssignedPlansBeforeSideEffects(t *testing.T) {
	for _, kind := range []string{planKindFree, planKindPrepaid, planKindSubscription, planKindCustom} {
		for _, trusted := range []bool{false, true} {
			for _, offer := range []string{"", "free"} {
				t.Run(kind+"/"+stringBool(trusted)+"/"+offer, func(t *testing.T) {
					f, actor := legacyAdmissionFixture(t, kind)
					f.exec(`UPDATE team_billing_account SET stripe_customer_id=NULL WHERE team_id=$1`, f.team)
					var before string
					if err := f.h.Pool.QueryRow(t.Context(), `SELECT row_to_json(a)::text FROM team_billing_account a WHERE team_id=$1`, f.team).Scan(&before); err != nil {
						t.Fatal(err)
					}
					req := legacyAdmissionRequest(trusted)
					req.Offer = offer
					got := legacyAdmissionPost(t, f, actor, trusted, req)
					if got.Code != http.StatusConflict || !strings.Contains(got.Body.String(), "plan-aware checkout") {
						t.Fatalf("incompatible admission: %d %s", got.Code, got.Body)
					}
					var after string
					if err := f.h.Pool.QueryRow(t.Context(), `SELECT row_to_json(a)::text FROM team_billing_account a WHERE team_id=$1`, f.team).Scan(&after); err != nil || after != before {
						t.Fatalf("denied admission changed account: %v", err)
					}
					f.legacy.mu.Lock()
					count := len(f.legacy.sessions) + f.legacy.customerCalls
					f.legacy.mu.Unlock()
					if count != 0 {
						t.Fatalf("created %d provider sessions", count)
					}
				})
			}
		}
	}
}

func stringBool(v bool) string {
	if v {
		return "trusted"
	}
	return "ordinary"
}

func TestLegacyCheckoutAcceptedReplaySurvivesPlanChange(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, lost := range []bool{false, true} {
			name := stringBool(trusted)
			if lost {
				name += "/lost-response"
			}
			t.Run(name, func(t *testing.T) {
				f, actor := legacyAdmissionFixture(t, planKindLegacyPAYG)
				f.legacy.lose = lost
				req := legacyAdmissionRequest(trusted)
				first := legacyAdmissionPost(t, f, actor, trusted, req)
				want := 200
				if lost {
					want = 502
				}
				if first.Code != want {
					t.Fatalf("legacy entry: %d %s", first.Code, first.Body)
				}
				before, err := f.h.DB.GetTeamBillingCheckoutForRecovery(t.Context(), f.team)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = assignInTx(t, f.h, planAssignment{Team: f.team, Terms: testPlanTerms("free", planKindFree, 1, nil), Source: planSourcePlanChange, Reason: "fixture changed after accepted checkout"}); err != nil {
					t.Fatal(err)
				}
				replay := legacyAdmissionPost(t, f, actor, trusted, req)
				if replay.Code != 200 || (!lost && replay.Body.String() != first.Body.String()) {
					t.Fatalf("accepted replay: %d %s", replay.Code, replay.Body)
				}
				after, err := f.h.DB.GetTeamBillingCheckoutForRecovery(t.Context(), f.team)
				if err != nil {
					t.Fatal(err)
				}
				if !before.CheckoutInitializingAt.Time.Equal(after.CheckoutInitializingAt.Time) || *before.CheckoutRequestKey != *after.CheckoutRequestKey || before.StripeCheckoutActorID != after.StripeCheckoutActorID {
					t.Fatal("accepted identity changed")
				}
				var response StripeCheckoutSession
				if err = json.Unmarshal(replay.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				f.legacy.mu.Lock()
				count := len(f.legacy.sessions)
				var original StripeCheckoutSession
				for _, session := range f.legacy.sessions {
					original = session
				}
				f.legacy.mu.Unlock()
				if count != 1 || response.ID != original.ID || after.CheckoutSessionID == nil || *after.CheckoutSessionID != original.ID {
					t.Fatalf("accepted session changed: %d %+v %+v", count, response, original)
				}
				bad := req
				bad.SuccessURL += "?different=1"
				if got := legacyAdmissionPost(t, f, actor, trusted, bad); got.Code != 409 {
					t.Fatalf("different request reused accepted lease: %d %s", got.Code, got.Body)
				}
			})
		}
	}
}

func TestLegacyCheckoutUnassignedStillAllowed(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(stringBool(trusted), func(t *testing.T) {
			f, actor := legacyAdmissionFixture(t, planKindLegacyPAYG)
			f.exec(`UPDATE team_plan_assignment SET effective_to=clock_timestamp() WHERE team_id=$1 AND effective_to IS NULL`, f.team)
			if got := legacyAdmissionPost(t, f, actor, trusted, legacyAdmissionRequest(trusted)); got.Code != 200 {
				t.Fatalf("unassigned legacy entry: %d %s", got.Code, got.Body)
			}
		})
	}
}

func TestLegacyCheckoutSerializesAssignmentBeforeFreshAdmission(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, initial := range []bool{false, true} {
			name := stringBool(trusted)
			if initial {
				name += "/first-assignment"
			}
			t.Run(name, func(t *testing.T) {
				f, actor := legacyAdmissionFixture(t, planKindLegacyPAYG)
				if initial {
					f.exec(`UPDATE team_plan_assignment SET effective_to=clock_timestamp() WHERE team_id=$1 AND effective_to IS NULL`, f.team)
					// A customized team makes the INSERT trigger take only KEY SHARE, so this
					// catches a weaker team lock that serializes changes but misses first plans.
					f.exec(`UPDATE team SET limits_customized=true WHERE id=$1`, f.team)
				}
				tx, err := f.h.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.Background())
				if initial {
					_, err = tx.Exec(t.Context(), `INSERT INTO team_plan_assignment(team_id,plan_key,kind,terms,limits,effective_from,source,reason) VALUES($1,'free','free','{}','{"max_concurrent":1}',clock_timestamp(),'signup','fixture first assignment')`, f.team)
				} else {
					_, err = assignTeamPlan(t.Context(), tx, planAssignment{Team: f.team, Terms: testPlanTerms("free", planKindFree, 1, nil), Source: planSourcePlanChange, Reason: "fixture concurrent assignment"})
				}
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					_, err := f.h.beginLegacyCheckout(t.Context(), f.team, actor, legacyAdmissionRequest(trusted), "fixture-key", uuid.New(), trusted)
					done <- err
				}()
				// Observe the actual database lock wait, not a scheduling-dependent sleep.
				deadline := time.Now().Add(3 * time.Second)
				for {
					var waiting bool
					err = f.h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT id FROM team WHERE id=%FOR UPDATE%')`).Scan(&waiting)
					if err != nil {
						t.Fatal(err)
					}
					if waiting {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("checkout did not wait for assignment")
					}
					time.Sleep(10 * time.Millisecond)
				}
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err = <-done; !errors.Is(err, errLegacyCheckoutPlan) {
					t.Fatalf("concurrent assignment bypassed: %v", err)
				}
				var fresh bool
				if err = f.h.Pool.QueryRow(t.Context(), `SELECT checkout_initializing_at IS NULL AND checkout_request_key IS NULL FROM team_billing_account WHERE team_id=$1`, f.team).Scan(&fresh); err != nil || !fresh {
					t.Fatalf("denied checkout left a fence: %v", err)
				}
			})
		}
	}
}

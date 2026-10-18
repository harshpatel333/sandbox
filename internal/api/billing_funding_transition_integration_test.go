//go:build integration

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/superserve-ai/sandbox/internal/db"
)

func fundingTransitionFixture(t *testing.T) (*Handlers, uuid.UUID) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil || !strings.HasSuffix(cfg.ConnConfig.Database, "_test") {
		t.Fatal("requires migrated disposable test database")
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h := &Handlers{Pool: pool, DB: db.New(pool)}
	team, err := h.DB.CreateTeam(t.Context(), "transition-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	return h, team.ID
}

func TestFundingReleaseRevokedUnactivatedYC(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		t.Run(map[bool]string{false: "unattempted", true: "response-loss"}[attempted], func(t *testing.T) {
			h, team := fundingTransitionFixture(t)
			ctx := t.Context()
			customer := "cus_" + team.String()
			if _, err := h.Pool.Exec(ctx, `INSERT INTO team_billing_account(team_id,stripe_customer_id) VALUES($1,$2)`, team, customer); err != nil {
				t.Fatal(err)
			}
			if _, err := h.Pool.Exec(ctx, `INSERT INTO billing_yc_operation(team_id,approved_by,evidence_reference,request,resource_terms,customer_id,state,revoked_at,revoked_by,provider_attempt_at) VALUES($1,'operator','fixture',jsonb_build_object('company_identity',$1::uuid::text),'{}',$2,'ended',now(),'operator',CASE WHEN $3 THEN now() END)`, team, customer, attempted); err != nil {
				t.Fatal(err)
			}
			err := h.commitFreeFundingPlan(ctx, team, uuid.New())
			if attempted {
				if !errors.Is(err, errFundingPlanTransition) {
					t.Fatalf("unknown provider outcome released: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = h.commitFreeFundingPlan(ctx, team, uuid.New()); err != nil {
				t.Fatal("retry", err)
			}
			var releases int
			var intact, free bool
			if err = h.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM billing_plan_release WHERE team_id=$1),(SELECT state='ended' AND revoked_at IS NOT NULL FROM billing_yc_operation WHERE team_id=$1),(SELECT offer_key='free' AND state='committed' FROM billing_signup_choice WHERE team_id=$1)`, team).Scan(&releases, &intact, &free); err != nil || releases != 1 || !intact || !free {
				t.Fatalf("release lost history or replayed: %d %v %v %v", releases, intact, free, err)
			}
			if _, err = h.Pool.Exec(ctx, `UPDATE team_billing_account SET checkout_initializing_at=now(),checkout_request_key='replacement' WHERE team_id=$1`, team); err != nil {
				t.Fatal("replacement checkout blocked", err)
			}
			object, _ := json.Marshal(map[string]string{"id": "sub_replacement", "customer": customer})
			handled, err := h.processProWebhook(ctx, stripeEventEnvelope{ID: "evt_" + uuid.NewString(), Type: "customer.subscription.created", Data: stripeEventEnvelopeData{Object: object}})
			if err != nil || handled {
				t.Fatalf("replacement routed to historical YC: %v %v", handled, err)
			}
		})
	}
}

func TestFundingReleaseRequiresTerminalProviderProof(t *testing.T) {
	for _, status := range []string{"active", "past_due", "canceled", "incomplete_expired"} {
		t.Run(status, func(t *testing.T) {
			h, team := fundingTransitionFixture(t)
			ctx := t.Context()
			customer, subscription := "cus_"+team.String(), "sub_"+team.String()
			if _, err := h.Pool.Exec(ctx, `INSERT INTO team_billing_account(team_id,stripe_customer_id,stripe_subscription_id,stripe_subscription_status,commercial_billing_anchor) VALUES($1,$2,$3,'active',now())`, team, customer, subscription); err != nil {
				t.Fatal(err)
			}
			if _, err := h.Pool.Exec(ctx, `UPDATE billing_signup_choice SET offer_key='pro_monthly',state='committed',generation=now(),actor_id=$2,base_price_id='price_base',request_payload='{}',subscription_id=$3 WHERE team_id=$1`, team, uuid.New(), subscription); err != nil {
				t.Fatal(err)
			}
			contract := uuid.New()
			if _, err := h.Pool.Exec(ctx, `INSERT INTO billing_commercial_contract(id,team_id,offer_key,starts_at,customer_id,subscription_id,base_price_id,charge_amount_cents,charge_interval_months,entitlement_amount_cents,entitlement_cadence,entitlement_anchor) SELECT $1,$2,'pro_monthly',commercial_billing_anchor,$3,$4,'price_base',120000,1,100000,'monthly',commercial_billing_anchor FROM team_billing_account WHERE team_id=$2`, contract, team, customer, subscription); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/v1/subscriptions/"+subscription {
					t.Errorf("unexpected provider mutation: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": subscription, "customer": customer, "status": status, "items": map[string]any{"data": []map[string]string{{"id": "si_example"}}}})
			}))
			defer server.Close()
			h.Stripe = &stripeHTTPClient{baseURL: server.URL, httpClient: server.Client(), apiVersion: "2026-05-27.dahlia"}
			err := h.commitFreeFundingPlan(ctx, team, uuid.New())
			if status == "active" || status == "past_due" {
				if !errors.Is(err, errFundingPlanTransition) {
					t.Fatalf("nonterminal provider released: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var contractState string
			if err = h.Pool.QueryRow(ctx, `SELECT status FROM billing_commercial_contract WHERE id=$1`, contract).Scan(&contractState); err != nil || contractState != "ended" {
				t.Fatalf("contract not retained and ended: %q %v", contractState, err)
			}
			var saved string
			if err = h.Pool.QueryRow(ctx, `SELECT stripe_subscription_status FROM team_billing_account WHERE team_id=$1`, team).Scan(&saved); err != nil || saved != status {
				t.Fatalf("terminal projection not updated: %q %v", saved, err)
			}
		})
	}
}

func TestFundingReleaseCompletedProCheckout(t *testing.T) {
	for _, scenario := range []string{"completed", "non-UTC", "different-generation", "missing-generation", "malformed-generation", "checkout-generation-mismatch", "subscription-generation-mismatch", "competing-attempt"} {
		t.Run(scenario, func(t *testing.T) {
			competing := scenario == "competing-attempt"
			h, team := fundingTransitionFixture(t)
			ctx := t.Context()
			actor := uuid.New()
			customer, subscription := "cus_"+team.String(), "sub_"+team.String()
			session := "cs_" + strings.ReplaceAll(team.String(), "-", "")
			generation := time.Now().UTC().Truncate(time.Microsecond)
			key := "pro:fixture:" + team.String()
			frozenGeneration := generation.Format(time.RFC3339Nano)
			switch scenario {
			case "non-UTC":
				frozenGeneration = generation.In(time.FixedZone("fixture", -5*60*60)).Format(time.RFC3339Nano)
			case "different-generation":
				frozenGeneration = generation.Add(time.Second).Format(time.RFC3339Nano)
			case "missing-generation":
				frozenGeneration = ""
			case "malformed-generation":
				frozenGeneration = "not-a-timestamp"
			}
			metadata := map[string]string{"checkout_generation": frozenGeneration, "commercial_offer": "pro_monthly", "activation_user_id": actor.String()}
			params, _ := json.Marshal(StripeCreateCheckoutSessionParams{CustomerID: customer, IdempotencyKey: key, Metadata: metadata})
			if _, err := h.Pool.Exec(ctx, `INSERT INTO team_billing_account(team_id,stripe_customer_id,stripe_subscription_id,stripe_subscription_status) VALUES($1,$2,$3,'active')`, team, customer, subscription); err != nil {
				t.Fatal(err)
			}
			if _, err := h.Pool.Exec(ctx, `UPDATE billing_signup_choice SET offer_key='pro_monthly',state='committed',generation=$2,actor_id=$3,base_price_id='price_base',request_payload=$4,session_id=$5,subscription_id=$6 WHERE team_id=$1`, team, generation, actor, params, session, subscription); err != nil {
				t.Fatal(err)
			}
			attempts := []uuid.UUID{}
			if competing {
				attempts = append(attempts, uuid.New())
			}
			if _, err := h.Pool.Exec(ctx, `UPDATE team_billing_account SET checkout_initializing_at=$2,checkout_request_key=$3,checkout_session_id=$4,checkout_may_exist=true,checkout_pending_attempt_ids=$5 WHERE team_id=$1`, team, generation, key, session, attempts); err != nil {
				t.Fatal(err)
			}
			gets := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets++
				if r.Method != "GET" {
					t.Errorf("unexpected provider mutation: %s", r.Method)
					w.WriteHeader(500)
					return
				}
				providerMetadata := map[string]string{"checkout_generation": frozenGeneration, "commercial_offer": "pro_monthly", "activation_user_id": actor.String()}
				if (scenario == "checkout-generation-mismatch" && r.URL.Path == "/v1/checkout/sessions/"+session) || (scenario == "subscription-generation-mismatch" && r.URL.Path == "/v1/subscriptions/"+subscription) {
					providerMetadata["checkout_generation"] = generation.Add(time.Second).Format(time.RFC3339Nano)
				}
				switch r.URL.Path {
				case "/v1/subscriptions/" + subscription:
					_ = json.NewEncoder(w).Encode(map[string]any{"id": subscription, "customer": customer, "status": "canceled", "metadata": providerMetadata, "items": map[string]any{"data": []map[string]string{{"id": "si_example"}}}})
				case "/v1/checkout/sessions/" + session:
					_ = json.NewEncoder(w).Encode(StripeRetrievedCheckoutSession{ID: session, CustomerID: customer, SubscriptionID: subscription, ClientReferenceID: team.String(), Status: "complete", Mode: "subscription", Metadata: providerMetadata})
				default:
					t.Errorf("unexpected provider path: %s", r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			h.Stripe = &stripeHTTPClient{baseURL: server.URL, httpClient: server.Client(), apiVersion: "2026-05-27.dahlia"}
			err := h.commitFreeFundingPlan(ctx, team, actor)
			if competing {
				if !errors.Is(err, errFundingPlanTransition) || gets != 0 {
					t.Fatalf("competing attempt not fenced: %v, requests=%d", err, gets)
				}
				var preserved bool
				if err = h.Pool.QueryRow(ctx, `SELECT checkout_initializing_at=$2 AND checkout_may_exist AND cardinality(checkout_pending_attempt_ids)=1 AND NOT EXISTS(SELECT 1 FROM billing_plan_release WHERE team_id=$1) FROM team_billing_account WHERE team_id=$1`, team, generation).Scan(&preserved); err != nil || !preserved {
					t.Fatalf("competing state changed: %v %v", preserved, err)
				}
				return
			}
			if scenario != "completed" && scenario != "non-UTC" {
				if !errors.Is(err, errFundingPlanTransition) {
					t.Fatalf("unverified generation released: %v", err)
				}
				var preserved bool
				if err = h.Pool.QueryRow(ctx, `SELECT checkout_initializing_at=$2 AND checkout_may_exist AND NOT EXISTS(SELECT 1 FROM billing_plan_release WHERE team_id=$1) FROM team_billing_account WHERE team_id=$1`, team, generation).Scan(&preserved); err != nil || !preserved {
					t.Fatalf("generation rejection changed checkout state: %v %v", preserved, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var correct bool
			if err = h.Pool.QueryRow(ctx, `SELECT a.checkout_initializing_at IS NULL AND NOT a.checkout_may_exist AND cardinality(a.checkout_pending_attempt_ids)=0 AND r.previous_account->>'checkout_session_id'=$2 AND r.provider_evidence->'checkout'->>'id'=$2 AND c.offer_key='free' FROM team_billing_account a JOIN billing_plan_release r USING(team_id) JOIN billing_signup_choice c USING(team_id) WHERE a.team_id=$1`, team, session).Scan(&correct); err != nil || !correct {
				t.Fatalf("terminal checkout release lost evidence or retained fence: %v %v", correct, err)
			}
		})
	}
}

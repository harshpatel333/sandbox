//go:build integration

package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func (f planChangeV2Fixture) committedCheckout(t *testing.T) {
	t.Helper()
	generation := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	session := "cs_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	metadata := map[string]string{"checkout_generation": generation.Format(time.RFC3339Nano), "commercial_offer": "pro_monthly", "activation_user_id": f.actor.String()}
	params := StripeCreateCheckoutSessionParams{CustomerID: f.stripe.customer, IdempotencyKey: "pro:" + uuid.NewString(), Metadata: metadata}
	payload, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.h.Pool.Exec(t.Context(), `INSERT INTO billing_contract_payment(team_id,contract_id,invoice_id,payment_reference,customer_id,subscription_id,base_price_id,service_period_start,service_period_end,base_amount_paid_cents,invoice_amount_paid_cents,invoice_amount_remaining_cents,currency,paid_at,verified_at,evidence)
        SELECT team_id,id,$2,'example-payment',customer_id,subscription_id,base_price_id,starts_at,billing_anniversary_boundary(entitlement_anchor,1),charge_amount_cents,charge_amount_cents,0,currency,clock_timestamp(),clock_timestamp(),'{"paid":true}' FROM billing_commercial_contract WHERE id=$1`, f.plan(t).ContractID, "in_"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err = f.h.Pool.Exec(t.Context(), `UPDATE billing_signup_choice SET offer_key='pro_monthly',state='committed',generation=$2,actor_id=$3,base_price_id='price_pro',request_payload=$4,session_id=$5,subscription_id=$6 WHERE team_id=$1`, f.team, generation, f.actor, payload, session, f.stripe.sub); err != nil {
		t.Fatal(err)
	}
	if _, err = f.h.Pool.Exec(t.Context(), `UPDATE team_billing_account SET checkout_initializing_at=$2,checkout_request_key=$3,checkout_session_id=$4,checkout_subscription_id=$5,checkout_completed_at=$2,trial_ended_at=$2 WHERE team_id=$1`, f.team, generation, params.IdempotencyKey, session, f.stripe.sub); err != nil {
		t.Fatal(err)
	}
	f.stripe.set(func(s *renewalStripe) {
		s.metadata = metadata
		s.checkout = &StripeRetrievedCheckoutSession{ID: session, CustomerID: s.customer, SubscriptionID: s.sub, ClientReferenceID: f.team.String(), Status: "complete", Mode: "subscription", Metadata: metadata}
	})
}

func TestTeamPlanChangeReleasesOnlyTheEndedSourcePurchase(t *testing.T) {
	for _, conflict := range []string{"", "replacement subscription", "checkout provenance"} {
		t.Run(conflict, func(t *testing.T) {
			f := newPlanChangeV2Fixture(t, planKindSubscription)
			f.committedCheckout(t)
			source := f.plan(t)
			c, err := f.request(t, "starter", "release")
			if err != nil {
				t.Fatal(err)
			}
			f.tick(t)
			if conflict == "replacement subscription" {
				if _, err := f.h.Pool.Exec(t.Context(), `UPDATE team_billing_account SET stripe_subscription_id=$2 WHERE team_id=$1`, f.team, "sub_"+uuid.NewString()); err != nil {
					t.Fatal(err)
				}
			}
			f.stripe.set(func(s *renewalStripe) {
				s.status = "canceled"
				s.endedAt = time.Now().Add(-time.Minute).Unix()
				if conflict == "checkout provenance" {
					copy := *s.checkout
					copy.ClientReferenceID = uuid.NewString()
					s.checkout = &copy
				}
			})
			f.tick(t)
			c = f.reload(t, c.ID)
			var releases, payments int
			if err := f.h.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM billing_plan_release WHERE team_id=$1),(SELECT count(*) FROM billing_contract_payment WHERE team_id=$1)`, f.team).Scan(&releases, &payments); err != nil {
				t.Fatal(err)
			}
			if conflict != "" {
				if c.State != "scheduled" || c.LastError == nil || f.plan(t).ID != source.ID || releases != 0 || payments != 1 {
					t.Fatalf("conflicting authority was released: %+v releases=%d payments=%d", c, releases, payments)
				}
				return
			}
			if c.State != "completed" || f.plan(t).PlanKey != "starter" || !c.EffectiveAt.Equal(time.Unix(f.stripe.endedAt, 0)) || releases != 1 || payments != 1 {
				t.Fatalf("source purchase was not released atomically: %+v releases=%d payments=%d", c, releases, payments)
			}
			var ready, audited bool
			if err := f.h.Pool.QueryRow(t.Context(), `SELECT a.stripe_subscription_id IS NULL AND a.stripe_customer_id=$2 AND a.trial_ended_at IS NOT NULL AND a.commercial_billing_anchor IS NOT NULL
                AND a.checkout_initializing_at IS NULL AND a.checkout_session_id IS NULL AND a.checkout_subscription_id IS NULL AND a.checkout_completed_at IS NULL
                AND b.state='selecting' AND b.offer_key IS NULL AND b.subscription_id IS NULL,
                r.plan_change_id=$4 AND r.subscription_id=$3 AND r.previous_choice->>'state'='committed' AND r.previous_account->>'stripe_subscription_id'=$3 AND r.provider_evidence->>'id'=$3 AND r.provider_evidence->'checkout'->>'client_reference_id'=($1::uuid)::text
                FROM team_billing_account a JOIN billing_signup_choice b USING(team_id) JOIN billing_plan_release r USING(team_id) WHERE a.team_id=$1`, f.team, f.stripe.customer, f.stripe.sub, c.ID).Scan(&ready, &audited); err != nil || !ready || !audited {
				t.Fatalf("release lost provenance/history or retained old checkout authority: %t %t %v", ready, audited, err)
			}
			f.tick(t)
			if again, err := f.request(t, "starter", "release"); err != nil || again.ID != c.ID || f.plan(t).ID != *c.AppliedPlan {
				t.Fatalf("replay changed the committed release: %+v %v", again, err)
			}
			if err := f.h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM billing_plan_release WHERE team_id=$1`, f.team).Scan(&releases); err != nil || releases != 1 {
				t.Fatalf("release replay duplicated audit: %d %v", releases, err)
			}
		})
	}
}

func TestTeamPlanChangeTerminalStateUsesActualBoundary(t *testing.T) {
	for _, status := range []string{"canceled", "incomplete_expired"} {
		for _, confirmed := range []bool{false, true} {
			t.Run(status+map[bool]string{false: "/before-confirmation", true: "/after-confirmation"}[confirmed], func(t *testing.T) {
				f := newPlanChangeV2Fixture(t, planKindSubscription)
				c, err := f.request(t, "starter", "early-terminal")
				if err != nil {
					t.Fatal(err)
				}
				if confirmed {
					f.tick(t)
					var next time.Time
					if err := f.h.Pool.QueryRow(t.Context(), `SELECT next_check_at FROM team_plan_change WHERE id=$1`, c.ID).Scan(&next); err != nil || next.After(time.Now().Add(2*time.Minute)) {
						t.Fatalf("confirmed future cancellation is not rechecked: %v %v", next, err)
					}
				}
				ended := time.Now().Add(-time.Minute).Truncate(time.Second)
				f.stripe.set(func(s *renewalStripe) { s.status, s.endedAt = status, ended.Unix() })
				f.tick(t)
				got := f.reload(t, c.ID)
				if got.State != "completed" || !got.EffectiveAt.Equal(ended) || f.plan(t).PlanKey != "starter" {
					t.Fatalf("terminal subscription retained future paid authority: %+v", got)
				}
			})
		}
	}
}

func TestTeamPlanChangeTerminalWithoutEndTimeAppliesAtObservation(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	c, err := f.request(t, "starter", "terminal-no-time")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	f.stripe.set(func(s *renewalStripe) { s.status = "incomplete_expired" })
	f.tick(t)
	got := f.reload(t, c.ID)
	if got.State != "completed" || got.EffectiveAt.Before(before) || got.EffectiveAt.After(time.Now()) || len(f.stripe.writes) != 0 {
		t.Fatalf("terminal state without ended_at retained future authority: %+v", got)
	}
}

func TestTeamPlanChangeFreeReleaseAndAssignmentCommitTogether(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	f.committedCheckout(t)
	c, err := f.request(t, "free", "free-release")
	if err != nil {
		t.Fatal(err)
	}
	f.stripe.set(func(s *renewalStripe) { s.status, s.endedAt = "canceled", time.Now().Add(-time.Minute).Unix() })
	f.tick(t)
	c = f.reload(t, c.ID)
	ready, err := planChangePausesReady(t.Context(), f.h.Pool)
	if err != nil {
		t.Fatal(err)
	}
	var releases int
	var subscription *string
	var state string
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM billing_plan_release WHERE team_id=$1),a.stripe_subscription_id,b.state FROM team_billing_account a JOIN billing_signup_choice b USING(team_id) WHERE a.team_id=$1`, f.team).Scan(&releases, &subscription, &state); err != nil {
		t.Fatal(err)
	}
	if ready {
		if c.State != "applied" || f.plan(t).PlanKey != "free" || releases != 1 || subscription != nil || state != "selecting" {
			t.Fatalf("Free assignment/release did not commit together: %+v releases=%d choice=%s", c, releases, state)
		}
	} else if c.State != "scheduled" || c.LastError == nil || f.plan(t).PlanKey != "pro_monthly" || releases != 0 || subscription == nil || *subscription != f.stripe.sub || state != "committed" {
		t.Fatalf("failed Free assignment detached its authority: %+v releases=%d choice=%s", c, releases, state)
	}
}

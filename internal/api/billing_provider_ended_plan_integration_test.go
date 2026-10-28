//go:build integration

package api

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// providerEndedUpgrade records the fixture's Pro contract as bought through an
// upgrade, with a paid renewal and a non-expiring bridge credit grant.
func providerEndedUpgrade(t *testing.T, f planChangeV2Fixture) (string, string) {
	t.Helper()
	conn, err := f.h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(t.Context(), `SET session_replication_role=replica`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(t.Context(), `RESET session_replication_role`)
	renewal, grant := "in_renewal_"+uuid.NewString(), "credgr_"+uuid.NewString()
	start := f.stripe.periodEnd.AddDate(0, -1, 0)
	if _, err = conn.Exec(t.Context(), `INSERT INTO billing_pro_upgrade(id,team_id,actor_id,customer_id,subscription_id,offer_key,request_payload,anchor,period_start,period_end,quoted_at,amount_cents,state,contract_id,grant_id,payment_evidence)
 SELECT gen_random_uuid(),$1,$2,$3,$4,'pro_monthly','{}',$5,$5,$6,$5,25000,'active',p.contract_id,$7,'{"invoice":"in_first"}' FROM team_plan_assignment p WHERE p.team_id=$1 AND p.effective_to IS NULL`,
		f.team, f.actor, f.stripe.customer, f.stripe.sub, start, f.stripe.periodEnd, grant); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(t.Context(), `INSERT INTO billing_contract_payment(team_id,contract_id,invoice_id,payment_reference,customer_id,subscription_id,base_price_id,service_period_start,service_period_end,base_amount_paid_cents,invoice_amount_paid_cents,invoice_amount_remaining_cents,currency,paid_at,verified_at,evidence)
 SELECT $1,p.contract_id,$2,'pi_renewal',$3,$4,'price_pro',$5,$6,25000,25000,0,'usd',$5,$5,'{"invoice":"verified"}' FROM team_plan_assignment p WHERE p.team_id=$1 AND p.effective_to IS NULL`,
		f.team, renewal, f.stripe.customer, f.stripe.sub, start, f.stripe.periodEnd); err != nil {
		t.Fatal(err)
	}
	var paid proPaidInvoice
	if err = json.Unmarshal([]byte(`{"id":"`+renewal+`","status":"paid","amount_paid":25000}`), &paid); err != nil {
		t.Fatal(err)
	}
	f.stripe.set(func(s *renewalStripe) { s.invoices = []proPaidInvoice{paid}; s.grants = map[string]int64{grant: 0} })
	return renewal, grant
}

func TestProviderEndedSubscriptionMovesTheTeamToFree(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	if err := f.h.scheduleProviderEndedFreeChange(t.Context(), f.stripe.customer, f.stripe.sub); err != nil {
		t.Fatal(err)
	}
	if c, ok, err := pendingTeamPlanChange(t.Context(), f.h.Pool, f.team); err != nil || ok {
		t.Fatalf("a live renewing subscription scheduled a move to Free: %+v %v", c, err)
	}
	f.stripe.set(func(s *renewalStripe) { s.status, s.endedAt = "canceled", time.Now().Add(-time.Minute).Unix() })
	for range 2 {
		if err := f.h.scheduleProviderEndedFreeChange(t.Context(), f.stripe.customer, f.stripe.sub); err != nil {
			t.Fatal(err)
		}
	}
	c, ok, err := pendingTeamPlanChange(t.Context(), f.h.Pool, f.team)
	if err != nil || !ok || !c.ProviderEnded || c.TargetPlanKey != "free" || !c.PausesRunning || c.Subscription == nil || *c.Subscription != f.stripe.sub {
		t.Fatalf("ended subscription did not schedule a move to Free: %+v %v %v", c, ok, err)
	}
	f.tick(t)
	f.tick(t)
	c = f.reload(t, c.ID)
	var contract string
	if err = f.h.Pool.QueryRow(t.Context(), `SELECT status FROM billing_commercial_contract WHERE subscription_id=$1`, f.stripe.sub).Scan(&contract); err != nil {
		t.Fatal(err)
	}
	if plan := f.plan(t); c.State == "scheduled" || plan.Kind != planKindFree || contract != "ended" {
		t.Fatalf("ended subscription did not move the team to Free: change=%+v plan=%+v contract=%s", c, plan, contract)
	}
	if len(f.stripe.writes) != 0 {
		t.Fatalf("an ended subscription was written to: %v", f.stripe.writes)
	}
}

func TestReleasedUpgradeCountsVerifiedRenewalsAndVoidsBridgeCredit(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	renewal, grant := providerEndedUpgrade(t, f)
	client := f.h.Stripe.(*stripeHTTPClient)
	if closed, err := f.h.proInvoicesClosed(t.Context(), client, f.team, f.stripe.customer, f.stripe.sub, nil); err != nil || closed {
		t.Fatalf("an unrecorded paid renewal counted as closed: %v %v", closed, err)
	}
	paid, err := verifiedContractInvoices(t.Context(), f.h.Pool, f.team, f.stripe.sub)
	if err != nil || len(paid) != 1 || paid[0] != renewal {
		t.Fatalf("verified renewals: %v %v", paid, err)
	}
	if closed, err := f.h.proInvoicesClosed(t.Context(), client, f.team, f.stripe.customer, f.stripe.sub, paid); err != nil || !closed {
		t.Fatalf("an upgrade's verified renewal kept its invoices open: %v %v", closed, err)
	}
	for range 2 {
		if err = voidUnspentBridgeCredit(t.Context(), f.h.Pool, client, f.team, f.stripe.sub); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.stripe.voids) != 1 || f.stripe.voids[0] != grant {
		t.Fatalf("unspent bridge credit not voided exactly once: %v", f.stripe.voids)
	}
}

func TestProviderEndedChangeFollowsAStripeCancellationTime(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	cancelAt := time.Now().Add(time.Hour).Truncate(time.Second)
	f.stripe.set(func(s *renewalStripe) { s.cancelAt = cancelAt.Unix() })
	if err := f.h.scheduleProviderEndedFreeChange(t.Context(), f.stripe.customer, f.stripe.sub); err != nil {
		t.Fatal(err)
	}
	c, ok, err := pendingTeamPlanChange(t.Context(), f.h.Pool, f.team)
	if err != nil || !ok || !c.EffectiveAt.Equal(cancelAt) {
		t.Fatalf("cancellation time not followed: %+v %v", c, err)
	}
	f.tick(t)
	if c = f.reload(t, c.ID); c.State != "scheduled" || c.ProviderCancelAt == nil || !c.EffectiveAt.Equal(cancelAt) || len(f.stripe.writes) != 0 {
		t.Fatalf("a mid-period Stripe cancellation was not kept: %+v %v", c, f.stripe.writes)
	}
	if plan := f.plan(t); plan.Kind != planKindSubscription {
		t.Fatalf("the paid plan ended before its cancellation time: %+v", plan)
	}
}

func TestProviderEndedChangeWithdrawnWhenStripeResumes(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	f.stripe.set(func(s *renewalStripe) { s.cancel = true })
	if err := f.h.scheduleProviderEndedFreeChange(t.Context(), f.stripe.customer, f.stripe.sub); err != nil {
		t.Fatal(err)
	}
	c, ok, err := pendingTeamPlanChange(t.Context(), f.h.Pool, f.team)
	if err != nil || !ok {
		t.Fatalf("scheduled cancellation did not schedule a move to Free: %v", err)
	}
	f.stripe.set(func(s *renewalStripe) { s.cancel = false })
	f.tick(t)
	if c = f.reload(t, c.ID); c.State != "undone" || c.needsUndoReconciliation() || len(f.stripe.writes) != 0 {
		t.Fatalf("a resumed subscription kept its move to Free or was cancelled again: %+v %v", c, f.stripe.writes)
	}
	if plan := f.plan(t); plan.Kind != planKindSubscription {
		t.Fatalf("a resumed subscription left its plan: %+v", plan)
	}
}

func TestProviderEndedChangeSkipsCohortAdmission(t *testing.T) {
	f := newTeamCohortTestFixture(t)
	f.onFree(t)
	insert := func(providerEnded bool) error {
		_, err := f.h.Pool.Exec(t.Context(), `INSERT INTO team_plan_change(team_id,from_assignment_id,target_plan_key,target_kind,target_terms,target_limits,effective_at,pauses_running,
  subscription_id,requested_by,requested_via,reason,idempotency_key,provider_ended)
 SELECT $1,id,'free','free','{"pricing_version":"v2"}','{"max_concurrent":1}',clock_timestamp(),true,'sub_ended',$2,'staff','cancelled in Stripe',$3,$4
 FROM team_plan_assignment WHERE team_id=$1 AND effective_to IS NULL`, f.team, f.actor, uuid.NewString(), providerEnded)
		return err
	}
	var pgErr *pgconn.PgError
	if err := insert(false); !errors.As(err, &pgErr) || pgErr.Code != "55000" {
		t.Fatalf("an ordinary plan change skipped closed admission: %v", err)
	}
	if err := insert(true); err != nil {
		t.Fatalf("a Stripe-ended subscription could not leave for Free while entry is closed: %v", err)
	}
}

func TestProviderEndedWebhookSchedulesTheMoveToFree(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	providerEndedUpgrade(t, f)
	f.stripe.set(func(s *renewalStripe) { s.status, s.endedAt = "canceled", time.Now().Add(-time.Minute).Unix() })
	object, _ := json.Marshal(map[string]any{"id": f.stripe.sub, "customer": f.stripe.customer, "status": "canceled"})
	envelope := stripeEventEnvelope{ID: "evt_" + uuid.NewString(), Type: "customer.subscription.deleted", Data: stripeEventEnvelopeData{Object: object}}
	payload, _ := json.Marshal(envelope)
	if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO stripe_webhook_event(event_id,event_type,payload) VALUES($1,$2,$3)`, envelope.ID, envelope.Type, payload); err != nil {
		t.Fatal(err)
	}
	if handled, err := f.h.processProWebhook(t.Context(), envelope); !handled || err != nil {
		t.Fatalf("deleted subscription event: %v %v", handled, err)
	}
	if c, ok, err := pendingTeamPlanChange(t.Context(), f.h.Pool, f.team); err != nil || !ok || !c.ProviderEnded {
		t.Fatalf("a Stripe deletion did not schedule the move to Free: %+v %v %v", c, ok, err)
	}
}

func TestProviderEndedChangeTracksLaterStripeCancellations(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	schedule := func() teamPlanChange {
		t.Helper()
		if err := f.h.scheduleProviderEndedFreeChange(t.Context(), f.stripe.customer, f.stripe.sub); err != nil {
			t.Fatal(err)
		}
		c, ok, err := pendingTeamPlanChange(t.Context(), f.h.Pool, f.team)
		if err != nil || !ok {
			t.Fatalf("no move to Free scheduled: %v", err)
		}
		return c
	}
	f.stripe.set(func(s *renewalStripe) { s.cancelAt = s.periodEnd.AddDate(0, 2, 0).Unix() })
	if err := f.h.scheduleProviderEndedFreeChange(t.Context(), f.stripe.customer, f.stripe.sub); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := pendingTeamPlanChange(t.Context(), f.h.Pool, f.team); err != nil || ok {
		t.Fatalf("a cancellation beyond the current period was scheduled before its renewal: %v %v", ok, err)
	}
	inPeriod := f.stripe.periodEnd.Add(-time.Hour)
	f.stripe.set(func(s *renewalStripe) { s.cancelAt = inPeriod.Unix() })
	c := schedule()
	f.tick(t)
	if c = f.reload(t, c.ID); c.State != "scheduled" || !c.EffectiveAt.Equal(inPeriod) || c.Failures != 0 {
		t.Fatalf("a cancellation within the period was not followed: %+v", c)
	}
	earlier := f.stripe.periodEnd.Add(-2 * time.Hour)
	f.stripe.set(func(s *renewalStripe) { s.cancelAt = earlier.Unix() })
	f.tick(t)
	moved, ok, err := pendingTeamPlanChange(t.Context(), f.h.Pool, f.team)
	if c = f.reload(t, c.ID); err != nil || !ok || c.State != "undone" || moved.ID == c.ID || !moved.EffectiveAt.Equal(earlier) {
		t.Fatalf("an earlier Stripe cancellation was not rescheduled: old=%+v new=%+v %v", c, moved, err)
	}
	f.stripe.set(func(s *renewalStripe) { s.cancelAt = 0 })
	f.tick(t)
	if _, ok, err = pendingTeamPlanChange(t.Context(), f.h.Pool, f.team); err != nil || ok {
		t.Fatalf("a resumed subscription kept a move to Free: %v %v", ok, err)
	}
	f.stripe.set(func(s *renewalStripe) { s.status, s.endedAt = "canceled", time.Now().Add(-time.Minute).Unix() })
	final := schedule()
	f.tick(t)
	f.tick(t)
	if final = f.reload(t, final.ID); final.State == "scheduled" || f.plan(t).Kind != planKindFree || len(f.stripe.writes) != 0 {
		t.Fatalf("cancelling again after a resume did not move the team to Free: %+v %+v %v", final, f.plan(t), f.stripe.writes)
	}
}

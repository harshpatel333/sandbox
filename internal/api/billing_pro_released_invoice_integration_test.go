//go:build integration

package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReleasedProInvoiceIsDurableBeforeAcknowledgmentAndNewPurchaseDispatch(t *testing.T) {
	f := newPlanChangeV2Fixture(t, planKindSubscription)
	f.committedCheckout(t)
	if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO billing_invoice_account(team_id,customer_id,subscription_id,adjustment_price_id,adjustment_event_name,adjustment_meter_id,enrolled_at) VALUES($1,$2,$3,'price_rounding','rounding','meter_rounding',clock_timestamp()-interval '2 months')`, f.team, f.stripe.customer, f.stripe.sub); err != nil {
		t.Fatal(err)
	}
	c, err := f.request(t, "starter", "released-invoice")
	if err != nil {
		t.Fatal(err)
	}
	f.stripe.set(func(s *renewalStripe) { s.status, s.endedAt = "canceled", time.Now().Add(-time.Minute).Unix() })
	f.tick(t)
	c = f.reload(t, c.ID)
	if c.State != "completed" {
		t.Fatalf("source release: %+v", c)
	}
	current := f.plan(t)
	var ended int64
	if err = f.h.Pool.QueryRow(t.Context(), `SELECT (provider_evidence->'invoice_account'->'Base'->>'TerminatedAt')::bigint FROM billing_plan_release WHERE plan_change_id=$1`, c.ID).Scan(&ended); err != nil || ended != f.stripe.endedAt {
		t.Fatalf("release snapshot omitted provider termination: %d %v", ended, err)
	}
	id, eventID := "in_"+uuid.NewString(), "evt_"+uuid.NewString()
	object, _ := json.Marshal(map[string]any{"id": id, "customer": f.stripe.customer, "parent": map[string]any{"subscription_details": map[string]any{"subscription": f.stripe.sub}}})
	event := stripeEventEnvelope{ID: eventID, Type: "invoice.paid", Data: stripeEventEnvelopeData{Object: object}}
	payload, _ := json.Marshal(event)
	if _, err = f.h.Pool.Exec(t.Context(), `INSERT INTO stripe_webhook_event(event_id,event_type,payload) VALUES($1,$2,$3)`, event.ID, event.Type, payload); err != nil {
		t.Fatal(err)
	}
	newer := uuid.New()
	now := time.Now().UTC()
	if _, err = f.h.Pool.Exec(t.Context(), `INSERT INTO billing_pro_upgrade(id,team_id,actor_id,customer_id,offer_key,request_payload,anchor,period_start,period_end,quoted_at,amount_cents,next_check_at) VALUES($1,$2,$3,$4,'pro_monthly','{}',$5,$5,$6,$5,12500,'infinity')`, newer, f.team, f.actor, f.stripe.customer, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	conn, err := f.h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(t.Context(), `CREATE FUNCTION pg_temp.fail_released_invoice() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected durable queue failure'; END $$;
 CREATE TRIGGER released_invoice_fault BEFORE INSERT ON billing_pro_released_invoice FOR EACH ROW EXECUTE FUNCTION pg_temp.fail_released_invoice()`); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.h.Pool.Exec(t.Context(), `DROP TRIGGER IF EXISTS released_invoice_fault ON billing_pro_released_invoice`)
	})
	handled, err := f.h.processProWebhook(t.Context(), event)
	if !handled || err == nil || !strings.Contains(err.Error(), "injected durable queue failure") {
		conn.Release()
		t.Fatalf("queue failure acknowledged: %v %v", handled, err)
	}
	if _, err = conn.Exec(t.Context(), `DROP TRIGGER released_invoice_fault ON billing_pro_released_invoice`); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	conn.Release()
	var processed bool
	if err = f.h.Pool.QueryRow(t.Context(), `SELECT processed_at IS NOT NULL FROM stripe_webhook_event WHERE event_id=$1`, eventID).Scan(&processed); err != nil || processed {
		t.Fatalf("failed enqueue acknowledged: %v %v", processed, err)
	}
	for range 2 {
		if handled, err = f.h.processProWebhook(t.Context(), event); err != nil || !handled {
			t.Fatalf("released invoice: %v %v", handled, err)
		}
	}
	var safe bool
	err = f.h.Pool.QueryRow(t.Context(), `SELECT
 (SELECT count(*)=1 FROM billing_pro_released_invoice WHERE invoice_id=$1 AND contract_id IS NOT NULL AND state='pending')
 AND (SELECT processed_at IS NOT NULL FROM stripe_webhook_event WHERE event_id=$2)
 AND (SELECT next_check_at='infinity'::timestamptz FROM billing_pro_upgrade WHERE id=$3)
 AND (SELECT stripe_subscription_id IS NULL FROM team_billing_account WHERE team_id=$4)
 AND (SELECT effective_to IS NULL FROM team_plan_assignment WHERE id=$5)`, id, eventID, newer, f.team, current.ID).Scan(&safe)
	if err != nil || !safe {
		t.Fatalf("invoice loss, duplicate or authority reactivation: %v %v", safe, err)
	}
	var before, after int64
	if err = f.h.Pool.QueryRow(t.Context(), `SELECT revision FROM billing_pro_released_invoice WHERE invoice_id=$1`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err = f.h.Pool.Exec(t.Context(), `UPDATE billing_pro_released_invoice SET state='recorded',receipt='{}',payment_reference='prior',credit_state='covered',verified_revision=revision,next_check_at='infinity' WHERE invoice_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if handled, err = f.h.processProWebhook(t.Context(), event); err != nil || !handled {
		t.Fatalf("later observation enqueue: %v %v", handled, err)
	}
	if err = f.h.Pool.QueryRow(t.Context(), `SELECT revision,verified_revision<revision AND next_check_at<=clock_timestamp() AND receipt='{}'::jsonb FROM billing_pro_released_invoice WHERE invoice_id=$1`, id).Scan(&after, &safe); err != nil || !safe || after <= before {
		t.Fatalf("completed invoice observation lost: %d %d %v %v", before, after, safe, err)
	}

	if err = f.h.reconcileReleasedUsageInvoice(t.Context(), f.h.Stripe.(*stripeHTTPClient), id, f.team, after, proPaidInvoice{}); err == nil {
		t.Fatal("unseen usage invoice bypassed its missing frozen close plan")
	}

}

package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/superserve-ai/sandbox/internal/db"
)

// scheduleProviderEndedFreeChange moves a team whose catalog plan subscription
// was cancelled in Stripe, from the portal or the dashboard, to Free through the
// same plan change a customer's own switch uses: the worker ends the contract
// once its invoices close, assigns Free, which turns Auto Topoff off, and pauses
// the team. A team with a plan change already pending keeps that change.
func (h *Handlers) scheduleProviderEndedFreeChange(ctx context.Context, customer, subscription string) error {
	client, ok := h.Stripe.(*stripeHTTPClient)
	if !ok || subscription == "" {
		return nil
	}
	var current teamPlan
	var actor uuid.UUID
	err := h.Pool.QueryRow(ctx, `SELECT p.id,p.team_id,COALESCE(p.assigned_by,(SELECT actor_id FROM billing_pro_upgrade WHERE contract_id=k.id LIMIT 1),
  (SELECT actor_id FROM billing_signup_choice WHERE team_id=p.team_id AND subscription_id=$2))
 FROM team_plan_assignment p JOIN billing_commercial_contract k ON k.id=p.contract_id
 WHERE p.effective_to IS NULL AND k.customer_id=$1 AND k.subscription_id=$2 AND k.offer_key IN `+paidPlanOffers+`
  AND NOT EXISTS(SELECT 1 FROM team_plan_change c WHERE c.team_id=p.team_id AND c.state='scheduled')`, customer, subscription).Scan(&current.ID, &current.TeamID, &actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	sub, err := client.invoiceSubscription(ctx, subscription)
	if err != nil {
		return err
	}
	if !subscriptionWillEnd(sub) {
		return nil
	}
	ready, err := planChangePausesReady(ctx, h.Pool)
	if err != nil {
		return err
	}
	if !ready {
		return errors.New(planChangePausesUnavailable)
	}
	terms, err := h.planTermsForPricingVersion(ctx, "free", "v2")
	if err != nil {
		return err
	}
	frozen, limits, _, err := planAssignment{Terms: terms}.frozen()
	if err != nil {
		return err
	}
	effective := subscriptionStopAt(sub)
	if subscriptionEnded(sub) {
		effective = h.nowUTC()
	}
	_, err = h.Pool.Exec(ctx, `INSERT INTO team_plan_change(team_id,from_assignment_id,target_plan_key,target_kind,target_terms,target_limits,effective_at,pauses_running,
  subscription_id,requested_by,requested_via,reason,idempotency_key,next_check_at,provider_ended)
 VALUES($1,$2,'free',$3,$4,$5,$6,true,$7,$8,'staff','The subscription was cancelled in Stripe',$9,clock_timestamp(),true) ON CONFLICT DO NOTHING`,
		current.TeamID, current.ID, terms.Kind, frozen, limits, effective, subscription, actor, "provider-ended:"+subscription+":"+current.ID.String())
	return err
}

// verifiedContractInvoices lists the subscription's invoices whose payment
// was verified for its contract, so an upgrade's renewals count as closed.
func verifiedContractInvoices(ctx context.Context, q db.DBTX, team uuid.UUID, subscription string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT p.invoice_id FROM billing_contract_payment p JOIN billing_commercial_contract k ON k.id=p.contract_id
 WHERE k.team_id=$1 AND k.subscription_id=$2
  AND NOT EXISTS(SELECT 1 FROM billing_pro_payment_verification v WHERE v.invoice_id=p.invoice_id AND v.review_required)`, team, subscription)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// voidUnspentBridgeCredit voids the non-expiring credit an upgrade issued for
// the rest of a billing month once the plan is released, so an unspent
// remainder cannot offset a later purchase on the same customer.
func voidUnspentBridgeCredit(ctx context.Context, q db.DBTX, c *stripeHTTPClient, team uuid.UUID, subscription string) error {
	rows, err := q.Query(ctx, `SELECT `+proUpgradeColumns+` FROM billing_pro_upgrade WHERE team_id=$1 AND subscription_id=$2 AND grant_id IS NOT NULL AND state IN ('credited','active')`, team, subscription)
	if err != nil {
		return err
	}
	upgrades, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (proUpgrade, error) { return scanProUpgrade(r) })
	if err != nil {
		return err
	}
	for _, u := range upgrades {
		if u.bridgeCreditAmount() == 0 {
			continue
		}
		grant, err := c.retrieveEntitlementGrant(ctx, *u.Grant)
		if err != nil {
			return err
		}
		if grant.ExpiresAt != 0 || grant.VoidedAt != 0 {
			continue
		}
		if err = c.doForm(ctx, http.MethodPost, "/v1/billing/credit_grants/"+url.PathEscape(*u.Grant)+"/void", url.Values{}, nil, "plan-release-credit-void:"+*u.Grant); err != nil {
			return err
		}
	}
	return nil
}

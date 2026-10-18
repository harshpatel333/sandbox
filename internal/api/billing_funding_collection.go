package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/superserve-ai/sandbox/internal/billing"
	"github.com/superserve-ai/sandbox/internal/db"
)

// A released contract is only an inventory cursor for its existing customer.
// It never provides new allowances or base-fee coverage to the replacement.
func (h *Handlers) releasedFundingCreditContract(ctx context.Context, team uuid.UUID, a invoiceAccount, reader db.DBTX) (uuid.UUID, bool, error) {
	var contract uuid.UUID
	if a.Base.Price != "" {
		return contract, false, nil
	}
	err := reader.QueryRow(ctx, `SELECT c.id FROM billing_plan_release r
 JOIN billing_commercial_contract c ON c.team_id=r.team_id AND c.customer_id=r.customer_id AND c.subscription_id=r.subscription_id AND c.status='ended'
 JOIN team_billing_account a ON a.team_id=r.team_id AND a.stripe_customer_id=r.customer_id
 WHERE r.team_id=$1 AND r.customer_id=$2 AND a.stripe_subscription_id=$3
 AND r.subscription_id<>$3 AND r.provider_status IN ('canceled','incomplete_expired')
 ORDER BY r.released_at DESC,c.created_at DESC,c.id LIMIT 1`, team, a.Customer, a.Subscription).Scan(&contract)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return contract, err == nil, err
}

func (h *Handlers) fundingCreditInventoryReady(ctx context.Context, contract uuid.UUID, customer string, client *stripeHTTPClient) error {
	// A never-activated contract may have no issuance scheduler row. Keep it
	// dormant; only its resumable inventory fields are needed for old credits.
	if _, err := h.Pool.Exec(ctx, `INSERT INTO billing_entitlement_work(contract_id,next_check_at) VALUES($1,'infinity') ON CONFLICT DO NOTHING`, contract); err != nil {
		return err
	}
	return h.creditInventoryReady(ctx, contract, customer, client)
}

func (h *Handlers) preflightReleasedFundingCredits(ctx context.Context, team uuid.UUID, a invoiceAccount, client *stripeHTTPClient, reader db.DBTX) (bool, error) {
	_, released, err := h.releasedFundingCreditContract(ctx, team, a, reader)
	if err != nil || !released {
		return released, err
	}
	sub, err := client.invoiceSubscription(ctx, a.Subscription)
	if err != nil {
		return true, err
	}
	if sub.Customer != a.Customer || sub.ID != a.Subscription {
		return true, errors.New("replacement credit subscription identity changed")
	}
	prices := []string{a.Price}
	for _, item := range sub.Items.Data {
		if item.Price.ID != a.Price {
			prices = append(prices, item.Price.ID)
		}
	}
	sort.Strings(prices)
	// Enrollment holds its account transaction. This compatibility check uses
	// only the provider, so it never waits for another pool connection. The
	// held invoice is authorized later by the durable inventory and promise
	// checks in prepareReleasedFundingCredits, outside that transaction.
	var page struct {
		Data    []entitlementGrant `json:"data"`
		HasMore bool               `json:"has_more"`
	}
	q := url.Values{"customer": {a.Customer}, "limit": {"100"}}
	if err = client.doForm(ctx, http.MethodGet, "/v1/billing/credit_grants?"+q.Encode(), nil, &page, ""); err != nil {
		return true, err
	}
	if page.HasMore || len(page.Data) > 100 {
		return true, errors.New("replacement credit preflight exceeds inventory bound; operator reconciliation required")
	}
	at := time.Now().UTC().Unix()
	seen := make(map[string]bool, len(page.Data))
	for _, grant := range page.Data {
		if grant.ID == "" || grant.Customer != a.Customer || seen[grant.ID] {
			return true, errors.New("replacement credit inventory identity mismatch")
		}
		seen[grant.ID] = true
		if grant.VoidedAt != 0 || grant.EffectiveAt > at || (grant.ExpiresAt != 0 && grant.ExpiresAt <= at) {
			continue
		}
		balance, err := client.grantLedgerBalance(ctx, a.Customer, grant.ID)
		if err != nil {
			return true, err
		}
		if balance > 0 {
			if err = creditScope(grant, a.Customer, at, prices); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

func (h *Handlers) prepareReleasedFundingCredits(ctx context.Context, p billing.ExportPeriod, a invoiceAccount, invoiceID string, plan *billing.InvoiceClosePlan, client *stripeHTTPClient) (bool, error) {
	contract, released, err := h.releasedFundingCreditContract(ctx, p.TeamID, a, h.Pool)
	if err != nil || !released {
		return released, err
	}
	if err = h.fundingCreditInventoryReady(ctx, contract, a.Customer, client); err != nil {
		return true, err
	}
	plan.CreditVersion = 1
	plan.CreditPrices = []string{a.Price}
	for _, resource := range plan.Resources {
		plan.CreditPrices = append(plan.CreditPrices, resource.PriceID)
	}
	sort.Strings(plan.CreditPrices)
	grants, err := h.eligibleInvoiceCredits(ctx, a.Customer, plan.InvoiceEnd, plan.CreditPrices, client)
	if err != nil {
		return true, err
	}
	if err = h.verifyPreservedFundingCredits(ctx, p, contract, a.Customer, plan.InvoiceEnd, plan.CreditPrices, grants, client); err != nil {
		return true, err
	}
	plan.CreditBeforeCents = 0
	for _, grant := range grants {
		plan.CreditBeforeCents, err = checkedCreditSum(plan.CreditBeforeCents, grant.Before)
		if err != nil {
			return true, err
		}
	}
	plan.CreditGrants = grants
	return true, client.rejectCompetingCreditInvoice(ctx, a.Customer, invoiceID, plan.InvoiceEnd)
}

// An absent or newly expired issued allowance must not silently become a cash
// charge for usage that overlapped its promised service period. The normal
// inventory scan alone cannot distinguish that from never having had credit.
func (h *Handlers) verifyPreservedFundingCredits(ctx context.Context, p billing.ExportPeriod, contract uuid.UUID, customer string, invoiceEnd int64, prices []string, grants []billing.InvoiceCreditGrant, client *stripeHTTPClient) error {
	rows, err := h.Pool.Query(ctx, `SELECT e.state,COALESCE(e.stripe_credit_grant_id,'') FROM billing_usage_entitlement e
 JOIN billing_commercial_contract c ON c.id=e.contract_id
 WHERE c.team_id=$1 AND c.customer_id=$2 AND c.subscription_id=(SELECT subscription_id FROM billing_commercial_contract WHERE id=$3)
 AND e.period_start<$5 AND e.period_end>$4 AND e.state IN ('issued','uncertain')
 ORDER BY e.period_start,e.id LIMIT 101`, p.TeamID, customer, contract, p.Start, p.End)
	if err != nil {
		return err
	}
	type promise struct{ state, grant string }
	var promises []promise
	for rows.Next() {
		var promise promise
		if err = rows.Scan(&promise.state, &promise.grant); err != nil {
			rows.Close()
			return err
		}
		promises = append(promises, promise)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(promises) > 100 {
		return errors.New("preserved credit promises exceed reconciliation bound")
	}
	for _, promise := range promises {
		if promise.state != "issued" || promise.grant == "" {
			return errors.New("preserved credit issuance is unresolved")
		}
		grant, err := client.retrieveEntitlementGrant(ctx, promise.grant)
		if err != nil {
			return err
		}
		if err = creditScope(grant, customer, invoiceEnd, prices); err != nil {
			return errors.New("preserved allowance is missing, expired, or incompatible with replacement invoice; reconcile before collection")
		}
		if err = h.verifyOwnedEntitlementGrant(ctx, grant); err != nil {
			return err
		}
		balance, err := client.grantLedgerBalance(ctx, customer, promise.grant)
		if err != nil {
			return err
		}
		found := balance == 0
		for _, current := range grants {
			found = found || current.ID == promise.grant && current.Before == balance
		}
		if !found {
			return errors.New("promised preserved allowance is absent from the replacement credit plan")
		}
	}
	return nil
}

func (h *Handlers) invoiceCreditInventoryContract(ctx context.Context, team uuid.UUID, a invoiceAccount) (uuid.UUID, error) {
	var contract uuid.UUID
	err := h.Pool.QueryRow(ctx, `SELECT id FROM billing_commercial_contract WHERE team_id=$1 AND customer_id=$2 AND subscription_id=$3 ORDER BY created_at DESC LIMIT 1`, team, a.Customer, a.Subscription).Scan(&contract)
	if !errors.Is(err, pgx.ErrNoRows) {
		return contract, err
	}
	contract, released, err := h.releasedFundingCreditContract(ctx, team, a, h.Pool)
	if err != nil {
		return contract, err
	}
	if !released {
		return uuid.Nil, errors.New("scoped credit plan has no current contract or audited release")
	}
	return contract, nil
}

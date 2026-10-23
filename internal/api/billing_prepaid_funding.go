package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog/log"
)

// Funding observation fails above eight live grants, so unspent purchases are
// bounded below that, leaving room for allowances and staff credit.
const prepaidLiveGrantLimit = 5

var errPrepaidCapacity = errors.New("prepaid credit is at its observation capacity")
var errPrepaidTooManyUnspent = errors.New("too many unspent credit purchases")

// enrolPrepaidObservation reserves a funding observation slot before any money
// moves. Observation alone changes nothing: eligibility still follows today's
// rule until the team's credit is observed and prepaid enforcement begins.
func (h *Handlers) enrolPrepaidObservation(ctx context.Context, team uuid.UUID) error {
	_, err := h.Pool.Exec(ctx, `INSERT INTO billing_funding_account(team_id,observe) VALUES($1,true)
 ON CONFLICT(team_id) DO UPDATE SET observe=true WHERE NOT billing_funding_account.observe`, team)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "P0001" {
		return errPrepaidCapacity
	}
	return err
}

func (h *Handlers) prepaidUnspentPurchases(ctx context.Context, team uuid.UUID) (int, error) {
	var live int
	err := h.Pool.QueryRow(ctx, `SELECT count(*) FROM billing_prepaid_purchase WHERE team_id=$1 AND payment_state<>'failed' AND credit_voided_at IS NULL`, team).Scan(&live)
	return live, err
}

// Prepaid enforcement starts only once a fresh observation already shows the
// purchased credit, so switching rules never blips a paying team ineligible.
func (h *Handlers) startPrepaidEnforcement(ctx context.Context) error {
	_, err := h.Pool.Exec(ctx, `UPDATE billing_funding_account f SET enforce=true,prepaid=true
 WHERE f.observe AND NOT f.prepaid AND f.observed_generation=f.generation AND f.credit_usd>0 AND f.credit_valid_until>clock_timestamp()
  AND EXISTS(SELECT 1 FROM billing_prepaid_purchase p JOIN team_billing_account a USING(team_id)
   WHERE p.team_id=f.team_id AND p.credit_state='issued' AND p.customer_id=a.stripe_customer_id AND f.customer_id=a.stripe_customer_id AND f.credit_checked_at>p.credited_at)`)
	return err
}

// A fully consumed purchase grant is voided so it stops counting toward the
// grant limits. Voiding changes the grant, so it waits for the same fence as issuance.
func (h *Handlers) voidSpentPrepaidGrants(ctx context.Context, client *stripeHTTPClient) error {
	rows, err := h.Pool.Query(ctx, `SELECT id,team_id,customer_id,stripe_credit_grant_id FROM billing_prepaid_purchase
 WHERE credit_state='issued' AND credit_voided_at IS NULL AND COALESCE(credit_balance_checked_at,'-infinity'::timestamptz)<now()-interval '1 hour'
 ORDER BY COALESCE(credit_balance_checked_at,'-infinity'::timestamptz) LIMIT 10`)
	if err != nil {
		return err
	}
	type live struct {
		ID, Team        uuid.UUID
		Customer, Grant string
	}
	var due []live
	for rows.Next() {
		var l live
		if err = rows.Scan(&l.ID, &l.Team, &l.Customer, &l.Grant); err != nil {
			rows.Close()
			return err
		}
		due = append(due, l)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var errs []error
	for _, l := range due {
		errs = append(errs, h.voidSpentPrepaidGrant(ctx, client, l.ID, l.Team, l.Customer, l.Grant))
	}
	return errors.Join(errs...)
}

func (h *Handlers) voidSpentPrepaidGrant(ctx context.Context, client *stripeHTTPClient, id, team uuid.UUID, customer, grant string) error {
	balance, err := client.grantLedgerBalance(ctx, customer, grant)
	if err != nil {
		return err
	}
	if balance != 0 {
		_, err = h.Pool.Exec(ctx, `UPDATE billing_prepaid_purchase SET credit_balance_checked_at=clock_timestamp() WHERE id=$1`, id)
		return err
	}
	release, err := h.holdCreditIssuanceFence(ctx, team)
	if errors.Is(err, errCreditIssuanceDeferred) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	if balance, err = client.grantLedgerBalance(ctx, customer, grant); err != nil || balance != 0 {
		return err
	}
	var voided entitlementGrant
	if err = client.doForm(ctx, http.MethodPost, "/v1/billing/credit_grants/"+url.PathEscape(grant)+"/void", url.Values{}, &voided, "prepaid-credit-void:v1:"+id.String()); err != nil {
		return err
	}
	if voided.ID != grant || voided.VoidedAt == 0 {
		return errors.New("credit grant void was not confirmed")
	}
	log.Info().Str("prepaid_purchase_id", id.String()).Str("grant_id", grant).Msg("voided fully consumed prepaid credit grant")
	_, err = h.Pool.Exec(ctx, `UPDATE billing_prepaid_purchase SET credit_voided_at=clock_timestamp(),credit_balance_checked_at=clock_timestamp() WHERE id=$1 AND credit_voided_at IS NULL`, id)
	return err
}

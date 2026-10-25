package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

var errPrepaidNoPaymentReference = errors.New("paid purchase has no payment reference")

// Card disputes can be opened for 120 days, so paid purchases are rechecked
// daily for that long after payment.
const prepaidReversalWindow = "120 days"

type prepaidChargeState struct {
	ID             string `json:"id"`
	Customer       string `json:"customer"`
	Amount         int64  `json:"amount"`
	AmountRefunded int64  `json:"amount_refunded"`
	Disputed       bool   `json:"disputed"`
}

type prepaidReversal struct {
	Kind     string
	Reversed int64
	Evidence []byte
}

func (h *Handlers) reviewPrepaidReversals(ctx context.Context, client *stripeHTTPClient) error {
	rows, err := h.Pool.Query(ctx, `SELECT id,team_id,customer_id,source,amount_cents,payment_intent_id,invoice_id FROM billing_prepaid_purchase
 WHERE payment_state='succeeded' AND paid_at>clock_timestamp()-$1::interval
  AND COALESCE(reversal_checked_at,'-infinity'::timestamptz)<clock_timestamp()-interval '1 day'
 ORDER BY COALESCE(reversal_checked_at,'-infinity'::timestamptz),id LIMIT 10`, prepaidReversalWindow)
	if err != nil {
		return err
	}
	type paid struct {
		ID, Team         uuid.UUID
		Customer, Source string
		Amount           int64
		Intent, Invoice  *string
	}
	due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (paid, error) {
		var p paid
		err := row.Scan(&p.ID, &p.Team, &p.Customer, &p.Source, &p.Amount, &p.Intent, &p.Invoice)
		return p, err
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range due {
		reversal, err := client.prepaidPaymentReversal(ctx, p.Customer, p.Amount, p.Intent, p.Invoice)
		if errors.Is(err, errPrepaidNoPaymentReference) {
			log.Error().Str("prepaid_purchase_id", p.ID.String()).Msg("paid prepaid purchase has no payment reference; reversals cannot be checked")
			err = nil
		}
		if err == nil && reversal != nil {
			err = h.recordPrepaidReversal(ctx, p.ID, p.Team, p.Source, p.Amount, *reversal)
		}
		if err == nil {
			_, err = h.Pool.Exec(ctx, `UPDATE billing_prepaid_purchase SET reversal_checked_at=clock_timestamp() WHERE id=$1`, p.ID)
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// prepaidPaymentReversal reads the charge behind a purchase. A first purchase
// was paid by its first invoice, which can also be reversed by a credit note.
func (c *stripeHTTPClient) prepaidPaymentReversal(ctx context.Context, customer string, amount int64, intent, invoice *string) (*prepaidReversal, error) {
	var intents []string
	var creditNotes int64
	switch {
	case intent != nil:
		intents = []string{*intent}
	case invoice != nil:
		inv, err := c.RetrieveBillingInvoice(ctx, *invoice)
		if err != nil {
			return nil, err
		}
		if inv.Customer != customer {
			return nil, errors.New("purchase invoice ownership changed")
		}
		creditNotes = inv.PostPaymentCreditNotes
		var payments struct {
			Data []struct {
				Invoice string `json:"invoice"`
				Payment struct {
					Type   string `json:"type"`
					Intent string `json:"payment_intent"`
				} `json:"payment"`
			} `json:"data"`
			HasMore bool `json:"has_more"`
		}
		q := url.Values{"invoice": {*invoice}, "status": {"paid"}, "limit": {"100"}}
		if err = c.doForm(ctx, http.MethodGet, "/v1/invoice_payments?"+q.Encode(), nil, &payments, ""); err != nil {
			return nil, err
		}
		if payments.HasMore {
			return nil, errors.New("purchase invoice payments exceed the supported bound")
		}
		for _, p := range payments.Data {
			if p.Invoice == *invoice && p.Payment.Type == "payment_intent" && p.Payment.Intent != "" {
				intents = append(intents, p.Payment.Intent)
			}
		}
	default:
		return nil, errPrepaidNoPaymentReference
	}
	reversed, disputed := creditNotes, false
	var charges []prepaidChargeState
	for _, id := range intents {
		pi, err := c.retrievePrepaidPaymentIntentCharge(ctx, id)
		if err != nil {
			return nil, err
		}
		if pi.Customer != customer || pi.LatestCharge == "" {
			return nil, errors.New("purchase payment intent ownership or charge changed")
		}
		var charge prepaidChargeState
		if err = c.doForm(ctx, http.MethodGet, "/v1/charges/"+url.PathEscape(pi.LatestCharge), nil, &charge, ""); err != nil {
			return nil, err
		}
		if charge.ID != pi.LatestCharge || charge.Customer != customer {
			return nil, errors.New("purchase charge ownership changed")
		}
		charges = append(charges, charge)
		reversed += charge.AmountRefunded
		disputed = disputed || charge.Disputed
	}
	if reversed == 0 && !disputed {
		return nil, nil
	}
	kind := "refund"
	if disputed {
		kind = "dispute"
		reversed = amount
	}
	evidence, _ := json.Marshal(map[string]any{"charges": charges, "post_payment_credit_notes": creditNotes})
	return &prepaidReversal{Kind: kind, Reversed: min(reversed, amount), Evidence: evidence}, nil
}

type prepaidIntentCharge struct {
	ID           string `json:"id"`
	Customer     string `json:"customer"`
	LatestCharge string `json:"latest_charge"`
}

func (c *stripeHTTPClient) retrievePrepaidPaymentIntentCharge(ctx context.Context, id string) (prepaidIntentCharge, error) {
	var pi prepaidIntentCharge
	err := c.doForm(ctx, http.MethodGet, "/v1/payment_intents/"+url.PathEscape(id), nil, &pi, "")
	if err == nil && pi.ID != id {
		err = errors.New("payment intent identity changed")
	}
	return pi, err
}

// A full refund returned all of the customer's money, so its unspent credit is
// revoked without waiting for staff. Partial refunds and disputes (which can
// be won) are held for a staff decision. A dispute also stops auto-recharge.
func (h *Handlers) recordPrepaidReversal(ctx context.Context, id, team uuid.UUID, source string, amount int64, r prepaidReversal) error {
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO billing_prepaid_reversal(purchase_id,team_id,kind,reversed_cents,evidence) VALUES($1,$2,$3,$4,$5)
 ON CONFLICT(purchase_id) DO UPDATE SET kind=EXCLUDED.kind,reversed_cents=EXCLUDED.reversed_cents,evidence=EXCLUDED.evidence
 WHERE billing_prepaid_reversal.resolution IS NULL AND EXCLUDED.reversed_cents>=billing_prepaid_reversal.reversed_cents
  AND (billing_prepaid_reversal.kind,billing_prepaid_reversal.reversed_cents) IS DISTINCT FROM (EXCLUDED.kind,EXCLUDED.reversed_cents)`, id, team, r.Kind, r.Reversed, r.Evidence); err != nil {
		return err
	}
	if r.Kind == "refund" && r.Reversed >= amount {
		if _, err = tx.Exec(ctx, `UPDATE billing_prepaid_reversal SET resolution='revoke',resolved_at=clock_timestamp(),resolution_reason='payment fully refunded'
 WHERE purchase_id=$1 AND resolution IS NULL`, id); err != nil {
			return err
		}
	}
	if r.Kind == "dispute" {
		if _, err = tx.Exec(ctx, `UPDATE billing_prepaid_auto_recharge SET enabled=false,disabled_reason='a credit purchase was disputed',disabled_at=clock_timestamp(),notified_at=NULL
 WHERE team_id=$1 AND enabled`, team); err != nil {
			return err
		}
	}
	log.Warn().Str("prepaid_purchase_id", id.String()).Str("kind", r.Kind).Int64("reversed_cents", r.Reversed).Str("source", source).Msg("prepaid purchase payment was reversed")
	return tx.Commit(ctx)
}

// Revocation voids whatever the purchase's grant still holds. Spent credit is
// never clawed back here; staff bill it separately if they decide to.
func (h *Handlers) revokeReversedPrepaidCredit(ctx context.Context, client *stripeHTTPClient) error {
	rows, err := h.Pool.Query(ctx, `SELECT r.purchase_id,r.team_id,p.customer_id,p.credit_state,p.stripe_credit_grant_id,p.credit_voided_at IS NOT NULL
 FROM billing_prepaid_reversal r JOIN billing_prepaid_purchase p ON p.id=r.purchase_id
 WHERE r.resolution='revoke' AND r.revoked_at IS NULL ORDER BY r.resolved_at LIMIT 10`)
	if err != nil {
		return err
	}
	type revoke struct {
		ID, Team              uuid.UUID
		Customer, CreditState string
		Grant                 *string
		Voided                bool
	}
	due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (revoke, error) {
		var r revoke
		err := row.Scan(&r.ID, &r.Team, &r.Customer, &r.CreditState, &r.Grant, &r.Voided)
		return r, err
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range due {
		errs = append(errs, h.revokePrepaidGrant(ctx, client, r.ID, r.Team, r.Customer, r.CreditState, r.Grant, r.Voided))
	}
	return errors.Join(errs...)
}

func (h *Handlers) revokePrepaidGrant(ctx context.Context, client *stripeHTTPClient, id, team uuid.UUID, customer, creditState string, grant *string, voided bool) error {
	switch {
	case creditState == "none" || creditState == "pending":
		_, err := h.Pool.Exec(ctx, `UPDATE billing_prepaid_reversal SET revoked_at=clock_timestamp(),revoked_balance_cents=0 WHERE purchase_id=$1 AND revoked_at IS NULL`, id)
		return err
	case creditState == "uncertain" || grant == nil:
		return nil
	case voided:
		_, err := h.Pool.Exec(ctx, `UPDATE billing_prepaid_reversal SET revoked_at=clock_timestamp(),revoked_balance_cents=0 WHERE purchase_id=$1 AND revoked_at IS NULL`, id)
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
	balance, err := client.grantLedgerBalance(ctx, customer, *grant)
	if err != nil {
		return err
	}
	var g entitlementGrant
	if err = client.doForm(ctx, http.MethodPost, "/v1/billing/credit_grants/"+url.PathEscape(*grant)+"/void", url.Values{}, &g, "prepaid-credit-revoke:v1:"+id.String()); err != nil {
		return err
	}
	if g.ID != *grant || g.VoidedAt == 0 {
		return errors.New("credit grant revocation was not confirmed")
	}
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE billing_prepaid_purchase SET credit_voided_at=clock_timestamp() WHERE id=$1 AND credit_voided_at IS NULL`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE billing_prepaid_reversal SET revoked_at=clock_timestamp(),revoked_balance_cents=$2 WHERE purchase_id=$1 AND revoked_at IS NULL`, id, balance); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE billing_funding_account SET next_check_at=LEAST(next_check_at,clock_timestamp()) WHERE team_id=$1 AND observe`, team); err != nil {
		return err
	}
	log.Warn().Str("prepaid_purchase_id", id.String()).Int64("revoked_cents", balance).Msg("revoked unspent credit of a reversed prepaid purchase")
	return tx.Commit(ctx)
}

// prepaidCreditWithheld reports whether a reversed purchase must not be
// credited: any reversal that staff have not decided to keep.
func (h *Handlers) prepaidCreditWithheld(ctx context.Context, id uuid.UUID) (bool, error) {
	var withheld bool
	err := h.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_prepaid_reversal WHERE purchase_id=$1 AND resolution IS DISTINCT FROM 'keep')`, id).Scan(&withheld)
	return withheld, err
}

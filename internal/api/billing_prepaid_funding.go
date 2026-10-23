package api

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog/log"
)

// Spent purchase grants are voided only after the invoice that consumes them
// closes, so a month of purchases stays live. Grant reads page up to 2000
// grants; this guard keeps a runaway well below that.
const prepaidLiveGrantLimit = 200

var errPrepaidCapacity = errors.New("prepaid credit is at its observation capacity")
var errPrepaidTooManyUnspent = errors.New("too many unspent credit purchases")

// enrolPrepaidObservation takes a funding observation slot once a purchase is
// paid. Observation alone changes nothing: eligibility still follows today's
// rule until the team's credit is observed and prepaid enforcement begins.
func (h *Handlers) enrolPrepaidObservation(ctx context.Context, team uuid.UUID) error {
	if _, err := h.Pool.Exec(ctx, `INSERT INTO billing_funding_account(team_id) VALUES($1) ON CONFLICT(team_id) DO NOTHING`, team); err != nil {
		return err
	}
	_, err := h.Pool.Exec(ctx, `UPDATE billing_funding_account SET observe=true WHERE team_id=$1 AND NOT observe`, team)
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
		_, err = h.Pool.Exec(ctx, `UPDATE billing_prepaid_purchase SET credit_balance_checked_at=clock_timestamp()-interval '50 minutes' WHERE id=$1`, id)
		return err
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

// prepaidCapacityAvailable is checked before any money moves; the slot itself
// is taken after payment so abandoned checkouts never hold one.
func (h *Handlers) prepaidCapacityAvailable(ctx context.Context, team uuid.UUID) (bool, error) {
	var ok bool
	err := h.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT observe FROM billing_funding_account WHERE team_id=$1),false)
 OR (SELECT count(*) FROM billing_funding_account WHERE observe)<(SELECT max_observed_teams FROM billing_funding_policy WHERE singleton)`, team).Scan(&ok)
	return ok, err
}

// Prepaid teams accumulate one grant per purchase, so their balance is read
// once for the customer's metered credit instead of once per grant. Any grant
// the aggregate cannot describe (price-scoped, expiring, not yet effective)
// sends the team back to the per-grant observation.
func (h *Handlers) aggregateFundingGrants(ctx context.Context, client *stripeHTTPClient, team uuid.UUID, customer string, at time.Time) (fundingGrantSnapshot, bool, error) {
	if h.Pool == nil {
		return fundingGrantSnapshot{}, false, nil
	}
	var prepaid bool
	if err := h.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_prepaid_purchase WHERE team_id=$1 AND customer_id=$2 AND credit_state='issued')`, team, customer).Scan(&prepaid); err != nil || !prepaid {
		return fundingGrantSnapshot{}, false, err
	}
	head, start, active, ok, err := client.meteredGrantInventory(ctx, customer, at)
	if err != nil || !ok {
		return fundingGrantSnapshot{}, false, err
	}
	before, err := client.meteredCreditBalance(ctx, customer)
	if err != nil {
		return fundingGrantSnapshot{}, false, err
	}
	return fundingGrantSnapshot{
		Total: before.Ledger, Start: start,
		Evidence: map[string]any{"metered_ledger_cents": before.Ledger, "metered_available_cents": before.Available, "active_grants": active},
		Reserved: func() *big.Int { return big.NewInt(before.Ledger - before.Available) },
		Stable: func(ctx context.Context) error {
			after, err := client.meteredCreditBalance(ctx, customer)
			if err != nil {
				return err
			}
			if after != before {
				return errors.New("funding credit ledger changed during observation")
			}
			current, _, _, ok, err := client.meteredGrantInventory(ctx, customer, at)
			if err != nil {
				return err
			}
			if !ok || current != head {
				return errors.New("funding credit grants changed during observation")
			}
			return nil
		},
	}, true, nil
}

func (c *stripeHTTPClient) meteredGrantInventory(ctx context.Context, customer string, at time.Time) (string, time.Time, int, bool, error) {
	start, head, active := at, "", 0
	cursor := ""
	for page := 0; page < 20; page++ {
		q := url.Values{"customer": {customer}, "limit": {"100"}}
		if cursor != "" {
			q.Set("starting_after", cursor)
		}
		var list struct {
			Data    []entitlementGrant `json:"data"`
			HasMore bool               `json:"has_more"`
		}
		if err := c.doForm(ctx, http.MethodGet, "/v1/billing/credit_grants?"+q.Encode(), nil, &list, ""); err != nil {
			return "", at, 0, false, err
		}
		for _, g := range list.Data {
			if g.ID == "" || g.Customer != customer {
				return "", at, 0, false, errors.New("funding credit customer mismatch")
			}
			if head == "" {
				head = g.ID
			}
			if g.VoidedAt != 0 || (g.ExpiresAt != 0 && g.ExpiresAt <= at.Unix()) {
				continue
			}
			if g.EffectiveAt > at.Unix() || g.ExpiresAt != 0 || g.Applicability.Scope.PriceType != "metered" || len(g.Applicability.Scope.Prices) != 0 || g.Amount.Monetary.Currency != "usd" {
				return "", at, 0, false, nil
			}
			active++
			effective := g.EffectiveAt
			if effective == 0 {
				effective = g.Created
			}
			if effective <= 0 {
				return "", at, 0, false, errors.New("funding credit has no effective boundary")
			}
			if time.Unix(effective, 0).Before(start) {
				start = time.Unix(effective, 0)
			}
		}
		if !list.HasMore {
			return head, start, active, true, nil
		}
		if len(list.Data) == 0 {
			return "", at, 0, false, errors.New("empty incomplete credit grant page")
		}
		cursor = list.Data[len(list.Data)-1].ID
	}
	return "", at, 0, false, errors.New("funding credit inventory exceeds the observation bound")
}

func (c *stripeHTTPClient) meteredCreditBalance(ctx context.Context, customer string) (fundingGrantBalance, error) {
	type amount struct {
		Monetary *struct {
			Currency string `json:"currency"`
			Value    int64  `json:"value"`
		} `json:"monetary"`
	}
	var summary struct {
		Balances []struct {
			Ledger    amount `json:"ledger_balance"`
			Available amount `json:"available_balance"`
		} `json:"balances"`
	}
	q := url.Values{"customer": {customer}, "filter[type]": {"applicability_scope"}, "filter[applicability_scope][price_type]": {"metered"}}
	if err := c.doForm(ctx, http.MethodGet, "/v1/billing/credit_balance_summary?"+q.Encode(), nil, &summary, ""); err != nil {
		return fundingGrantBalance{}, err
	}
	if len(summary.Balances) != 1 || summary.Balances[0].Ledger.Monetary == nil || summary.Balances[0].Available.Monetary == nil {
		return fundingGrantBalance{}, errors.New("metered credit balance evidence incomplete")
	}
	l, a := summary.Balances[0].Ledger.Monetary, summary.Balances[0].Available.Monetary
	if l.Currency != "usd" || a.Currency != "usd" || l.Value < 0 || a.Value < 0 || a.Value > l.Value {
		return fundingGrantBalance{}, errors.New("metered credit balance evidence invalid")
	}
	return fundingGrantBalance{Ledger: l.Value, Available: a.Value}, nil
}

package api

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
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
	_, err := h.Pool.Exec(ctx, `UPDATE billing_funding_account SET observe=true,prepaid_enrolment_due_at=NULL WHERE team_id=$1 AND (NOT observe OR prepaid_enrolment_due_at IS NOT NULL)`, team)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "P0001" {
		if _, markErr := h.Pool.Exec(ctx, `UPDATE billing_funding_account SET prepaid_enrolment_due_at=COALESCE(prepaid_enrolment_due_at,clock_timestamp()) WHERE team_id=$1`, team); markErr != nil {
			return markErr
		}
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

// Prepaid teams accumulate one grant per purchase, so their non-expiring
// metered grants are read as one customer balance. Grants of any other shape
// (for example allowances after a plan change) are read one by one under the
// same bound as the per-grant observation. Finalized periods have already
// debited the ledger, so the reconciliation window starts no earlier than the
// last one.
func (h *Handlers) aggregateFundingGrants(ctx context.Context, client *stripeHTTPClient, team uuid.UUID, customer string, at time.Time) (fundingGrantSnapshot, bool, error) {
	if h.Pool == nil {
		return fundingGrantSnapshot{}, false, nil
	}
	var prepaid bool
	var floor *time.Time
	if err := h.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_prepaid_purchase WHERE team_id=$1 AND customer_id=$2 AND credit_state='issued'),
 (SELECT max(p.period_end) FROM team_billing_period p JOIN billing_invoice_close j USING(team_id,period_start,period_end) WHERE p.team_id=$1 AND p.finalized_at IS NOT NULL AND j.state='released')`, team, customer).Scan(&prepaid, &floor); err != nil || !prepaid {
		return fundingGrantSnapshot{}, false, err
	}
	inv, err := client.meteredGrantInventory(ctx, customer, at)
	if err != nil {
		return fundingGrantSnapshot{}, false, err
	}
	if len(inv.Others) > 8 {
		return fundingGrantSnapshot{}, false, errors.New("active funding credit inventory exceeds observation bound")
	}
	metered := fundingGrantBalance{}
	if inv.Metered > 0 {
		if metered, err = client.meteredCreditBalance(ctx, customer); err != nil {
			return fundingGrantSnapshot{}, false, err
		}
	}
	start := at
	if inv.Metered > 0 && inv.MeteredStart.Before(start) {
		start = inv.MeteredStart
	}
	var expiry pgtype.Timestamptz
	var required []string
	others := map[string]fundingGrantBalance{}
	total := metered.Ledger
	for _, g := range inv.Others {
		if g.Applicability.Scope.PriceType != "metered" && required == nil {
			if required, err = h.fundingCreditResourcePrices(ctx, team, customer); err != nil {
				return fundingGrantSnapshot{}, false, err
			}
		}
		if !fundingGrantCoversResources(g, required) {
			return fundingGrantSnapshot{}, false, errors.New("funding credit applicability is unknown")
		}
		balance, err := fundingProviderGrantBalance(ctx, client, customer, g.ID)
		if err != nil {
			return fundingGrantSnapshot{}, false, err
		}
		if balance.Ledger == 0 {
			continue
		}
		if balance.Ledger > g.Amount.Monetary.Value || total > int64(^uint64(0)>>1)-balance.Ledger {
			return fundingGrantSnapshot{}, false, errors.New("invalid funding credit ledger total")
		}
		others[g.ID] = balance
		total += balance.Ledger
		effective := g.EffectiveAt
		if effective == 0 {
			effective = g.Created
		}
		if effective <= 0 {
			return fundingGrantSnapshot{}, false, errors.New("funding credit has no effective boundary")
		}
		if time.Unix(effective, 0).Before(start) {
			start = time.Unix(effective, 0)
		}
		if g.ExpiresAt > 0 && (!expiry.Valid || time.Unix(g.ExpiresAt, 0).Before(expiry.Time)) {
			expiry = pgtype.Timestamptz{Valid: true, Time: time.Unix(g.ExpiresAt, 0)}
		}
	}
	if floor != nil && start.Before(*floor) {
		start = *floor
	}
	grants := map[string]entitlementGrant{}
	for _, g := range inv.Others {
		grants[g.ID] = g
	}
	return fundingGrantSnapshot{
		Total: total, Start: start, Expiry: expiry,
		Evidence: map[string]any{"metered_ledger_cents": metered.Ledger, "metered_available_cents": metered.Available, "metered_grants": inv.Metered, "other_grants": others},
		Reserved: func() *big.Int {
			reserved := big.NewInt(metered.Ledger - metered.Available)
			for _, b := range others {
				reserved.Add(reserved, big.NewInt(b.Ledger-b.Available))
			}
			return reserved
		},
		Stable: func(ctx context.Context) error {
			current, err := client.meteredGrantInventory(ctx, customer, at)
			if err != nil {
				return err
			}
			if current.Head != inv.Head || current.Metered != inv.Metered || len(current.Others) != len(inv.Others) {
				return errors.New("funding credit grants changed during observation")
			}
			if inv.Metered > 0 {
				after, err := client.meteredCreditBalance(ctx, customer)
				if err != nil {
					return err
				}
				if after != metered {
					return errors.New("funding credit ledger changed during observation")
				}
			}
			for id, before := range others {
				after, err := fundingProviderGrantBalance(ctx, client, customer, id)
				if err != nil {
					return err
				}
				if after != before {
					return errors.New("funding credit ledger changed during observation")
				}
				grant, err := client.retrieveEntitlementGrant(ctx, id)
				if err != nil {
					return err
				}
				a, _ := json.Marshal(grant)
				b, _ := json.Marshal(grants[id])
				if string(a) != string(b) {
					return errors.New("funding credit grant changed during observation")
				}
			}
			return nil
		},
	}, true, nil
}

type meteredInventory struct {
	Head         string
	Metered      int
	MeteredStart time.Time
	Others       []entitlementGrant
}

func (c *stripeHTTPClient) meteredGrantInventory(ctx context.Context, customer string, at time.Time) (meteredInventory, error) {
	inv := meteredInventory{MeteredStart: at}
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
			return inv, err
		}
		for _, g := range list.Data {
			if g.ID == "" || g.Customer != customer {
				return inv, errors.New("funding credit customer mismatch")
			}
			if inv.Head == "" {
				inv.Head = g.ID
			}
			if g.VoidedAt != 0 || g.EffectiveAt > at.Unix() || (g.ExpiresAt != 0 && g.ExpiresAt <= at.Unix()) {
				continue
			}
			if g.ExpiresAt != 0 || g.Applicability.Scope.PriceType != "metered" || len(g.Applicability.Scope.Prices) != 0 || g.Amount.Monetary.Currency != "usd" {
				inv.Others = append(inv.Others, g)
				continue
			}
			inv.Metered++
			effective := g.EffectiveAt
			if effective == 0 {
				effective = g.Created
			}
			if effective <= 0 {
				return inv, errors.New("funding credit has no effective boundary")
			}
			if time.Unix(effective, 0).Before(inv.MeteredStart) {
				inv.MeteredStart = time.Unix(effective, 0)
			}
		}
		if !list.HasMore {
			return inv, nil
		}
		if len(list.Data) == 0 {
			return inv, errors.New("empty incomplete credit grant page")
		}
		cursor = list.Data[len(list.Data)-1].ID
	}
	return inv, errors.New("funding credit inventory exceeds the observation bound")
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
	if len(summary.Balances) == 0 {
		return fundingGrantBalance{}, nil
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

// A paid prepaid team must be observed for its balance to count and for
// auto-recharge to work; enrolment that failed after payment is retried here.
func (h *Handlers) retryPrepaidEnrolment(ctx context.Context) error {
	rows, err := h.Pool.Query(ctx, `SELECT team_id FROM billing_funding_account WHERE prepaid_enrolment_due_at IS NOT NULL ORDER BY prepaid_enrolment_due_at LIMIT 20`)
	if err != nil {
		return err
	}
	teams, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	var errs []error
	for _, team := range teams {
		allowed, err := prepaidPlanAllowsPurchase(ctx, h, team)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !allowed {
			_, err = h.Pool.Exec(ctx, `UPDATE billing_funding_account SET prepaid_enrolment_due_at=NULL WHERE team_id=$1`, team)
			errs = append(errs, err)
			continue
		}
		if err = h.enrolPrepaidObservation(ctx, team); err != nil {
			log.Error().Err(err).Str("team_id", team.String()).Msg("paid prepaid team is not observed; its balance and auto-recharge wait for an observation slot")
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

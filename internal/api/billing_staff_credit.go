package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

var errStaffCreditConflict = errors.New("staff credit conflicts with an earlier request")

type staffCreditRequest struct {
	Key         string     `json:"idempotency_key"`
	AmountCents int64      `json:"amount_cents"`
	Reason      string     `json:"reason"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

type staffCredit struct {
	ID          uuid.UUID  `json:"id"`
	AmountCents int64      `json:"amount_cents"`
	ExpiresAt   *time.Time `json:"expires_at"`
	Reason      string     `json:"reason"`
	RequestedBy uuid.UUID  `json:"requested_by"`
	RequestedAt time.Time  `json:"requested_at"`
	State       string     `json:"state"`
	GrantID     *string    `json:"stripe_credit_grant_id"`
	IssuedAt    *time.Time `json:"issued_at"`
	LastError   *string    `json:"last_error"`
	customer    string
}

const staffCreditColumns = `id,amount_cents,expires_at,reason,requested_by,requested_at,state,stripe_credit_grant_id,issued_at,last_error,customer_id`

func scanStaffCredit(row pgx.Row) (staffCredit, error) {
	var s staffCredit
	err := row.Scan(&s.ID, &s.AmountCents, &s.ExpiresAt, &s.Reason, &s.RequestedBy, &s.RequestedAt, &s.State, &s.GrantID, &s.IssuedAt, &s.LastError, &s.customer)
	return s, err
}

// CreateStaffCreditGrant grants a team one-off usage credit. The credit applies
// to metered usage only, never to plan fees, and works on any plan.
func (h *Handlers) CreateStaffCreditGrant(c *gin.Context) {
	team, ok := ycTeam(c)
	if !ok {
		return
	}
	actor, ok := h.requirePlatformBilling(c, platformBillingWritePermission)
	if !ok {
		return
	}
	var req staffCreditRequest
	if c.ShouldBindJSON(&req) != nil {
		respondErrorMsg(c, "bad_request", "invalid credit grant", http.StatusBadRequest)
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if strings.TrimSpace(req.Key) == "" || len(req.Key) > 200 || req.AmountCents < 1 || req.AmountCents > 100000000 || req.Reason == "" || len(req.Reason) > 2000 {
		respondErrorMsg(c, "bad_request", "idempotency_key, amount_cents (1 to 100000000) and reason are required", http.StatusBadRequest)
		return
	}
	if req.ExpiresAt != nil && (!req.ExpiresAt.After(time.Now().Add(time.Hour)) || req.ExpiresAt.After(time.Now().AddDate(5, 0, 0))) {
		respondErrorMsg(c, "bad_request", "expires_at must be between one hour and five years away", http.StatusBadRequest)
		return
	}
	credit, err := h.grantStaffCredit(c.Request.Context(), team, actor, req)
	switch {
	case errors.Is(err, errStaffCreditConflict):
		respondErrorMsg(c, "conflict", err.Error(), http.StatusConflict)
		return
	case err != nil && credit.ID != uuid.Nil:
		log.Error().Err(err).Str("team_id", team.String()).Str("staff_credit_id", credit.ID.String()).Msg("staff credit grant pending")
		respondErrorMsg(c, "unavailable", "the credit was recorded but Stripe did not confirm it; retry with the same idempotency key", http.StatusServiceUnavailable)
		return
	case err != nil:
		log.Error().Err(err).Str("team_id", team.String()).Msg("staff credit grant failed")
		respondError(c, ErrInternal)
		return
	}
	c.JSON(http.StatusOK, credit)
}

func (h *Handlers) ListStaffCreditGrants(c *gin.Context) {
	team, ok := ycTeam(c)
	if !ok {
		return
	}
	if _, ok = h.requirePlatformBilling(c, platformBillingReadPermission); !ok {
		return
	}
	rows, err := h.Pool.Query(c.Request.Context(), `SELECT `+staffCreditColumns+` FROM billing_staff_credit_grant WHERE team_id=$1 ORDER BY requested_at DESC LIMIT 50`, team)
	if err == nil {
		var credits []staffCredit
		credits, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (staffCredit, error) { return scanStaffCredit(row) })
		if err == nil {
			if credits == nil {
				credits = []staffCredit{}
			}
			c.JSON(http.StatusOK, gin.H{"credits": credits})
			return
		}
	}
	log.Error().Err(err).Str("team_id", team.String()).Msg("list staff credit grants failed")
	respondError(c, ErrInternal)
}

// grantStaffCredit records the intent durably, then issues or adopts the one
// Stripe grant that carries the row's id. Repeating a request is safe.
func (h *Handlers) grantStaffCredit(ctx context.Context, team, actor uuid.UUID, req staffCreditRequest) (staffCredit, error) {
	client, ok := h.Stripe.(*stripeHTTPClient)
	if !ok {
		return staffCredit{}, errors.New("staff credit requires the Stripe provider")
	}
	var customer *string
	err := h.Pool.QueryRow(ctx, `SELECT stripe_customer_id FROM team_billing_account WHERE team_id=$1`, team).Scan(&customer)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && customer == nil {
		return staffCredit{}, errors.Join(errStaffCreditConflict, errors.New("team has no billing customer"))
	}
	if err != nil {
		return staffCredit{}, err
	}
	if _, err = h.Pool.Exec(ctx, `INSERT INTO billing_staff_credit_grant(team_id,idempotency_key,customer_id,amount_cents,expires_at,reason,requested_by)
        VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(team_id,idempotency_key) DO NOTHING`, team, req.Key, *customer, req.AmountCents, req.ExpiresAt, req.Reason, actor); err != nil {
		return staffCredit{}, err
	}
	credit, err := scanStaffCredit(h.Pool.QueryRow(ctx, `SELECT `+staffCreditColumns+` FROM billing_staff_credit_grant WHERE team_id=$1 AND idempotency_key=$2`, team, req.Key))
	if err != nil {
		return staffCredit{}, err
	}
	sameExpiry := (credit.ExpiresAt == nil) == (req.ExpiresAt == nil) && (credit.ExpiresAt == nil || credit.ExpiresAt.Equal(*req.ExpiresAt))
	if credit.AmountCents != req.AmountCents || !sameExpiry {
		return staffCredit{}, errors.Join(errStaffCreditConflict, errors.New("idempotency key was used for a different credit"))
	}
	if credit.State == "issued" {
		return credit, nil
	}
	grant, err := client.issueStaffCredit(ctx, credit, func() error {
		_, e := h.Pool.Exec(ctx, `UPDATE billing_staff_credit_grant SET attempts=attempts+1 WHERE id=$1 AND state='pending'`, credit.ID)
		return e
	})
	if err != nil {
		_, _ = h.Pool.Exec(context.WithoutCancel(ctx), `UPDATE billing_staff_credit_grant SET last_error=$2 WHERE id=$1 AND state='pending'`, credit.ID, err.Error())
		return credit, err
	}
	evidence, err := json.Marshal(grant)
	if err != nil {
		return credit, err
	}
	if _, err = h.Pool.Exec(ctx, `UPDATE billing_staff_credit_grant SET state='issued',stripe_credit_grant_id=$2,issued_at=clock_timestamp(),issuance_evidence=$3,last_error=NULL WHERE id=$1 AND state='pending'`, credit.ID, grant.ID, evidence); err != nil {
		return credit, err
	}
	return scanStaffCredit(h.Pool.QueryRow(ctx, `SELECT `+staffCreditColumns+` FROM billing_staff_credit_grant WHERE id=$1`, credit.ID))
}

// issueStaffCredit adopts the grant already carrying this credit's id, or
// creates it. Searching first keeps a retry after Stripe's idempotency window
// from granting twice.
func (c *stripeHTTPClient) issueStaffCredit(ctx context.Context, credit staffCredit, beforeCreate func() error) (entitlementGrant, error) {
	grant, found, err := c.findStaffCreditGrant(ctx, credit)
	if err != nil || found {
		return grant, err
	}
	if err = beforeCreate(); err != nil {
		return grant, err
	}
	form := url.Values{
		"customer":                   {credit.customer},
		"name":                       {"Superserve credit"},
		"category":                   {"promotional"},
		"amount[type]":               {"monetary"},
		"amount[monetary][currency]": {"usd"},
		"amount[monetary][value]":    {strconv.FormatInt(credit.AmountCents, 10)},
		"applicability_config[scope][price_type]": {"metered"},
		"priority":                     {"50"},
		"metadata[staff_credit_grant]": {credit.ID.String()},
	}
	if credit.ExpiresAt != nil {
		form.Set("expires_at", strconv.FormatInt(credit.ExpiresAt.Unix(), 10))
	}
	if err = c.doForm(ctx, http.MethodPost, "/v1/billing/credit_grants", form, &grant, "staff-credit:"+credit.ID.String()); err != nil {
		return grant, err
	}
	return grant, validStaffCreditGrant(grant, credit)
}

func (c *stripeHTTPClient) findStaffCreditGrant(ctx context.Context, credit staffCredit) (entitlementGrant, bool, error) {
	after := ""
	for page := 0; page < 20; page++ {
		var list struct {
			Data    []entitlementGrant `json:"data"`
			HasMore bool               `json:"has_more"`
		}
		q := url.Values{"customer": {credit.customer}, "limit": {"100"}}
		if after != "" {
			q.Set("starting_after", after)
		}
		if err := c.doForm(ctx, http.MethodGet, "/v1/billing/credit_grants?"+q.Encode(), nil, &list, ""); err != nil {
			return entitlementGrant{}, false, err
		}
		for _, g := range list.Data {
			if g.Metadata["staff_credit_grant"] == credit.ID.String() {
				return g, true, validStaffCreditGrant(g, credit)
			}
		}
		if !list.HasMore || len(list.Data) == 0 {
			return entitlementGrant{}, false, nil
		}
		after = list.Data[len(list.Data)-1].ID
	}
	return entitlementGrant{}, false, errors.New("credit grant inventory exceeds the search bound; operator review required")
}

func validStaffCreditGrant(g entitlementGrant, credit staffCredit) error {
	expires := int64(0)
	if credit.ExpiresAt != nil {
		expires = credit.ExpiresAt.Unix()
	}
	if g.ID == "" || g.Customer != credit.customer || g.Amount.Type != "monetary" || g.Amount.Monetary.Currency != "usd" || g.Amount.Monetary.Value != credit.AmountCents ||
		g.VoidedAt != 0 || g.Priority != 50 || g.ExpiresAt != expires || g.Applicability.Scope.PriceType != "metered" || len(g.Applicability.Scope.Prices) != 0 ||
		g.Metadata["staff_credit_grant"] != credit.ID.String() {
		return fmt.Errorf("Stripe credit grant %s does not match the staff credit; operator review required", g.ID)
	}
	return nil
}

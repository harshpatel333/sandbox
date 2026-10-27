package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"github.com/superserve-ai/sandbox/internal/db"
)

type pendingStarterRequest struct {
	PurchaseID uuid.UUID  `json:"purchase_id"`
	Generation *time.Time `json:"checkout_generation"`
	Cancel     bool       `json:"cancel"`
}

type pendingStarterCheckout struct {
	Account         db.TeamBillingAccount
	Purchase        uuid.UUID
	Amount          int64
	Payment, Credit string
}

// loadPendingStarter deliberately follows the account fence, rather than the
// latest purchase: an older unpaid session must never release a newer checkout.
func (h *Handlers) loadPendingStarter(ctx context.Context, team, actor uuid.UUID) (pendingStarterCheckout, error) {
	var p pendingStarterCheckout
	var err error
	p.Account, err = h.DB.GetTeamBillingCheckoutForRecovery(ctx, team)
	if err != nil {
		return p, err
	}
	a := p.Account
	if !a.StripeCheckoutActorID.Valid || uuid.UUID(a.StripeCheckoutActorID.Bytes) != actor || !a.CheckoutInitializingAt.Valid || a.CheckoutSessionID == nil || a.StripeCustomerID == nil {
		return p, pgx.ErrNoRows
	}
	err = h.Pool.QueryRow(ctx, `SELECT id,amount_cents,payment_state,credit_state FROM billing_prepaid_purchase WHERE team_id=$1 AND actor_id=$2 AND customer_id=$3 AND checkout_session_id=$4 AND source='checkout'`, team, actor, *a.StripeCustomerID, *a.CheckoutSessionID).Scan(&p.Purchase, &p.Amount, &p.Payment, &p.Credit)
	if !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	// The account session can commit before its purchase association. Discover
	// only the purchase identified by that exact provider session, never the
	// newest local purchase. An explicit action binds it under the account lock.
	client, ok := h.Stripe.(*stripeHTTPClient)
	if !ok {
		return p, errors.New("Starter provider verification unavailable")
	}
	s, err := client.RetrieveCheckoutSession(ctx, *a.CheckoutSessionID)
	if err != nil {
		return p, err
	}
	if _, hasPrepaidIdentity := s.Metadata["prepaid_purchase_id"]; !hasPrepaidIdentity {
		knownPro, err := h.pendingStarterIsKnownPro(ctx, s, p, actor)
		if err != nil {
			return p, err
		}
		if knownPro {
			// This says only that this original fence is not a Starter purchase.
			// Pro recovery retains exclusive authority over its unchanged fence.
			return p, pgx.ErrNoRows
		}
	}
	p.Purchase, err = uuid.Parse(s.Metadata["prepaid_purchase_id"])
	if err != nil || !pendingStarterIdentity(s, p, actor) {
		return p, errors.New("Starter checkout purchase identity differs")
	}
	err = h.Pool.QueryRow(ctx, `SELECT amount_cents,payment_state,credit_state FROM billing_prepaid_purchase WHERE id=$1 AND team_id=$2 AND actor_id=$3 AND customer_id=$4 AND checkout_session_id IS NULL AND source='checkout'`, p.Purchase, team, actor, *a.StripeCustomerID).Scan(&p.Amount, &p.Payment, &p.Credit)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, errors.New("Starter checkout purchase association is unavailable")
	}
	return p, err
}

func (h *Handlers) pendingStarterIsKnownPro(ctx context.Context, s StripeRetrievedCheckoutSession, p pendingStarterCheckout, actor uuid.UUID) (bool, error) {
	choice, err := h.getProChoice(ctx, p.Account.TeamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var params StripeCreateCheckoutSessionParams
	if json.Unmarshal(choice.Payload, &params) != nil {
		return false, nil
	}
	offer, err := choice.frozenOffer()
	if err != nil || (offer.Family != "" && offer.Family != "pro") || (offer.Family == "" && choice.Offer != "pro_monthly" && choice.Offer != "pro_annual") {
		return false, nil
	}
	a := p.Account
	providerGeneration, providerErr := time.Parse(time.RFC3339Nano, s.Metadata["checkout_generation"])
	frozenGeneration, frozenErr := time.Parse(time.RFC3339Nano, params.Metadata["checkout_generation"])
	_, frozenPrepaid := params.Metadata["prepaid_purchase_id"]
	return !frozenPrepaid && providerErr == nil && frozenErr == nil &&
		(choice.State == "pending" || choice.State == "committed") && choice.Actor == actor &&
		a.CheckoutInitializingAt.Valid && choice.Generation.Equal(a.CheckoutInitializingAt.Time) &&
		providerGeneration.Equal(choice.Generation) && frozenGeneration.Equal(choice.Generation) &&
		a.CheckoutSessionID != nil && choice.Session != nil && s.ID == *a.CheckoutSessionID && s.ID == *choice.Session &&
		a.StripeCustomerID != nil && s.CustomerID == *a.StripeCustomerID && s.CustomerID == params.CustomerID &&
		a.CheckoutRequestKey != nil && params.IdempotencyKey != "" && params.IdempotencyKey == *a.CheckoutRequestKey &&
		s.Mode == "subscription" && s.ClientReferenceID == choice.Team.String() && params.ClientReferenceID == choice.Team.String() &&
		s.Metadata["activation_user_id"] == actor.String() && params.Metadata["activation_user_id"] == actor.String() &&
		s.Metadata["commercial_offer"] == choice.Offer && params.Metadata["commercial_offer"] == choice.Offer, nil
}

func pendingStarterIdentity(s StripeRetrievedCheckoutSession, p pendingStarterCheckout, actor uuid.UUID) bool {
	a := p.Account
	generation, err := time.Parse(time.RFC3339Nano, s.Metadata["checkout_generation"])
	return err == nil && a.CheckoutInitializingAt.Valid && a.CheckoutSessionID != nil && a.StripeCustomerID != nil && s.ID == *a.CheckoutSessionID && s.CustomerID == *a.StripeCustomerID && s.Mode == "subscription" && s.ClientReferenceID == a.TeamID.String() && s.Metadata["activation_user_id"] == actor.String() && s.Metadata["prepaid_purchase_id"] == p.Purchase.String() && generation.Equal(a.CheckoutInitializingAt.Time)
}

func pendingStarterExpiredUnpaid(s StripeRetrievedCheckoutSession) bool {
	return s.Status == "expired" && s.PaymentStatus == "unpaid" && s.SubscriptionID == "" && s.PaymentIntentID == "" && s.InvoiceID == ""
}

// RecoverPendingStarterCheckout exposes an original accepted purchase without
// evaluating new-sale flags or changed redirect policy. Cancellation is explicit
// and requires both the purchase and generation returned by discovery.
func (h *Handlers) RecoverPendingStarterCheckout(c *gin.Context) {
	team, err := customerContextTeamID(c)
	if err != nil {
		return
	}
	if !h.requireCustomerTeamPermission(c, team, "billing:write") {
		return
	}
	actor, err := customerActorID(c)
	if err != nil {
		return
	}
	if h.Pool == nil {
		respondError(c, ErrInternal)
		return
	}
	var input *pendingStarterRequest
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input == nil || !errors.Is(decoder.Decode(new(any)), io.EOF) || ((input.PurchaseID == uuid.Nil) != (input.Generation == nil)) || (input.Cancel && input.PurchaseID == uuid.Nil) {
		respondErrorMsg(c, "bad_request", "purchase and checkout generation are required together", 400)
		return
	}
	if input.PurchaseID != uuid.Nil {
		log.Info().Str("team_id", team.String()).Str("purchase_id", input.PurchaseID.String()).Bool("cancel", input.Cancel).Msg("Starter checkout recovery requested")
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	p, err := h.loadPendingStarter(ctx, team, actor)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(200, gin.H{"outcome": "no_pending_starter_checkout"})
		return
	}
	if err != nil {
		respondError(c, ErrInternal)
		return
	}
	if input.PurchaseID != uuid.Nil && (input.PurchaseID != p.Purchase || !input.Generation.Equal(p.Account.CheckoutInitializingAt.Time)) {
		respondCheckoutRecoveryUnavailable(c)
		return
	}
	if input.PurchaseID == uuid.Nil {
		c.JSON(200, gin.H{"outcome": "pending_starter_checkout", "purchase_id": p.Purchase, "amount_cents": p.Amount, "checkout_generation": p.Account.CheckoutInitializingAt.Time.UTC()})
		return
	}
	client, ok := h.Stripe.(*stripeHTTPClient)
	if !ok {
		respondErrorMsg(c, "service_unavailable", "checkout recovery unavailable", 503)
		return
	}
	outcome, target, err := h.reconcilePendingStarter(ctx, client, p, actor, input.Cancel)
	if err != nil {
		log.Warn().Err(err).Str("team_id", team.String()).Str("purchase_id", p.Purchase.String()).Bool("cancel", input.Cancel).Str("outcome", "payment_confirmation_pending").Msg("pending Starter checkout recovery retained original fence")
		c.JSON(http.StatusAccepted, gin.H{"outcome": "payment_confirmation_pending"})
		return
	}
	log.Info().Str("team_id", team.String()).Str("purchase_id", p.Purchase.String()).Bool("cancel", input.Cancel).Str("outcome", outcome).Bool("unpaid_cancellation_confirmed", input.Cancel && outcome == "retryable").Msg("Starter checkout recovery completed")
	response := gin.H{"outcome": outcome, "purchase_id": p.Purchase, "amount_cents": p.Amount, "checkout_generation": p.Account.CheckoutInitializingAt.Time.UTC()}
	if target != "" {
		response["url"] = target
	}
	c.JSON(200, response)
}

func (h *Handlers) reconcilePendingStarter(ctx context.Context, client *stripeHTTPClient, p pendingStarterCheckout, actor uuid.UUID, cancel bool) (string, string, error) {
	lease, err := claimStripeWebhookProcessingLease(ctx, h.DB, *p.Account.StripeCustomerID)
	if err != nil {
		return "", "", err
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer done()
		_ = h.DB.ReleaseStripeWebhookProcessingLease(cleanup, db.ReleaseStripeWebhookProcessingLeaseParams{CustomerID: lease.CustomerID, Token: lease.Token})
	}()
	s, err := client.RetrieveCheckoutSession(ctx, *p.Account.CheckoutSessionID)
	if err != nil {
		return "", "", err
	}
	if !pendingStarterIdentity(s, p, actor) {
		return "", "", errors.New("Starter checkout identity differs")
	}
	if cancel && s.Status == "open" && s.PaymentStatus == "unpaid" && s.SubscriptionID == "" && s.PaymentIntentID == "" && s.InvoiceID == "" {
		var reply StripeRetrievedCheckoutSession
		// A lost acknowledgement intentionally retains the fence. A retry GET can
		// observe verified expiration and finish the same local transaction.
		if err = client.doForm(ctx, http.MethodPost, "/v1/checkout/sessions/"+url.PathEscape(s.ID)+"/expire", url.Values{}, &reply, "expire-starter:"+s.ID); err != nil {
			return "", "", err
		}
		s, err = client.RetrieveCheckoutSession(ctx, s.ID)
		if err != nil {
			return "", "", err
		}
		if !pendingStarterIdentity(s, p, actor) {
			return "", "", errors.New("Starter expiration identity differs")
		}
	}
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	q := h.DB.WithTx(tx)
	if err = lease.lock(ctx, q); err != nil {
		return "", "", err
	}
	if _, err = q.LockTeamBillingAccount(ctx, p.Account.TeamID); err != nil {
		return "", "", err
	}
	current, err := q.GetTeamBillingCheckoutForRecovery(ctx, p.Account.TeamID)
	if err != nil {
		return "", "", err
	}
	// Expiration webhooks can retire this fence while provider I/O is in flight.
	// Only an otherwise empty account and this verified unpaid expiration may
	// converge; a newer generation or established subscription remains protected.
	retired := !current.CheckoutMayExist && len(current.CheckoutPendingAttemptIds) == 0 && !current.StripeCheckoutActorClaimedAt.Valid && !current.CheckoutInitializingAt.Valid && current.CheckoutSessionID == nil && current.CheckoutRequestKey == nil && !current.StripeCheckoutActorID.Valid && !current.CheckoutCompletedAt.Valid && current.CheckoutSubscriptionID == nil && derefString(current.StripeCustomerID) == s.CustomerID && pendingStarterExpiredUnpaid(s) && !billingAccountHasEstablishedSubscription(db.GetTeamBillingAccountRow{StripeSubscriptionID: current.StripeSubscriptionID, StripeSubscriptionStatus: current.StripeSubscriptionStatus})
	if !retired && (!current.CheckoutInitializingAt.Valid || !current.CheckoutInitializingAt.Time.Equal(p.Account.CheckoutInitializingAt.Time) || derefString(current.CheckoutRequestKey) != derefString(p.Account.CheckoutRequestKey) || !current.StripeCheckoutActorID.Valid || uuid.UUID(current.StripeCheckoutActorID.Bytes) != actor) {
		return "", "", errors.New("Starter checkout generation changed")
	}
	locked := p
	locked.Account = current
	if !retired && !pendingStarterIdentity(s, locked, actor) {
		return "", "", errors.New("Starter checkout tuple changed")
	}
	var payment, credit string
	err = tx.QueryRow(ctx, `SELECT payment_state,credit_state FROM billing_prepaid_purchase WHERE id=$1 AND team_id=$2 AND actor_id=$3 AND customer_id=$4 AND (checkout_session_id=$5 OR checkout_session_id IS NULL) AND source='checkout' FOR UPDATE`, p.Purchase, current.TeamID, actor, s.CustomerID, s.ID).Scan(&payment, &credit)
	if err != nil {
		return "", "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE billing_prepaid_purchase SET checkout_session_id=$2 WHERE id=$1 AND checkout_session_id IS NULL`, p.Purchase, s.ID); err != nil {
		return "", "", err
	}
	if pendingStarterExpiredUnpaid(s) && payment != "succeeded" && credit != "issued" && !current.CheckoutCompletedAt.Valid && current.CheckoutSubscriptionID == nil && !billingAccountHasEstablishedSubscription(db.GetTeamBillingAccountRow{StripeSubscriptionID: current.StripeSubscriptionID, StripeSubscriptionStatus: current.StripeSubscriptionStatus}) {
		if err = q.RecordStripeCheckoutExpiration(ctx, db.RecordStripeCheckoutExpirationParams{TeamID: current.TeamID, StripeCustomerID: s.CustomerID, CheckoutGeneration: p.Account.CheckoutInitializingAt.Time, CheckoutSessionID: s.ID}); err != nil {
			return "", "", err
		}
		if _, err = tx.Exec(ctx, `UPDATE billing_prepaid_purchase SET payment_state='failed',failure_code='checkout_expired',failure_reason='The unpaid checkout expired',payment_evidence=COALESCE(payment_evidence,'{}'::jsonb)||jsonb_build_object('customer_expiration',$2::jsonb) WHERE id=$1 AND payment_state IN ('pending','uncertain','processing')`, p.Purchase, mustStarterEvidence(s)); err != nil {
			return "", "", err
		}
		if !retired {
			if err = q.AbortTeamBillingCheckout(ctx, db.AbortTeamBillingCheckoutParams{TeamID: current.TeamID, LeaseStartedAt: current.CheckoutInitializingAt}); err != nil {
				return "", "", err
			}
		}
		if err = tx.Commit(ctx); err != nil {
			return "", "", err
		}
		return "retryable", "", nil
	}
	if err = tx.Commit(ctx); err != nil {
		return "", "", err
	}
	if s.Status == "complete" || s.PaymentStatus == "paid" || payment == "succeeded" || credit == "issued" {
		// Ordinary settlement owns payment/credit reconciliation. Never clear a
		// completed purchase to make room for a different commercial offer.
		if err = h.settleCheckoutPurchase(ctx, client, p.Purchase); err != nil {
			return "", "", err
		}
		return "payment_confirmation_pending", "", nil
	}
	if !cancel && s.Status == "open" && s.PaymentStatus == "unpaid" && !current.CheckoutCompletedAt.Valid && current.CheckoutSubscriptionID == nil && !billingAccountHasEstablishedSubscription(db.GetTeamBillingAccountRow{StripeSubscriptionID: current.StripeSubscriptionID, StripeSubscriptionStatus: current.StripeSubscriptionStatus}) && payment != "failed" {
		u, e := url.Parse(s.URL)
		if e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && !strings.ContainsAny(s.URL, "\r\n") {
			return "recovered", s.URL, nil
		}
	}
	return "payment_confirmation_pending", "", nil
}

func mustStarterEvidence(s StripeRetrievedCheckoutSession) []byte { b, _ := json.Marshal(s); return b }

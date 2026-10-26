package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var errPrepaidOperationConflict = errors.New("checkout operation is bound to another purchase request")

type prepaidOperationResponse struct {
	ID         string    `json:"id,omitempty"`
	URL        string    `json:"url,omitempty"`
	PurchaseID uuid.UUID `json:"purchase_id"`
	State      string    `json:"state"`
}

func (h *Handlers) bindPrepaidCheckoutOperation(ctx context.Context, team, actor uuid.UUID, req prepaidCheckoutBody) error {
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO billing_prepaid_checkout_operation(id,team_id,actor_id,amount_cents,success_url,cancel_url) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO NOTHING`, req.OperationID, team, actor, req.AmountCents, req.SuccessURL, req.CancelURL)
	if err != nil {
		return err
	}
	var matches bool
	if err = tx.QueryRow(ctx, `SELECT team_id=$2 AND actor_id=$3 AND amount_cents=$4 AND success_url=$5 AND cancel_url=$6 FROM billing_prepaid_checkout_operation WHERE id=$1`, req.OperationID, team, actor, req.AmountCents, req.SuccessURL, req.CancelURL).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return errPrepaidOperationConflict
	}
	return tx.Commit(ctx)
}

// Replays precede new-entry checks: a changed plan or rollout switch must not
// turn a lost response for an accepted purchase into another payment.
func (h *Handlers) resumePrepaidCheckoutOperation(c *gin.Context, client *stripeHTTPClient, team, actor uuid.UUID, req prepaidCheckoutBody) (bool, error) {
	ctx := c.Request.Context()
	var customer, payment, credit string
	var session *string
	var raw []byte
	err := h.Pool.QueryRow(ctx, `SELECT customer_id,payment_state,credit_state,checkout_session_id,payment_request FROM billing_prepaid_purchase WHERE id=$1 AND team_id=$2 AND actor_id=$3 AND amount_cents=$4 AND source='checkout'`, req.OperationID, team, actor, req.AmountCents).Scan(&customer, &payment, &credit, &session, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	response := prepaidOperationResponse{PurchaseID: req.OperationID, State: "pending"}
	terminal := func() bool {
		if payment == "failed" {
			response.State = "failed"
		} else if credit == "issued" {
			response.State = "credited"
		} else if payment == "succeeded" {
			response.State = "paid"
		} else {
			return false
		}
		c.JSON(http.StatusOK, response)
		return true
	}
	if terminal() {
		return true, nil
	}
	if session == nil {
		if len(raw) > 0 {
			err = h.settleCheckoutPurchase(ctx, client, req.OperationID)
			if err != nil && !errors.Is(err, errPrepaidPaymentPending) {
				return true, err
			}
		} else {
			found, findErr := client.findPrepaidCheckout(ctx, customer, req.OperationID)
			if findErr != nil {
				return true, findErr
			}
			if found != "" {
				if _, err = h.Pool.Exec(ctx, `UPDATE billing_prepaid_purchase SET checkout_session_id=$2,payment_state='processing' WHERE id=$1 AND checkout_session_id IS NULL AND payment_state='uncertain'`, req.OperationID, found); err != nil {
					return true, err
				}
			} else {
				account, accountErr := h.DB.GetTeamBillingAccount(ctx, team)
				if accountErr != nil {
					return true, accountErr
				}
				// The original subscription request is protected by its existing lease.
				if !billingAccountHasEstablishedSubscription(account) {
					return false, nil
				}
			}
		}
		if err = h.Pool.QueryRow(ctx, `SELECT payment_state,credit_state,checkout_session_id FROM billing_prepaid_purchase WHERE id=$1`, req.OperationID).Scan(&payment, &credit, &session); err != nil {
			return true, err
		}
		if terminal() {
			return true, nil
		}
	}
	if session == nil {
		c.JSON(http.StatusAccepted, response)
		return true, nil
	}
	saved, err := client.retrievePrepaidCheckout(ctx, *session)
	if err != nil {
		return true, err
	}
	if saved.Customer != customer || saved.Metadata["prepaid_purchase_id"] != req.OperationID.String() {
		return true, errors.New("checkout session differs from the operation")
	}
	if saved.Status == "expired" {
		if err = h.failPrepaidPayment(ctx, req.OperationID, "", "the checkout was not completed", false); err != nil {
			return true, err
		}
		if err = h.Pool.QueryRow(ctx, `SELECT payment_state,credit_state FROM billing_prepaid_purchase WHERE id=$1`, req.OperationID).Scan(&payment, &credit); err != nil {
			return true, err
		}
		if terminal() {
			return true, nil
		}
		c.JSON(http.StatusAccepted, response)
		return true, nil
	}
	if saved.Status == "open" {
		response.ID, response.URL = saved.ID, saved.URL
		c.JSON(http.StatusOK, response)
	} else {
		c.JSON(http.StatusAccepted, response)
	}
	return true, nil
}

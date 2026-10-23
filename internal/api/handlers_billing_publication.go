package api

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/superserve-ai/sandbox/internal/db"
)

func isCheckoutPublicationConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// checkoutSubmitMessageEnabled decides only whether a new generation begins
// with the submit message; replays follow what the generation recorded. The
// message promises the signup trial credit, so it needs that grant untouched.
func (h *Handlers) checkoutSubmitMessageEnabled(ctx context.Context, team uuid.UUID) (bool, error) {
	if h.Config == nil || !h.Config.StripeCheckoutSubmitMessage {
		return false, nil
	}
	return h.DB.TeamSignupTrialCreditUnspent(ctx, team)
}

func (h *Handlers) beginCheckoutWithPublicationDecision(ctx context.Context, team, actor uuid.UUID, req billingCheckoutSessionRequest, requestKey string, attempt uuid.UUID, submitMessage bool) (db.TeamBillingAccount, error) {
	var account db.TeamBillingAccount
	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return account, err
	}
	defer tx.Rollback(ctx)
	q := h.DB.WithTx(tx)
	if err := q.BeginStripeCheckoutWithPublicationDecision(ctx, db.BeginStripeCheckoutWithPublicationDecisionParams{
		TeamID: team, UserID: actor, OperationID: req.OperationID, HomeRegion: req.HomeRegion,
		RequestKey: requestKey, Decision: req.Decision, AttemptID: attempt, SubmitMessage: submitMessage,
	}); err != nil {
		return account, err
	}
	account, err = q.GetTeamBillingCheckoutForRecovery(ctx, team)
	if err != nil {
		return account, err
	}
	return account, tx.Commit(ctx)
}

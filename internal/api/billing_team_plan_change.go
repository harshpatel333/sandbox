package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/rs/zerolog/log"
	"github.com/superserve-ai/sandbox/internal/db"
)

// A downgrade can be undone until shortly before it takes effect, so an undo
// that is still talking to Stripe can never race the change applying.
const planChangeUndoCutoff = 10 * time.Minute

// A downgrade that would end the current period within this window waits for
// the next one, leaving time to tell Stripe not to renew.
const planChangeMinNotice = time.Hour

var errTeamPlanChangeConflict = errors.New("plan change conflicts with the team's billing state")

type teamPlanChangeConflictError struct{ reason string }

func (e teamPlanChangeConflictError) Error() string { return e.reason }
func (e teamPlanChangeConflictError) Unwrap() error { return errTeamPlanChangeConflict }

func teamPlanChangeConflict(reason string) error { return teamPlanChangeConflictError{reason} }

type teamPlanChange struct {
	ID               uuid.UUID       `json:"id"`
	Team             uuid.UUID       `json:"team_id"`
	FromAssignment   uuid.UUID       `json:"from_assignment_id"`
	TargetPlanKey    string          `json:"target_plan_key"`
	TargetKind       string          `json:"target_kind"`
	TargetTerms      frozenPlanTerms `json:"target_terms"`
	TargetLimits     planLimits      `json:"target_limits"`
	EffectiveAt      time.Time       `json:"effective_at"`
	PausesRunning    bool            `json:"pauses_running"`
	Subscription     *string         `json:"subscription_id"`
	RequestedBy      uuid.UUID       `json:"requested_by"`
	RequestedVia     string          `json:"requested_via"`
	Reason           string          `json:"reason"`
	Key              string          `json:"idempotency_key"`
	RequestedAt      time.Time       `json:"requested_at"`
	State            string          `json:"state"`
	ProviderCancelAt *time.Time      `json:"provider_cancel_at"`
	UndoneAt         *time.Time      `json:"undone_at"`
	UndoneBy         *uuid.UUID      `json:"undone_by"`
	AppliedAt        *time.Time      `json:"applied_at"`
	AppliedPlan      *uuid.UUID      `json:"applied_assignment_id"`
	CompletedAt      *time.Time      `json:"completed_at"`
	Failures         int             `json:"-"`
	LastError        *string         `json:"last_error"`
	lease            uuid.UUID
}

// Undoable reports whether the change can still be undone.
func (c teamPlanChange) Undoable(now time.Time) bool {
	return c.State == "scheduled" && now.Before(c.EffectiveAt.Add(-planChangeUndoCutoff))
}

const teamPlanChangeColumns = `id,team_id,from_assignment_id,target_plan_key,target_kind,target_terms,target_limits,effective_at,pauses_running,subscription_id,
requested_by,requested_via,reason,idempotency_key,requested_at,state,provider_cancel_at,undone_at,undone_by,applied_at,applied_assignment_id,completed_at,failures,last_error`

func scanTeamPlanChange(row pgx.Row) (teamPlanChange, error) {
	var c teamPlanChange
	var terms, limits []byte
	err := row.Scan(&c.ID, &c.Team, &c.FromAssignment, &c.TargetPlanKey, &c.TargetKind, &terms, &limits, &c.EffectiveAt, &c.PausesRunning, &c.Subscription,
		&c.RequestedBy, &c.RequestedVia, &c.Reason, &c.Key, &c.RequestedAt, &c.State, &c.ProviderCancelAt, &c.UndoneAt, &c.UndoneBy, &c.AppliedAt, &c.AppliedPlan,
		&c.CompletedAt, &c.Failures, &c.LastError)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(terms, &c.TargetTerms); err != nil {
		return c, err
	}
	return c, json.Unmarshal(limits, &c.TargetLimits)
}

func pendingTeamPlanChange(ctx context.Context, q db.DBTX, team uuid.UUID) (teamPlanChange, bool, error) {
	c, err := scanTeamPlanChange(q.QueryRow(ctx, `SELECT `+teamPlanChangeColumns+` FROM team_plan_change WHERE team_id=$1 AND state='scheduled'`, team))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// teamPlanChangeRequest asks to move a team to a self-serve plan.
type teamPlanChangeRequest struct {
	Team   uuid.UUID
	Target string
	Key    string
	Actor  uuid.UUID
	Via    string
	Reason string
}

// scheduleTeamPlanChange records a downgrade or a move to Free. Moves that
// apply now are applied by the worker straight away; the rest wait for the
// end of the billing period and can be undone until shortly before.
func (h *Handlers) scheduleTeamPlanChange(ctx context.Context, req teamPlanChangeRequest) (teamPlanChange, error) {
	if planKindOfOffer(req.Target) == "" {
		return teamPlanChange{}, errUnknownPlan
	}
	on, err := h.pricingV2Active(ctx)
	if err != nil {
		return teamPlanChange{}, err
	}
	if !on {
		return teamPlanChange{}, teamPlanChangeConflict("plan changes are not available yet")
	}
	terms, err := h.planTerms(ctx, req.Target)
	if err != nil {
		return teamPlanChange{}, err
	}
	return h.scheduleTeamPlanChangeTo(ctx, req, terms)
}

func (h *Handlers) scheduleTeamPlanChangeTo(ctx context.Context, req teamPlanChangeRequest, terms planTerms) (teamPlanChange, error) {
	if prior, err := scanTeamPlanChange(h.Pool.QueryRow(ctx, `SELECT `+teamPlanChangeColumns+` FROM team_plan_change WHERE team_id=$1 AND idempotency_key=$2`, req.Team, req.Key)); err == nil {
		if prior.TargetPlanKey != req.Target {
			return prior, teamPlanChangeConflict("idempotency key was used for a different plan change")
		}
		return prior, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return teamPlanChange{}, err
	}
	kind := planKindOfOffer(req.Target)
	if terms.Retired {
		return teamPlanChange{}, teamPlanChangeConflict("this plan is no longer offered")
	}
	if terms.Limits.MaxSandboxes == nil {
		return teamPlanChange{}, errors.New("target plan has no limits")
	}
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return teamPlanChange{}, err
	}
	defer tx.Rollback(ctx)
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp() FROM team WHERE id=$1 FOR NO KEY UPDATE`, req.Team).Scan(&now); err != nil {
		return teamPlanChange{}, err
	}
	current, assigned, err := currentTeamPlan(ctx, tx, req.Team)
	if err != nil {
		return teamPlanChange{}, err
	}
	if !assigned {
		return teamPlanChange{}, teamPlanChangeConflict("plan changes are not available for this team yet")
	}
	if _, pending, err := pendingTeamPlanChange(ctx, tx, req.Team); err != nil || pending {
		if err == nil {
			err = teamPlanChangeConflict("the team already has a scheduled plan change; undo it first")
		}
		return teamPlanChange{}, err
	}
	timing, blocked := planChangeRule(current.Kind, kind)
	if blocked != "" {
		return teamPlanChange{}, teamPlanChangeConflict(blocked)
	}
	if timing == planChangeAfterPurchase {
		return teamPlanChange{}, teamPlanChangeConflict("upgrades take effect when the plan is purchased")
	}
	var account struct {
		Anchor       pgtype.Timestamptz
		Subscription *string
	}
	err = tx.QueryRow(ctx, `SELECT commercial_billing_anchor,stripe_subscription_id FROM team_billing_account WHERE team_id=$1`, req.Team).Scan(&account.Anchor, &account.Subscription)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return teamPlanChange{}, err
	}
	effective := now
	var subscription *string
	if timing == planChangeEndOfPeriod {
		if effective, err = h.planChangePeriodEnd(ctx, req.Team, account.Anchor, now); err != nil {
			return teamPlanChange{}, err
		}
		if current.Kind == planKindSubscription {
			if account.Subscription == nil {
				return teamPlanChange{}, teamPlanChangeConflict("the team's subscription could not be found")
			}
			subscription = account.Subscription
		}
	}
	frozen, limits, _, err := planAssignment{Terms: terms}.frozen()
	if err != nil {
		return teamPlanChange{}, err
	}
	c, err := scanTeamPlanChange(tx.QueryRow(ctx, `INSERT INTO team_plan_change(team_id,from_assignment_id,target_plan_key,target_kind,target_terms,target_limits,effective_at,pauses_running,
        subscription_id,requested_by,requested_via,reason,idempotency_key,next_check_at)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,CASE WHEN $9::text IS NULL THEN $7 ELSE clock_timestamp() END) RETURNING `+teamPlanChangeColumns,
		req.Team, current.ID, req.Target, kind, frozen, limits, effective, kind == planKindFree, subscription, req.Actor, req.Via, req.Reason, req.Key))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return teamPlanChange{}, teamPlanChangeConflict("the team already has a scheduled plan change; undo it first")
	}
	if err != nil {
		return teamPlanChange{}, err
	}
	return c, tx.Commit(ctx)
}

// planChangePeriodEnd is the end of the team's billing period, or of the next
// one when the current period ends too soon to stop a renewal.
func (h *Handlers) planChangePeriodEnd(ctx context.Context, team uuid.UUID, anchor pgtype.Timestamptz, now time.Time) (time.Time, error) {
	_, end, err := h.currentBillingPeriod(ctx, team, anchor, now)
	if err != nil || end.Sub(now) >= planChangeMinNotice {
		return end, err
	}
	_, end, err = h.currentBillingPeriod(ctx, team, anchor, end.Add(time.Second))
	return end, err
}

// undoTeamPlanChange withdraws a scheduled change. A Pro subscription told not
// to renew is told to renew again first, so the team keeps its plan.
func (h *Handlers) undoTeamPlanChange(ctx context.Context, team, id, actor uuid.UUID) (teamPlanChange, error) {
	c, err := scanTeamPlanChange(h.Pool.QueryRow(ctx, `SELECT `+teamPlanChangeColumns+` FROM team_plan_change WHERE id=$1 AND team_id=$2`, id, team))
	if err != nil {
		return c, err
	}
	if c.State == "undone" {
		return c, nil
	}
	if !c.Undoable(time.Now()) {
		return c, teamPlanChangeConflict("this plan change can no longer be undone")
	}
	if c.ProviderCancelAt != nil {
		if err = h.setSubscriptionRenewal(ctx, *c.Subscription, true, "plan-change-undo:"+c.ID.String()); err != nil {
			return c, err
		}
	}
	tag, err := h.Pool.Exec(ctx, `UPDATE team_plan_change SET state='undone',undone_at=clock_timestamp(),undone_by=$2,next_check_at='infinity'
        WHERE id=$1 AND state='scheduled' AND effective_at>clock_timestamp()+$3::interval`, c.ID, actor, planChangeUndoCutoff.String())
	if err != nil {
		return c, err
	}
	if tag.RowsAffected() == 0 {
		return c, teamPlanChangeConflict("this plan change can no longer be undone")
	}
	return scanTeamPlanChange(h.Pool.QueryRow(ctx, `SELECT `+teamPlanChangeColumns+` FROM team_plan_change WHERE id=$1`, c.ID))
}

// setSubscriptionRenewal tells Stripe whether a subscription renews at the end
// of its current period, and checks that it took.
func (h *Handlers) setSubscriptionRenewal(ctx context.Context, subscription string, renew bool, key string) error {
	client, ok := h.Stripe.(*stripeHTTPClient)
	if !ok {
		return errors.New("plan changes require the Stripe provider")
	}
	var sub invoiceSubscription
	form := url.Values{"cancel_at_period_end": {fmt.Sprint(!renew)}, "proration_behavior": {"none"}}
	if err := client.doForm(ctx, http.MethodPost, "/v1/subscriptions/"+url.PathEscape(subscription), form, &sub, key); err != nil {
		return err
	}
	if sub.ID != subscription || sub.CancelAtPeriodEnd == renew || sub.CancelAt != 0 && renew {
		return errors.New("subscription renewal did not change as requested")
	}
	return nil
}

func subscriptionPeriodEnd(sub invoiceSubscription) time.Time {
	var end int64
	for _, item := range sub.Items.Data {
		end = max(end, item.End)
	}
	return time.Unix(end, 0).UTC()
}

const teamPlanChangeReviewFailures = 6

func teamPlanChangeRetryDelay(failures int) time.Duration {
	return time.Duration(math.Min(float64(time.Hour), float64(30*time.Second)*math.Pow(2, float64(min(failures, 10)))))
}

// teamPlanChangeTick leases one due change for longer than a job may run;
// every state write is fenced by the lease token.
// Nothing advances while pricing v2 is off; undo still works, so turning it
// off never strands a team.
func (h *Handlers) teamPlanChangeTick(ctx context.Context) (bool, error) {
	if on, err := h.pricingV2Active(ctx); err != nil || !on {
		return false, err
	}
	return h.runDueTeamPlanChange(ctx, uuid.Nil)
}

// runDueTeamPlanChange advances one due change, of the given team unless team
// is uuid.Nil.
func (h *Handlers) runDueTeamPlanChange(ctx context.Context, team uuid.UUID) (bool, error) {
	token := uuid.New()
	c, err := scanTeamPlanChange(h.Pool.QueryRow(ctx, `WITH due AS (SELECT id AS due_id FROM team_plan_change WHERE next_check_at<=clock_timestamp()
        AND state IN ('scheduled','applied') AND (lease_until IS NULL OR lease_until<clock_timestamp()) AND ($2::uuid=$3::uuid OR team_id=$2::uuid)
        ORDER BY next_check_at,id FOR UPDATE SKIP LOCKED LIMIT 1)
        UPDATE team_plan_change p SET lease_token=$1,lease_until=clock_timestamp()+interval '3 minutes' FROM due WHERE p.id=due.due_id
        RETURNING `+teamPlanChangeColumns, token, team, uuid.Nil))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	c.lease = token
	jobCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	next, err := h.advanceTeamPlanChange(jobCtx, c)
	cancel()
	msg := ""
	if err != nil {
		msg = err.Error()
		next = time.Now().Add(teamPlanChangeRetryDelay(c.Failures))
		event := log.Warn()
		if c.Failures+1 >= teamPlanChangeReviewFailures {
			event = log.Error()
		}
		event.Err(err).Str("plan_change_id", c.ID.String()).Str("team_id", c.Team.String()).Int("failures", c.Failures+1).Msg("plan change pending")
	}
	ack, cancelAck := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelAck()
	_, err = h.Pool.Exec(ack, `UPDATE team_plan_change SET lease_token=NULL,lease_until=NULL,
        next_check_at=CASE WHEN state IN ('completed','undone') THEN 'infinity'::timestamptz ELSE $3::timestamptz END,
        failures=CASE WHEN $4='' THEN 0 ELSE failures+1 END,last_error=NULLIF($4,'') WHERE id=$1 AND lease_token=$2`, c.ID, token, next, msg)
	return true, err
}

// advanceTeamPlanChange moves one change forward and returns when to look
// again: stop the subscription renewing, apply the plan when it takes effect,
// then pause the team's running sandboxes if the new plan is Free.
func (h *Handlers) advanceTeamPlanChange(ctx context.Context, c teamPlanChange) (time.Time, error) {
	now := time.Now()
	switch {
	case c.State == "scheduled" && c.Subscription != nil && c.ProviderCancelAt == nil:
		return h.stopPlanChangeRenewal(ctx, c, now)
	case c.State == "scheduled" && now.Before(c.EffectiveAt):
		return c.EffectiveAt, nil
	case c.State == "scheduled":
		return h.applyTeamPlanChange(ctx, c)
	case c.State == "applied":
		if err := h.pauseTeamForPlan(ctx, c.Team); err != nil {
			return now, err
		}
		_, err := h.Pool.Exec(ctx, `UPDATE team_plan_change SET state='completed',completed_at=clock_timestamp() WHERE id=$1 AND lease_token=$2 AND state='applied'`, c.ID, c.lease)
		return now, err
	}
	return now, nil
}

// stopPlanChangeRenewal tells Stripe not to renew the Pro subscription at the
// period end the change takes effect. If that period has already renewed, the
// change waits for the next period end instead of cutting a paid month short.
func (h *Handlers) stopPlanChangeRenewal(ctx context.Context, c teamPlanChange, now time.Time) (time.Time, error) {
	client, ok := h.Stripe.(*stripeHTTPClient)
	if !ok {
		return now, errors.New("plan changes require the Stripe provider")
	}
	sub, err := client.invoiceSubscription(ctx, *c.Subscription)
	if err != nil {
		return now, err
	}
	if sub.Status != "active" && sub.Status != "past_due" {
		return now, fmt.Errorf("subscription is %s", sub.Status)
	}
	end := subscriptionPeriodEnd(sub)
	if end.After(c.EffectiveAt) {
		_, err = h.Pool.Exec(ctx, `UPDATE team_plan_change SET effective_at=$3 WHERE id=$1 AND lease_token=$2 AND state='scheduled' AND provider_cancel_at IS NULL`, c.ID, c.lease, end)
		if err != nil {
			return now, err
		}
		log.Error().Str("plan_change_id", c.ID.String()).Str("team_id", c.Team.String()).Time("effective_at", end).Msg("plan change deferred to the next period end")
		c.EffectiveAt = end
	}
	if !end.Equal(c.EffectiveAt) {
		return now, errors.New("subscription period does not end when the plan change takes effect")
	}
	if !sub.CancelAtPeriodEnd {
		if err = h.setSubscriptionRenewal(ctx, *c.Subscription, false, "plan-change-cancel:"+c.ID.String()); err != nil {
			return now, err
		}
	}
	tag, err := h.Pool.Exec(ctx, `UPDATE team_plan_change SET provider_cancel_at=clock_timestamp() WHERE id=$1 AND lease_token=$2 AND state='scheduled' AND provider_cancel_at IS NULL`, c.ID, c.lease)
	if err == nil && tag.RowsAffected() == 0 {
		err = errors.New("plan change moved on while stopping the renewal")
	}
	return c.EffectiveAt, err
}

// applyTeamPlanChange moves the team onto the target plan. A change whose team
// has moved plans since it was requested is withdrawn instead.
func (h *Handlers) applyTeamPlanChange(ctx context.Context, c teamPlanChange) (time.Time, error) {
	now := time.Now()
	if c.Subscription != nil && c.ProviderCancelAt == nil {
		return now, errors.New("subscription renewal was not stopped")
	}
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return now, err
	}
	defer tx.Rollback(ctx)
	if err = tx.QueryRow(ctx, `SELECT 1 FROM team_plan_change WHERE id=$1 AND lease_token=$2 AND state='scheduled' FOR UPDATE`, c.ID, c.lease).Scan(new(int)); err != nil {
		return now, err
	}
	current, assigned, err := currentTeamPlan(ctx, tx, c.Team)
	if err != nil {
		return now, err
	}
	if !assigned || current.ID != c.FromAssignment {
		if _, err = tx.Exec(ctx, `UPDATE team_plan_change SET state='undone',undone_at=clock_timestamp() WHERE id=$1`, c.ID); err != nil {
			return now, err
		}
		log.Warn().Str("plan_change_id", c.ID.String()).Str("team_id", c.Team.String()).Msg("plan change withdrawn: the team changed plans first")
		return now, tx.Commit(ctx)
	}
	terms := planTerms{OfferKey: c.TargetPlanKey, DisplayName: c.TargetTerms.DisplayName, FeeCents: c.TargetTerms.FeeCents, IntervalMonths: c.TargetTerms.IntervalMonths,
		MonthlyCreditCents: c.TargetTerms.MonthlyCreditCents, VersionID: c.TargetTerms.VersionID, Version: c.TargetTerms.Version, Family: c.TargetTerms.Family,
		Grants: c.TargetTerms.Grants, Matching: c.TargetTerms.Matching, Limits: c.TargetLimits}
	actor := c.RequestedBy
	plan, err := assignTeamPlan(ctx, tx, planAssignment{Team: c.Team, Terms: terms, Kind: c.TargetKind, Source: planSourcePlanChange, Actor: &actor,
		Reason: c.Reason, Key: "plan-change:" + c.ID.String()})
	if err != nil {
		return now, err
	}
	state := "applied"
	if !c.PausesRunning {
		state = "completed"
	}
	if _, err = tx.Exec(ctx, `UPDATE team_plan_change SET state='applied',applied_at=clock_timestamp(),applied_assignment_id=$2 WHERE id=$1`, c.ID, plan.ID); err != nil {
		return now, err
	}
	if state == "completed" {
		if _, err = tx.Exec(ctx, `UPDATE team_plan_change SET state='completed',completed_at=clock_timestamp() WHERE id=$1`, c.ID); err != nil {
			return now, err
		}
	}
	return now, tx.Commit(ctx)
}

// pauseTeamForPlan pauses every running sandbox of a team that has moved to
// Free. It is provided by the Free plan's allowance enforcement.
func (h *Handlers) pauseTeamForPlan(ctx context.Context, team uuid.UUID) error {
	return errors.New("pausing a team for a plan change is not available yet")
}

type teamPlanChangeBody struct {
	TargetPlan string `json:"target_plan"`
	Key        string `json:"idempotency_key"`
	Reason     string `json:"reason"`
}

func decodeTeamPlanChange(c *gin.Context) (teamPlanChangeBody, bool) {
	var body teamPlanChangeBody
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		respondErrorMsg(c, "bad_request", "target_plan and idempotency_key are required", http.StatusBadRequest)
		return body, false
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if planKindOfOffer(body.TargetPlan) == "" {
		respondErrorMsg(c, "invalid_plan", "target_plan must be free, starter or pro_monthly", http.StatusBadRequest)
		return body, false
	}
	if strings.TrimSpace(body.Key) == "" || len(body.Key) > 200 || len(body.Reason) > 2000 {
		respondErrorMsg(c, "bad_request", "idempotency_key (up to 200 characters) is required and reason is at most 2000 characters", http.StatusBadRequest)
		return body, false
	}
	return body, true
}

func (h *Handlers) respondTeamPlanChange(c *gin.Context, team uuid.UUID, change teamPlanChange, err error) {
	var conflict teamPlanChangeConflictError
	switch {
	case errors.As(err, &conflict):
		respondErrorMsg(c, "conflict", conflict.reason, http.StatusConflict)
	case errors.Is(err, errUnknownPlan):
		respondErrorMsg(c, "invalid_plan", "this plan is not available", http.StatusBadRequest)
	case errors.Is(err, pgx.ErrNoRows):
		respondErrorMsg(c, "not_found", "plan change not found", http.StatusNotFound)
	case err != nil:
		log.Error().Err(err).Str("team_id", team.String()).Msg("plan change failed")
		respondErrorMsg(c, "unavailable", "the plan change could not be completed; retry with the same request", http.StatusServiceUnavailable)
	default:
		c.JSON(http.StatusOK, teamPlanChangeJSON(change, time.Now()))
	}
}

func teamPlanChangeJSON(c teamPlanChange, now time.Time) gin.H {
	return gin.H{"id": c.ID, "target_plan_key": c.TargetPlanKey, "target_display_name": c.TargetTerms.DisplayName, "target_kind": c.TargetKind,
		"effective_at": c.EffectiveAt, "pauses_running": c.PausesRunning, "state": c.State, "undoable": c.Undoable(now), "limits": customerLimits(c.TargetLimits)}
}

func customerActor(c *gin.Context) (uuid.UUID, bool) {
	actor := actorIDFromContext(c)
	if actor == nil {
		respondError(c, ErrUnauthorized)
		return uuid.Nil, false
	}
	return *actor, true
}

// RequestBillingPlanChange lets a team downgrade or switch to Free.
func (h *Handlers) RequestBillingPlanChange(c *gin.Context) {
	team, err := customerContextTeamID(c)
	if err != nil || !h.requireCustomerTeamPermission(c, team, "billing:write") {
		return
	}
	actor, ok := customerActor(c)
	if !ok {
		return
	}
	body, ok := decodeTeamPlanChange(c)
	if !ok {
		return
	}
	reason := body.Reason
	if reason == "" {
		reason = "requested by the team"
	}
	change, err := h.scheduleTeamPlanChange(c.Request.Context(), teamPlanChangeRequest{Team: team, Target: body.TargetPlan, Key: body.Key, Actor: actor, Via: "customer", Reason: reason})
	h.respondTeamPlanChange(c, team, change, err)
}

// UndoBillingPlanChange withdraws the team's scheduled plan change.
func (h *Handlers) UndoBillingPlanChange(c *gin.Context) {
	team, err := customerContextTeamID(c)
	if err != nil || !h.requireCustomerTeamPermission(c, team, "billing:write") {
		return
	}
	actor, ok := customerActor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("change_id"))
	if err != nil {
		respondErrorMsg(c, "not_found", "plan change not found", http.StatusNotFound)
		return
	}
	change, err := h.undoTeamPlanChange(c.Request.Context(), team, id, actor)
	h.respondTeamPlanChange(c, team, change, err)
}

// RequestTeamPlanChange lets staff downgrade a team or move it to Free, with a
// recorded reason.
func (h *Handlers) RequestTeamPlanChange(c *gin.Context) {
	team, ok := ycTeam(c)
	if !ok {
		return
	}
	actor, ok := h.requirePlatformBilling(c, platformBillingWritePermission)
	if !ok {
		return
	}
	body, ok := decodeTeamPlanChange(c)
	if !ok {
		return
	}
	if body.Reason == "" {
		respondErrorMsg(c, "bad_request", "reason is required", http.StatusBadRequest)
		return
	}
	change, err := h.scheduleTeamPlanChange(c.Request.Context(), teamPlanChangeRequest{Team: team, Target: body.TargetPlan, Key: body.Key, Actor: actor, Via: "staff", Reason: body.Reason})
	h.respondTeamPlanChange(c, team, change, err)
}

// UndoTeamPlanChange lets staff withdraw a team's scheduled plan change.
func (h *Handlers) UndoTeamPlanChange(c *gin.Context) {
	team, ok := ycTeam(c)
	if !ok {
		return
	}
	actor, ok := h.requirePlatformBilling(c, platformBillingWritePermission)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("change_id"))
	if err != nil {
		respondErrorMsg(c, "not_found", "plan change not found", http.StatusNotFound)
		return
	}
	change, err := h.undoTeamPlanChange(c.Request.Context(), team, id, actor)
	h.respondTeamPlanChange(c, team, change, err)
}

// StartTeamPlanChangeService applies scheduled plan changes as they fall due.
func (h *Handlers) StartTeamPlanChangeService(ctx context.Context) {
	if h.Pool == nil {
		return
	}
	go func() {
		for {
			worked, err := h.teamPlanChangeTick(ctx)
			if err != nil && ctx.Err() == nil {
				log.Warn().Err(err).Msg("plan change tick failed")
			}
			delay := 5 * time.Second
			if worked {
				delay = time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
	}()
}

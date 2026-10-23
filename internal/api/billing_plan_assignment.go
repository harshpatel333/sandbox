package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"github.com/superserve-ai/sandbox/internal/db"
)

// Plan kinds: how a plan is paid for, whatever its name.
const (
	planKindFree         = "free"
	planKindPrepaid      = "prepaid"
	planKindSubscription = "subscription"
	planKindLegacyPAYG   = "legacy_payg"
	planKindCustom       = "custom"
)

// Where an assignment came from.
const (
	planSourceSignup         = "signup"
	planSourceMigration      = "migration"
	planSourceCheckout       = "checkout"
	planSourceCreditPurchase = "credit_purchase"
	planSourcePlanChange     = "plan_change"
	planSourceStaff          = "staff"
)

var (
	errPlanRetired          = errors.New("plan is retired")
	errPlanAssignmentReused = errors.New("idempotency key was used for a different plan assignment")
)

// frozenPlanTerms is the copy of a plan's terms an assignment keeps, so later
// catalog versions never change what a team was given.
type frozenPlanTerms struct {
	OfferKey           string        `json:"offer_key"`
	DisplayName        string        `json:"display_name"`
	FeeCents           int64         `json:"fee_cents"`
	IntervalMonths     int32         `json:"interval_months"`
	MonthlyCreditCents int64         `json:"monthly_credit_cents"`
	VersionID          uuid.UUID     `json:"version_id"`
	Version            int32         `json:"version"`
	Family             string        `json:"family"`
	Grants             []planGrant   `json:"grants"`
	Matching           *planMatching `json:"matching"`
}

func freezePlanTerms(t planTerms) frozenPlanTerms {
	return frozenPlanTerms{OfferKey: t.OfferKey, DisplayName: t.DisplayName, FeeCents: t.FeeCents, IntervalMonths: t.IntervalMonths,
		MonthlyCreditCents: t.MonthlyCreditCents, VersionID: t.VersionID, Version: t.Version, Family: t.Family, Grants: t.Grants, Matching: t.Matching}
}

// planAssignment asks for a team to be on a plan from now on.
type planAssignment struct {
	Team       uuid.UUID
	Terms      planTerms
	Kind       string
	Source     string
	Actor      *uuid.UUID
	Reason     string
	ContractID *uuid.UUID
	Key        string
}

// teamPlan is one stretch of a team's plan history.
type teamPlan struct {
	ID            uuid.UUID       `json:"id"`
	TeamID        uuid.UUID       `json:"team_id"`
	PlanKey       string          `json:"plan_key"`
	Kind          string          `json:"kind"`
	VersionID     *uuid.UUID      `json:"plan_version_id"`
	Terms         frozenPlanTerms `json:"terms"`
	Limits        planLimits      `json:"limits"`
	EffectiveFrom time.Time       `json:"effective_from"`
	EffectiveTo   *time.Time      `json:"effective_to"`
	ContractID    *uuid.UUID      `json:"contract_id"`
	Source        string          `json:"source"`
	AssignedBy    *uuid.UUID      `json:"assigned_by"`
	Reason        string          `json:"reason"`
	Key           *string         `json:"idempotency_key"`
}

const teamPlanColumns = `id,team_id,plan_key,kind,plan_version_id,terms,limits,effective_from,effective_to,contract_id,source,assigned_by,reason,idempotency_key`

func scanTeamPlan(row pgx.Row) (teamPlan, error) {
	var p teamPlan
	var terms, limits []byte
	if err := row.Scan(&p.ID, &p.TeamID, &p.PlanKey, &p.Kind, &p.VersionID, &terms, &limits, &p.EffectiveFrom, &p.EffectiveTo, &p.ContractID, &p.Source, &p.AssignedBy, &p.Reason, &p.Key); err != nil {
		return p, err
	}
	if err := json.Unmarshal(terms, &p.Terms); err != nil {
		return p, err
	}
	return p, json.Unmarshal(limits, &p.Limits)
}

// currentTeamPlan returns the plan a team is on now; ok is false for a team
// that has never been assigned one.
func currentTeamPlan(ctx context.Context, q db.DBTX, team uuid.UUID) (teamPlan, bool, error) {
	p, err := scanTeamPlan(q.QueryRow(ctx, `SELECT `+teamPlanColumns+` FROM team_plan_assignment WHERE team_id=$1 AND effective_to IS NULL`, team))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

func teamPlanHistory(ctx context.Context, q db.DBTX, team uuid.UUID, limit int) ([]teamPlan, error) {
	rows, err := q.Query(ctx, `SELECT `+teamPlanColumns+` FROM team_plan_assignment WHERE team_id=$1 ORDER BY effective_from DESC, created_at DESC LIMIT $2`, team, limit)
	if err != nil {
		return nil, err
	}
	plans, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (teamPlan, error) { return scanTeamPlan(row) })
	if plans == nil {
		plans = []teamPlan{}
	}
	return plans, err
}

func validPlanKind(kind string) bool {
	switch kind {
	case planKindFree, planKindPrepaid, planKindSubscription, planKindLegacyPAYG, planKindCustom:
		return true
	}
	return false
}

// assignTeamPlan moves a team onto a plan within tx: it closes the open
// assignment and opens the new one at the same instant, and the team takes the
// new plan's frozen limits. A repeated idempotency key returns the assignment
// it created.
func assignTeamPlan(ctx context.Context, tx pgx.Tx, a planAssignment) (teamPlan, error) {
	if a.Terms.Retired {
		return teamPlan{}, errPlanRetired
	}
	if !validPlanKind(a.Kind) || a.Terms.OfferKey == "" || a.Terms.Limits.MaxSandboxes == nil || a.Reason == "" {
		return teamPlan{}, errors.New("plan assignment needs a plan, kind, limits and reason")
	}
	var key *string
	if a.Key != "" {
		key = &a.Key
		prior, err := scanTeamPlan(tx.QueryRow(ctx, `SELECT `+teamPlanColumns+` FROM team_plan_assignment WHERE team_id=$1 AND idempotency_key=$2`, a.Team, a.Key))
		if err == nil {
			if prior.PlanKey != a.Terms.OfferKey || prior.Terms.VersionID != a.Terms.VersionID || prior.Kind != a.Kind {
				return teamPlan{}, errPlanAssignmentReused
			}
			return prior, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return teamPlan{}, err
		}
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp() FROM team WHERE id=$1 FOR NO KEY UPDATE`, a.Team).Scan(&now); err != nil {
		return teamPlan{}, err
	}
	terms, err := json.Marshal(freezePlanTerms(a.Terms))
	if err != nil {
		return teamPlan{}, err
	}
	limits, err := json.Marshal(a.Terms.Limits)
	if err != nil {
		return teamPlan{}, err
	}
	var version *uuid.UUID
	if a.Terms.VersionID != uuid.Nil {
		version = &a.Terms.VersionID
	}
	if _, err = tx.Exec(ctx, `UPDATE team_plan_assignment SET effective_to=$2 WHERE team_id=$1 AND effective_to IS NULL`, a.Team, now); err != nil {
		return teamPlan{}, err
	}
	return scanTeamPlan(tx.QueryRow(ctx, `INSERT INTO team_plan_assignment(team_id,plan_key,kind,plan_version_id,terms,limits,effective_from,contract_id,source,assigned_by,reason,idempotency_key)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING `+teamPlanColumns,
		a.Team, a.Terms.OfferKey, a.Kind, version, terms, limits, now, a.ContractID, a.Source, a.Actor, a.Reason, key))
}

// GetTeamPlanAssignments shows staff the plan a team is on and its history.
func (h *Handlers) GetTeamPlanAssignments(c *gin.Context) {
	team, ok := ycTeam(c)
	if !ok {
		return
	}
	if _, ok = h.requirePlatformBilling(c, platformBillingReadPermission); !ok {
		return
	}
	history, err := teamPlanHistory(c.Request.Context(), h.Pool, team, 50)
	if err != nil {
		log.Error().Err(err).Str("team_id", team.String()).Msg("list plan assignments failed")
		respondError(c, ErrInternal)
		return
	}
	var current *teamPlan
	if len(history) > 0 && history[0].EffectiveTo == nil {
		current = &history[0]
	}
	c.JSON(http.StatusOK, gin.H{"current": current, "history": history})
}

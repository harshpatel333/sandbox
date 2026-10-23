package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/sandbox/internal/db"
)

const (
	freeAllowanceSweepName              = "billing-free-allowance"
	freeAllowancePageSize         int32 = 500
	freeAllowanceLeaseSeconds           = 120
	freeAllowanceExhaustedCode          = "free_allowance_exhausted"
	teamPauseReasonPlanSwitchFree       = "plan_switch_free"
)

func (h *Handlers) refreshFreeAllowances(ctx context.Context) {
	afterID, err := h.DB.ClaimSweepLease(ctx, db.ClaimSweepLeaseParams{
		Name: freeAllowanceSweepName, LockedBy: db.SweepHolderID(),
		LeaseSeconds: int32(freeAllowanceLeaseSeconds),
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Error().Err(err).Msg("billing: claim free allowance sweep lease failed")
		}
		return
	}
	for ctx.Err() == nil {
		teams, err := h.DB.ListFreeTeamsWithActiveSandboxes(ctx, db.ListFreeTeamsWithActiveSandboxesParams{AfterTeamID: afterID, BatchLimit: freeAllowancePageSize})
		if err != nil {
			log.Error().Err(err).Msg("billing: list free teams with active sandboxes failed")
			return
		}
		next, advance := processSweepPage(ctx, teams, freeAllowancePageSize, func(teamID uuid.UUID) bool {
			if _, err := h.DB.RefreshFreeAllowance(ctx, teamID); err != nil {
				log.Error().Err(err).Str("team_id", teamID.String()).Msg("billing: refresh free allowance failed")
			}
			return true
		})
		if advance {
			h.advanceSweepCursor(ctx, freeAllowanceSweepName, next)
		}
		if !advance || !next.Valid {
			return
		}
		afterID = next
	}
}

func requestTeamPauseAll(ctx context.Context, q *db.Queries, teamID uuid.UUID, reason string) (int32, error) {
	return q.RequestTeamPauseAll(ctx, db.RequestTeamPauseAllParams{TeamID: teamID, Reason: reason})
}

func respondFreeAllowanceExhausted(c *gin.Context, resetsAt time.Time) {
	c.JSON(http.StatusPaymentRequired, gin.H{
		"error": gin.H{
			"code":      freeAllowanceExhaustedCode,
			"message":   "The Free plan's daily compute allowance is used up. Sandboxes can start again when it resets, or upgrade for unlimited runtime.",
			"resets_at": resetsAt.UTC().Format(time.RFC3339),
		},
	})
}

type freeAllowanceResponse struct {
	Applies         bool     `json:"applies"`
	WindowStartedAt *string  `json:"window_started_at"`
	ResetsAt        *string  `json:"resets_at"`
	AllowanceCents  *float64 `json:"allowance_cents"`
	UsedCents       *float64 `json:"used_cents"`
	RemainingCents  *float64 `json:"remaining_cents"`
	Paused          *bool    `json:"paused"`
	PausedReason    *string  `json:"paused_reason"`
	AsOf            *string  `json:"as_of"`
}

func (h *Handlers) GetFreeAllowance(c *gin.Context) {
	teamID, ok := h.requireBillingRead(c)
	if !ok {
		return
	}
	row, err := h.DB.GetFreeAllowance(c.Request.Context(), teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusOK, freeAllowanceResponse{})
		return
	}
	if err != nil {
		log.Error().Err(err).Str("team_id", teamID.String()).Msg("billing: read free allowance failed")
		respondError(c, ErrInternal)
		return
	}
	allowance, err := numericFloat64(row.AllowanceUsd)
	if err != nil {
		log.Error().Err(err).Str("team_id", teamID.String()).Msg("billing: free allowance amount unreadable")
		respondError(c, ErrInternal)
		return
	}
	used, err := numericFloat64(row.UsedUsd)
	if err != nil {
		log.Error().Err(err).Str("team_id", teamID.String()).Msg("billing: free allowance usage unreadable")
		respondError(c, ErrInternal)
		return
	}
	resp := freeAllowanceFromUSD(allowance, used, row.Exhausted, row.AsOf)
	if row.HasWindow {
		started, resets := row.WindowStartedAt.UTC().Format(time.RFC3339), row.ResetsAt.UTC().Format(time.RFC3339)
		resp.WindowStartedAt, resp.ResetsAt = &started, &resets
	}
	c.JSON(http.StatusOK, resp)
}

func freeAllowanceFromUSD(allowanceUSD, usedUSD float64, exhausted bool, asOf time.Time) freeAllowanceResponse {
	allowance := usdCents(allowanceUSD)
	used := math.Min(usdCents(usedUSD), allowance)
	remaining := allowance - used
	asOfText := asOf.UTC().Format(time.RFC3339)
	resp := freeAllowanceResponse{
		Applies:        true,
		AllowanceCents: &allowance,
		UsedCents:      &used,
		RemainingCents: &remaining,
		Paused:         &exhausted,
		AsOf:           &asOfText,
	}
	if exhausted {
		reason := freeAllowanceExhaustedCode
		resp.PausedReason = &reason
	}
	return resp
}

func usdCents(usd float64) float64 {
	return math.Round(usd*10000) / 100
}

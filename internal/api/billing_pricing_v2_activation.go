package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/sandbox/internal/db"
)

type activationSkippedTeam struct {
	TeamID uuid.UUID `json:"team_id"`
	Reason string    `json:"reason"`
}

type activationReport struct {
	Region          string                  `json:"region"`
	AsOf            time.Time               `json:"as_of"`
	ToFree          int                     `json:"to_free"`
	ToLegacyPAYG    int                     `json:"to_legacy_payg"`
	AlreadyAssigned int                     `json:"already_assigned"`
	Skipped         int                     `json:"skipped"`
	SkippedReasons  map[string]int          `json:"skipped_reasons"`
	SkippedTeams    []activationSkippedTeam `json:"skipped_teams"`
	Pending         int                     `json:"pending"`
}

func summarizeActivation(region string, asOf time.Time, rows []db.ListPricingV2ActivationBucketsRow) activationReport {
	r := activationReport{Region: region, AsOf: asOf, SkippedReasons: map[string]int{}, SkippedTeams: []activationSkippedTeam{}}
	for _, row := range rows {
		switch row.Bucket {
		case "free":
			r.ToFree++
		case "legacy_payg":
			r.ToLegacyPAYG++
		case "assigned":
			r.AlreadyAssigned++
		default:
			r.Skipped++
			r.SkippedReasons[row.Reason]++
			r.SkippedTeams = append(r.SkippedTeams, activationSkippedTeam{TeamID: row.TeamID, Reason: row.Reason})
		}
	}
	r.Pending = r.ToFree + r.ToLegacyPAYG
	return r
}

// GetPricingV2ActivationReport is the read-only dry run of activation for
// this region: which teams would move to Free, which stay on legacy PAYG,
// and which need a human decision.
func (h *Handlers) GetPricingV2ActivationReport(c *gin.Context) {
	if _, ok := h.requirePlatformBilling(c, "platform:billing:read"); !ok {
		return
	}
	if h.Config == nil || h.Config.TeamCreationRegion == "" {
		respondError(c, ErrInternal)
		return
	}
	rows, err := h.DB.ListPricingV2ActivationBuckets(c.Request.Context(), db.ListPricingV2ActivationBucketsParams{
		Region: h.Config.TeamCreationRegion, SystemTeamID: h.systemTeamID(),
	})
	if err != nil {
		log.Error().Err(err).Msg("billing: pricing v2 activation report failed")
		respondError(c, ErrInternal)
		return
	}
	c.JSON(http.StatusOK, summarizeActivation(h.Config.TeamCreationRegion, h.nowUTC(), rows))
}

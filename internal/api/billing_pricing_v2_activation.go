package api

import (
	"context"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/sandbox/internal/db"
)

const (
	activationTargetFree       = "free"
	activationTargetLegacyPAYG = "legacy_payg"
	activationCreditReads      = 8
	activationCreditReadCap    = 200
	activationCreditBudget     = 20 * time.Second
)

// activationTeam is one team's activation outcome. Target is empty for teams
// activation leaves alone; Reason says why.
type activationTeam struct {
	TeamID      uuid.UUID
	Bucket      string
	Target      string
	Reason      string
	CreditCents *int64
}

type activationSkippedTeam struct {
	TeamID uuid.UUID `json:"team_id"`
	Reason string    `json:"reason"`
}

type activationLapsedTeam struct {
	TeamID      uuid.UUID `json:"team_id"`
	CreditCents int64     `json:"credit_cents"`
}

type activationReport struct {
	Region           string                  `json:"region"`
	PricingVersion   string                  `json:"pricing_version"`
	AsOf             time.Time               `json:"as_of"`
	ToFree           int                     `json:"to_free"`
	ToLegacyPAYG     int                     `json:"to_legacy_payg"`
	AlreadyAssigned  int                     `json:"already_assigned"`
	Skipped          int                     `json:"skipped"`
	SkippedReasons   map[string]int          `json:"skipped_reasons"`
	SkippedTeams     []activationSkippedTeam `json:"skipped_teams"`
	LapsedWithCredit []activationLapsedTeam  `json:"lapsed_with_credit"`
	LapsedNoCredit   []activationLapsedTeam  `json:"lapsed_no_credit"`
	LapsedUnchecked  []uuid.UUID             `json:"lapsed_unchecked"`
	CreditReads      int                     `json:"credit_reads"`
	ElapsedMS        int64                   `json:"elapsed_ms"`
	Pending          int                     `json:"pending"`
}

// resolveActivationTeams settles each team's target. Teams whose subscription
// has lapsed keep pay-as-you-go while they hold unexpired Stripe credit and
// move to Free once they have none. Credit reads are capped in number and in
// time so one request stays well inside proxy timeouts and Stripe rate limits:
// a lapsed team past the cap is left "unchecked" and a failed or throttled
// read "credit_unknown", both untouched for a later pass rather than guessed.
func (h *Handlers) resolveActivationTeams(ctx context.Context, rows []db.ListPricingV2ActivationBucketsRow) ([]activationTeam, int) {
	teams := make([]activationTeam, len(rows))
	reader, _ := h.Stripe.(stripeCreditBalanceReader)
	ctx, cancel := context.WithTimeout(ctx, activationCreditBudget)
	defer cancel()
	reads := 0
	sem := make(chan struct{}, activationCreditReads)
	var wg sync.WaitGroup
	for i, row := range rows {
		teams[i] = activationTeam{TeamID: row.TeamID, Bucket: row.Bucket, Reason: row.Reason}
		switch row.Bucket {
		case activationTargetFree, activationTargetLegacyPAYG:
			teams[i].Target = row.Bucket
		case "lapsed":
			if row.StripeCustomerID == "" {
				teams[i].Target, teams[i].Reason = activationTargetFree, "lapsed_no_credit"
				teams[i].CreditCents = new(int64)
				continue
			}
			if reader == nil {
				teams[i].Bucket, teams[i].Reason = "skipped", "credit_unknown"
				continue
			}
			if reads >= activationCreditReadCap {
				teams[i].Reason = "lapsed_unchecked"
				continue
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				teams[i].Reason = "lapsed_unchecked"
				continue
			}
			reads++
			wg.Add(1)
			go func(t *activationTeam, customer string) {
				defer wg.Done()
				defer func() { <-sem }()
				balance, err := reader.GetCustomerCreditBalance(ctx, customer)
				if err != nil {
					log.Warn().Err(err).Str("team_id", t.TeamID.String()).Msg("billing: pricing v2 activation credit read failed")
					t.Bucket, t.Reason = "skipped", "credit_unknown"
					return
				}
				cents := int64(math.Round(balance.AvailableUSD * 100))
				t.CreditCents = &cents
				if cents > 0 {
					t.Target, t.Reason = activationTargetLegacyPAYG, "lapsed_with_credit"
				} else {
					t.Target, t.Reason = activationTargetFree, "lapsed_no_credit"
				}
			}(&teams[i], row.StripeCustomerID)
		}
	}
	wg.Wait()
	return teams, reads
}

func summarizeActivation(region string, asOf time.Time, teams []activationTeam) activationReport {
	r := activationReport{Region: region, AsOf: asOf, SkippedReasons: map[string]int{}, SkippedTeams: []activationSkippedTeam{},
		LapsedWithCredit: []activationLapsedTeam{}, LapsedNoCredit: []activationLapsedTeam{}, LapsedUnchecked: []uuid.UUID{}}
	for _, t := range teams {
		switch {
		case t.Reason == "lapsed_unchecked":
			r.LapsedUnchecked = append(r.LapsedUnchecked, t.TeamID)
		case t.Target == activationTargetFree:
			r.ToFree++
		case t.Target == activationTargetLegacyPAYG:
			r.ToLegacyPAYG++
		case t.Bucket == "assigned":
			r.AlreadyAssigned++
		default:
			r.Skipped++
			r.SkippedReasons[t.Reason]++
			r.SkippedTeams = append(r.SkippedTeams, activationSkippedTeam{TeamID: t.TeamID, Reason: t.Reason})
		}
		if t.CreditCents != nil {
			lapsed := activationLapsedTeam{TeamID: t.TeamID, CreditCents: *t.CreditCents}
			if t.Reason == "lapsed_with_credit" {
				r.LapsedWithCredit = append(r.LapsedWithCredit, lapsed)
			} else {
				r.LapsedNoCredit = append(r.LapsedNoCredit, lapsed)
			}
		}
	}
	r.Pending = r.ToFree + r.ToLegacyPAYG + len(r.LapsedUnchecked) + r.SkippedReasons["credit_unknown"]
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
	started := time.Now()
	teams, reads := h.resolveActivationTeams(c.Request.Context(), rows)
	report := summarizeActivation(h.Config.TeamCreationRegion, h.nowUTC(), teams)
	report.CreditReads, report.ElapsedMS = reads, time.Since(started).Milliseconds()
	c.JSON(http.StatusOK, report)
}

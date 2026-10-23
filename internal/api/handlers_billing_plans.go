package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

type billingPlanLimits struct {
	MaxSandboxes         int32  `json:"max_sandboxes"`
	MaxTemplateVCPU      *int32 `json:"max_template_vcpu"`
	MaxTemplateMemoryMiB *int32 `json:"max_template_memory_mib"`
	MaxTemplateDiskMiB   *int32 `json:"max_template_disk_mib"`
}

type billingPlan struct {
	Key                string            `json:"key"`
	Family             string            `json:"family"`
	Name               string            `json:"name"`
	IntervalMonths     int32             `json:"interval_months"`
	PriceCents         int64             `json:"price_cents"`
	MonthlyCreditCents int64             `json:"monthly_credit_cents"`
	Limits             billingPlanLimits `json:"limits"`
}

type billingPlanPrice struct {
	OfferKey               string `json:"offer_key"`
	PriceCents             int64  `json:"price_cents"`
	MonthlyEquivalentCents int64  `json:"monthly_equivalent_cents"`
	Available              bool   `json:"available"`
}

type billingPlanCard struct {
	Family             string            `json:"family"`
	Version            int32             `json:"version"`
	Name               string            `json:"name"`
	Tagline            string            `json:"tagline"`
	Features           []string          `json:"features"`
	DisplayOnly        bool              `json:"display_only"`
	Monthly            *billingPlanPrice `json:"monthly"`
	Annual             *billingPlanPrice `json:"annual"`
	CreditSummary      string            `json:"credit_summary"`
	MonthlyCreditCents *int64            `json:"monthly_credit_cents"`
	CreditSchedule     []planGrant       `json:"credit_schedule"`
	Matching           *planMatching     `json:"matching"`
	Limits             planLimits        `json:"limits"`
}

type signupCredits struct {
	TrialCreditCents int64 `json:"trial_credit_cents"`
	CardBonusCents   int64 `json:"card_bonus_cents"`
}

func (h *Handlers) planCard(v planVersion) billingPlanCard {
	card := billingPlanCard{Family: v.Family, Version: v.Version, Name: v.DisplayName, Tagline: v.Tagline, Features: v.Features, DisplayOnly: v.DisplayOnly,
		CreditSummary: planCreditSummary(v.CreditSchedule), CreditSchedule: v.CreditSchedule, Matching: v.Matching, Limits: v.Limits}
	if credit, uniform := uniformMonthlyCredit(v.CreditSchedule); uniform {
		card.MonthlyCreditCents = &credit
	}
	if v.MonthlyFeeCents != nil {
		card.Monthly = &billingPlanPrice{OfferKey: v.Family + "_monthly", PriceCents: *v.MonthlyFeeCents, MonthlyEquivalentCents: *v.MonthlyFeeCents, Available: v.sellable() && h.planPriced(planTerms{OfferKey: v.Family + "_monthly", Version: v.Version})}
	}
	if v.AnnualFeeCents != nil {
		card.Annual = &billingPlanPrice{OfferKey: v.Family + "_annual", PriceCents: *v.AnnualFeeCents, MonthlyEquivalentCents: *v.AnnualFeeCents / 12, Available: v.sellable() && h.planPriced(planTerms{OfferKey: v.Family + "_annual", Version: v.Version})}
	}
	if card.CreditSchedule == nil {
		card.CreditSchedule = []planGrant{}
	}
	return card
}

// purchasablePlans lists the offers new customers can check out, in the shape
// the plan catalog first published.
func purchasablePlans(cards []billingPlanCard) []billingPlan {
	plans := []billingPlan{}
	for _, card := range cards {
		for _, price := range []*billingPlanPrice{card.Monthly, card.Annual} {
			if price == nil || !price.Available || card.MonthlyCreditCents == nil || card.Limits.MaxSandboxes == nil {
				continue
			}
			months := int32(1)
			if price == card.Annual {
				months = 12
			}
			plans = append(plans, billingPlan{Key: price.OfferKey, Family: card.Family, Name: card.Name, IntervalMonths: months, PriceCents: price.PriceCents,
				MonthlyCreditCents: *card.MonthlyCreditCents, Limits: billingPlanLimits{MaxSandboxes: *card.Limits.MaxSandboxes, MaxTemplateVCPU: card.Limits.MaxTemplateVCPU,
					MaxTemplateMemoryMiB: card.Limits.MaxTemplateMemoryMiB, MaxTemplateDiskMiB: card.Limits.MaxTemplateDiskMiB}})
		}
	}
	return plans
}

// GetPublicBillingPlans returns the published public plan cards and signup
// credit amounts. Like public pricing it is unauthenticated so signup pages render
// from the catalog.
func (h *Handlers) GetPublicBillingPlans(c *gin.Context) {
	if h.Pool == nil {
		respondErrorMsg(c, "service_unavailable", "plans are not available", http.StatusServiceUnavailable)
		return
	}
	ctx := c.Request.Context()
	rows, err := h.Pool.Query(ctx, `SELECT `+planVersionColumns+` FROM billing_plan_version WHERE state='published' AND public ORDER BY sort_order,family`)
	if err != nil {
		log.Error().Err(err).Msg("list billing plans failed")
		respondError(c, ErrInternal)
		return
	}
	cards := []billingPlanCard{}
	for rows.Next() {
		v, err := scanPlanVersion(rows)
		if err != nil {
			rows.Close()
			log.Error().Err(err).Msg("list billing plans failed")
			respondError(c, ErrInternal)
			return
		}
		cards = append(cards, h.planCard(v))
	}
	if err = rows.Err(); err != nil {
		log.Error().Err(err).Msg("list billing plans failed")
		respondError(c, ErrInternal)
		return
	}
	var signup signupCredits
	if err = h.Pool.QueryRow(ctx, `SELECT billing_signup_promotion_cents('trial'),billing_signup_promotion_cents('card_bonus')`).Scan(&signup.TrialCreditCents, &signup.CardBonusCents); err != nil {
		log.Error().Err(err).Msg("read signup credits failed")
		respondError(c, ErrInternal)
		return
	}
	c.Header("Cache-Control", publicPricingCacheControl)
	c.JSON(http.StatusOK, gin.H{"cards": cards, "plans": purchasablePlans(cards), "signup": signup})
}

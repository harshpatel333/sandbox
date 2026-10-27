package api

import (
	"context"
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

// billingPlan is a purchasable offer, in the shape the plan catalog first
// published.
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

type billingPlanAllowance struct {
	planAllowance
	Summary string `json:"summary"`
}

type billingPlanCard struct {
	Family             string                `json:"family"`
	Version            int32                 `json:"version"`
	Kind               string                `json:"kind"`
	Name               string                `json:"name"`
	PriceLabel         string                `json:"price_label"`
	Tagline            string                `json:"tagline"`
	Features           []string              `json:"features"`
	DisplayOnly        bool                  `json:"display_only"`
	Monthly            *billingPlanPrice     `json:"monthly"`
	Annual             *billingPlanPrice     `json:"annual"`
	CreditSummary      string                `json:"credit_summary"`
	MonthlyCreditCents *int64                `json:"monthly_credit_cents"`
	Allowance          *billingPlanAllowance `json:"allowance"`
	Matching           *planMatching         `json:"matching"`
	Limits             planLimits            `json:"limits"`
	FundingPolicy      string                `json:"funding_policy"`
	Topoff             *planTopoff           `json:"topoff"`
}

func allowanceSummary(a *planAllowance) string {
	if a == nil {
		return ""
	}
	period := map[string]string{"day_window": "every 24 hours", "month": "every month"}[a.Period]
	if a.Compute != nil {
		return formatUSDCents(a.AmountCents) + " of compute " + period
	}
	return formatUSDCents(a.AmountCents) + " of usage included " + period
}

func (h *Handlers) planCard(v planVersion, allowance *planAllowance) billingPlanCard {
	card := billingPlanCard{Family: v.Family, Version: v.Version, Kind: v.Kind, Name: v.DisplayName, PriceLabel: v.PriceLabel, Tagline: v.Tagline,
		Features: v.Features, DisplayOnly: v.DisplayOnly, CreditSummary: planCreditSummary(v.CreditSchedule), Matching: v.Matching, Limits: v.Limits,
		FundingPolicy: fundingPolicyPostpaid, Topoff: v.Topoff}
	if v.prepaidFunded() {
		card.FundingPolicy = fundingPolicyPrepaid
	}
	if allowance != nil {
		card.Allowance = &billingPlanAllowance{planAllowance: *allowance, Summary: allowanceSummary(allowance)}
	}
	if credit, uniform := uniformMonthlyCredit(v.CreditSchedule); uniform {
		card.MonthlyCreditCents = &credit
	}
	priced := func(months int32, key string) bool {
		return v.sellable() && h.planPriced(planTerms{OfferKey: key, Version: v.Version, PricingVersion: v.PricingVersion, IntervalMonths: months})
	}
	if v.MonthlyFeeCents != nil {
		card.Monthly = &billingPlanPrice{OfferKey: v.Family + "_monthly", PriceCents: *v.MonthlyFeeCents, MonthlyEquivalentCents: *v.MonthlyFeeCents, Available: priced(1, v.Family+"_monthly")}
	}
	if v.AnnualFeeCents != nil {
		card.Annual = &billingPlanPrice{OfferKey: v.Family + "_annual", PriceCents: *v.AnnualFeeCents, MonthlyEquivalentCents: *v.AnnualFeeCents / 12, Available: priced(12, v.Family+"_annual")}
	}
	return card
}

// purchasablePlans lists the offers new customers can check out.
func purchasablePlans(cards []billingPlanCard) []billingPlan {
	plans := []billingPlan{}
	for _, card := range cards {
		for _, price := range []*billingPlanPrice{card.Monthly, card.Annual} {
			if price == nil || !price.Available || card.MonthlyCreditCents == nil || card.Limits.MaxConcurrent == nil {
				continue
			}
			months := int32(1)
			if price == card.Annual {
				months = 12
			}
			plans = append(plans, billingPlan{Key: price.OfferKey, Family: card.Family, Name: card.Name, IntervalMonths: months, PriceCents: price.PriceCents,
				MonthlyCreditCents: *card.MonthlyCreditCents, Limits: billingPlanLimits{MaxSandboxes: *card.Limits.MaxConcurrent, MaxTemplateVCPU: card.Limits.MaxVCPU,
					MaxTemplateMemoryMiB: card.Limits.MaxMemoryMiB, MaxTemplateDiskMiB: card.Limits.MaxDiskMiB}})
		}
	}
	return plans
}

// billingOffer is one checkout offer: a plan family at one billing interval.
type billingOffer struct {
	OfferKey            string      `json:"offer_key"`
	Family              string      `json:"family"`
	DisplayName         string      `json:"display_name"`
	PriceCents          int64       `json:"price_cents"`
	IntervalMonths      int32       `json:"interval_months"`
	IncludedCreditCents int64       `json:"included_credit_cents"`
	FundingPolicy       string      `json:"funding_policy"`
	Topoff              *planTopoff `json:"topoff"`
	Sellable            bool        `json:"sellable"`
}

// catalogOffers lists every priced interval of the catalog's cards, sellable
// or not, so clients render offers without knowing plan names.
func catalogOffers(cards []billingPlanCard) []billingOffer {
	offers := []billingOffer{}
	for _, card := range cards {
		for _, price := range []*billingPlanPrice{card.Monthly, card.Annual} {
			if price == nil {
				continue
			}
			months := int32(1)
			if price == card.Annual {
				months = 12
			}
			offer := billingOffer{OfferKey: price.OfferKey, Family: card.Family, DisplayName: card.Name, PriceCents: price.PriceCents, IntervalMonths: months,
				FundingPolicy: card.FundingPolicy, Topoff: card.Topoff, Sellable: price.Available}
			if card.MonthlyCreditCents != nil {
				offer.IncludedCreditCents = *card.MonthlyCreditCents
			}
			offers = append(offers, offer)
		}
	}
	return offers
}

type billingCatalog struct {
	PricingVersion string            `json:"pricing_version"`
	Cards          []billingPlanCard `json:"cards"`
	Plans          []billingPlan     `json:"plans"`
}

func (h *Handlers) billingCatalog(ctx context.Context, model string, open bool) (billingCatalog, error) {
	out := billingCatalog{PricingVersion: model, Cards: []billingPlanCard{}, Plans: []billingPlan{}}
	rows, err := h.Pool.Query(ctx, `SELECT `+planVersionColumns+` FROM billing_plan_version WHERE state='published' AND public AND pricing_version=$1 ORDER BY sort_order,family`, model)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var versions []planVersion
	for rows.Next() {
		v, err := scanPlanVersion(rows)
		if err != nil {
			return out, err
		}
		versions = append(versions, v)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	for _, v := range versions {
		allowance, err := h.resolveAllowance(ctx, v.Allowance)
		if err != nil {
			return out, err
		}
		card := h.planCard(v, allowance)
		if !open {
			if card.Monthly != nil {
				card.Monthly.Available = false
			}
			if card.Annual != nil {
				card.Annual.Available = false
			}
		}
		out.Cards = append(out.Cards, card)
	}
	out.Plans = purchasablePlans(out.Cards)
	return out, nil
}

// GetPublicBillingPlans describes new signup offers, never an authenticated
// team's commercial authority. Existing teams use /billing/capabilities.
func (h *Handlers) GetPublicBillingPlans(c *gin.Context) {
	if h.Pool == nil {
		respondErrorMsg(c, "service_unavailable", "plans are not available", http.StatusServiceUnavailable)
		return
	}
	state, err := h.signupPricingState(c.Request.Context())
	if err != nil {
		respondError(c, ErrInternal)
		return
	}
	catalog, err := h.billingCatalog(c.Request.Context(), state.Version, state.EntryOpen || state.LegacyOpen)
	if err != nil {
		log.Error().Err(err).Msg("load public billing catalog failed")
		respondError(c, ErrInternal)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"pricing_version": catalog.PricingVersion, "cards": catalog.Cards, "plans": catalog.Plans,
		"signup_admission": gin.H{"schema_version": 1, "pricing_version": state.Version, "source": pricingSource(state), "entry_open": state.EntryOpen || state.LegacyOpen, "epoch": pricingEpoch(state)}})
}

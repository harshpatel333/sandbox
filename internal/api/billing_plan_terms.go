package api

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// planTerms are a catalog plan's current terms. Contracts and plan
// assignments freeze their own copy, so these only apply to new purchases,
// assignments and reporting. MonthlyCreditCents is set only when the schedule
// is one ongoing monthly grant, the shape subscription contracts represent.
// Available reports whether checkout may sell the plan. Retired families keep
// returning their last published terms with Retired set, so reporting can
// still value contracts against them; checkout and assignment refuse them.
type planTerms struct {
	OfferKey           string
	DisplayName        string
	FeeCents           int64
	IntervalMonths     int32
	MonthlyCreditCents int64
	VersionID          uuid.UUID
	Version            int32
	PricingVersion     string
	Family             string
	Kind               string
	Grants             []planGrant
	Matching           *planMatching
	Allowance          *planAllowance
	Limits             planLimits
	Available          bool
	Retired            bool
}

var errUnknownPlan = errors.New("unsupported offer")

func planOfferInterval(offerKey string) (string, int32, bool) {
	for suffix, months := range map[string]int32{"_monthly": 1, "_annual": 12} {
		if family, ok := strings.CutSuffix(offerKey, suffix); ok && planFamilyPattern(family) {
			return family, months, true
		}
	}
	return "", 0, false
}

const pricingSwitchCacheTTL = 5 * time.Second

type pricingSwitchCache struct {
	mu      sync.Mutex
	version string
	readAt  time.Time
}

var pricingSwitch pricingSwitchCache

// pricingVersion is the one reader of the pricing switch: "legacy" or "v2".
// The database row is authoritative; this process caches it for a few
// seconds so hot paths do not query it on every call.
func (h *Handlers) pricingVersion(ctx context.Context) (string, error) {
	if h.Pool == nil {
		return "legacy", nil
	}
	pricingSwitch.mu.Lock()
	defer pricingSwitch.mu.Unlock()
	if pricingSwitch.version != "" && time.Since(pricingSwitch.readAt) < pricingSwitchCacheTTL {
		return pricingSwitch.version, nil
	}
	var version string
	if err := h.Pool.QueryRow(ctx, `SELECT billing_pricing_version()`).Scan(&version); err != nil {
		return "", err
	}
	pricingSwitch.version, pricingSwitch.readAt = version, time.Now()
	return version, nil
}

// pricingV2Enabled reports whether the v2 pricing model is live in this region.
func (h *Handlers) pricingV2Enabled(ctx context.Context) (bool, error) {
	version, err := h.pricingVersion(ctx)
	return version == "v2", err
}

func forgetPricingVersion() {
	pricingSwitch.mu.Lock()
	pricingSwitch.version = ""
	pricingSwitch.mu.Unlock()
}

// planTerms is the one lookup for a plan's current terms. It reads the
// family's published version in the live pricing model, or its most recently
// retired one when the family is no longer offered. Subscriptions are keyed
// "<family>_monthly" or "<family>_annual"; every other kind is keyed by its
// bare family and has FeeCents and IntervalMonths of zero.
func (h *Handlers) planTerms(ctx context.Context, offerKey string) (planTerms, error) {
	family, months, ok := planOfferInterval(offerKey)
	if !ok && planFamilyPattern(offerKey) {
		family, ok = offerKey, true
	}
	if !ok || h.Pool == nil {
		return planTerms{}, errUnknownPlan
	}
	model, err := h.pricingVersion(ctx)
	if err != nil {
		return planTerms{}, err
	}
	v, err := scanPlanVersion(h.Pool.QueryRow(ctx, `SELECT `+planVersionColumns+` FROM billing_plan_version
 WHERE family=$1 AND pricing_version=$2 AND state<>'draft' ORDER BY state='published' DESC, retired_at DESC LIMIT 1`, family, model))
	if errors.Is(err, pgx.ErrNoRows) {
		return planTerms{}, errUnknownPlan
	}
	if err != nil {
		return planTerms{}, err
	}
	return h.termsOfVersion(ctx, v, months)
}

// planTermsVersion returns the terms of one exact version, for staff who
// assign a specific version. months is 1 or 12 for a subscription and 0 for
// every other kind.
func (h *Handlers) planTermsVersion(ctx context.Context, versionID uuid.UUID, months int32) (planTerms, error) {
	if h.Pool == nil {
		return planTerms{}, errUnknownPlan
	}
	v, err := scanPlanVersion(h.Pool.QueryRow(ctx, `SELECT `+planVersionColumns+` FROM billing_plan_version WHERE id=$1 AND state<>'draft'`, versionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return planTerms{}, errUnknownPlan
	}
	if err != nil {
		return planTerms{}, err
	}
	return h.termsOfVersion(ctx, v, months)
}

// listAssignableVersions lists the published versions of the live pricing
// model that staff can assign, including ones checkout does not sell.
func (h *Handlers) listAssignableVersions(ctx context.Context) ([]planVersion, error) {
	model, err := h.pricingVersion(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := h.Pool.Query(ctx, `SELECT `+planVersionColumns+` FROM billing_plan_version WHERE state='published' AND pricing_version=$1 AND NOT display_only ORDER BY sort_order,family`, model)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (planVersion, error) { return scanPlanVersion(row) })
}

func (h *Handlers) termsOfVersion(ctx context.Context, v planVersion, months int32) (planTerms, error) {
	if v.DisplayOnly {
		return planTerms{}, errUnknownPlan
	}
	offerKey := v.Family
	var feeCents int64
	switch months {
	case 0:
		if v.hasFee() {
			return planTerms{}, errUnknownPlan
		}
	case 1, 12:
		fee := v.feeCents(months)
		if fee == nil {
			return planTerms{}, errUnknownPlan
		}
		feeCents = *fee
		offerKey += map[int32]string{1: "_monthly", 12: "_annual"}[months]
	default:
		return planTerms{}, errUnknownPlan
	}
	allowance, err := h.resolveAllowance(ctx, v.Allowance)
	if err != nil {
		return planTerms{}, err
	}
	retired := v.State == "retired"
	credit, _ := uniformMonthlyCredit(v.CreditSchedule)
	return planTerms{OfferKey: offerKey, DisplayName: v.DisplayName, FeeCents: feeCents, IntervalMonths: months, MonthlyCreditCents: credit,
		VersionID: v.ID, Version: v.Version, PricingVersion: v.PricingVersion, Family: v.Family, Kind: v.Kind, Grants: v.CreditSchedule,
		Matching: v.Matching, Allowance: allowance, Limits: v.Limits, Available: v.sellable() && !retired, Retired: retired}, nil
}

// resolveAllowance prices a compute allowance at the current public rate
// card, rounded to the nearest cent.
func (h *Handlers) resolveAllowance(ctx context.Context, a *planAllowance) (*planAllowance, error) {
	if a == nil || a.Compute == nil {
		return a, nil
	}
	var vcpu, memory float64
	err := h.Pool.QueryRow(ctx, `SELECT
 COALESCE(MAX(price_usd) FILTER (WHERE resource='vcpu'),0)::float8, COALESCE(MAX(price_usd) FILTER (WHERE resource='memory_gib'),0)::float8
 FROM pricing_rate WHERE plan_key=$1 AND unit='second' AND effective_from<=now() AND (effective_to IS NULL OR effective_to>now())`, publicPricingPlanKey).Scan(&vcpu, &memory)
	if err != nil {
		return nil, err
	}
	resolved := *a
	usd := float64(a.Compute.Seconds) * (float64(a.Compute.VCPU)*vcpu + float64(a.Compute.MemoryGiB)*memory)
	resolved.AmountCents = int64(math.Round(usd * 100))
	return &resolved, nil
}

// planPriced reports whether checkout may sell t in this environment. The
// seeded legacy versions reproduce what the configured Stripe prices charge,
// so they stay sold only where those prices are configured; later versions
// get their own prices on demand.
func (h *Handlers) planPriced(t planTerms) bool {
	return t.PricingVersion == "v2" || t.Version > 1 || (h.Config != nil && h.Config.PlanPriceID(t.OfferKey) != "")
}

// freezeContractLimits records the limits a contract was sold or assigned
// with, so later catalog versions do not change them.
func freezeContractLimits(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, contractID uuid.UUID, l planLimits) error {
	if l.MaxConcurrent == nil {
		return errors.New("frozen limits need max running sandboxes")
	}
	_, err := q.Exec(ctx, `INSERT INTO billing_contract_limits(contract_id,max_sandboxes,max_template_vcpu,max_template_memory_mib,max_template_disk_mib,max_templates)
 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT (contract_id) DO NOTHING`, contractID, *l.MaxConcurrent, l.MaxVCPU, l.MaxMemoryMiB, l.MaxDiskMiB, l.MaxTemplates)
	return err
}

// teamLimits are the limits a team works within, with manual overrides and
// plan limits already resolved.
type teamLimits struct {
	MaxConcurrent int32
	MaxVCPU       int32
	MaxMemoryMiB  int32
	MaxDiskMiB    int32
	MaxTemplates  int32
}

// teamEffectiveLimits is the one reader of a team's limits.
func (h *Handlers) teamEffectiveLimits(ctx context.Context, team uuid.UUID) (teamLimits, error) {
	l, err := h.DB.GetTeamEffectiveLimits(ctx, team)
	return teamLimits{MaxConcurrent: l.MaxConcurrent, MaxVCPU: l.MaxVcpu, MaxMemoryMiB: l.MaxMemoryMib, MaxDiskMiB: l.MaxDiskMib, MaxTemplates: l.MaxTemplates}, err
}

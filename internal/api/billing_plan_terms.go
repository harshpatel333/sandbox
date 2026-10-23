package api

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// planTerms are a catalog plan's current terms. Contracts freeze their own
// copy at assignment, so these only apply to new purchases and reporting.
// MonthlyCreditCents is set only when the schedule is one ongoing monthly
// grant, the shape subscription contracts represent. Available reports
// whether new customers may check the plan out. Retired families keep
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
	Family             string
	Grants             []planGrant
	Matching           *planMatching
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

// planTerms is the one lookup for a plan's current terms. It reads the
// family's published version, or its most recently retired one when the
// family is no longer offered; contracts and checkouts freeze what it returns.
// Fee-bearing plans are keyed "<family>_monthly" or "<family>_annual"; a
// version without fees is keyed by its bare family and has FeeCents and
// IntervalMonths of zero.
func (h *Handlers) planTerms(ctx context.Context, offerKey string) (planTerms, error) {
	family, months, ok := planOfferInterval(offerKey)
	if !ok && planFamilyPattern(offerKey) {
		family, ok = offerKey, true
	}
	if !ok || h.Pool == nil {
		return planTerms{}, errUnknownPlan
	}
	v, err := scanPlanVersion(h.Pool.QueryRow(ctx, `SELECT `+planVersionColumns+` FROM billing_plan_version WHERE family=$1 AND state<>'draft'
 ORDER BY state='published' DESC, retired_at DESC LIMIT 1`, family))
	if errors.Is(err, pgx.ErrNoRows) {
		return planTerms{}, errUnknownPlan
	}
	if err != nil {
		return planTerms{}, err
	}
	return termsOfVersion(v, months)
}

// planTermsVersion returns the terms of one exact version, for staff who
// assign a specific version. months is 1 or 12 for a fee-bearing version and
// 0 for a version without fees.
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
	return termsOfVersion(v, months)
}

// listAssignableVersions lists the published versions staff can assign,
// including ones checkout does not sell.
func (h *Handlers) listAssignableVersions(ctx context.Context) ([]planVersion, error) {
	rows, err := h.Pool.Query(ctx, `SELECT `+planVersionColumns+` FROM billing_plan_version WHERE state='published' AND NOT display_only ORDER BY sort_order,family`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (planVersion, error) { return scanPlanVersion(row) })
}

func termsOfVersion(v planVersion, months int32) (planTerms, error) {
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
	retired := v.State == "retired"
	credit, _ := uniformMonthlyCredit(v.CreditSchedule)
	return planTerms{OfferKey: offerKey, DisplayName: v.DisplayName, FeeCents: feeCents, IntervalMonths: months, MonthlyCreditCents: credit,
		VersionID: v.ID, Version: v.Version, Family: v.Family, Grants: v.CreditSchedule, Matching: v.Matching, Limits: v.Limits,
		Available: v.sellable() && !retired, Retired: retired}, nil
}

// freezeContractLimits records the limits a contract was sold or assigned
// with, so later catalog versions do not change them.
func freezeContractLimits(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, contractID uuid.UUID, l planLimits) error {
	if l.MaxSandboxes == nil {
		return errors.New("frozen limits need max sandboxes")
	}
	_, err := q.Exec(ctx, `INSERT INTO billing_contract_limits(contract_id,max_sandboxes,max_template_vcpu,max_template_memory_mib,max_template_disk_mib) VALUES($1,$2,$3,$4,$5)
 ON CONFLICT (contract_id) DO NOTHING`, contractID, *l.MaxSandboxes, l.MaxTemplateVCPU, l.MaxTemplateMemoryMiB, l.MaxTemplateDiskMiB)
	return err
}

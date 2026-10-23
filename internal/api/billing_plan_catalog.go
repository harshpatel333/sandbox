package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// planGrant is one timed-grant rule: starting StartMonths after the contract
// anchor, AmountCents is issued Repeat times (nil means indefinitely), every
// EveryMonths months, and each issue expires ExpiresAfterMonths later.
type planGrant struct {
	StartMonths        int32  `json:"start_months"`
	Repeat             *int32 `json:"repeat"`
	EveryMonths        int32  `json:"every_months"`
	AmountCents        int64  `json:"amount_cents"`
	ExpiresAfterMonths int32  `json:"expires_after_months"`
}

// planMatching credits MatchingBps basis points of customer-paid usage until
// CapCents of matched credit has been issued over the contract.
type planMatching struct {
	MatchingBps int32 `json:"matching_bps"`
	CapCents    int64 `json:"cap_cents"`
}

type planLimits struct {
	MaxSandboxes         *int32 `json:"max_sandboxes"`
	MaxTemplateVCPU      *int32 `json:"max_template_vcpu"`
	MaxTemplateMemoryMiB *int32 `json:"max_template_memory_mib"`
	MaxTemplateDiskMiB   *int32 `json:"max_template_disk_mib"`
}

type planVersion struct {
	ID              uuid.UUID     `json:"id"`
	Family          string        `json:"family"`
	Version         int32         `json:"version"`
	State           string        `json:"state"`
	Public          bool          `json:"public"`
	DisplayName     string        `json:"display_name"`
	Tagline         string        `json:"tagline"`
	Features        []string      `json:"features"`
	SortOrder       int32         `json:"sort_order"`
	DisplayOnly     bool          `json:"display_only"`
	SelfServe       bool          `json:"self_serve"`
	MonthlyFeeCents *int64        `json:"monthly_fee_cents"`
	AnnualFeeCents  *int64        `json:"annual_fee_cents"`
	CreditSchedule  []planGrant   `json:"credit_schedule"`
	Matching        *planMatching `json:"matching"`
	Limits          planLimits    `json:"limits"`
	CreatedBy       *uuid.UUID    `json:"created_by"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
	PublishedBy     *uuid.UUID    `json:"published_by"`
	PublishedAt     *time.Time    `json:"published_at"`
	RetiredBy       *uuid.UUID    `json:"retired_by"`
	RetiredAt       *time.Time    `json:"retired_at"`
}

const planVersionColumns = `id,family,version,state,public,display_name,tagline,features,sort_order,display_only,self_serve,monthly_fee_cents,annual_fee_cents,
credit_schedule,matching_bps,cap_cents,max_sandboxes,max_template_vcpu,max_template_memory_mib,max_template_disk_mib,
created_by,created_at,updated_at,published_by,published_at,retired_by,retired_at`

func scanPlanVersion(row pgx.Row) (planVersion, error) {
	var v planVersion
	var schedule []byte
	var bps *int32
	var capCents *int64
	err := row.Scan(&v.ID, &v.Family, &v.Version, &v.State, &v.Public, &v.DisplayName, &v.Tagline, &v.Features, &v.SortOrder, &v.DisplayOnly, &v.SelfServe,
		&v.MonthlyFeeCents, &v.AnnualFeeCents, &schedule, &bps, &capCents,
		&v.Limits.MaxSandboxes, &v.Limits.MaxTemplateVCPU, &v.Limits.MaxTemplateMemoryMiB, &v.Limits.MaxTemplateDiskMiB,
		&v.CreatedBy, &v.CreatedAt, &v.UpdatedAt, &v.PublishedBy, &v.PublishedAt, &v.RetiredBy, &v.RetiredAt)
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal(schedule, &v.CreditSchedule); err != nil {
		return v, err
	}
	if bps != nil && capCents != nil {
		v.Matching = &planMatching{MatchingBps: *bps, CapCents: *capCents}
	}
	return v, nil
}

// sellable reports whether new customers may check the version out: it is a
// public self-serve plan whose subscription contract can deliver its terms,
// one monthly credit and no matching rule.
func (v planVersion) sellable() bool {
	_, uniform := uniformMonthlyCredit(v.CreditSchedule)
	return v.SelfServe && v.Public && !v.DisplayOnly && v.hasFee() && uniform && v.Matching == nil
}

func (v planVersion) feeCents(months int32) *int64 {
	if months == 12 {
		return v.AnnualFeeCents
	}
	return v.MonthlyFeeCents
}

// validate mirrors the database constraints so staff see a precise error
// before a write is attempted.
func (v planVersion) validate() error {
	if !planFamilyPattern(v.Family) {
		return errors.New("family must be lowercase letters and digits joined by single underscores, starting with a letter")
	}
	if v.SortOrder < 0 || v.SortOrder > 1000 {
		return errors.New("card order must be between 0 and 1000")
	}
	if strings.ContainsRune(v.DisplayName+v.Tagline+strings.Join(v.Features, ""), 0) {
		return errors.New("text may not contain NUL characters")
	}
	if strings.TrimSpace(v.DisplayName) == "" || len(v.DisplayName) > 80 || len(v.Tagline) > 200 || len(v.Features) > 20 {
		return errors.New("display name is required; name, tagline and features have length limits")
	}
	for _, f := range v.Features {
		if strings.TrimSpace(f) == "" || len(f) > 200 {
			return errors.New("feature bullets must be non-empty and at most 200 characters")
		}
	}
	if err := validatePlanSchedule(v.CreditSchedule); err != nil {
		return err
	}
	if m := v.Matching; m != nil && (m.MatchingBps < 1 || m.MatchingBps > 10000 || m.CapCents <= 0) {
		return errors.New("matching must be 1-10000 basis points with a positive cap")
	}
	for _, fee := range []*int64{v.MonthlyFeeCents, v.AnnualFeeCents} {
		if fee != nil && *fee <= 0 {
			return errors.New("fees must be positive")
		}
	}
	for _, limit := range []*int32{v.Limits.MaxSandboxes, v.Limits.MaxTemplateVCPU, v.Limits.MaxTemplateMemoryMiB, v.Limits.MaxTemplateDiskMiB} {
		if limit != nil && *limit <= 0 {
			return errors.New("limits must be positive")
		}
	}
	if v.DisplayOnly {
		if v.MonthlyFeeCents != nil || v.AnnualFeeCents != nil || v.SelfServe || len(v.CreditSchedule) != 0 || v.Matching != nil {
			return errors.New("display-only plans carry no fees, credits, matching or self-serve checkout")
		}
		return nil
	}
	if v.SelfServe && !v.sellable() {
		return errors.New("self-serve checkout sells only public plans with a fee, one ongoing monthly credit grant expiring after a month, and no matching rule")
	}
	if !v.hasFee() {
		if len(v.CreditSchedule) == 0 && v.Matching == nil {
			return errors.New("a plan without fees needs a credit schedule or matching rule")
		}
		if !strings.Contains(v.Family, "_") {
			return errors.New("plans without fees use a family with an underscore, such as custom_example")
		}
		if v.Public {
			return errors.New("plans without fees are staff-assigned and cannot be public cards")
		}
		return nil
	}
	if strings.Contains(v.Family, "_") {
		return errors.New("plans with fees use a family without underscores")
	}
	if v.Limits.MaxSandboxes == nil {
		return errors.New("max sandboxes is required")
	}
	return nil
}

func (v planVersion) hasFee() bool {
	return v.MonthlyFeeCents != nil || v.AnnualFeeCents != nil
}

// planFamilyPattern accepts lowercase words of letters and digits joined by
// single underscores, never ending like an offer key's interval.
func planFamilyPattern(family string) bool {
	if family == "" || len(family) > 63 || family[0] < 'a' || family[0] > 'z' ||
		strings.HasSuffix(family, "_monthly") || strings.HasSuffix(family, "_annual") {
		return false
	}
	for _, word := range strings.Split(family, "_") {
		if word == "" {
			return false
		}
		for _, r := range word {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				return false
			}
		}
	}
	return true
}

func validatePlanSchedule(grants []planGrant) error {
	if len(grants) > 24 {
		return errors.New("a credit schedule has at most 24 grant rules")
	}
	for i, g := range grants {
		if g.StartMonths < 0 || g.StartMonths > 120 || g.EveryMonths < 1 || g.EveryMonths > 12 || g.ExpiresAfterMonths < 1 || g.ExpiresAfterMonths > 12 ||
			g.AmountCents < 1 || g.AmountCents > 100000000 || (g.Repeat != nil && (*g.Repeat < 1 || *g.Repeat > 120)) {
			return fmt.Errorf("credit grant %d: start 0-120 months, every and expiry 1-12 months, repeat 1-120 or ongoing, amount up to $1,000,000", i+1)
		}
	}
	return nil
}

// planGrantIssue is one expanded grant, at a month offset from the anchor.
type planGrantIssue struct {
	MonthOffset        int32 `json:"month_offset"`
	AmountCents        int64 `json:"amount_cents"`
	ExpiresAfterMonths int32 `json:"expires_after_months"`
}

// expandPlanSchedule lists every grant issued before horizonMonths, ordered by
// offset. It is the single expansion used by previews and materialisation.
func expandPlanSchedule(grants []planGrant, horizonMonths int32) []planGrantIssue {
	issues := []planGrantIssue{}
	for _, g := range grants {
		for n := int32(0); g.Repeat == nil || n < *g.Repeat; n++ {
			offset := g.StartMonths + n*g.EveryMonths
			if offset >= horizonMonths {
				break
			}
			issues = append(issues, planGrantIssue{MonthOffset: offset, AmountCents: g.AmountCents, ExpiresAfterMonths: g.ExpiresAfterMonths})
		}
	}
	sort.SliceStable(issues, func(i, j int) bool { return issues[i].MonthOffset < issues[j].MonthOffset })
	return issues
}

// uniformMonthlyCredit reports the amount of a schedule that issues the same
// one-month grant every month from the anchor, the shape self-serve
// subscription contracts represent.
func uniformMonthlyCredit(grants []planGrant) (int64, bool) {
	if len(grants) != 1 {
		return 0, false
	}
	g := grants[0]
	if g.StartMonths != 0 || g.Repeat != nil || g.EveryMonths != 1 || g.ExpiresAfterMonths != 1 {
		return 0, false
	}
	return g.AmountCents, true
}

// planPriceLookupKey names the Stripe price of a version's fee for one
// interval. Version ids are the same in every region, so every region and
// every caller finds the same price instead of creating another.
func planPriceLookupKey(versionID uuid.UUID, months int32) string {
	interval := "monthly"
	if months == 12 {
		interval = "annual"
	}
	return "superserve_plan_" + versionID.String() + "_" + interval
}

// ensurePlanPrice is the one creator of Stripe prices for catalog plans: it
// returns the licensed price for t's fee and interval, found by lookup key or
// created once, and verified against t.
func (c *stripeHTTPClient) ensurePlanPrice(ctx context.Context, t planTerms) (storagePrice, error) {
	if t.VersionID == uuid.Nil || t.FeeCents <= 0 || (t.IntervalMonths != 1 && t.IntervalMonths != 12) {
		return storagePrice{}, errors.New("plan terms have no fee to price")
	}
	o := proOffer{Key: t.OfferKey, Amount: t.FeeCents, Months: t.IntervalMonths, VersionID: t.VersionID}
	key := planPriceLookupKey(t.VersionID, t.IntervalMonths)
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.doForm(ctx, http.MethodGet, "/v1/prices?"+url.Values{"lookup_keys[]": {key}, "limit": {"2"}}.Encode(), nil, &list, ""); err != nil {
		return storagePrice{}, err
	}
	if len(list.Data) > 1 {
		return storagePrice{}, errors.New("multiple Stripe prices share a plan lookup key")
	}
	if len(list.Data) == 1 {
		o.Price = list.Data[0].ID
		p, err := c.verifiedPlanPrice(ctx, o)
		if err != nil {
			return p, fmt.Errorf("plan price %s is archived or differs from the version: %w", o.Price, err)
		}
		return p, nil
	}
	interval := "month"
	if t.IntervalMonths == 12 {
		interval = "year"
	}
	form := url.Values{}
	form.Set("currency", "usd")
	form.Set("unit_amount", fmt.Sprint(t.FeeCents))
	form.Set("recurring[interval]", interval)
	form.Set("recurring[interval_count]", "1")
	form.Set("recurring[usage_type]", "licensed")
	form.Set("lookup_key", key)
	form.Set("product_data[name]", "Superserve "+catalogPlanName(t.OfferKey))
	form.Set("metadata[plan_version_id]", t.VersionID.String())
	form.Set("metadata[commercial_offer]", t.OfferKey)
	var created struct {
		ID string `json:"id"`
	}
	if err := c.doForm(ctx, http.MethodPost, "/v1/prices", form, &created, "plan-price-"+key); err != nil {
		return storagePrice{}, err
	}
	o.Price = created.ID
	return c.verifiedPlanPrice(ctx, o)
}

// resolvePlanPrice sets the Stripe base price for a checkout. A configured
// price is kept while it still matches the frozen terms; otherwise the
// version's own price is used.
func (c *stripeHTTPClient) resolvePlanPrice(ctx context.Context, o *proOffer) error {
	if o.Price != "" {
		err := c.validateProPrice(ctx, *o)
		if !errors.Is(err, errProPriceMismatch) {
			return err
		}
	}
	price, err := c.ensurePlanPrice(ctx, planTerms{OfferKey: o.Key, FeeCents: o.Amount, IntervalMonths: o.Months, VersionID: o.VersionID})
	if err != nil {
		return err
	}
	o.Price = price.ID
	return nil
}

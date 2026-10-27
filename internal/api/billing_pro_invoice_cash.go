package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/superserve-ai/sandbox/internal/billing"
	"github.com/superserve-ai/sandbox/internal/db"
)

// A paid upgrade may share its first reporting month with Free or prepaid
// history. The old usage decision stays frozen; a separate proof authorizes
// only the accepted renewal fee and credit-covered protected usage.
func (h *Handlers) invoiceCollectionCashAllowed(ctx context.Context, inv StripeInvoiceAmounts) (bool, error) {
	if inv.Parent.SubscriptionDetails == nil {
		return false, errors.New("invoice subscription missing")
	}
	allowed, err := h.invoiceResidualCashAllowed(ctx, inv.ID, inv.Customer, inv.Parent.SubscriptionDetails.Subscription)
	if err != nil || allowed {
		return allowed, err
	}
	return h.freezeProInvoiceCash(ctx, inv)
}

func (h *Handlers) freezeProInvoiceCash(ctx context.Context, inv StripeInvoiceAmounts) (bool, error) {
	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	hash := invoiceSettlementHash(inv)
	var prior bool
	err = tx.QueryRow(ctx, `SELECT pro_invoice_cash_source_valid(invoice_id,$2) FROM billing_pro_invoice_cash_authority WHERE invoice_id=$1`, inv.ID, hash).Scan(&prior)
	if err == nil {
		return prior, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var p billing.ExportPeriod
	var rawPlan, rawFinal []byte
	err = tx.QueryRow(ctx, `SELECT team_id,period_start,period_end,plan,finalized_evidence FROM billing_invoice_close WHERE invoice_id=$1 AND state IN ('verified','released') AND billing_invoice_close_matches(team_id,period_start,period_end)`, inv.ID).Scan(&p.TeamID, &p.Start, &p.End, &rawPlan, &rawFinal)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var plan billing.InvoiceClosePlan
	var final billing.InvoiceCloseSettlement
	if err = json.Unmarshal(rawPlan, &plan); err != nil {
		return false, err
	}
	if err = json.Unmarshal(rawFinal, &final); err != nil {
		return false, err
	}
	var savedInvoice StripeInvoiceAmounts
	if err = json.Unmarshal(final.Invoice, &savedInvoice); err != nil || invoiceSettlementHash(savedInvoice) != hash {
		return false, nil
	}
	if plan.CreditVersion != 1 || plan.BaseCents <= 0 || final.NetCents < 0 || final.CreditAppliedCents < 0 {
		return false, nil
	}
	// The already-verified close retains the complete grant inventory. Recheck
	// scope from that immutable evidence, never from a later live balance.
	if len(plan.CreditPrices) != len(plan.Resources)+1 {
		return false, nil
	}
	seenPrices := map[string]bool{}
	for _, price := range plan.CreditPrices {
		if price == "" || seenPrices[price] {
			return false, nil
		}
		seenPrices[price] = true
	}
	for _, r := range plan.Resources {
		if !seenPrices[r.PriceID] {
			return false, nil
		}
	}
	creditBefore := int64(0)
	for _, saved := range plan.CreditGrants {
		var g entitlementGrant
		if err = json.Unmarshal(saved.Evidence, &g); err != nil {
			return false, err
		}
		if saved.Before > plan.CreditBeforeCents-creditBefore {
			return false, nil
		}
		creditBefore += saved.Before
		if g.ID != saved.ID || saved.Before < 0 || creditScope(g, inv.Customer, plan.InvoiceEnd, plan.CreditPrices) != nil {
			return false, nil
		}
	}
	if creditBefore != plan.CreditBeforeCents {
		return false, nil
	}
	var a invoiceAccount
	err = tx.QueryRow(ctx, `SELECT customer_id,subscription_id,adjustment_price_id,adjustment_event_name,adjustment_meter_id,enrolled_at FROM billing_invoice_account WHERE team_id=$1`, p.TeamID).Scan(&a.Customer, &a.Subscription, &a.Price, &a.Event, &a.Meter, &a.EnrolledAt)
	if err != nil {
		return false, err
	}
	if !seenPrices[a.Price] {
		return false, nil
	}
	var upgrade, contract, assignment uuid.UUID
	var paidStart time.Time
	// Historical cash permission starts at the actual checkout assignment, not
	// at the quote or a later current plan. The retained bridge calendar stays.
	err = tx.QueryRow(ctx, `SELECT u.id,c.id,t.id,t.effective_from,c.base_price_id,c.charge_amount_cents,c.charge_interval_months,c.entitlement_anchor,extract(epoch FROM c.starts_at)::bigint
 FROM billing_pro_upgrade u JOIN billing_commercial_contract c ON c.id=u.contract_id
 JOIN team_plan_assignment t ON t.team_id=u.team_id AND t.contract_id=c.id AND t.source='checkout' AND t.idempotency_key='checkout:'||c.id::text AND t.kind='subscription'
 WHERE u.team_id=$1 AND u.customer_id=$2 AND u.subscription_id=$3 AND u.period_start=$4 AND u.period_end=$5
 AND u.state IN ('active','ended') AND u.payment_evidence IS NOT NULL AND NOT u.payment_review_required
 AND c.team_id=u.team_id AND c.customer_id=u.customer_id AND c.subscription_id=u.subscription_id
 AND c.starts_at=u.period_end AND c.charge_interval_months=1 AND (c.ends_at IS NULL OR c.ends_at>=to_timestamp($6))
 AND t.effective_from>=$4 AND t.effective_from<$5 AND COALESCE(t.effective_to,'infinity')>=$5
 AND t.plan_version_id=(u.request_payload->'PlanTerms'->>'VersionID')::uuid
 AND NOT EXISTS(SELECT 1 FROM team_plan_assignment x WHERE x.team_id=u.team_id AND x.id<>t.id AND x.effective_from<$5 AND COALESCE(x.effective_to,'infinity')>t.effective_from)`, p.TeamID, inv.Customer, inv.Parent.SubscriptionDetails.Subscription, p.Start, p.End, plan.BasePeriodEnd).Scan(&upgrade, &contract, &assignment, &paidStart, &a.Base.Price, &a.Base.Amount, &a.Base.Months, &a.Base.Anchor, &a.Base.StartsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = validateInvoiceDocument(inv, invoicePlanPeriod(p, plan), a, plan); err != nil {
		return false, nil
	}
	if err = verifyInvoicePlanSettlement(inv, plan, plan.CreditBeforeCents, final.CreditRemainingCents); err != nil {
		return false, nil
	}
	// One repeatable-read transaction sees the full and protected raw intervals,
	// immutable rollup, close, grant inventory and assignment together.
	q := db.New(tx)
	full, err := q.GetTeamBillingChargeableUsage(ctx, db.GetTeamBillingChargeableUsageParams{TeamID: p.TeamID, PeriodStart: p.Start, PeriodEnd: p.End})
	if err != nil {
		return false, err
	}
	protected, err := q.GetTeamBillingChargeableUsage(ctx, db.GetTeamBillingChargeableUsageParams{TeamID: p.TeamID, PeriodStart: p.Start, PeriodEnd: paidStart})
	if err != nil {
		return false, err
	}
	rollup, err := q.GetTeamBillingUsageRollup(ctx, db.GetTeamBillingUsageRollupParams{TeamID: p.TeamID, PeriodStart: p.Start, PeriodEnd: p.End})
	if err != nil {
		return false, err
	}
	// Missing or changed raw measurements cannot establish a temporal split.
	var complete bool
	if err = tx.QueryRow(ctx, `SELECT storage_reports_complete_through($1,$2)`, p.TeamID, p.End).Scan(&complete); err != nil || !complete {
		return false, err
	}
	numeric := func(v pgtype.Numeric) (*big.Rat, error) {
		value, e := v.Value()
		if e != nil {
			return nil, e
		}
		r, ok := new(big.Rat).SetString(fmt.Sprint(value))
		if !ok || r.Sign() < 0 {
			return nil, errors.New("invalid raw usage")
		}
		return r, nil
	}
	fullValues := map[string]pgtype.Numeric{"cpu": full.VcpuSeconds, "memory": full.MemoryGibSeconds, "storage": full.BillableStorageGibSeconds}
	protectedValues := map[string]pgtype.Numeric{"cpu": protected.VcpuSeconds, "memory": protected.MemoryGibSeconds, "storage": protected.BillableStorageGibSeconds}
	rolledValues := map[string]pgtype.Numeric{"cpu": rollup.VcpuSeconds, "memory": rollup.MemoryMibSeconds, "storage": rollup.StorageMibSeconds}
	protectedCents := int64(0)
	split := map[string]any{}
	for _, r := range plan.Resources {
		f, ok := fullValues[r.Resource]
		if !ok || r.ExpectedCents < 0 {
			return false, nil
		}
		all, e := numeric(f)
		if e != nil {
			return false, e
		}
		before, e := numeric(protectedValues[r.Resource])
		if e != nil {
			return false, e
		}
		rolled, e := numeric(rolledValues[r.Resource])
		if e != nil {
			return false, e
		}
		if r.Resource != "cpu" {
			rolled.Quo(rolled, big.NewRat(1024, 1))
		}
		if all.Cmp(rolled) != 0 || before.Cmp(all) > 0 {
			return false, nil
		}
		price, e := meterDecimal(r.UnitUSD)
		if e != nil {
			return false, e
		}
		// UnitUSD is per resource-hour; raw quantities are resource-seconds.
		totalCost := new(big.Rat).Mul(all, price)
		totalCost.Quo(totalCost, big.NewRat(36, 1))
		protectedCost := new(big.Rat).Mul(before, price)
		protectedCost.Quo(protectedCost, big.NewRat(36, 1))
		bounded, e := protectedUsageCents(totalCost, protectedCost, r.ExpectedCents)
		if e != nil {
			return false, e
		}
		if bounded > plan.ExpectedCents-protectedCents {
			return false, nil
		}
		protectedCents += bounded
		split[r.Resource] = map[string]any{"raw_total": all.RatString(), "raw_protected": before.RatString(), "total_cost_cents": totalCost.RatString(), "protected_cost_cents": protectedCost.RatString(), "protected_billed_cents": bounded, "expected_cents": r.ExpectedCents}
	}
	if protectedCents > final.CreditAppliedCents {
		return false, nil
	}
	evidence, err := json.Marshal(map[string]any{"version": 1, "paid_assignment_from": paidStart, "resources": split, "plan": json.RawMessage(rawPlan), "finalized_evidence": json.RawMessage(rawFinal)})
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO billing_pro_invoice_cash_authority(invoice_id,settlement_hash,team_id,customer_id,subscription_id,upgrade_id,contract_id,assignment_id,protected_cents,credit_applied_cents,fee_cents,usage_net_cents,evidence)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT(invoice_id) DO NOTHING`, inv.ID, hash, p.TeamID, inv.Customer, a.Subscription, upgrade, contract, assignment, protectedCents, final.CreditAppliedCents, plan.BaseCents, final.NetCents, evidence)
	if err != nil {
		return false, err
	}
	if err = tx.QueryRow(ctx, `SELECT pro_invoice_cash_source_valid(invoice_id,$2) FROM billing_pro_invoice_cash_authority WHERE invoice_id=$1`, inv.ID, hash).Scan(&prior); err != nil {
		return false, err
	}
	if !prior {
		return false, nil
	}
	return true, tx.Commit(ctx)
}

// Attribute rounding and positive corrections to the protected side first.
// It can never exceed the actual billed resource amount (including a zero-cent
// final charge), and never authorizes cash using a negative correction.
func protectedUsageCents(total, protected *big.Rat, billed int64) (int64, error) {
	if total == nil || protected == nil || total.Sign() < 0 || protected.Sign() < 0 || protected.Cmp(total) > 0 || billed < 0 {
		return 0, errors.New("invalid protected usage cost")
	}
	floor := new(big.Int).Quo(total.Num(), total.Denom())
	ceil, rem := new(big.Int), new(big.Int)
	ceil.QuoRem(protected.Num(), protected.Denom(), rem)
	if rem.Sign() > 0 {
		ceil.Add(ceil, big.NewInt(1))
	}
	extra := new(big.Int).Sub(big.NewInt(billed), floor)
	if extra.Sign() > 0 {
		ceil.Add(ceil, extra)
	}
	if ceil.Cmp(big.NewInt(billed)) > 0 {
		return billed, nil
	}
	if !ceil.IsInt64() {
		return 0, errors.New("protected usage overflow")
	}
	return ceil.Int64(), nil
}

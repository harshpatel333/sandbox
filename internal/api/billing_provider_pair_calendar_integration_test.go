//go:build integration

package api

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/superserve-ai/sandbox/internal/billing"
	"github.com/superserve-ai/sandbox/internal/db"
)

func TestProviderPairLeapDayFeeReceiptPreservesActualCoverage(t *testing.T) {
	h, team := fundingTransitionFixture(t)
	f := newPairFixture(t)
	f.p.Identity.TeamID = team
	movePairFixture(f, time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC))
	verified, err := f.client(t).verifyInitialProviderPair(t.Context(), f.p)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE team SET home_region='use' WHERE id=$1`, team)
	exec(`INSERT INTO team_feature_flag(team_id,key,enabled) VALUES($1,'billing_commercial_offer_assignments_enabled',true)`, team)
	exec(`INSERT INTO team_billing_account(team_id,stripe_customer_id,stripe_subscription_id,commercial_billing_anchor) VALUES($1,$2,$3,$4)`, team, f.p.CustomerID, f.p.UsageSubscriptionID, f.p.Anchor)
	exec(`INSERT INTO billing_commercial_offer_assignment(id,team_id,approval_id,company_identity,home_region,offer_key,offer_version,idempotency_key,rate_card_key,rate_ids,pricing_at,resource_rates,request,assigned_by,reason,reference)
 VALUES($1,$2,$3,'example-company','use','yc_2026',1,'initial','payg',$4,clock_timestamp(),'[]','{}',$5,'fixture','fixture')`, f.p.Identity.AssignmentID, team, uuid.New(), []uuid.UUID{uuid.New()}, uuid.New())
	contract := uuid.New()
	end := annualBoundary(f.p.Anchor, 12)
	exec(`INSERT INTO billing_commercial_contract(id,team_id,offer_key,starts_at,ends_at,customer_id,subscription_id,base_price_id,charge_amount_cents,charge_interval_months,entitlement_kind,entitlement_amount_cents,entitlement_cadence,entitlement_anchor)
 VALUES($1,$2,'yc_2026',$3,$4,$5,$6,$7,1200000,12,'metered_usage',100000,'scheduled',$3)`, contract, team, f.p.Anchor, end, f.p.CustomerID, f.p.UsageSubscriptionID, f.p.Terms.FeePriceID)
	previous, err := ConfirmContractProviderBinding(t.Context(), tx, contract, verified)
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE billing_commercial_contract SET status='ended',revision=revision+1 WHERE id=$1`, contract)
	next := successorPairFixture(f, previous, true)
	start, finish, _ := billing.AnniversaryPeriod(next.FeeAnchor, time.Now())
	f.fee.Items.Data[0].Start, f.fee.Items.Data[0].End = start.Unix(), finish.Unix()
	start, finish, _ = billing.AnniversaryPeriod(next.Anchor, time.Now())
	for i := range f.usage.Items.Data {
		f.usage.Items.Data[i].Start, f.usage.Items.Data[i].End = start.Unix(), finish.Unix()
	}
	proof, err := f.client(t).verifySuccessorProviderPair(t.Context(), previous, next, f.invoice.ID)
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO billing_commercial_contract(id,team_id,offer_key,starts_at,customer_id,subscription_id,base_price_id,charge_amount_cents,charge_interval_months,entitlement_kind,entitlement_amount_cents,entitlement_cadence,entitlement_anchor)
 VALUES($1,$2,'pro_monthly',$3,$4,$5,$6,120000,1,'metered_usage',100000,'monthly',$7)`, next.ContractID, team, next.StartsAt, next.CustomerID, next.UsageSubscriptionID, next.Terms.FeePriceID, next.Anchor)
	if _, err = ConfirmSuccessorProviderPair(t.Context(), tx, proof); err != nil {
		t.Fatal(err)
	}
	f.invoice.Status, f.invoice.AmountPaid, f.invoice.AmountRemaining = "paid", next.Terms.FeeAmount, 0
	f.invoice.StatusTransitions.PaidAt = next.StartsAt.Add(time.Minute).Unix()
	cash, err := f.client(t).verifyProviderPairPayment(t.Context(), next, f.invoice.ID)
	if err != nil {
		t.Fatal(err)
	}
	payment, err := db.New(tx).RecordCommercialBasePayment(t.Context(), db.RecordCommercialBasePaymentParams{TeamID: team, ContractID: next.ContractID, InvoiceID: cash.Invoice.ID, PaymentReference: cash.Reference, CustomerID: next.CustomerID, SubscriptionID: &next.FeeSubscriptionID, BasePriceID: next.Terms.FeePriceID, ServicePeriodStart: time.Unix(cash.Start, 0), ServicePeriodEnd: time.Unix(cash.End, 0), BaseAmountPaidCents: next.Terms.FeeAmount, InvoiceAmountPaidCents: next.Terms.FeeAmount, Currency: "usd", PaidAt: time.Unix(cash.Invoice.StatusTransitions.PaidAt, 0), VerifiedAt: time.Now(), Evidence: cash.Raw})
	if err != nil {
		t.Fatal(err)
	}
	if !payment.ServicePeriodEnd.Equal(time.Date(2025, 3, 28, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("database reinterpreted actual fee coverage")
	}
	var coversUsage bool
	if err = tx.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM billing_contract_payment WHERE contract_id=$1 AND service_period_start<=$2 AND service_period_end>=$3)`, next.ContractID, next.StartsAt, annualBoundary(next.Anchor, 13)).Scan(&coversUsage); err != nil || coversUsage {
		t.Fatalf("actual fee receipt was stretched over original-S usage month: %v", err)
	}
}

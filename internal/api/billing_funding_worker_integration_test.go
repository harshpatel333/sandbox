//go:build integration

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/superserve-ai/sandbox/internal/db"
)

func claimFundingTestJob(t *testing.T, h *Handlers, team uuid.UUID) (fundingJob, fundingSnapshot) {
	t.Helper()
	j := fundingJob{Team: team, Token: uuid.New()}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE billing_funding_account SET lease_token=$2,lease_until=clock_timestamp()+interval '1 minute' WHERE team_id=$1`, team, j.Token); err != nil {
		t.Fatal(err)
	}
	s, err := h.fundingSnapshot(t.Context(), j)
	if err != nil {
		t.Fatal(err)
	}
	return j, s
}

func TestFundingPublicationPreservesIndependentFreshness(t *testing.T) {
	h, team := fundingTransitionFixture(t)
	ctx := t.Context()
	j, s := claimFundingTestJob(t, h, team)
	credit := &fundingCreditObservation{SpendableUSD: "10", ValidUntil: s.At.Add(time.Minute), Evidence: []byte(`{"fixture":"credit"}`)}
	provider := &fundingProviderObservation{PaymentMethodID: "pm_example", PaymentMethodReusable: true, CollectionReady: true, CashChargeID: "ch_example", CashCollected: true, ValidUntil: s.At.Add(5 * time.Minute), Evidence: []byte(`{"fixture":"provider"}`)}
	if err := h.publishFundingObservation(ctx, j, s, credit, provider, nil); err != nil {
		t.Fatal(err)
	}
	j, s = claimFundingTestJob(t, h, team)
	if !s.sameAuthority() {
		t.Fatal("publication did not bind current generation")
	}
	// A credit read failure cannot extend its TTL or discard the independent
	// successful payment proof, and the reverse applies on the next refresh.
	if err := h.publishFundingObservation(ctx, j, s, nil, provider, errors.New("credit unavailable")); err != nil {
		t.Fatal(err)
	}
	var creditUntil, providerUntil time.Time
	var ready bool
	if err := h.Pool.QueryRow(ctx, `SELECT credit_valid_until,provider_valid_until,payment_method_ready FROM billing_funding_account WHERE team_id=$1`, team).Scan(&creditUntil, &providerUntil, &ready); err != nil {
		t.Fatal(err)
	}
	if !creditUntil.Equal(credit.ValidUntil.Truncate(time.Microsecond)) || !ready {
		t.Fatalf("independent credit freshness changed: %v %v", creditUntil, ready)
	}
	j, s = claimFundingTestJob(t, h, team)
	if err := h.publishFundingObservation(ctx, j, s, credit, nil, errors.New("provider unavailable")); err != nil {
		t.Fatal(err)
	}
	var preserved time.Time
	if err := h.Pool.QueryRow(ctx, `SELECT provider_valid_until FROM billing_funding_account WHERE team_id=$1`, team).Scan(&preserved); err != nil || !preserved.Equal(providerUntil) {
		t.Fatalf("provider failure extended or destroyed TTL: %v %v", preserved, err)
	}
	// Changing the balance invalidates all old-generation authority. A good
	// new credit observation must not relabel an old payment proof as fresh.
	if _, err := h.Pool.Exec(ctx, `INSERT INTO team_credit_grant(team_id,amount_usd,remaining_usd,reason) VALUES($1,1,1,'fixture')`, team); err != nil {
		t.Fatal(err)
	}
	j, s = claimFundingTestJob(t, h, team)
	if s.sameAuthority() {
		t.Fatal("grant change did not invalidate generation")
	}
	if err := h.publishFundingObservation(ctx, j, s, credit, nil, errors.New("provider unavailable")); err != nil {
		t.Fatal(err)
	}
	var providerCleared bool
	if err := h.Pool.QueryRow(ctx, `SELECT provider_valid_until IS NULL AND NOT payment_method_ready AND NOT cash_collected AND cash_charge_id IS NULL FROM billing_funding_account WHERE team_id=$1`, team).Scan(&providerCleared); err != nil || !providerCleared {
		t.Fatalf("stale-generation provider proof survived: %v %v", providerCleared, err)
	}
}

func TestFundingPublicationRejectsStaleAuthority(t *testing.T) {
	for _, mutation := range []string{"generation", "mapping", "lease", "expiry"} {
		t.Run(mutation, func(t *testing.T) {
			h, team := fundingTransitionFixture(t)
			ctx := t.Context()
			j, s := claimFundingTestJob(t, h, team)
			var err error
			switch mutation {
			case "generation":
				_, err = h.Pool.Exec(ctx, `UPDATE billing_funding_account SET generation=generation+1 WHERE team_id=$1`, team)
			case "mapping":
				_, err = h.Pool.Exec(ctx, `INSERT INTO team_billing_account(team_id,stripe_customer_id) VALUES($1,$2)`, team, "cus_"+team.String())
				if err == nil {
					// Isolate the mapping compare-and-swap independently of its trigger.
					err = h.Pool.QueryRow(ctx, `SELECT generation FROM billing_funding_account WHERE team_id=$1`, team).Scan(&s.Generation)
				}
			case "lease":
				_, err = h.Pool.Exec(ctx, `UPDATE billing_funding_account SET lease_token=$2 WHERE team_id=$1`, team, uuid.New())
			case "expiry":
				_, err = h.Pool.Exec(ctx, `UPDATE billing_funding_account SET lease_until=clock_timestamp()-interval '1 second' WHERE team_id=$1`, team)
			}
			if err != nil {
				t.Fatal(err)
			}
			credit := &fundingCreditObservation{SpendableUSD: "10", ValidUntil: s.At.Add(time.Minute)}
			if err = h.publishFundingObservation(ctx, j, s, credit, nil, nil); err == nil {
				t.Fatal("stale observer authorized funds")
			}
			var fresh bool
			if err = h.Pool.QueryRow(ctx, `SELECT COALESCE(credit_valid_until>now(),false) FROM billing_funding_account WHERE team_id=$1`, team).Scan(&fresh); err != nil || fresh {
				t.Fatalf("stale proof persisted: %v %v", fresh, err)
			}
		})
	}
}

func TestFundingSchedulerRespectsOtherReplicaLease(t *testing.T) {
	h, _ := fundingTransitionFixture(t)
	ctx := t.Context()
	holder := uuid.NewString()
	if _, err := h.DB.ClaimSweepLease(ctx, db.ClaimSweepLeaseParams{Name: fundingSweepName, LockedBy: holder, LeaseSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		_, _ = h.Pool.Exec(cleanup, `DELETE FROM sweep_lease WHERE name=$1 AND locked_by=$2`, fundingSweepName, holder)
	})
	count, err := h.fundingObservationBatch(ctx)
	if err != nil || count != 0 {
		t.Fatalf("second replica acquired provider work: %d %v", count, err)
	}
}

func TestFundingWorkerLocalCreditSurvivesProviderFailure(t *testing.T) {
	for _, customer := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-customer", true: "provider-unavailable"}[customer], func(t *testing.T) {
			h, team := fundingTransitionFixture(t)
			ctx := t.Context()
			if customer {
				if _, err := h.Pool.Exec(ctx, `INSERT INTO team_billing_account(team_id,stripe_customer_id) VALUES($1,$2)`, team, "cus_"+team.String()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.Pool.Exec(ctx, `INSERT INTO team_credit_grant(team_id,amount_usd,remaining_usd,reason,expires_at) VALUES($1,5,5,'fixture',now()+interval '1 hour')`, team); err != nil {
				t.Fatal(err)
			}
			j, s := claimFundingTestJob(t, h, team)
			err := h.processFundingJob(ctx, j)
			if (err != nil) != customer {
				t.Fatalf("provider failure=%v err=%v", customer, err)
			}
			var funded bool
			var next time.Time
			if err := h.Pool.QueryRow(ctx, `SELECT credit_usd=5 AND credit_valid_until>now() AND NOT payment_method_ready AND NOT cash_collected AND observed_generation=generation,next_check_at FROM billing_funding_account WHERE team_id=$1`, team).Scan(&funded, &next); err != nil || !funded {
				t.Fatalf("local credit not independently published: %v %v", funded, err)
			}
			if next.Sub(s.At) < 29*time.Second || next.Sub(s.At) > 31*time.Second {
				t.Fatalf("credit refresh not anchored to observation: %v", next.Sub(s.At))
			}
		})
	}
}

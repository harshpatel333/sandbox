//go:build integration

package api

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/superserve-ai/sandbox/internal/billing/provisioning"
	"github.com/superserve-ai/sandbox/internal/db"
)

func TestOfferCompositionRevocationWaitsForDispatchResolution(t *testing.T) {
	a, service, r, o := offerCompositionFixture(t)
	ctx := t.Context()
	if err := service.Reconcile(ctx, o.TeamID, o.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.h.Pool.Exec(ctx, `SELECT public.revoke_yc_activation_authority($1,$2,$3)`, r.ApprovalID, r.TeamID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	deliveries, err := a.admission.Service.LeaseFinancialInvalidations(ctx, uuid.New(), 25)
	if err != nil {
		t.Fatal(err)
	}
	var delivery YCFinancialInvalidation
	for _, d := range deliveries {
		if d.AcceptanceID == r.ID {
			delivery = d
		}
	}
	if delivery.InvalidationID == uuid.Nil {
		t.Fatal("missing invalidation")
	}
	a.dispatchesResolved = nil
	if err = a.revokeFinancialDelivery(ctx, delivery); !errors.Is(err, provisioning.ErrUnavailable) {
		t.Fatal("missing dispatch proof allowed acknowledgement", err)
	}
	target := provisioning.InitialFinancialTarget(o.TeamID, o.AssignmentID)
	receipt, err := provisioning.LoadFinancialRevocationReceipt(ctx, a.h.Pool, target, delivery.InvalidationID)
	if err != nil || receipt.Authorization == nil || receipt.Authorization.ID != delivery.AuthorizationID || receipt.Authorization.OperationID != o.ID {
		t.Fatal("pending dispatch rolled back permanent denial or lost exact authority", receipt, err)
	}
	binding, err := loadContractProviderBinding(ctx, a.h.Pool, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err = a.WithProviderPairAuthority(ctx, binding, func(context.Context, pgx.Tx) error {
		called = true
		return nil
	}); err == nil || called {
		t.Fatal("pending acknowledgement admitted a new dispatch", err)
	}
	pending := errors.New("synthetic unresolved dispatch")
	checks := 0
	a.dispatchesResolved = func(ctx context.Context, q db.DBTX, got ContractProviderBinding) error {
		checks++
		if got.ContractID != o.ID || got.Identity != binding.Identity {
			t.Fatal("dispatch check received another binding")
		}
		// This is an adapter boundary test. The materialization owner's composed
		// worker test separately proves the durable dispatch journal predicate.
		if _, err := provisioning.LoadFinancialRevocationReceipt(ctx, q, target, delivery.InvalidationID); err != nil {
			t.Fatal("dispatch check ran before committed denial", err)
		}
		return pending
	}
	for range 2 {
		if err = a.revokeFinancialDelivery(ctx, delivery); !errors.Is(err, pending) {
			t.Fatal("already-revoked retry bypassed unresolved dispatch", err)
		}
		state, err := a.admission.Service.FinancialState(ctx, r.ClaimID)
		if err != nil || state == nil || state.PendingInvalidations != 1 || state.State == "revoked" {
			t.Fatal("unresolved dispatch acknowledged effective revocation", state, err)
		}
	}
	if checks != 2 {
		t.Fatal("resolution was not rechecked on every retry", checks)
	}
	wrong := delivery
	wrong.AuthorizationID = uuid.New()
	if err = a.revokeFinancialDelivery(ctx, wrong); !errors.Is(err, provisioning.ErrConflict) || checks != 2 {
		t.Fatal("replayed revocation accepted another authorization", err, checks)
	}
	pending = nil
	if err = a.revokeFinancialDelivery(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	state, err := a.admission.Service.FinancialState(ctx, r.ClaimID)
	if err != nil || state == nil || state.PendingInvalidations != 0 || state.State != "revoked" {
		t.Fatal("resolved dispatch did not permit acknowledgement", state, err)
	}
	replayed, err := provisioning.LoadFinancialRevocationReceipt(ctx, a.h.Pool, target, delivery.InvalidationID)
	if err != nil || replayed.AcknowledgementID != receipt.AcknowledgementID || replayed.FenceID != receipt.FenceID {
		t.Fatal("pending recovery replaced durable receipt", err)
	}
}

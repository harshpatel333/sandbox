package api

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type pricingPublicState struct {
	Version   string
	EntryOpen bool
	Epoch     uuid.UUID
}

func (h *Handlers) publicPricingState(ctx context.Context) (pricingPublicState, error) {
	state := pricingPublicState{Version: "legacy"}
	var available bool
	if err := h.Pool.QueryRow(ctx, `SELECT to_regprocedure('billing_pricing_public_state()') IS NOT NULL`).Scan(&available); err != nil {
		return state, err
	}
	if !available {
		var raw string
		err := h.Pool.QueryRow(ctx, `SELECT pricing_version FROM billing_pricing_switch WHERE singleton`).Scan(&raw)
		state.EntryOpen = raw == "legacy"
		return state, err
	}
	err := h.Pool.QueryRow(ctx, `SELECT pricing_version,entry_open,epoch FROM billing_pricing_public_state()`).Scan(&state.Version, &state.EntryOpen, &state.Epoch)
	return state, err
}

func lockPricingEntry(ctx context.Context, tx pgx.Tx) (string, bool, error) {
	var raw string
	if err := tx.QueryRow(ctx, `SELECT pricing_version FROM billing_pricing_switch WHERE singleton FOR SHARE`).Scan(&raw); err != nil {
		return "", false, err
	}
	var available bool
	if err := tx.QueryRow(ctx, `SELECT to_regprocedure('billing_pricing_public_state()') IS NOT NULL`).Scan(&available); err != nil {
		return "", false, err
	}
	if !available {
		return raw, raw == "legacy", nil
	}
	var singleton bool
	if err := tx.QueryRow(ctx, `SELECT singleton FROM billing_pricing_adoption WHERE singleton FOR SHARE`).Scan(&singleton); err != nil {
		return "", false, err
	}
	var open bool
	if err := tx.QueryRow(ctx, `SELECT entry_open FROM billing_pricing_public_state()`).Scan(&open); err != nil {
		return "", false, err
	}
	return raw, open, nil
}

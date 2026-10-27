package api

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// teamPricingState is descriptive. Every new financial acceptance repeats the
// same resolution with retained database locks, before a provider request.
func (h *Handlers) teamPricingState(ctx context.Context, team uuid.UUID) (pricingPublicState, error) {
	var available bool
	if err := h.Pool.QueryRow(ctx, `SELECT to_regprocedure('billing_team_pricing_state(uuid)') IS NOT NULL`).Scan(&available); err != nil {
		return pricingPublicState{}, err
	}
	if !available {
		return h.publicPricingState(ctx)
	}
	return scanTeamPricing(h.Pool.QueryRow(ctx, `SELECT * FROM billing_team_pricing_state($1)`, team), team)
}

func scanTeamPricing(row pgx.Row, team uuid.UUID) (pricingPublicState, error) {
	s := pricingPublicState{Team: team}
	var epoch, enrollment *uuid.UUID
	var open bool
	err := row.Scan(&s.Version, &s.Source, &open, &epoch, &enrollment)
	if epoch != nil {
		s.Epoch = *epoch
	}
	s.EnrollmentEpoch = enrollment
	s.EntryOpen = open && s.Version == "v2"
	s.LegacyOpen = open && s.Source == "legacy"
	return s, err
}

func lockTeamPricingEntry(ctx context.Context, tx pgx.Tx, team uuid.UUID) (pricingPublicState, error) {
	var available bool
	if err := tx.QueryRow(ctx, `SELECT to_regprocedure('billing_lock_team_pricing_state(uuid)') IS NOT NULL`).Scan(&available); err != nil {
		return pricingPublicState{}, err
	}
	if !available {
		raw, open, err := lockPricingEntry(ctx, tx)
		return pricingPublicState{Version: raw, EntryOpen: open, LegacyOpen: raw == "legacy", Team: team}, err
	}
	return scanTeamPricing(tx.QueryRow(ctx, `SELECT * FROM billing_lock_team_pricing_state($1)`, team), team)
}

func assertTeamAdmission(ctx context.Context, tx pgx.Tx, expected pricingPublicState) error {
	actual, err := lockTeamPricingEntry(ctx, tx, expected.Team)
	if err != nil {
		return err
	}
	if actual.Version != expected.Version || actual.Source != expected.Source || actual.Epoch != expected.Epoch ||
		(!actual.EntryOpen && !actual.LegacyOpen) {
		return errProEntryClosed
	}
	if actual.EntryOpen {
		var epoch uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT billing_assert_team_pricing_entry($1)`, expected.Team).Scan(&epoch); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handlers) signupPricingState(ctx context.Context) (pricingPublicState, error) {
	s, err := h.publicPricingState(ctx)
	if err != nil || s.EntryOpen || !s.LegacyOpen {
		return s, err
	}
	var available bool
	if err = h.Pool.QueryRow(ctx, `SELECT to_regprocedure('billing_new_team_ready()') IS NOT NULL`).Scan(&available); err != nil || !available {
		return s, err
	}
	var epoch uuid.UUID
	err = h.Pool.QueryRow(ctx, `SELECT epoch FROM billing_new_team_gate WHERE singleton AND billing_new_team_ready()`).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return pricingPublicState{Version: "v2", Source: "new_team", EntryOpen: true, Epoch: epoch}, nil
}

func pricingSource(s pricingPublicState) string {
	if s.Source != "" {
		return s.Source
	}
	if s.EntryOpen {
		return "all_teams"
	}
	return "legacy"
}
func pricingEpoch(s pricingPublicState) *uuid.UUID {
	if s.Epoch == uuid.Nil {
		return nil
	}
	e := s.Epoch
	return &e
}

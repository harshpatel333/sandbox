package api

import (
	"context"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/superserve-ai/sandbox/internal/db"
)

// ProviderPairEntitlementAuthority is supplied by the financial-authority adapter.
// It reads current committed shared authority before opening the regional tx,
// validates the exact binding/phase/permission, then locks team/account -> the
// finalized permanent target fence. It calls action only while authority is valid
// and holds the fence through commit. No network I/O belongs in action.
type ProviderPairEntitlementAuthority interface {
	WithProviderPairAuthority(context.Context, ContractProviderBinding, func(context.Context, pgx.Tx) error) error
}

func (h *Handlers) withProviderPairEntitlementAuthority(ctx context.Context, binding ContractProviderBinding, action func(context.Context, pgx.Tx) error) error {
	if h.ProviderPairEntitlementAuthority == nil {
		return errors.New("paired entitlement financial authority adapter unavailable")
	}
	return h.ProviderPairEntitlementAuthority.WithProviderPairAuthority(ctx, binding, func(ctx context.Context, tx pgx.Tx) error {
		if tx == nil {
			return errors.New("paired entitlement requires regional authority transaction")
		}
		saved, err := loadContractProviderBinding(ctx, tx, binding.ContractID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(saved, binding) {
			return errors.New("paired entitlement binding changed")
		}
		var allowed bool
		if err = tx.QueryRow(ctx, `SELECT c.status='active' AND (c.ends_at IS NULL OR c.ends_at>clock_timestamp())
   AND NOT EXISTS(SELECT 1 FROM billing_provider_pair_revocation r WHERE r.contract_id=c.id)
   FROM billing_commercial_contract c WHERE c.id=$1 AND c.team_id=$2 FOR UPDATE`, binding.ContractID, binding.Identity.TeamID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return errors.New("paired entitlement authority is inactive or revoked")
		}
		// The adapter has proved current shared authority and the permanent finalized
		// fence. Refresh only observation freshness; never extend the frozen term.
		if _, err = tx.Exec(ctx, `UPDATE billing_provider_pair_waiver SET authority_checked_at=clock_timestamp(),authority_valid_until=least(ends_at,clock_timestamp()+interval '30 seconds')
   WHERE contract_id=$1 AND team_id=$2 AND revoked_at IS NULL AND starts_at<=clock_timestamp() AND ends_at>clock_timestamp()`, binding.ContractID, binding.Identity.TeamID); err != nil {
			return err
		}
		return action(ctx, tx)
	})
}

func (h *Handlers) prepareProviderPairEntitlement(ctx context.Context, b ContractProviderBinding, params db.BeginImmediateUsageEntitlementIssuanceParams, prices []map[string]string) (db.BillingUsageEntitlement, error) {
	var e db.BillingUsageEntitlement
	err := h.withProviderPairEntitlementAuthority(ctx, b, func(ctx context.Context, tx pgx.Tx) error {
		q := h.DB.WithTx(tx)
		var err error
		e, err = q.ClaimCurrentUsageEntitlement(ctx, db.ClaimCurrentUsageEntitlementParams{TeamID: b.Identity.TeamID, ContractID: b.ContractID})
		if errors.Is(err, pgx.ErrNoRows) {
			e, err = q.GetCurrentUsageEntitlement(ctx, db.GetCurrentUsageEntitlementParams{TeamID: b.Identity.TeamID, Kind: "metered_usage"})
		}
		if err != nil {
			return err
		}
		if e.ContractID != b.ContractID {
			return errors.New("current allowance belongs to a different contract")
		}
		if e.State == "pending" {
			params.EntitlementID = e.ID
			e, err = q.BeginImmediateUsageEntitlementIssuance(ctx, params)
			if err != nil {
				return err
			}
			_, err = entitlementGrantForm(e.RequestPayload, prices)
			return err
		}
		if e.State == "uncertain" {
			e, err = q.GetReplayableUsageEntitlement(ctx, db.GetReplayableUsageEntitlementParams{TeamID: b.Identity.TeamID, EntitlementID: e.ID})
		}
		return err
	})
	return e, err
}

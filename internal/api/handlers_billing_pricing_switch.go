package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog/log"
)

type pricingSwitchState struct {
	PricingVersion string  `json:"pricing_version"`
	ChangedBy      *string `json:"changed_by"`
	Reason         *string `json:"reason"`
	ChangedAt      string  `json:"changed_at"`
}

// GetPlatformPricingVersion reports this region's pricing switch.
func (h *Handlers) GetPlatformPricingVersion(c *gin.Context) {
	if _, ok := h.requirePlatformBilling(c, platformBillingReadPermission); !ok {
		return
	}
	h.writePricingSwitch(c)
}

func (h *Handlers) writePricingSwitch(c *gin.Context) {
	var s pricingSwitchState
	err := h.Pool.QueryRow(c.Request.Context(), `SELECT pricing_version,changed_by::text,reason,to_char(changed_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"') FROM billing_pricing_switch`).
		Scan(&s.PricingVersion, &s.ChangedBy, &s.Reason, &s.ChangedAt)
	if err != nil {
		log.Error().Err(err).Msg("read pricing switch failed")
		respondError(c, ErrInternal)
		return
	}
	c.JSON(http.StatusOK, s)
}

// SetPlatformPricingVersion flips this region's pricing switch. Setting the
// version it already has changes nothing, so a retry is safe.
func (h *Handlers) SetPlatformPricingVersion(c *gin.Context) {
	actor, ok := h.requirePlatformBilling(c, platformBillingWritePermission)
	if !ok {
		return
	}
	var req struct {
		PricingVersion string `json:"pricing_version"`
		Reason         string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || (req.PricingVersion != "legacy" && req.PricingVersion != "v2") {
		respondErrorMsg(c, "bad_request", "pricing_version must be legacy or v2", http.StatusBadRequest)
		return
	}
	reason, ok := checkCatalogReason(c, req.Reason)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		log.Error().Err(err).Msg("begin pricing transition failed")
		respondErrorMsg(c, "service_unavailable", "Pricing transition is temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT pricing_version,changed_by::text,reason,changed_at,epoch,adoption_state,changed
 FROM public.billing_transition_pricing_version($1,$2,$3)`, req.PricingVersion, actor, reason)
	if err != nil {
		respondPricingTransitionError(c, err)
		return
	}
	var changed bool
	state, err := pgx.CollectExactlyOneRow(rows, func(row pgx.CollectableRow) (pricingSwitchState, error) {
		var state pricingSwitchState
		var at time.Time
		var epoch uuid.UUID
		var adoptionState string
		err := row.Scan(&state.PricingVersion, &state.ChangedBy, &state.Reason, &at, &epoch, &adoptionState, &changed)
		state.ChangedAt = at.UTC().Format("2006-01-02T15:04:05Z")
		return state, err
	})
	if err != nil {
		respondPricingTransitionError(c, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		log.Error().Err(err).Msg("commit pricing transition failed")
		respondErrorMsg(c, "service_unavailable", "Pricing transition is temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if changed {
		forgetPricingVersion()
	}
	c.JSON(http.StatusOK, state)
}

func respondPricingTransitionError(c *gin.Context, err error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "22023" && pgErr.ConstraintName == "billing_pricing_transition_input":
			respondErrorMsg(c, "bad_request", "Invalid pricing transition input", http.StatusBadRequest)
			return
		case pgErr.Code == "42501" && pgErr.ConstraintName == "billing_pricing_transition_actor":
			respondError(c, ErrForbidden)
			return
		case pgErr.Code == "55000" && pgErr.ConstraintName == "billing_pricing_transition_prerequisite":
			respondErrorMsg(c, "conflict", "Pricing transition prerequisites are not satisfied", http.StatusConflict)
			return
		}
	}
	log.Error().Err(err).Msg("pricing transition failed")
	respondErrorMsg(c, "service_unavailable", "Pricing transition is temporarily unavailable", http.StatusServiceUnavailable)
}

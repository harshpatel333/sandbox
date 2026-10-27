package api

import (
	"github.com/gin-gonic/gin"
	"net/http"
)

// GetBillingCapabilities supplies current-team admission and its matching
// catalog. These capabilities do not replace billing permissions or previews.
func (h *Handlers) GetBillingCapabilities(c *gin.Context) {
	team, ok := h.requireBillingRead(c)
	if !ok {
		return
	}
	state, err := h.teamPricingState(c.Request.Context(), team)
	if err != nil {
		respondErrorMsg(c, "service_unavailable", "team plan admission requires verification", http.StatusServiceUnavailable)
		return
	}
	catalog, err := h.billingCatalog(c.Request.Context(), state.Version, state.EntryOpen || state.LegacyOpen)
	if err != nil {
		respondError(c, ErrInternal)
		return
	}
	fresh := state.Version == "v2" && state.EntryOpen
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"schema_version": 1, "team_id": team, "pricing_version": state.Version,
		"admission_source": pricingSource(state), "entry_open": state.EntryOpen || state.LegacyOpen,
		"epoch": pricingEpoch(state), "enrollment_epoch": state.EnrollmentEpoch,
		"fresh_entry": gin.H{"starter": fresh, "pro": fresh, "auto_recharge": fresh, "choose_free": fresh},
		"actions":     gin.H{"recover_payment": true, "disable_auto_recharge": true}, "catalog": catalog, "offers": catalogOffers(catalog.Cards)})
}

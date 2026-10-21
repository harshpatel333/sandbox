package api

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/superserve-ai/sandbox/internal/billing/provisioning"
)

// ProvisionAcceptedOffer is an internal durable-consumer boundary. The configured
// adapter resolves exact acceptance and assignment authority from server storage.
func (h *Handlers) ProvisionAcceptedOffer(ctx context.Context, team, assignment uuid.UUID) (uuid.UUID, error) {
	return h.OfferProvisioning.Enqueue(ctx, team, assignment)
}
func (h *Handlers) AcceptedOfferProvisioningStatus(ctx context.Context, team, assignment uuid.UUID) (provisioning.Status, error) {
	return h.OfferProvisioning.Status(ctx, team, assignment)
}

func (h *Handlers) startOfferProvisioning(ctx context.Context) {
	service := h.OfferProvisioning
	if !service.Available() {
		return
	}
	go func() {
		for ctx.Err() == nil {
			started := time.Now()
			count, err := service.RunBatch(ctx)
			currentBillingRecorder().RecordBillingWork(ctx, "offer_provisioning", err != nil, time.Since(started), int64(count))
			if err != nil {
				log.Warn().Msg("offer provisioning reconciliation requires recovery")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
}

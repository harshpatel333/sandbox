package api

import (
	"context"

	"github.com/google/uuid"
)

var prepaidPricingV2Enabled = func(ctx context.Context, h *Handlers) (bool, error) {
	return false, nil
}

var prepaidPlanAllowsPurchase = func(ctx context.Context, h *Handlers, team uuid.UUID) (bool, error) {
	return false, nil
}

var assignStarterAfterPurchase = func(ctx context.Context, h *Handlers, team, purchase uuid.UUID) error {
	return nil
}

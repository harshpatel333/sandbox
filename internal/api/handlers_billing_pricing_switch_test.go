package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPricingTransitionErrorRequiresMatchingConstraint(t *testing.T) {
	for _, tc := range []struct {
		code, marker string
		status       int
	}{
		{"22023", "billing_pricing_transition_input", 400},
		{"42501", "billing_pricing_transition_actor", 403},
		{"55000", "billing_pricing_transition_prerequisite", 409},
		{"22023", "", 503}, {"42501", "", 503}, {"55000", "", 503},
		{"22023", "billing_pricing_transition_actor", 503},
		{"42501", "billing_pricing_transition_prerequisite", 503},
		{"55000", "billing_pricing_transition_input", 503},
		{"42P01", "", 503}, {"42883", "", 503},
		{"40P01", "", 503}, {"40001", "", 503}, {"57014", "", 503}, {"55P03", "", 503},
	} {
		t.Run(tc.code+"/"+tc.marker, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			err := fmt.Errorf("RPC: %w", &pgconn.PgError{Code: tc.code, ConstraintName: tc.marker, Message: "database-private-error"})
			respondPricingTransitionError(c, err)
			if w.Code != tc.status || strings.Contains(w.Body.String(), "database-private-error") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
		})
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	respondPricingTransitionError(c, context.Canceled)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("cancel=%d", w.Code)
	}
}

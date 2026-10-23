//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/superserve-ai/sandbox/internal/api"
	"github.com/superserve-ai/sandbox/internal/config"
)

const checkoutSubmitMessage = "Not ready? Use the back arrow to continue with $5 in credit."

func TestIntegration_CheckoutSubmitMessageFollowsGeneration(t *testing.T) {
	for _, path := range []string{"legacy_route", "publication_decision"} {
		// "disabled" begins without the message; "flag_off" begins with it and
		// replays after the setting is turned off; "null" and "stale" model a
		// generation begun by a writer that predates the message. The message
		// promises the signup trial credit, so "spent", "expired", "no_grant"
		// and "trial_ended" begin without it.
		for _, generation := range []string{"new", "disabled", "flag_off", "null", "stale", "spent", "expired", "no_grant", "trial_ended"} {
			t.Run(path+"/"+generation, func(t *testing.T) {
				ctx := context.Background()
				f := newPublicationCheckoutFixture(t)
				router := func(enabled bool) *gin.Engine {
					return newBillingRouterWithConfig(t, f.stripe, testPool, func(cfg *config.Config) {
						cfg.StripeCheckoutSubmitMessage = enabled
					})
				}
				f.router = router(generation != "disabled")
				if generation != "no_grant" {
					remaining, expires := "5", "NULL"
					switch generation {
					case "spent":
						remaining = "4.99"
					case "expired":
						expires = "now() - interval '1 minute'"
					}
					if _, err := testPool.Exec(ctx, `INSERT INTO team_credit_grant(team_id,amount_usd,remaining_usd,reason,expires_at) VALUES($1,5,`+remaining+`,'signup trial credit',`+expires+`)`, f.team); err != nil {
						t.Fatal(err)
					}
				}
				if generation == "trial_ended" {
					if _, err := testPool.Exec(ctx, `INSERT INTO team_billing_account(team_id,trial_ended_at) VALUES($1,now()) ON CONFLICT(team_id) DO UPDATE SET trial_ended_at=now()`, f.team); err != nil {
						t.Fatal(err)
					}
				}
				request := func() {
					t.Helper()
					var code int
					if path == "legacy_route" {
						code = do(f.router, "POST", "/stripe/checkout-session", f.key, checkoutRegressionBody).Code
					} else {
						code = f.request(t, f.body("standard"), f.actor, f.key, nil).Code
					}
					if code != http.StatusOK {
						t.Fatalf("checkout request: %d", code)
					}
				}
				request()
				want := checkoutSubmitMessage
				switch generation {
				case "disabled", "spent", "expired", "no_grant", "trial_ended":
					want = ""
				case "flag_off":
					f.router = router(false)
				case "null", "stale":
					query := `UPDATE team_billing_account SET checkout_submit_message_generation = NULL WHERE team_id = $1`
					if generation == "stale" {
						query = `UPDATE team_billing_account SET checkout_submit_message_generation = checkout_initializing_at - interval '1 hour' WHERE team_id = $1`
					}
					if _, err := testPool.Exec(ctx, query, f.team); err != nil {
						t.Fatal(err)
					}
					f.stripe.mu.Lock()
					f.stripe.checkoutCalls, f.stripe.requests, f.stripe.sessions = nil, nil, nil
					f.stripe.mu.Unlock()
					request()
					want = ""
				}
				request()
				f.stripe.assertCalls(t, 2)
				calls := []api.StripeCreateCheckoutSessionParams{f.stripe.checkoutCalls[0], f.stripe.checkoutCalls[1]}
				for i, call := range calls {
					if call.SubmitMessage != want {
						t.Fatalf("call %d submit message = %q, want %q", i+1, call.SubmitMessage, want)
					}
				}
			})
		}
	}
}

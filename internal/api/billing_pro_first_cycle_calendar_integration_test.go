//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/superserve-ai/sandbox/internal/billing"
)

func TestProFirstCycleDelayedProviderCreationKeepsMonthEndCalendar(t *testing.T) {
	for _, year := range []int{2027, 2028} {
		t.Run(fmt.Sprint(year), func(t *testing.T) {
			anchor := time.Date(year, 1, 31, 12, 0, 0, 0, time.UTC)
			h, team, offer, params := newProFirstCycleQuoteFixture(t, anchor)
			requestProFirstCycleQuote(t, h, team, offer, params, http.StatusAccepted)
			u, err := h.proUpgrade(t.Context(), team)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(u.Payload, &params); err != nil {
				t.Fatal(err)
			}
			params.ResourceTerms = []StripeCheckoutResourceTerms{{PriceID: "price_cpu", MeterID: "meter_cpu", EventName: "cpu", UnitAmountDecimal: "1"}}
			var sub invoiceSubscription
			if err = json.Unmarshal([]byte(`{"id":"sub_example","customer":"cus_example","status":"active","billing_mode":{"type":"flexible"},"billing_cycle_anchor_config":{"day_of_month":31,"hour":12,"minute":0,"second":0,"month":null},"items":{"data":[{"price":{"id":"price_cpu","currency":"usd","billing_scheme":"per_unit","unit_amount_decimal":"1","recurring":{"usage_type":"metered","interval":"month","interval_count":1,"meter":"meter_cpu"}}}]}}`), &sub); err != nil {
				t.Fatal(err)
			}
			sub.ID = "sub_" + team.String()
			sub.Customer = u.Customer
			sub.StartDate = anchor.Unix()
			// Stripe exposes March31 as its anchor but its first full period ends in
			// February. This response shape was separately verified with test clocks.
			sub.BillingCycleAnchor = time.Date(year, 3, 31, 12, 0, 0, 0, time.UTC).Unix()
			sub.Items.Data[0].Start, sub.Items.Data[0].End = anchor.Unix(), u.End.Unix()
			creates := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions":
					_, _ = w.Write([]byte(`{"data":[]}`))
				case r.Method == http.MethodPost && r.URL.Path == "/v1/subscriptions":
					creates++
					_ = r.ParseForm()
					for key, want := range map[string]string{"backdate_start_date": fmt.Sprint(anchor.Unix()), "billing_cycle_anchor_config[day_of_month]": "31", "billing_cycle_anchor_config[hour]": "12", "billing_cycle_anchor_config[minute]": "0", "billing_cycle_anchor_config[second]": "0", "billing_mode[type]": "flexible", "proration_behavior": "none"} {
						if r.Form.Get(key) != want {
							t.Errorf("%s=%q want%q", key, r.Form.Get(key), want)
						}
					}
					if r.Form.Has("billing_cycle_anchor") {
						t.Error("explicit clamped timestamp destroys original month-end day")
					}
					_ = json.NewEncoder(w).Encode(sub)
				case r.Method == http.MethodPost && r.URL.Path == "/v1/subscriptions/"+sub.ID:
					_ = json.Unmarshal([]byte(`{"behavior":"keep_as_draft"}`), &sub.PauseCollection)
					_ = json.NewEncoder(w).Encode(sub)
				case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions/"+sub.ID:
					_ = json.NewEncoder(w).Encode(sub)
				default:
					t.Errorf("unexpected provider request %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			client := &stripeHTTPClient{baseURL: srv.URL, httpClient: srv.Client(), apiVersion: "2026-05-27.dahlia"}
			h.Now = func() time.Time { return anchor.Add(5 * time.Second) }
			conn, err := h.Pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			for _, delay := range []time.Duration{5 * time.Second, time.Hour} {
				h.Now = func() time.Time { return anchor.Add(delay) }
				if _, err = h.prepareProUpgradeSubscription(t.Context(), conn, &u, params, client); err != nil {
					t.Fatalf("delayed creation calendar failed: %v", err)
				}
			}
			if creates != 1 {
				t.Fatalf("replayed provider creation: %d", creates)
			}
			for _, at := range []time.Time{anchor, u.End, time.Date(year, 3, 31, 12, 0, 0, 0, time.UTC)} {
				start, end, ok := providerInvoicePeriod(sub, at)
				wantStart, wantEnd, _ := billing.AnniversaryPeriod(anchor, at)
				if !ok || !start.Equal(wantStart) || !end.Equal(wantEnd) {
					t.Fatalf("provider drift at%s: %s->%s", at, start, end)
				}
			}
			sub.BillingCycleAnchorConfig.Second++
			if _, _, ok := providerInvoicePeriod(sub, anchor); ok {
				t.Fatal("accepted a configured calendar differing from frozen start")
			}
		})
	}
}

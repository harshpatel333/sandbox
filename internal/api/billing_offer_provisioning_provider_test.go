package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/superserve-ai/sandbox/internal/billing/provisioning"
)

func offerProviderFixture(t *testing.T) (provisioning.Operation, *pairFixture, map[provisioning.Step]json.RawMessage) {
	t.Helper()
	f := newPairFixture(t)
	f.p.UsageSubscriptionID = "sub_usageExample"
	f.usage.ID = f.p.UsageSubscriptionID
	f.p.WaiverCouponID = "offer_" + f.p.Identity.OperationID.String()
	f.invoice.Discounts[0].Coupon = f.p.WaiverCouponID
	marshal := func(v any) json.RawMessage {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	o := provisioning.Operation{ID: f.p.Identity.OperationID, TeamID: f.p.Identity.TeamID, AssignmentID: f.p.Identity.AssignmentID, Offer: provisioning.AcceptedOffer{
		AcceptanceID: f.p.Identity.AcceptanceID, TermsDigest: f.p.Identity.TermsDigest, Merchant: f.p.Identity.Merchant, Live: f.p.Identity.Live, APIVersion: f.p.Identity.APIVersion,
		Snapshot: marshal(map[string]any{"team_id": f.p.Identity.TeamID, "assignment_id": f.p.Identity.AssignmentID, "acceptance_id": f.p.Identity.AcceptanceID, "approval_revision": f.p.Identity.ApprovalSemanticRevision}), ProviderTerms: marshal(f.p.Terms),
	}, Requests: map[provisioning.Step]provisioning.SavedRequest{}}
	createdUsage := f.usage
	createdUsage.PauseCollection = nil
	return o, f, map[provisioning.Step]json.RawMessage{
		provisioning.Customer: marshal(map[string]any{"id": f.p.CustomerID, "livemode": false, "metadata": pairMetadata(f.p.Identity, "customer"), "email": "private@example.com"}),
		provisioning.Waiver:   marshal(map[string]any{"id": f.p.WaiverCouponID, "livemode": false, "metadata": pairMetadata(f.p.Identity, "waiver"), "duration": "once", "percent_off": 100, "applies_to": map[string]any{"products": []string{f.p.Terms.FeeProductID}}}),
		provisioning.Fee:      marshal(f.fee), provisioning.Usage: marshal(createdUsage), provisioning.UsageHold: marshal(f.usage),
	}
}

func saveOfferTestRequest(t *testing.T, p *offerProvisioningProvider, o *provisioning.Operation, step provisioning.Step) provisioning.SavedRequest {
	t.Helper()
	request, err := p.Prepare(t.Context(), *o, step)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	return provisioning.SavedRequest{OperationID: o.ID, Step: step, Key: fmt.Sprintf("offer-provisioning/%s/%s/v1", o.ID, step), Offer: o.Offer, Request: request, State: "dispatched", DispatchedAt: &now}
}

func TestOfferProviderBuildsOriginalPairAndProjectsReceipts(t *testing.T) {
	o, f, responses := offerProviderFixture(t)
	p := &offerProvisioningProvider{}
	for _, step := range []provisioning.Step{provisioning.Customer, provisioning.Waiver, provisioning.Fee, provisioning.Usage, provisioning.UsageHold} {
		r := saveOfferTestRequest(t, p, &o, step)
		form, _ := url.ParseQuery(r.Request.Payload)
		switch step {
		case provisioning.Fee:
			if form.Get("items[0][price]") != f.p.Terms.FeePriceID || form.Get("discounts[0][coupon]") != f.p.WaiverCouponID || form.Get("items[0][quantity]") != "1" || form.Get("items[1][price]") != "" {
				t.Fatal("fee mixes usage or loses annual waiver")
			}
		case provisioning.Usage:
			if form.Get("backdate_start_date") != strconv.FormatInt(f.p.Anchor.Unix(), 10) || form.Get("items[3][price]") != f.p.Terms.Rounding.PriceID || form.Get("items[4][price]") != "" || form.Get("discounts[0][coupon]") != "" {
				t.Fatal("usage shifted original anchor or mixed fee")
			}
		case provisioning.UsageHold:
			if r.Request.Path != "/v1/subscriptions/"+f.p.UsageSubscriptionID || len(form) != 2 || form.Get("pause_collection[behavior]") != "keep_as_draft" {
				t.Fatal("hold is not exact and durable")
			}
		}
		receipt, err := projectOfferProviderReceipt(r, responses[step])
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		var fields map[string]any
		_ = json.Unmarshal(receipt, &fields)
		if fields["email"] != nil || fields["metadata"] != nil {
			t.Fatal("unnecessary provider account data persisted")
		}
		r.State, r.Receipt = "confirmed", receipt
		o.Requests[step] = r
	}
	input, err := offerPairInput(o)
	if err != nil || !reflect.DeepEqual(input, f.p) {
		t.Fatalf("original identity/calendar changed: %#v %v", input, err)
	}
	// A future verifier receives the actual sealed object, not a JSON reconstruction.
	client := f.client(t)
	// This fixture server uses the dependency's original coupon endpoint.
	f.p.WaiverCouponID = "waiver_example"
	f.invoice.Discounts[0].Coupon = f.p.WaiverCouponID
	r := o.Requests[provisioning.Waiver]
	r.Receipt = json.RawMessage(`{"id":"waiver_example"}`)
	o.Requests[provisioning.Waiver] = r
	p.client = client
	v, err := p.Verify(t.Context(), o)
	proof, ok := v.Proof.(VerifiedProviderPair)
	if err != nil || !ok || proof.seal == ([32]byte{}) || proof.seal != pairSeal(proof) || proof.Input.Identity.OperationID != o.ID {
		t.Fatalf("sealed proof lost: %v", err)
	}
}

func TestOfferProviderRecoveryNeverWritesAndRequiresCompleteUniqueInventory(t *testing.T) {
	for _, mode := range []string{"found", "paginated", "missing", "duplicate", "wrong_acceptance", "missing_mode", "endless", "truncated_schema"} {
		t.Run(mode, func(t *testing.T) {
			o, _, responses := offerProviderFixture(t)
			p := &offerProvisioningProvider{}
			r := saveOfferTestRequest(t, p, &o, provisioning.Customer)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet {
					t.Fatal("recovery attempted POST")
				}
				var value any
				switch req.URL.Path {
				case "/v1/account":
					value = map[string]any{"id": o.Offer.Merchant}
				case "/v1/balance":
					value = map[string]any{"livemode": o.Offer.Live}
				case "/v1/customers":
					calls++
					var customer map[string]any
					_ = json.Unmarshal(responses[provisioning.Customer], &customer)
					more := false
					data := []any{customer}
					switch mode {
					case "paginated":
						if calls == 1 {
							data = []any{map[string]any{"id": "cus_unrelated"}}
							more = true
						} else if req.URL.Query().Get("starting_after") != "cus_unrelated" {
							t.Error("lost pagination cursor")
						}
					case "missing":
						data = []any{}
					case "duplicate":
						other := map[string]any{"id": "cus_duplicate", "livemode": false, "metadata": customer["metadata"]}
						data = append(data, other)
					case "wrong_acceptance":
						customer["metadata"].(map[string]any)["acceptance_id"] = "wrong"
					case "missing_mode":
						delete(customer, "livemode")
					case "endless":
						more = true
						data = []any{map[string]any{"id": fmt.Sprintf("cus_page%d", calls)}}
					case "truncated_schema":
						value = map[string]any{"data": data}
					}
					if value == nil {
						value = map[string]any{"data": data, "has_more": more}
					}
				default:
					t.Error("unexpected recovery endpoint")
					http.NotFound(w, req)
					return
				}
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer server.Close()
			p.client = &stripeHTTPClient{baseURL: server.URL, secretKey: "local-synthetic", apiVersion: o.Offer.APIVersion, httpClient: server.Client()}
			receipt, err := p.Recover(t.Context(), r)
			if mode == "found" || mode == "paginated" {
				if err != nil || len(receipt) == 0 {
					t.Fatal(err)
				}
			} else if !errors.Is(err, provisioning.ErrUnknown) {
				t.Fatalf("uncertain inventory accepted: %v", err)
			}
			if calls > 20 {
				t.Fatal("unbounded inventory traversal")
			}
		})
	}
}

func TestOfferProviderRejectsReceiptScopeAndChangedCalendar(t *testing.T) {
	o, _, responses := offerProviderFixture(t)
	p := &offerProvisioningProvider{}
	for _, step := range []provisioning.Step{provisioning.Customer, provisioning.Waiver, provisioning.Fee, provisioning.Usage, provisioning.UsageHold} {
		r := saveOfferTestRequest(t, p, &o, step)
		var changed map[string]any
		_ = json.Unmarshal(responses[step], &changed)
		changed["livemode"] = true
		bad, _ := json.Marshal(changed)
		if _, err := projectOfferProviderReceipt(r, bad); err == nil {
			t.Fatalf("%s accepted wrong merchant mode", step)
		}
		if step == provisioning.UsageHold {
			changed["livemode"] = false
			changed["pause_collection"] = map[string]any{"behavior": "keep_as_draft", "resumes_at": 1}
			bad, _ = json.Marshal(changed)
			if _, err := projectOfferProviderReceipt(r, bad); err == nil {
				t.Fatal("temporary hold accepted")
			}
		}
		receipt, err := projectOfferProviderReceipt(r, responses[step])
		if err != nil {
			t.Fatal(err)
		}
		r.State, r.Receipt = "confirmed", receipt
		o.Requests[step] = r
	}
	changed := o
	changed.Offer = o.Offer
	changed.Offer.Snapshot = json.RawMessage(`{}`)
	if _, err := p.Prepare(t.Context(), changed, provisioning.Customer); err == nil {
		t.Fatal("missing acceptance binding accepted")
	}
}

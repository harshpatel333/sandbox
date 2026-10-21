package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

type fixtureRenewalSteps struct {
	*fixtureRenewalProvider
	snapshot     renewalScheduleSnapshot
	now          func() renewalChoiceProof
	calls        atomic.Int32
	observeCalls atomic.Int32
	loseCreate   bool
	observed     bool
}

func renewalStepSnapshot(k renewalChoiceKey, op renewalChoiceOperation) renewalScheduleSnapshot {
	return renewalScheduleSnapshot{BindingHash: renewalHash(op.Binding), Anchor: op.Binding.Anchor, Boundary: k.Boundary, CurrentPhase: url.Values{"phases[0][start_date]": {strconv.FormatInt(op.Binding.Anchor.Unix(), 10)}, "phases[0][end_date]": {strconv.FormatInt(k.Boundary.Unix(), 10)}, "phases[0][items][0][price]": {"price_annual"}, "phases[0][items][0][quantity]": {"1"}, "phases[0][collection_method]": {"charge_automatically"}, "phases[0][discounts][0][coupon]": {"coupon_initial"}}}
}
func (p *fixtureRenewalSteps) PrepareStep(_ context.Context, k renewalChoiceKey, op renewalChoiceOperation) (renewalChoiceStepRequest, error) {
	snap := renewalStepSnapshot(k, op)
	if len(op.Steps) > 0 {
		snap.Schedule = "sub_sched_owned"
		snap.Owned = true
	}
	return buildRenewalScheduleStep(k, op, snap)
}
func (p *fixtureRenewalSteps) proof(k renewalChoiceKey, op renewalChoiceOperation, r renewalChoiceStepRequest) renewalChoiceStepProof {
	base := p.now()
	proof := renewalChoiceStepProof{RequestHash: renewalHash(r), Outcome: "confirmed", Schedule: "sub_sched_owned", BindingHash: renewalHash(op.Binding), VerifiedAt: base.VerifiedAt, Evidence: json.RawMessage(`{"verified":true}`)}
	if r.Kind == "update_schedule" {
		v := fixtureRenewalProof(k, op, base.VerifiedAt)
		proof.Choice = &v
	}
	return proof
}
func (p *fixtureRenewalSteps) DispatchStep(_ context.Context, k renewalChoiceKey, op renewalChoiceOperation, r renewalChoiceStepRequest) (renewalChoiceStepProof, error) {
	p.calls.Add(1)
	if p.loseCreate && r.Kind == "create_schedule" {
		return renewalChoiceStepProof{}, errors.New("response lost")
	}
	return p.proof(k, op, r), nil
}
func (p *fixtureRenewalSteps) ObserveStep(_ context.Context, k renewalChoiceKey, op renewalChoiceOperation, r renewalChoiceStepRequest) (renewalChoiceStepProof, error) {
	p.observeCalls.Add(1)
	proof := p.proof(k, op, r)
	if !p.observed {
		proof.Outcome = "no_effect"
	}
	return proof, nil
}

func TestRenewalChoiceStagedCreationRecovery(t *testing.T) {
	s, store, a := newRenewalFixture(t)
	submitRenewal(t, s, a, "free")
	p := &fixtureRenewalSteps{fixtureRenewalProvider: &fixtureRenewalProvider{}, loseCreate: true, now: func() renewalChoiceProof { return renewalChoiceProof{VerifiedAt: store.now} }}
	s.Provider = p
	for range 2 {
		if err := s.reconcile(t.Context(), a.key); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if err := s.reconcile(t.Context(), a.key); err != nil {
			t.Fatal(err)
		}
	}
	j, _ := s.status(t.Context(), a.key, a.actor)
	if p.calls.Load() != 1 || j.Requested.State != "outcome_unknown" || j.Confirmed.Request.Choice != "annual" {
		t.Fatal("lost creation was retried or adopted", j)
	}
	_, err := s.submit(t.Context(), a.key, a.actor, renewalChoiceRequest{ID: uuid.New(), Choice: "annual", Acceptance: a.authority.Acceptance, ApprovalRevision: a.authority.Revision, ChoiceRevision: j.Revision})
	if !errors.Is(err, errRenewalPending) {
		t.Fatal(err)
	}
	p.observed = true
	if err := s.reconcile(t.Context(), a.key); err != nil {
		t.Fatal(err)
	}
	j, _ = s.status(t.Context(), a.key, a.actor)
	if j.Confirmed.Request.Choice != "annual" || j.Requested.State != "dispatched" {
		t.Fatal("creation confirmed choice", j)
	}
	// A new service instance resumes from saved creation evidence without another create.
	restart := *s
	for range 2 {
		if err := restart.reconcile(t.Context(), a.key); err != nil {
			t.Fatal(err)
		}
	}
	j, _ = s.status(t.Context(), a.key, a.actor)
	if p.calls.Load() != 2 || j.Confirmed.Request.Choice != "free" || len(j.Requested.Steps) != 2 || j.Requested.Steps[0].Request.Idempotency == j.Requested.Steps[1].Request.Idempotency {
		t.Fatal(j)
	}
}

func TestRenewalChoiceScheduleWirePreservesTerm(t *testing.T) {
	s, _, a := newRenewalFixture(t)
	a.monthly = true
	j := submitRenewal(t, s, a, "monthly")
	op := *j.Requested
	snapshot := renewalStepSnapshot(a.key, op)
	snapshot.Schedule = "sub_sched_owned"
	snapshot.Owned = true
	request, err := buildRenewalScheduleStep(a.key, op, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	form, _ := url.ParseQuery(request.Form)
	for key, want := range map[string]string{"phases[0][discounts][0][coupon]": "coupon_initial", "phases[0][end_date]": strconv.FormatInt(a.key.Boundary.Unix(), 10), "phases[1][start_date]": strconv.FormatInt(a.key.Boundary.Unix(), 10), "phases[1][items][0][price]": "price_monthly", "phases[1][duration][interval_count]": "1", "proration_behavior": "none", "end_behavior": "release"} {
		if form.Get(key) != want {
			t.Fatalf("%s: %q", key, form.Get(key))
		}
	}
	if _, exists := form["phases[1][discounts]"]; !exists {
		t.Fatal("initial waiver not cleared")
	}
	snapshot.Owned = false
	if _, err = buildRenewalScheduleStep(a.key, op, snapshot); err == nil {
		t.Fatal("foreign schedule adopted")
	}
}

type fixtureRenewalVerifier struct{ calls atomic.Int32 }

func (v *fixtureRenewalVerifier) Inspect(context.Context, renewalChoiceKey, renewalChoiceOperation) (renewalScheduleSnapshot, error) {
	return renewalScheduleSnapshot{}, errRenewalDisabled
}
func (v *fixtureRenewalVerifier) VerifyStep(context.Context, renewalChoiceKey, renewalChoiceOperation, renewalChoiceStepRequest, []byte) (renewalChoiceStepProof, error) {
	v.calls.Add(1)
	return renewalChoiceStepProof{}, nil
}
func (v *fixtureRenewalVerifier) ObserveStep(context.Context, renewalChoiceKey, renewalChoiceOperation, renewalChoiceStepRequest) (renewalChoiceStepProof, error) {
	return renewalChoiceStepProof{}, nil
}
func (v *fixtureRenewalVerifier) ObserveChoice(context.Context, renewalChoiceKey, renewalChoiceOperation) (renewalChoiceProof, error) {
	return renewalChoiceProof{}, nil
}
func TestRenewalChoiceScheduleTransportSingleWrite(t *testing.T) {
	s, _, a := newRenewalFixture(t)
	j := submitRenewal(t, s, a, "free")
	op := *j.Requested
	r, err := buildRenewalScheduleStep(a.key, op, renewalStepSnapshot(a.key, op))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.Header.Get("Idempotency-Key") != r.Idempotency || req.Header.Get("Stripe-Version") != renewalStripeVersion {
			t.Error("request identity lost")
		}
		http.Redirect(w, req, "/redirect", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	verifier := &fixtureRenewalVerifier{}
	p := &renewalScheduleProvider{Client: &stripeHTTPClient{baseURL: server.URL, httpClient: server.Client(), apiVersion: renewalStripeVersion}, Merchant: op.Binding.Merchant, Verifier: verifier, BoundaryWritesVerified: true}
	if _, err = p.DispatchStep(t.Context(), a.key, op, r); err == nil {
		t.Fatal("redirect accepted")
	}
	if calls.Load() != 1 || verifier.calls.Load() != 0 {
		t.Fatal("redirect replayed")
	}
	p.BoundaryWritesVerified = false
	if _, err = p.DispatchStep(t.Context(), a.key, op, r); err == nil || calls.Load() != 1 {
		t.Fatal("unsafe write dispatched")
	}
}

func TestRenewalChoiceStagedUpgradePreservesUnknownFence(t *testing.T) {
	s, store, a := newRenewalFixture(t)
	submitRenewal(t, s, a, "free")
	s.Provider = &fixtureRenewalProvider{dispatch: func(renewalChoiceKey, renewalChoiceOperation) (renewalChoiceProof, error) {
		return renewalChoiceProof{}, errors.New("lost response")
	}}
	if err := s.reconcile(t.Context(), a.key); err != nil {
		t.Fatal(err)
	}
	p := &fixtureRenewalSteps{fixtureRenewalProvider: &fixtureRenewalProvider{}, now: func() renewalChoiceProof { return renewalChoiceProof{VerifiedAt: store.now} }}
	s.Provider = p
	if err := s.reconcile(t.Context(), a.key); !errors.Is(err, errRenewalPending) {
		t.Fatal(err)
	}
	j, _ := s.status(t.Context(), a.key, a.actor)
	if p.calls.Load() != 0 || len(j.Requested.Steps) != 0 || j.Requested.State != "outcome_unknown" {
		t.Fatal("adapter change released unknown fence", j)
	}
}

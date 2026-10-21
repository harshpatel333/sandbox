//go:build integration

package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/superserve-ai/sandbox/internal/billing/provisioning"
)

// Only provider configuration and execution are fixtures; admission, acceptance,
// assignment, the durable consumer and provisioning journal use real adapters.
type acceptedJournalAuthority struct {
	admission *YCAcceptedProvisioningAdmission
}

func (a acceptedJournalAuthority) WithAcceptedOffer(ctx context.Context, team, assignment uuid.UUID, f func(pgx.Tx, provisioning.AcceptedOffer) error) error {
	return a.admission.WithAcceptedOffer(ctx, team, assignment, func(tx pgx.Tx, o provisioning.AcceptedOffer) error { return f(tx, acceptedJournalMapping(o)) })
}
func (a acceptedJournalAuthority) WithDispatchGuard(ctx context.Context, team, assignment uuid.UUID, f func(provisioning.AcceptedOffer) error) error {
	return a.admission.WithDispatchGuard(ctx, team, assignment, func(o provisioning.AcceptedOffer) error { return f(acceptedJournalMapping(o)) })
}
func acceptedJournalMapping(o provisioning.AcceptedOffer) provisioning.AcceptedOffer {
	o.Merchant = "acct_synthetic"
	o.APIVersion = "synthetic-version"
	o.ProviderTerms = json.RawMessage(`{"fixture":true}`)
	return o
}
func (acceptedJournalAuthority) Finalize(context.Context, pgx.Tx, provisioning.Operation, json.RawMessage) error {
	return provisioning.ErrUnavailable
}
func (acceptedJournalAuthority) Readiness(context.Context, provisioning.Operation) (provisioning.Readiness, error) {
	return provisioning.Readiness{}, nil
}

type acceptanceNoProvider struct{ calls int }

func (*acceptanceNoProvider) Prepare(context.Context, provisioning.Operation, provisioning.Step) (provisioning.Request, error) {
	return provisioning.Request{}, provisioning.ErrUnavailable
}
func (p *acceptanceNoProvider) Create(context.Context, provisioning.SavedRequest) (json.RawMessage, error) {
	p.calls++
	return nil, provisioning.ErrUnavailable
}
func (*acceptanceNoProvider) Recover(context.Context, provisioning.SavedRequest) (json.RawMessage, error) {
	return nil, provisioning.ErrUnavailable
}
func (*acceptanceNoProvider) Verify(context.Context, provisioning.Operation) (json.RawMessage, error) {
	return nil, provisioning.ErrUnavailable
}

type acceptanceLostJournalAck struct {
	ycAcceptanceProvisioner
	lose bool
}

func (p *acceptanceLostJournalAck) Enqueue(ctx context.Context, team, assignment uuid.UUID) (uuid.UUID, error) {
	id, err := p.ycAcceptanceProvisioner.Enqueue(ctx, team, assignment)
	if err == nil && p.lose {
		p.lose = false
		return uuid.Nil, errors.New("synthetic lost journal acknowledgement")
	}
	return id, err
}

func TestYCAcceptanceRealConsumerReachesProvisioningJournal(t *testing.T) {
	s, r := acceptedCatalogFixture(t)
	ctx := t.Context()
	h := s.Catalog.(ycAcceptedCatalog).h
	admission := &YCAcceptedProvisioningAdmission{Service: s, Handlers: h}
	provider := &acceptanceNoProvider{}
	journal := &provisioning.Service{Pool: s.Pool, Authority: acceptedJournalAuthority{admission: admission}, Provider: provider}
	port := &acceptanceLostJournalAck{ycAcceptanceProvisioner: ycAcceptanceProvisioner{service: journal}, lose: true}
	s.Provisioning = port
	if work, err := s.RunOnce(ctx); !work || err == nil {
		t.Fatal("lost durable enqueue acknowledgement not surfaced", work, err)
	}
	var operation, assignment uuid.UUID
	var frozen provisioning.AcceptedOffer
	var raw []byte
	if err := s.Pool.QueryRow(ctx, `SELECT id,assignment_id,accepted_offer FROM billing_offer_provisioning_operation WHERE team_id=$1`, r.TeamID).Scan(&operation, &assignment, &raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &frozen); err != nil || frozen.AcceptanceID != r.ID || frozen.TermsDigest != r.TermsDigest {
		t.Fatal("journal lost accepted identity", frozen, err)
	}
	// Even while the local acknowledgement is missing, controls must route to the
	// provisioner rather than claim a competing acceptance-level hold succeeded.
	if err := s.control(ctx, uuid.New(), ycAcceptanceControl{RequestID: uuid.New(), AcceptanceID: r.ID, Action: "hold", Reason: "Synthetic hold after handoff"}); err == nil {
		t.Fatal("acceptance hold overtook provisioner journal")
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE yc_offer_acceptance SET next_attempt_at=now() WHERE id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	restarted := *s
	if work, err := restarted.RunOnce(ctx); !work || err != nil {
		t.Fatal("restart failed to replay actual journal", work, err)
	}
	current, err := loadYCAcceptance(ctx, s.Pool, r.TeamID, r.ClaimID)
	if err != nil || current.OperationID == nil || *current.OperationID != operation || current.AssignmentID == nil || *current.AssignmentID != assignment || current.State != "dispatched" {
		t.Fatal("handoff identities diverged", current, err)
	}
	status, err := s.Status(ctx, r.UserID, r.TeamID, r.ClaimID)
	if err != nil || status.State != "provisioning" || status.Ready || provider.calls != 0 {
		t.Fatal("journal metadata implied provider readiness", status, err)
	}
	if _, err = s.Shared.Exec(ctx, `SELECT public.revoke_yc_activation_authority($1,$2,$3)`, r.ApprovalID, r.TeamID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	called := false
	if err = admission.WithDispatchGuard(ctx, r.TeamID, assignment, func(provisioning.AcceptedOffer) error { called = true; return nil }); err == nil || called {
		t.Fatal("pending journal authorized revoked dispatch", err)
	}
}

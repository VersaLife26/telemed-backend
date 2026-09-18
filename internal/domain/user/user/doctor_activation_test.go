package user

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/events"
)

type stubDoctorApps struct {
	app      DoctorApplication
	err      error
	attached []uuid.UUID
}

func (s *stubDoctorApps) ApplicationByPhone(context.Context, string) (DoctorApplication, error) {
	if s.err != nil {
		return DoctorApplication{}, s.err
	}
	return s.app, nil
}

func (s *stubDoctorApps) ApplicationByEmail(context.Context, string) (DoctorApplication, error) {
	if s.err != nil {
		return DoctorApplication{}, s.err
	}
	return s.app, nil
}

func (s *stubDoctorApps) ApplicationByID(context.Context, uuid.UUID) (DoctorApplication, error) {
	if s.err != nil {
		return DoctorApplication{}, s.err
	}
	return s.app, nil
}

func (s *stubDoctorApps) Attach(_ context.Context, applicationID, _ uuid.UUID, _ string) error {
	s.attached = append(s.attached, applicationID)
	return nil
}

func (s *stubDoctorApps) DoctorIDByUserID(context.Context, uuid.UUID) (uuid.UUID, error) {
	if s.err != nil {
		return uuid.Nil, s.err
	}
	return s.app.ID, nil
}

func TestApplicationApprovedConsumer_NoClientIsNoop(t *testing.T) {
	c := NewApplicationApprovedConsumer(&Service{log: zerolog.Nop()}, zerolog.Nop())
	env := envelopeFor(t, events.SubjectDoctorApplicationApproved, events.DoctorApplicationApproved{
		ApplicationID: uuid.New(),
	})
	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestApplicationApprovedConsumer_MalformedPayload(t *testing.T) {
	svc := &Service{doctors: &stubDoctorApps{}, log: zerolog.Nop()}
	c := NewApplicationApprovedConsumer(svc, zerolog.Nop())
	env := events.Envelope{
		Subject: events.SubjectDoctorApplicationApproved,
		Payload: []byte(`{"application_id": 1}`),
	}
	if err := c.Handle(context.Background(), env); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestApplicationApprovedConsumer_MissingApplicationIsRetried(t *testing.T) {
	svc := &Service{doctors: &stubDoctorApps{err: ErrDoctorApplicationNotFound}, log: zerolog.Nop()}
	c := NewApplicationApprovedConsumer(svc, zerolog.Nop())
	env := envelopeFor(t, events.SubjectDoctorApplicationApproved, events.DoctorApplicationApproved{
		ApplicationID: uuid.New(),
	})
	if err := c.Handle(context.Background(), env); !errors.Is(err, ErrDoctorApplicationNotFound) {
		t.Fatalf("Handle = %v, want ErrDoctorApplicationNotFound so JetStream redelivers", err)
	}
}

func TestApplicationApprovedConsumer_PendingApplicationIsAcknowledged(t *testing.T) {
	svc := &Service{doctors: &stubDoctorApps{app: DoctorApplication{Status: "pending"}}, log: zerolog.Nop()}
	c := NewApplicationApprovedConsumer(svc, zerolog.Nop())
	env := envelopeFor(t, events.SubjectDoctorApplicationApproved, events.DoctorApplicationApproved{
		ApplicationID: uuid.New(),
	})
	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestDoctorSession_IssuesTokenWithDoctorID(t *testing.T) {
	issuer := newTestIssuer(t)
	expectedDoctorID := uuid.New()
	doctorUser := User{
		ID:    uuid.New(),
		Phone: "+94771234567",
		Name:  "Dr. Nimal Perera",
		Role:  RoleDoctor,
	}

	svc := &Service{
		tokens:  issuer,
		doctors: &stubDoctorApps{app: DoctorApplication{ID: expectedDoctorID}},
		log:     zerolog.Nop(),
	}

	docID := svc.resolveDoctorID(context.Background(), doctorUser.ID)
	if docID != expectedDoctorID {
		t.Fatalf("resolveDoctorID = %v, want %v", docID, expectedDoctorID)
	}

	token, _, err := issuer.IssueAccessToken(doctorUser, docID)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	principal, err := issuer.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if principal.DoctorID != expectedDoctorID {
		t.Errorf("DoctorID = %v, want %v", principal.DoctorID, expectedDoctorID)
	}
	if principal.Role != RoleDoctor {
		t.Errorf("Role = %v, want %v", principal.Role, RoleDoctor)
	}
	if principal.UserID != doctorUser.ID {
		t.Errorf("UserID = %v, want %v", principal.UserID, doctorUser.ID)
	}
}

package credentialing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/domain/admin/directory"
	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
)

// Projector maintains doctor_projection from the two events that describe a
// credentialing application: doctor.registered (the application itself) and
// doctor.documents_updated (the credentials attached to it afterwards).
//
// Both payloads are canonical events.* types shared with doctor-service, so a
// field one side adds and the other does not is a compile error rather than an
// empty string. This projector previously declared its own eleven-field
// payload struct against a producer that sent four, and the two sides did not
// even agree on the specialty's field name (`specialty_code` here,
// `specialty` there). encoding/json does not complain about a field nobody
// sent, so every doctor in the queue projected with a blank name, a blank
// email, a zero experience, a 0001-01-01 registration date and no documents --
// which made the flagship admin screen useless and reported nothing.
//
// Two events rather than one is not an accident of design. A doctor registers
// first and uploads credentials second, so doctor.registered is always
// published before any document exists: an application projected from it alone
// necessarily has an empty document viewer, and a reviewer cannot verify
// credentials they cannot open.
//
// Identity (email, phone) still comes from user-service via directory.Client,
// keyed on the UserID the event carries: those are contact details that
// change, and a stale copy on a replayed event would be worse than a lookup.
// The name is taken from the event, because the name a doctor APPLIED under is
// a fact about the application -- it must not shift under a reviewer mid-review
// because the user record was edited.
type Projector struct {
	repo projectionStore
	dir  directory.Client
	log  zerolog.Logger
}

// projectionStore is the slice of Repository this projector needs. It is
// declared here, next to its only consumer, so the projection rules can be
// tested against an in-memory fake instead of a Postgres container -- these
// are the rules that decide what a reviewer sees, and they are worth testing
// on every `go test`, not only when Docker is available.
type projectionStore interface {
	UpsertDoctorFromEvent(ctx context.Context, eventID uuid.UUID, d DoctorSummary) error
	ApplyDocumentKeys(ctx context.Context, doctorID uuid.UUID, d DoctorSummary, updatedAt time.Time) error
	SetVerificationStatus(ctx context.Context, doctorID uuid.UUID, status string) error
}

func NewProjector(repo projectionStore, dir directory.Client, log zerolog.Logger) *Projector {
	return &Projector{repo: repo, dir: dir, log: log.With().Str("component", "credentialing_projector").Logger()}
}

// Subscribe registers the durable consumer. Call once at boot; it blocks
// until ctx is cancelled, so run it in its own goroutine like the outbox
// relay.
func (p *Projector) Subscribe(ctx context.Context, sub events.Subscriber) error {
	return sub.Subscribe(ctx, "admin-credentialing-doctor-registered",
		[]events.Subject{
			events.SubjectDoctorRegistered,
			events.SubjectDoctorDocumentsUpdated,
			events.SubjectDoctorApplicationSubmitted,
			events.SubjectDoctorApplicationApproved,
			events.SubjectDoctorApplicationRejected,
		},
		p.handle)
}

func (p *Projector) handle(ctx context.Context, env events.Envelope) error {
	switch env.Subject {
	case events.SubjectDoctorRegistered:
		return p.onRegistered(ctx, env)
	case events.SubjectDoctorDocumentsUpdated:
		return p.onDocumentsUpdated(ctx, env)
	case events.SubjectDoctorApplicationSubmitted:
		return p.onApplicationSubmitted(ctx, env)
	case events.SubjectDoctorApplicationApproved, events.SubjectDoctorApplicationRejected:
		return p.onApplicationDecided(ctx, env)
	default:
		return nil
	}
}

func (p *Projector) onApplicationSubmitted(ctx context.Context, env events.Envelope) error {
	var payload events.DoctorApplicationSubmitted
	if err := env.Decode(&payload); err != nil {
		return fmt.Errorf("credentialing: decode doctor.application_submitted: %w", err)
	}
	summary := DoctorSummary{
		DoctorID:        payload.ApplicationID,
		FullName:        payload.FullName,
		Email:           payload.Email,
		Phone:           payload.Phone,
		SLMCNumber:      payload.SLMCNumber,
		YearsExperience: payload.YearsExperience,
		SpecialtyCode:   payload.Specialty,
		FeeCents:        payload.FeeCents,
		RegisteredAt:    payload.CreatedAt,
	}
	return p.repo.UpsertDoctorFromEvent(ctx, env.ID, summary)
}

func (p *Projector) onApplicationDecided(ctx context.Context, env events.Envelope) error {
	status := "approved"
	var applicationID uuid.UUID
	switch env.Subject {
	case events.SubjectDoctorApplicationApproved:
		var payload events.DoctorApplicationApproved
		if err := env.Decode(&payload); err != nil {
			return fmt.Errorf("credentialing: decode doctor.application_approved: %w", err)
		}
		applicationID = payload.ApplicationID
	case events.SubjectDoctorApplicationRejected:
		status = "rejected"
		var payload events.DoctorApplicationRejected
		if err := env.Decode(&payload); err != nil {
			return fmt.Errorf("credentialing: decode doctor.application_rejected: %w", err)
		}
		applicationID = payload.ApplicationID
	}
	return p.repo.SetVerificationStatus(ctx, applicationID, status)
}

func (p *Projector) onRegistered(ctx context.Context, env events.Envelope) error {
	var payload events.DoctorRegistered
	if err := env.Decode(&payload); err != nil {
		return fmt.Errorf("credentialing: decode doctor.registered: %w", err)
	}

	summary := DoctorSummary{
		DoctorID:             payload.DoctorID,
		FullName:             payload.FullName,
		SLMCNumber:           payload.SLMCNumber,
		YearsExperience:      payload.YearsExperience,
		SpecialtyCode:        payload.Specialty,
		SLMCCertificateKey:   payload.SLMCCertificateKey,
		NICDocumentKey:       payload.NICDocumentKey,
		DegreeCertificateKey: payload.DegreeCertificateKey,
		PhotoKey:             payload.PhotoKey,
		RegisteredAt:         payload.CreatedAt,
	}

	u, err := p.dir.User(ctx, payload.UserID)
	switch {
	case errors.Is(err, directory.ErrNotFound):
		// Project the application anyway: a doctor missing from the queue is
		// worse than a doctor shown without contact details, because a
		// reviewer cannot act on what they cannot see. Say so loudly.
		p.log.Warn().
			Str("event_id", env.ID.String()).
			Str("doctor_id", payload.DoctorID.String()).
			Str("user_id", logger.MaskID(payload.UserID.String())).
			Msg("doctor.registered names a user the directory does not know; projecting without contact details")
	case err != nil:
		return fmt.Errorf("credentialing: resolve doctor's user %s: %w", payload.UserID, err)
	default:
		summary.Email, summary.Phone = u.Email, u.Phone
		if summary.FullName == "" {
			// A producer older than the full_name field. The user record is a
			// worse answer than the application's own name, but it beats a
			// nameless row a reviewer cannot identify.
			summary.FullName = u.FullName
		}
	}

	if summary.Empty() {
		// Normal at registration -- documents are uploaded afterwards, and
		// doctor.documents_updated will fill them in. Logged at debug so the
		// signal is available when a reviewer reports an empty viewer, without
		// warning on the ordinary case the way this used to.
		p.log.Debug().
			Str("doctor_id", payload.DoctorID.String()).
			Msg("doctor.registered carries no credential documents yet; awaiting doctor.documents_updated")
	}

	return p.repo.UpsertDoctorFromEvent(ctx, env.ID, summary)
}

func (p *Projector) onDocumentsUpdated(ctx context.Context, env events.Envelope) error {
	var payload events.DoctorDocumentsUpdated
	if err := env.Decode(&payload); err != nil {
		return fmt.Errorf("credentialing: decode doctor.documents_updated: %w", err)
	}

	// UpdatedAt is the ordering guard the repository writes against. A
	// producer that omits it would make every delivery look simultaneous and
	// the guard would silently stop guarding, so fall back to the envelope's
	// own timestamp rather than writing a zero.
	updatedAt := payload.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = env.OccurredAt
	}

	return p.repo.ApplyDocumentKeys(ctx, payload.DoctorID, DoctorSummary{
		DoctorID:             payload.DoctorID,
		SLMCCertificateKey:   payload.SLMCCertificateKey,
		NICDocumentKey:       payload.NICDocumentKey,
		DegreeCertificateKey: payload.DegreeCertificateKey,
		PhotoKey:             payload.PhotoKey,
	}, updatedAt)
}

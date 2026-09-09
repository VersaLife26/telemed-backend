package users

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/domain/admin/directory"
	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
)

// Projector keeps user_projection current from the canonical user lifecycle
// events.
//
// It resolves names, emails and phones through directory.Client rather than
// reading them off the event. events.UserRegistered deliberately carries only
// the id, role, language and timestamp: a full phone number is PHI-adjacent
// and that event fans out to every consumer on the platform. This projector
// used to declare a private RegisteredPayload with full_name/email/phone
// fields nobody ever sent, so every projected row was nameless and
// unsearchable, and encoding/json reported nothing.
type Projector struct {
	repo projectionStore
	dir  directory.Client
	log  zerolog.Logger
}

// projectionStore is the slice of Repository this projector needs, declared
// next to its only consumer so the projection rules can be tested against an
// in-memory fake rather than a Postgres container.
type projectionStore interface {
	UpsertFromRegistration(ctx context.Context, eventID uuid.UUID, r Registration) error
	SetStatus(ctx context.Context, eventID, userID uuid.UUID, status string) error
}

func NewProjector(repo projectionStore, dir directory.Client, log zerolog.Logger) *Projector {
	return &Projector{repo: repo, dir: dir, log: log.With().Str("component", "users_projector").Logger()}
}

func (p *Projector) Subscribe(ctx context.Context, sub events.Subscriber) error {
	return sub.Subscribe(ctx, "admin-users-projection",
		[]events.Subject{events.SubjectUserRegistered, events.SubjectUserSuspended, events.SubjectUserReinstated},
		p.handle)
}

func (p *Projector) handle(ctx context.Context, env events.Envelope) error {
	switch env.Subject {
	case events.SubjectUserRegistered:
		return p.onRegistered(ctx, env)

	case events.SubjectUserSuspended, events.SubjectUserReinstated:
		// One canonical type backs both subjects, and it carries the resulting
		// status explicitly rather than leaving each consumer to infer it from
		// the subject name. Falling back to the subject keeps an older
		// producer that omits the field working.
		var payload events.UserStatusChanged
		if err := env.Decode(&payload); err != nil {
			return fmt.Errorf("users: decode %s: %w", env.Subject, err)
		}
		status := payload.Status
		if status == "" {
			status = "suspended"
			if env.Subject == events.SubjectUserReinstated {
				status = "active"
			}
		}
		return p.repo.SetStatus(ctx, env.ID, payload.UserID, status)

	default:
		return nil
	}
}

func (p *Projector) onRegistered(ctx context.Context, env events.Envelope) error {
	var payload events.UserRegistered
	if err := env.Decode(&payload); err != nil {
		return fmt.Errorf("users: decode user.registered: %w", err)
	}

	u, err := p.dir.User(ctx, payload.UserID)
	switch {
	case errors.Is(err, directory.ErrNotFound):
		// The account was created and then hard-deleted before this event was
		// consumed. Projecting an id with no identity would put an
		// unsearchable ghost row in the admin console, so skip it -- but say
		// so, because "we chose not to project this" is an operational fact.
		p.log.Warn().
			Str("event_id", env.ID.String()).
			Str("user_id", logger.MaskID(payload.UserID.String())).
			Msg("user.registered for a user the directory does not know; skipping projection")
		return nil
	case err != nil:
		// Unreachable directory: return the error so JetStream redelivers.
		// Projecting a nameless row here is exactly the bug this replaced.
		return fmt.Errorf("users: resolve %s: %w", payload.UserID, err)
	}

	return p.repo.UpsertFromRegistration(ctx, env.ID, Registration{
		UserID:       payload.UserID,
		FullName:     u.FullName,
		Email:        u.Email,
		Phone:        u.Phone,
		Role:         payload.Role,
		RegisteredAt: payload.CreatedAt,
	})
}

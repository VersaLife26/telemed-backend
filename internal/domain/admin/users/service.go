package users

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/domain/admin/audit"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// Service implements search plus the suspend/reinstate commands. Neither
// command mutates user_projection directly -- this service does not own
// user-service's data (ADR-004). Each publishes an admin.* command event via
// the outbox; user-service is expected to apply it and publish
// user.suspended/user.reinstated back, which Projector consumes into
// user_projection. The admin console should treat the response as "request
// recorded", not "already applied" -- see docs/API.md.
type Service struct {
	pool   database.Pool
	repo   *Repository
	outbox *events.Outbox
}

func NewService(pool database.Pool, repo *Repository, outbox *events.Outbox) *Service {
	return &Service{pool: pool, repo: repo, outbox: outbox}
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (User, error) { return s.repo.Get(ctx, id) }

func (s *Service) List(ctx context.Context, f ListFilter) ([]User, int64, error) {
	return s.repo.List(ctx, f)
}

// Activity returns recent account and booking events for a projected user.
// Missing user → ErrNotFound (404), not an empty page: an empty page means
// the account exists and nothing has been logged yet.
func (s *Service) Activity(ctx context.Context, id uuid.UUID, page, perPage int) ([]ActivityEntry, int64, error) {
	if _, err := s.repo.Get(ctx, id); err != nil {
		return nil, 0, err
	}
	return s.repo.ListActivity(ctx, id, page, perPage)
}

func (s *Service) Suspend(ctx context.Context, userID, adminID uuid.UUID, reason string) error {
	if _, err := s.repo.Get(ctx, userID); err != nil {
		return err
	}
	if err := s.publishCommand(ctx, events.SubjectAdminUserSuspendRequested, userID, adminID, reason); err != nil {
		return err
	}
	audit.Stage(ctx, audit.Draft{
		Action: "user.suspend_requested", ResourceType: "user", ResourceID: userID.String(),
		NewValue: map[string]any{"reason": reason},
	})
	return nil
}

func (s *Service) Reinstate(ctx context.Context, userID, adminID uuid.UUID, reason string) error {
	if _, err := s.repo.Get(ctx, userID); err != nil {
		return err
	}
	if err := s.publishCommand(ctx, events.SubjectAdminUserReinstateRequested, userID, adminID, reason); err != nil {
		return err
	}
	audit.Stage(ctx, audit.Draft{
		Action: "user.reinstate_requested", ResourceType: "user", ResourceID: userID.String(),
		NewValue: map[string]any{"reason": reason},
	})
	return nil
}

// publishCommand enqueues the canonical admin command. The payload type is
// events.AdminUserStatusRequested -- the same struct user-service decodes --
// so a field one side adds and the other does not is a compile error rather
// than an empty string on the wire.
func (s *Service) publishCommand(ctx context.Context, subject events.Subject, userID, adminID uuid.UUID, reason string) error {
	return database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return s.outbox.Enqueue(ctx, tx, subject, userID.String(), events.AdminUserStatusRequested{
			UserID: userID, Reason: reason, AdminID: adminID,
		})
	})
}

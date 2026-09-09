package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
	"telemed/internal/platform/middleware"
)

// Account suspension.
//
// admin-service owns the decision and the audit trail; it owns none of the
// account data (ADR-004), so it publishes admin.user_suspend_requested and
// admin.user_reinstate_requested as COMMANDS. This file is where those
// commands become facts. Until it existed, nothing consumed them: the admin
// console wrote an audit row, returned 202, and changed nothing -- which is
// worse than an error, because it looks like it worked.
//
// The loop closes here: apply the change, then publish user.suspended /
// user.reinstated, which admin-service's own projector consumes so the console
// finally shows the state it asked for.

// ErrNotSuspendable is returned for a status transition that is not
// administratively meaningful -- reinstating an account that was never
// suspended, or suspending one that is already gone. It is not an error the
// consumer should retry; the command has been considered and declined.
var ErrNotSuspendable = errors.New("user: account is not in a suspendable state")

// SuspendUser applies admin.user_suspend_requested.
func (s *Service) SuspendUser(ctx context.Context, cmd events.AdminUserStatusRequested) error {
	return s.applyAdminStatus(ctx, cmd, StatusSuspended)
}

// ReinstateUser applies admin.user_reinstate_requested.
func (s *Service) ReinstateUser(ctx context.Context, cmd events.AdminUserStatusRequested) error {
	return s.applyAdminStatus(ctx, cmd, StatusActive)
}

// applyAdminStatus is the whole loop for one command.
//
// Inside one transaction (ADR-005): lock the row, check the transition is
// meaningful, write the status, revoke every session on a suspension, and
// enqueue the resulting fact. Either all of that commits or none of it does --
// an account marked suspended whose sessions survived, or whose event never
// reached the admin console, are both states this cannot produce.
//
// Everything outside the transaction (the Redis denylist) is best-effort and
// happens strictly AFTER the commit, so the cache can only ever lag the
// database, never lead it.
func (s *Service) applyAdminStatus(ctx context.Context, cmd events.AdminUserStatusRequested, target Status) error {
	if cmd.UserID == uuid.Nil {
		return fmt.Errorf("user: admin status command names no user")
	}

	changedAt := time.Now().UTC()
	applied := false

	err := database.InTx(ctx, s.repo.Pool(), pgx.TxOptions{}, func(tx pgx.Tx) error {
		u, err := s.repo.FindUserByIDForUpdate(ctx, tx, cmd.UserID)
		if err != nil {
			return err
		}

		switch {
		case u.Status == StatusDeleted || u.DeletedAt != nil:
			// A deleted account must not be resurrected by a reinstate
			// command, and suspending it changes nothing. PDPA erasure is a
			// different lifecycle with a different reversal path (support,
			// inside the grace window), and quietly re-activating a row the
			// reaper is about to anonymise would be a data-protection
			// incident, not a convenience.
			return ErrNotSuspendable
		case u.Status == target:
			// Already there. At-least-once delivery makes this the ordinary
			// case for a redelivery, not an anomaly: acknowledge and stop.
			// Re-publishing the fact would be harmless but noisy, and would
			// re-revoke sessions the user has legitimately re-established
			// since -- a redelivered suspension must not log a reinstated
			// user back out.
			return nil
		}

		if err := s.repo.SetStatus(ctx, tx, u.ID, target); err != nil {
			return err
		}

		if target == StatusSuspended {
			// Kill every session. Refresh already refuses a suspended user,
			// but revoking here means a stolen refresh token cannot even be
			// presented, and the account cannot rotate its way back in during
			// the window before the denylist is written.
			if _, err := s.repo.RevokeAllForUser(ctx, tx, u.ID); err != nil {
				return err
			}
		}

		subject := events.SubjectUserReinstated
		if target == StatusSuspended {
			subject = events.SubjectUserSuspended
		}
		applied = true
		return s.outbox.Enqueue(ctx, tx, subject, u.ID.String(), events.UserStatusChanged{
			UserID:    u.ID,
			Status:    string(target),
			Reason:    cmd.Reason,
			ActorID:   cmd.AdminID,
			ChangedAt: changedAt,
		})
	})
	if err != nil {
		return err
	}
	if !applied {
		return nil
	}

	s.syncSuspensionDenylist(ctx, cmd.UserID, target)

	s.log.Info().
		Str("user_id", logger.MaskID(cmd.UserID.String())).
		Str("status", string(target)).
		Str("actor_id", logger.MaskID(cmd.AdminID.String())).
		Msg("applied administrative account status change")
	return nil
}

// syncSuspensionDenylist writes or clears the short-lived Redis key the
// gateway checks on every authenticated request. See
// middleware/suspension.go for why this exists alongside refresh refusal and
// why it is allowed to fail.
//
// A failure here is logged at error level and not returned: the transaction
// has already committed, so returning an error would trigger a redelivery
// that re-runs a change which is now a no-op, and the account would stay
// suspended regardless -- just with enforcement deferred to the access
// token's own expiry rather than immediate.
func (s *Service) syncSuspensionDenylist(ctx context.Context, userID uuid.UUID, target Status) {
	if s.cache == nil {
		return
	}
	var err error
	if target == StatusSuspended {
		err = middleware.MarkSuspended(ctx, s.cache, userID)
	} else {
		err = middleware.ClearSuspended(ctx, s.cache, userID)
	}
	if err == nil {
		return
	}
	msg := "could not clear the suspension denylist; the reinstated user may be " +
		"refused for up to the denylist TTL"
	if target == StatusSuspended {
		msg = "could not write the suspension denylist; the suspension still holds, " +
			"but an access token already issued stays usable until it expires"
	}
	s.log.Error().Err(err).Str("user_id", logger.MaskID(userID.String())).Msg(msg)
}

// StatusApplier is the slice of Service the command consumer needs. It is
// declared here, next to its only consumer, so the consumer's decode and
// dispatch rules can be tested without a database -- the parts that decide
// whether a command is retried, dropped or applied are exactly the parts
// worth testing on every `go test`.
type StatusApplier interface {
	SuspendUser(ctx context.Context, cmd events.AdminUserStatusRequested) error
	ReinstateUser(ctx context.Context, cmd events.AdminUserStatusRequested) error
}

var _ StatusApplier = (*Service)(nil)

// AdminCommandConsumer applies admin.* account commands.
//
// It is a separate type rather than methods on Service so that main.go wires
// it explicitly: a consumer that exists but was never subscribed is exactly
// how this loop stayed open in the first place.
type AdminCommandConsumer struct {
	svc StatusApplier
	log zerolog.Logger
}

// NewAdminCommandConsumer builds the consumer.
func NewAdminCommandConsumer(svc StatusApplier, log zerolog.Logger) *AdminCommandConsumer {
	return &AdminCommandConsumer{svc: svc, log: log.With().Str("component", "admin_command_consumer").Logger()}
}

// Subscribe registers the durable consumer. Call once at boot; it blocks until
// ctx is cancelled, so run it in its own goroutine like the outbox relay.
func (c *AdminCommandConsumer) Subscribe(ctx context.Context, sub events.Subscriber) error {
	return sub.Subscribe(ctx, "user-admin-status-commands",
		[]events.Subject{
			events.SubjectAdminUserSuspendRequested,
			events.SubjectAdminUserReinstateRequested,
		}, c.Handle)
}

// Handle applies one command. It is idempotent on the resulting state rather
// than on the envelope id: a redelivered suspension for an already-suspended
// account is a no-op, which is a stronger guarantee than de-duplicating ids
// and needs no extra table.
func (c *AdminCommandConsumer) Handle(ctx context.Context, env events.Envelope) error {
	var cmd events.AdminUserStatusRequested
	if err := env.Decode(&cmd); err != nil {
		return fmt.Errorf("user: decode %s: %w", env.Subject, err)
	}

	var err error
	switch env.Subject {
	case events.SubjectAdminUserSuspendRequested:
		err = c.svc.SuspendUser(ctx, cmd)
	case events.SubjectAdminUserReinstateRequested:
		err = c.svc.ReinstateUser(ctx, cmd)
	default:
		return nil
	}

	switch {
	case errors.Is(err, ErrUserNotFound), errors.Is(err, ErrNotFound):
		// A command naming a user this service does not have will never
		// succeed on redelivery. Acknowledge it and say so loudly, rather
		// than retrying five times and landing it in the dead-letter path
		// where nobody looks.
		c.log.Error().
			Str("event_id", env.ID.String()).
			Str("subject", string(env.Subject)).
			Str("user_id", logger.MaskID(cmd.UserID.String())).
			Msg("admin status command names a user this service does not know; dropping")
		return nil
	case errors.Is(err, ErrNotSuspendable):
		c.log.Warn().
			Str("event_id", env.ID.String()).
			Str("subject", string(env.Subject)).
			Str("user_id", logger.MaskID(cmd.UserID.String())).
			Msg("admin status command declined: the account is deleted")
		return nil
	}
	return err
}

package scheduling

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// JoinWaitlist queues a patient for the next cancellation on a doctor-day.
//
// The Redis sorted set the documentation calls for is built here, but it is an
// index over the waitlists table, never the ledger. Push failures are logged and
// swallowed: losing your place in a queue because a cache blipped is not an
// acceptable failure mode, and PromoteWaitlist falls back to Postgres ordering.
func (s *Service) JoinWaitlist(ctx context.Context, patientID, doctorID uuid.UUID, date Date) (WaitlistEntry, error) {
	today := DateIn(s.clock.Now(), s.loc)
	if date.Before(today) {
		return WaitlistEntry{}, ErrWaitlistDateInPast
	}

	entry := WaitlistEntry{
		ID:            uuid.New(),
		PatientID:     patientID,
		DoctorID:      doctorID,
		PreferredDate: date,
		Status:        WaitlistWaiting,
	}

	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.repo.InsertWaitlistEntry(ctx, tx, &entry); err != nil {
			if database.IsUniqueViolation(err) {
				return ErrWaitlistDuplicate
			}
			return err
		}
		return s.outbox.Enqueue(ctx, tx, events.SubjectWaitlistJoined, entry.ID.String(), events.WaitlistJoined{
			WaitlistID: entry.ID,
			PatientID:  patientID,
			DoctorID:   doctorID,
			// YYYY-MM-DD in the business timezone. Position is deliberately
			// absent from the canonical payload: it is stale the instant it is
			// published, because the entry ahead may cancel a millisecond
			// later. A consumer that needs it asks.
			PreferredDate: date.String(),
			JoinedAt:      entry.QueuedAt,
		})
	})
	if err != nil {
		return WaitlistEntry{}, err
	}

	if err := s.queue.Push(ctx, doctorID, date, entry.ID, entry.QueuedAt); err != nil {
		s.log.Warn().Err(err).Str("waitlist_id", maskID(entry.ID)).
			Msg("waitlist index push failed; promotion will fall back to Postgres")
	}
	return entry, nil
}

// LeaveWaitlist withdraws an entry. Ownership is enforced, and a mismatch reads
// as "not found" so the endpoint cannot enumerate other patients' entries.
func (s *Service) LeaveWaitlist(ctx context.Context, entryID, patientID uuid.UUID, isAdmin bool) error {
	entry, err := s.repo.GetWaitlistEntry(ctx, s.pool, entryID)
	if err != nil {
		return err
	}
	if !isAdmin && entry.PatientID != patientID {
		return ErrWaitlistNotFound
	}
	if entry.Status == WaitlistCancelled {
		return nil
	}

	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		rows, err := s.repo.SetWaitlistStatus(ctx, tx, entryID, WaitlistCancelled, true)
		if err != nil {
			return err
		}
		if rows != 1 {
			return ErrWaitlistNotFound
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.queue.Remove(ctx, entry.DoctorID, entry.PreferredDate, entryID); err != nil {
		s.log.Warn().Err(err).Msg("waitlist index removal failed")
	}
	return nil
}

// ListWaitlist returns a patient's live entries.
func (s *Service) ListWaitlist(ctx context.Context, patientID uuid.UUID) ([]WaitlistEntry, error) {
	return s.repo.ListWaitlistForPatient(ctx, s.pool, patientID)
}

// PromoteWaitlist offers a freed slot to the longest-waiting patient for that
// doctor and civil date.
//
// The offer is a five-minute exclusive hold. It is recorded in Postgres
// (slots.reserved_for / reserved_until, status BLOCKED) rather than only as a
// Redis TTL, because a Redis failover during those five minutes must not hand
// the slot to somebody else while the first patient is still in checkout.
//
// Candidate order comes from the Redis sorted set when it is warm and from
// Postgres when it is not. Both produce the same order; the index only saves a
// query.
func (s *Service) PromoteWaitlist(ctx context.Context, doctorID, slotID uuid.UUID) error {
	now := s.clock.Now()

	slot, err := s.repo.GetSlot(ctx, s.pool, slotID)
	if err != nil {
		return err
	}
	if slot.Status != SlotAvailable || !slot.StartAt.After(now) {
		return nil
	}
	date := DateIn(slot.StartAt, s.loc)

	candidates, err := s.waitlistCandidates(ctx, doctorID, date)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		s.metrics.WaitlistPromotions.WithLabelValues("no_candidates").Inc()
		return nil
	}

	// Walk the queue: an entry can lose the race to another promotion running
	// for a different freed slot, in which case we simply try the next one.
	for i := range candidates {
		entry := candidates[i]
		offered, err := s.offerSlot(ctx, entry, slot, now)
		if err != nil {
			return err
		}
		if offered {
			s.metrics.WaitlistPromotions.WithLabelValues("offered").Inc()
			if err := s.queue.Remove(ctx, doctorID, date, entry.ID); err != nil {
				s.log.Warn().Err(err).Msg("waitlist index removal failed after promotion")
			}
			s.log.Info().
				Str("waitlist_id", maskID(entry.ID)).
				Str("slot_id", maskID(slot.ID)).
				Dur("window", WaitlistOfferWindow).
				Msg("waitlist slot offered")
			return nil
		}
	}
	s.metrics.WaitlistPromotions.WithLabelValues("no_candidates").Inc()
	return nil
}

// waitlistCandidates returns waiting entries in join order, preferring the
// Redis index and falling back to Postgres.
func (s *Service) waitlistCandidates(ctx context.Context, doctorID uuid.UUID, date Date) ([]WaitlistEntry, error) {
	const lookahead = 10

	ids, err := s.queue.Head(ctx, doctorID, date, lookahead)
	if err != nil {
		s.log.Warn().Err(err).Msg("waitlist index read failed; falling back to Postgres")
		ids = nil
	}
	if len(ids) > 0 {
		entries, err := s.repo.GetWaitingEntriesByID(ctx, s.pool, ids)
		if err != nil {
			return nil, err
		}
		if len(entries) > 0 {
			return entries, nil
		}
		// The index named entries that are no longer waiting. Drop them so the
		// next promotion does not pay for the same miss.
		if err := s.queue.Remove(ctx, doctorID, date, ids...); err != nil {
			s.log.Debug().Err(err).Msg("waitlist index cleanup failed")
		}
	}
	return s.repo.ListWaitingEntries(ctx, s.pool, doctorID, date, lookahead)
}

// offerSlot atomically claims one waitlist entry and reserves the slot for it.
// Returning (false, nil) means somebody else claimed the entry or the slot in
// the meantime and the caller should try the next candidate.
func (s *Service) offerSlot(ctx context.Context, entry WaitlistEntry, slot Slot, now time.Time) (bool, error) {
	expiresAt := now.Add(WaitlistOfferWindow)
	claimed := false

	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		rows, err := s.repo.ClaimWaitlistEntry(ctx, tx, entry.ID, entry.Version, slot.ID, expiresAt, now)
		if err != nil {
			return err
		}
		if rows != 1 {
			return nil // lost the race for this entry
		}

		locked, err := s.repo.LockSlot(ctx, tx, slot.ID)
		if err != nil {
			return err
		}
		if locked.Status != SlotAvailable {
			// The slot went while we were claiming. Roll the whole thing back
			// so the patient keeps their place in the queue.
			return errSkipOffer
		}
		n, err := s.repo.ReserveSlot(ctx, tx, locked.ID, locked.StartAt, entry.PatientID, expiresAt, locked.Version)
		if err != nil {
			return err
		}
		if n != 1 {
			return errSkipOffer
		}
		claimed = true

		return s.outbox.Enqueue(ctx, tx, events.SubjectWaitlistSlotOffer, entry.ID.String(), events.WaitlistSlotOffered{
			WaitlistID: entry.ID,
			PatientID:  entry.PatientID,
			DoctorID:   entry.DoctorID,
			SlotID:     locked.ID,
			StartAt:    locked.StartAt,
			// ExpiresAt is when the hold lapses and the offer passes to the
			// next patient. The SMS copy depends on it, so it is not optional.
			ExpiresAt: expiresAt,
		})
	})
	if errors.Is(err, errSkipOffer) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return claimed, nil
}

// errSkipOffer aborts an offer transaction without surfacing as a failure.
var errSkipOffer = errors.New("scheduling: offer superseded")

// SweepExpiredOffers releases lapsed waitlist reservations and falls through to
// the next patient in the queue.
//
// This is the "fall through on expiry" half of the promotion design. It runs on
// a one-minute cron, so an offer lives for five to six minutes in practice --
// stated in the notification copy as "about five minutes" for exactly that
// reason.
func (s *Service) SweepExpiredOffers(ctx context.Context) (int, error) {
	now := s.clock.Now()
	const batch = 200

	expired, err := s.repo.ListExpiredReservations(ctx, s.pool, now, batch)
	if err != nil {
		return 0, err
	}

	swept := 0
	for _, e := range expired {
		var freed bool
		err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			slot, err := s.repo.LockSlot(ctx, tx, e.SlotID)
			if err != nil {
				return err
			}
			if slot.Status != SlotBlocked || slot.ReservedUntil == nil || slot.ReservedUntil.After(now) {
				return nil // taken or renewed since we listed it
			}
			rows, err := s.repo.ReleaseSlot(ctx, tx, slot.ID, slot.StartAt, slot.Version)
			if err != nil {
				return err
			}
			if rows != 1 {
				return ErrVersionConflict
			}
			freed = true
			return nil
		})
		if err != nil {
			s.log.Error().Err(err).Str("slot_id", maskID(e.SlotID)).Msg("expired reservation sweep failed")
			continue
		}
		if freed {
			swept++
		}
	}

	// Retire or requeue the entries whose offers lapsed, then re-promote each
	// freed slot to the next patient.
	lapsed, err := s.repo.ListLapsedOffers(ctx, s.pool, now, batch)
	if err != nil {
		return swept, err
	}
	for i := range lapsed {
		entry := &lapsed[i]
		next := WaitlistWaiting
		if entry.OfferCount >= WaitlistMaxOffers {
			// Three ignored offers is a patient who is not watching their
			// phone. Retiring them stops one dormant entry from starving
			// everybody behind it, five minutes at a time.
			next = WaitlistExpired
		}
		err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			_, err := s.repo.SetWaitlistStatus(ctx, tx, entry.ID, next, true)
			return err
		})
		if err != nil {
			s.log.Error().Err(err).Str("waitlist_id", maskID(entry.ID)).Msg("lapsed offer cleanup failed")
			continue
		}
		s.metrics.WaitlistPromotions.WithLabelValues("expired").Inc()

		if next == WaitlistWaiting {
			if err := s.queue.Push(ctx, entry.DoctorID, entry.PreferredDate, entry.ID, s.clock.Now()); err != nil {
				s.log.Debug().Err(err).Msg("waitlist index requeue failed")
			}
		} else if err := s.queue.Remove(ctx, entry.DoctorID, entry.PreferredDate, entry.ID); err != nil {
			s.log.Debug().Err(err).Msg("waitlist index removal failed")
		}

		if entry.OfferedSlotID != nil {
			if err := s.PromoteWaitlist(ctx, entry.DoctorID, *entry.OfferedSlotID); err != nil {
				s.log.Error().Err(err).Msg("re-promotion after expiry failed")
			}
		}
	}
	return swept, nil
}

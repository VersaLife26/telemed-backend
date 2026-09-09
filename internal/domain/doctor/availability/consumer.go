package availability

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// DurableName is the JetStream durable consumer name. Fixed and stable: a
// rename here starts a brand-new consumer at DeliverAllPolicy and replays
// the platform's entire slot event history against this projection.
// DurableName must not contain "." -- JetStream rejects a durable whose name
// contains a subject-token separator, and the consumer then fails to start.
// That failure was silent in effect: the projection never ran, so
// ?available=now returned nothing against 1,577 real slots.
const DurableName = "doctor-service_availability-projection"

var uuidZero uuid.UUID

// Consumer subscribes to the slot lifecycle events published by the
// scheduling service and folds them into the local availability projection.
type Consumer struct {
	pool database.Pool
	repo *Repository
	loc  *time.Location
	log  zerolog.Logger
}

// NewConsumer builds a Consumer. loc is the business timezone
// (Asia/Colombo) used to bucket slots into search-facing calendar dates.
func NewConsumer(pool database.Pool, loc *time.Location, log zerolog.Logger) *Consumer {
	return &Consumer{pool: pool, repo: NewRepository(), loc: loc, log: log}
}

// Subjects the consumer wants delivered.
//
// slot.generated is deliberately NOT here, and its absence is a fix rather
// than an omission.
//
// This projection is keyed on slot_id. scheduling-service's slot.generated is
// a BATCH announcement -- events.SlotsGenerated is {doctor_id, from_date,
// to_date, count} -- and carries no slot_id at all, because materialising
// thirty days of slots would otherwise mean publishing several hundred events
// per doctor per night. Subscribing to it meant every such event hit the
// "payload missing slot_id, dropping" branch below and was logged as an error
// against a projection that could never have consumed it.
//
// The consequence is real and is reported rather than hidden: a doctor's
// freshly generated slots do not enter this projection until one of them is
// booked or released, so `?available=true` search under-reports a brand-new
// doctor. Closing it needs a per-slot event or a scheduling-service query, and
// that is a contract decision for the platform, not something to paper over
// with a struct that decodes to zeroes.
func (c *Consumer) Subjects() []events.Subject {
	return []events.Subject{
		events.SubjectSlotBooked,
		events.SubjectSlotReleased,
		// slot.withdrawn is what a doctor registering leave over an
		// already-generated day produces. Without it this projection keeps
		// counting those slots as available and search advertises the doctor on
		// the exact day they are away -- which would make the leave feature
		// look broken from the patient side while scheduling-service was
		// entirely correct.
		events.SubjectSlotWithdrawn,
	}
}

// Run subscribes and blocks until ctx is cancelled. Call it in its own
// goroutine at boot, same as the outbox relay.
func (c *Consumer) Run(ctx context.Context, sub events.Subscriber) error {
	return sub.Subscribe(ctx, DurableName, c.Subjects(), c.handle)
}

// statusFor maps the event subject to the slot status it asserts.
//
// slot.released means "this slot can be booked"; slot.booked means it cannot;
// slot.withdrawn means it never will be again, because the doctor took the hour
// back. Withdrawn is NOT folded into booked or into released: released would
// put the slot back on the market on the day the doctor is on leave, and booked
// would record a lie in a table an operator reads during an incident.
//
// A slot this projection has never heard of simply has no row, which search
// treats identically to "not available".
func statusFor(subject events.Subject) (SlotStatus, bool) {
	switch subject {
	case events.SubjectSlotReleased:
		return SlotAvailable, true
	case events.SubjectSlotBooked:
		return SlotBooked, true
	case events.SubjectSlotWithdrawn:
		return SlotWithdrawn, true
	default:
		return "", false
	}
}

func (c *Consumer) handle(ctx context.Context, env events.Envelope) error {
	status, ok := statusFor(env.Subject)
	if !ok {
		// A subject we did not ask for should not be deliverable, but a
		// misconfigured filter must not crash the consumer loop.
		c.log.Warn().Str("subject", string(env.Subject)).Msg("availability consumer: unexpected subject, ignoring")
		return nil
	}

	var payload SlotEventPayload
	if err := env.Decode(&payload); err != nil {
		// A malformed payload will never become valid on redelivery. Log and
		// acknowledge rather than retrying forever.
		c.log.Error().Err(err).Str("event_id", env.ID.String()).Str("subject", string(env.Subject)).
			Msg("availability consumer: unparseable payload, dropping")
		return nil
	}
	if payload.SlotID == uuidZero || payload.DoctorID == uuidZero {
		c.log.Error().Str("event_id", env.ID.String()).Str("subject", string(env.Subject)).
			Msg("availability consumer: payload missing slot_id or doctor_id, dropping")
		return nil
	}

	err := database.InTx(ctx, c.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return c.repo.ApplySlotEvent(ctx, tx, env.ID, env.OccurredAt, status, payload, c.loc)
	})
	if err != nil {
		return fmt.Errorf("availability consumer: apply %s for slot %s: %w", env.Subject, payload.SlotID, err)
	}
	return nil
}

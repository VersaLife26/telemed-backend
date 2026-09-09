// Package availability maintains a local, eventually-consistent projection
// of doctor availability built entirely from events consumed off NATS
// JetStream (slot.generated, slot.booked, slot.released). It never reads or
// writes the scheduling service's database.
//
// Why this package exists: the source SDD's doctor search query does
//
//	EXISTS (SELECT 1 FROM slots s WHERE s.doctor_id = d.id AND s.status = 'AVAILABLE' ...)
//
// but `slots` lives in telemed_scheduling, a different logical database
// (ADR-004 in _shared/DECISIONS.md forbids the cross-service join this
// implies). This package is how doctor search answers "is this doctor
// available" without one. See docs/DESIGN.md for the full design writeup,
// including the event-payload contract this package assumes -- scheduling
// service does not exist yet in this multi-repo build, so the payload shape
// below is this repo's proposal, not a verified contract.
package availability

import (
	"time"

	"github.com/google/uuid"
)

// SlotStatus is this package's reduced view of a scheduling-service slot:
// just enough to answer "can a patient book this doctor soon". It is not the
// scheduling service's authoritative slot state machine (which also has
// BLOCKED and CANCELLED); those never make a slot searchable, so they never
// need to appear here.
type SlotStatus string

const (
	SlotAvailable SlotStatus = "AVAILABLE"
	SlotBooked    SlotStatus = "BOOKED"
	// SlotWithdrawn is a slot the doctor took off the market for good --
	// today, always because they registered leave over a day whose slots had
	// already been generated.
	//
	// It is distinct from BOOKED even though search treats both as "not
	// available", because the two are read by humans during incidents and a
	// withdrawn slot recorded as booked is a lie. It is distinct from having no
	// row at all because last_event_id / last_event_at are what make this
	// projection converge under unordered redelivery, and deleting the row
	// would let a late slot.booked resurrect it.
	SlotWithdrawn SlotStatus = "WITHDRAWN"
)

// SlotEventPayload is this projection's narrow view of slot.booked,
// slot.released and slot.withdrawn. It is the intersection of the canonical
// events.SlotBooked, events.SlotReleased and events.SlotWithdrawn: all three
// carry slot_id, doctor_id, start_at and end_at with exactly these JSON names,
// and this projection needs nothing else from any of them.
//
// It is deliberately NOT a third re-declaration of a payload -- the fields are
// pinned against the canonical structs by TestSlotEventPayloadMatchesCanonical
// in consumer_contract_test.go, so a rename on either canonical type fails the
// build's test gate instead of silently decoding to zeroes.
//
// slot.generated does not fit here: it is a batch event with no slot_id. See
// Consumer.Subjects.
type SlotEventPayload struct {
	SlotID   uuid.UUID `json:"slot_id"`
	DoctorID uuid.UUID `json:"doctor_id"`
	StartAt  time.Time `json:"start_at"`
	EndAt    time.Time `json:"end_at"`
}

// SlotState is one row of our local slot mirror (doctor_slot_state).
type SlotState struct {
	SlotID      uuid.UUID
	DoctorID    uuid.UUID
	StartAt     time.Time
	SlotDate    time.Time // truncated to the Colombo-local date
	Status      SlotStatus
	LastEventID uuid.UUID
	LastEventAt time.Time
}

// DailySummary is one row of doctor_availability_summary: what search
// actually reads.
type DailySummary struct {
	DoctorID           uuid.UUID
	Date               time.Time
	AvailableSlotCount int
	NextAvailableAt    *time.Time
}

// Package appointments implements the admin console's appointment
// list/search, force-cancel and double-booking resolution (V2 docs section
// 7.3 page 5). It reads appointments_projection -- the same table
// internal/analytics.Projector populates from appointment.* events, so
// there is exactly one consumer of those events, not two competing ones.
// force-cancel and double-booking resolution are commands published over
// the outbox: this service holds no connection to telemed_scheduling and
// cannot mutate an appointment directly (ADR-004).
package appointments

import (
	"time"

	"github.com/google/uuid"
)

// Appointment is a read-only view over appointments_projection.
type Appointment struct {
	AppointmentID uuid.UUID
	DoctorID      uuid.UUID
	PatientID     uuid.UUID
	SpecialtyCode string
	District      string
	Status        string
	ScheduledAt   time.Time
	OccurredAt    time.Time
}

type ListFilter struct {
	Status    string
	DoctorID  uuid.UUID
	PatientID uuid.UUID
	From      time.Time
	To        time.Time
	Page      int
	PerPage   int
}

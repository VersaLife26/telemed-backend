// Package disputes implements patient complaints and refund requests
// (SDD/V2 docs section 7.3 page 8: "Disputes: Patient complaints, refund
// requests, chat mediation"). appointment_id/patient_id/doctor_id are UUIDs
// meaning rows in other services' databases -- no FK, per ADR-004.
package disputes

import (
	"time"

	"github.com/google/uuid"
)

type Dispute struct {
	ID                uuid.UUID
	AppointmentID     uuid.UUID
	PatientID         uuid.UUID
	DoctorID          uuid.UUID
	Category          string
	Description       string
	Status            string
	AssignedTo        *uuid.UUID
	Resolution        string
	RefundRequested   bool
	RefundAmountCents *int64
	Currency          string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Version           int
}

type Comment struct {
	ID            uuid.UUID
	DisputeID     uuid.UUID
	AuthorAdminID uuid.UUID
	Body          string
	CreatedAt     time.Time
}

// CreateParams opens a new dispute. Disputes normally arrive as a
// consequence of a patient complaint filed through a different channel
// (support ticket, patient app); this service is where an admin records and
// works them, not necessarily where they always originate, so Create is
// intentionally available directly on the admin API rather than only via an
// event.
type CreateParams struct {
	AppointmentID   uuid.UUID
	PatientID       uuid.UUID
	DoctorID        uuid.UUID
	Category        string
	Description     string
	RefundRequested bool
}

type ListFilter struct {
	Status     string
	AssignedTo uuid.UUID
	Page       int
	PerPage    int
}

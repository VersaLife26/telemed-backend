// Package prescriptions owns e-prescriptions: doctor-issued drug orders,
// their tamper-evident PDF + QR rendering, and the public verification
// endpoint a pharmacist scans against.
package prescriptions

import (
	"time"

	"github.com/google/uuid"
)

// Status is the prescription lifecycle.
type Status string

const (
	StatusIssued    Status = "issued"
	StatusDispensed Status = "dispensed"
	StatusCancelled Status = "cancelled"
)

// Item is one drug line on a prescription.
type Item struct {
	ID             uuid.UUID
	PrescriptionID uuid.UUID
	DrugName       string
	Strength       string
	Form           string
	Dosage         string
	Frequency      string
	DurationDays   int
	Quantity       int
	Instructions   string
	IsGeneric      bool
	SortOrder      int
}

// Prescription is one e-prescription with its drug lines.
type Prescription struct {
	ID                      uuid.UUID
	AppointmentID           uuid.UUID
	DoctorID                uuid.UUID
	PatientID               uuid.UUID
	DoctorName              string
	DoctorSLMC              string
	DoctorQualifications    string
	IssuedAt                time.Time
	PDFObjectKey            string
	VerificationHMAC        string
	Status                  Status
	FHIRMedicationRequestID string
	CreatedAt               time.Time
	UpdatedAt               time.Time
	Version                 int
	Items                   []Item
}

// Drug is one row of the Sri Lankan formulary reference table.
type Drug struct {
	ID           uuid.UUID
	Name         string
	GenericName  string
	Strength     string
	Form         string
	Manufacturer string
	Category     string
	IsControlled bool
	IsGeneric    bool
}

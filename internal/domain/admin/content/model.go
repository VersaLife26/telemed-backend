// Package content manages the four reference-data sets the admin console
// owns outright: specialties, symptoms, the Sri Lankan drug formulary, and
// waiting-room educational articles (V2 docs section 7.3 page 7 / 8.8).
// Creates and updates to specialties/symptoms/drugs publish content.* events
// via the outbox so other services can keep a local read copy without a
// cross-database join (ADR-004).
package content

import (
	"time"

	"github.com/google/uuid"
)

type Specialty struct {
	ID        uuid.UUID
	Code      string
	NameEN    string
	NameSI    string
	NameTA    string
	Active    bool
	CreatedAt time.Time
	UpdatedAt time.Time
	Version   int
}

type Symptom struct {
	ID             uuid.UUID
	Code           string
	NameEN         string
	NameSI         string
	NameTA         string
	SpecialtyCodes []string
	Active         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Version        int
}

type Drug struct {
	ID           uuid.UUID
	Name         string
	Strength     string
	Form         string
	Manufacturer string
	Active       bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Version      int
}

type Article struct {
	ID            uuid.UUID
	Title         string
	Slug          string
	Body          string
	Language      string
	SpecialtyCode string
	Published     bool
	PublishedAt   *time.Time
	AuthorAdminID *uuid.UUID
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Version       int
}

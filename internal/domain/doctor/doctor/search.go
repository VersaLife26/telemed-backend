package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SortField is a doctor-search sort option.
type SortField string

const (
	SortRating        SortField = "rating"
	SortFee           SortField = "fee"
	SortExperience    SortField = "experience"
	SortNextAvailable SortField = "next_available"
)

// Valid reports whether f is a sort option the search endpoint recognises.
func (f SortField) Valid() bool {
	switch f {
	case SortRating, SortFee, SortExperience, SortNextAvailable, "":
		return true
	}
	return false
}

// SearchFilters is the fully-normalised, validated shape of a doctor search.
// Nothing downstream of this struct touches an *http.Request.
type SearchFilters struct {
	Specialty        string
	Language         string
	MinFeeCents      *int64
	MaxFeeCents      *int64
	MinRating        *float64
	Query            string    // free text over display_name/bio
	AvailableAfter   time.Time // lower bound for "next available"; callers default this to time.Now()
	RequireAvailable bool      // true for available=now / available_after; false = availability is only used for sort/display
	SortBy           SortField
	Page, PerPage    int
}

func (f SearchFilters) offset() int { return (f.Page - 1) * f.PerPage }

func (f SearchFilters) needsAvailabilityJoin() bool {
	return f.RequireAvailable || f.SortBy == SortNextAvailable
}

// SearchResult is one row of a search response: the doctor plus the
// availability projection's answer for them, which the doctors table itself
// does not carry (see docs/DESIGN.md).
type SearchResult struct {
	Doctor             Doctor
	NextAvailableAt    *time.Time
	AvailableSlotCount int
}

// searchQuery is the assembled-but-not-yet-run shape of a search: every
// filter resolved to a SQL fragment and its bound parameter, in one place so
// it can be unit tested without a database. Search() is the only caller;
// tests call buildSearchQuery directly.
type searchQuery struct {
	joinClause  string
	whereClause string
	orderBy     string
	args        []any
	needsJoin   bool
}

// buildSearchQuery assembles the WHERE/JOIN/ORDER BY fragments and bound
// parameters for f. Every value is bound as a parameter ($1, $2, ...); the
// SQL text itself is built only from static fragments chosen by which
// filters are present, never by interpolating a filter's value into it.
//
// It never references telemed_scheduling's `slots` table (ADR-004):
// availability comes entirely from doctor_availability_summary, a local
// projection built from consumed slot events (see internal/availability).
func buildSearchQuery(f SearchFilters) searchQuery {
	var (
		conds []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	conds = append(conds, "d.verification_status = 'approved'", "d.deleted_at IS NULL")

	if f.Specialty != "" {
		conds = append(conds, "d.specialty = "+arg(f.Specialty))
	}
	if f.Language != "" {
		conds = append(conds, arg(f.Language)+" = ANY(d.languages)")
	}
	if f.MinFeeCents != nil {
		conds = append(conds, "d.fee_cents >= "+arg(*f.MinFeeCents))
	}
	if f.MaxFeeCents != nil {
		conds = append(conds, "d.fee_cents <= "+arg(*f.MaxFeeCents))
	}
	if f.MinRating != nil {
		conds = append(conds, "d.rating >= "+arg(*f.MinRating))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		conds = append(conds, "d.search_vector @@ websearch_to_tsquery('english', "+arg(q)+")")
	}

	needsJoin := f.needsAvailabilityJoin()
	joinClause := ""
	if needsJoin {
		after := f.AvailableAfter
		if after.IsZero() {
			after = time.Now().UTC()
		}
		joinClause = `
			LEFT JOIN LATERAL (
				SELECT MIN(das.next_available_at) AS next_available_at,
				       SUM(das.available_slot_count) AS available_slot_count
				FROM doctor_availability_summary das
				WHERE das.doctor_id = d.id
				  AND das.available_slot_count > 0
				  AND das.next_available_at >= ` + arg(after) + `
			) avail ON TRUE`
		if f.RequireAvailable {
			conds = append(conds, "avail.next_available_at IS NOT NULL")
		}
	}

	return searchQuery{
		joinClause:  joinClause,
		whereClause: strings.Join(conds, " AND "),
		orderBy:     orderByFor(f.SortBy),
		args:        args,
		needsJoin:   needsJoin,
	}
}

// Search runs the doctor search query built by buildSearchQuery.
func (r *Repository) Search(ctx context.Context, f SearchFilters) ([]SearchResult, int64, error) {
	sq := buildSearchQuery(f)
	args := sq.args
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	base := fmt.Sprintf(`
		FROM doctors d
		%s
		WHERE %s`, sq.joinClause, sq.whereClause)

	var total int64
	countQ := "SELECT COUNT(*) " + base
	if err := r.pool.QueryRow(ctx, countQ, sq.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("doctor: search count: %w", err)
	}

	selectCols := doctorColumns
	if sq.needsJoin {
		selectCols += ", avail.next_available_at, avail.available_slot_count"
	} else {
		selectCols += ", NULL::timestamptz, 0"
	}

	limitArg := arg(f.PerPage)
	offsetArg := arg(f.offset())
	listQ := fmt.Sprintf(`
		SELECT %s
		%s
		ORDER BY %s
		LIMIT %s OFFSET %s`, selectCols, base, sq.orderBy, limitArg, offsetArg)

	rows, err := r.pool.Query(ctx, listQ, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("doctor: search: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		d, nextAvail, availCount, err := scanSearchRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("doctor: scan search row: %w", err)
		}
		out = append(out, SearchResult{Doctor: d, NextAvailableAt: nextAvail, AvailableSlotCount: availCount})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("doctor: search rows: %w", err)
	}
	return out, total, nil
}

// scanSearchRow scans a row shaped like doctorColumns plus
// (next_available_at, available_slot_count). It duplicates scanDoctor's
// field list rather than reusing pgx.Rows as a pgx.Row because the extra two
// columns must be scanned in the same call.
func scanSearchRow(rows interface {
	Scan(dest ...any) error
}) (Doctor, *time.Time, int, error) {
	var d Doctor
	var subSpecialties, languages []string
	var quals []byte
	var bankEncrypted, bio, photoURL, rejectionReason *string
	var verifiedAt, deletedAt, nextAvailableAt *time.Time
	var verifiedBy *uuid.UUID
	var status string
	var availCount int

	err := rows.Scan(
		&d.ID, &d.UserID, &d.SLMCNumber, &d.Specialty, &subSpecialties, &d.ExperienceYears, &d.FeeCents,
		&d.Currency, &d.DisplayName, &languages, &bio, &photoURL, &quals,
		&status, &rejectionReason, &verifiedAt, &verifiedBy, &bankEncrypted,
		&d.Rating, &d.ReviewCount, &d.ConsultationCount, &d.NoShowRate, &d.AcceptsNewPatients,
		&d.CreatedAt, &d.UpdatedAt, &deletedAt, &d.Version,
		&nextAvailableAt, &availCount,
	)
	if err != nil {
		return Doctor{}, nil, 0, err
	}

	d.VerificationStatus = VerificationStatus(status)
	d.SubSpecialties = subSpecialties
	d.Languages = stringsToLanguages(languages)
	d.BankEncrypted = deref(bankEncrypted)
	d.Bio = deref(bio)
	d.PhotoURL = deref(photoURL)
	d.RejectionReason = deref(rejectionReason)
	d.VerifiedAt = verifiedAt
	d.VerifiedBy = verifiedBy
	if len(quals) > 0 {
		if jerr := json.Unmarshal(quals, &d.Qualifications); jerr != nil {
			return Doctor{}, nil, 0, fmt.Errorf("doctor: unmarshal qualifications: %w", jerr)
		}
	}
	return d, nextAvailableAt, availCount, nil
}

func orderByFor(s SortField) string {
	switch s {
	case SortFee:
		return "d.fee_cents ASC, d.id ASC"
	case SortExperience:
		return "d.experience_years DESC, d.id ASC"
	case SortNextAvailable:
		return "avail.next_available_at ASC NULLS LAST, d.id ASC"
	default:
		// SortRating and anything unrecognised. Rating is the platform default
		// because a patient with no stated preference is choosing on quality.
		return "d.rating DESC, d.review_count DESC, d.id ASC"
	}
}

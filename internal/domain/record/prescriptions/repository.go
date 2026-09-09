package prescriptions

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"telemed/internal/platform/database"
)

// dbtx is the narrow slice of pgx.Tx / database.Pool this repository needs.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Repository is the SQL layer for prescriptions, prescription_items and the
// read-only drugs formulary.
type Repository struct{}

// NewRepository constructs the (stateless) repository.
func NewRepository() *Repository { return &Repository{} }

const prescriptionColumns = `id, appointment_id, doctor_id, patient_id, doctor_name, doctor_slmc,
	COALESCE(doctor_qualifications, ''), issued_at, COALESCE(pdf_object_key, ''), verification_hmac,
	status, COALESCE(fhir_medication_request_id, ''), created_at, updated_at, version`

func scanPrescription(row pgx.Row) (Prescription, error) {
	var p Prescription
	var status string
	err := row.Scan(&p.ID, &p.AppointmentID, &p.DoctorID, &p.PatientID, &p.DoctorName, &p.DoctorSLMC,
		&p.DoctorQualifications, &p.IssuedAt, &p.PDFObjectKey, &p.VerificationHMAC,
		&status, &p.FHIRMedicationRequestID, &p.CreatedAt, &p.UpdatedAt, &p.Version)
	p.Status = Status(status)
	return p, err
}

// Create inserts a prescription and its items in one statement group. The
// caller is expected to run this inside a transaction alongside the outbox
// enqueue (see Service.Issue).
func (r *Repository) Create(ctx context.Context, db dbtx, p Prescription) (Prescription, error) {
	q := fmt.Sprintf(`
		INSERT INTO prescriptions (id, appointment_id, doctor_id, patient_id, doctor_name, doctor_slmc,
			doctor_qualifications, issued_at, pdf_object_key, verification_hmac, status, fhir_medication_request_id)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, NULLIF($9, ''), $10, $11, NULLIF($12, ''))
		RETURNING %s`, prescriptionColumns)
	row := db.QueryRow(ctx, q, p.ID, p.AppointmentID, p.DoctorID, p.PatientID, p.DoctorName, p.DoctorSLMC,
		p.DoctorQualifications, p.IssuedAt, p.PDFObjectKey, p.VerificationHMAC, string(p.Status), p.FHIRMedicationRequestID)
	out, err := scanPrescription(row)
	if err != nil {
		return Prescription{}, fmt.Errorf("prescriptions: create: %w", err)
	}

	for i := range p.Items {
		it := p.Items[i]
		it.PrescriptionID = out.ID
		it.SortOrder = i
		if it.ID == uuid.Nil {
			it.ID = uuid.New()
		}
		const itemQ = `
			INSERT INTO prescription_items
				(id, prescription_id, drug_name, strength, form, dosage, frequency, duration_days, quantity, instructions, is_generic, sort_order)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`
		if _, err := db.Exec(ctx, itemQ, it.ID, it.PrescriptionID, it.DrugName, it.Strength, it.Form,
			it.Dosage, it.Frequency, it.DurationDays, it.Quantity, it.Instructions, it.IsGeneric, it.SortOrder); err != nil {
			return Prescription{}, fmt.Errorf("prescriptions: create item: %w", err)
		}
		out.Items = append(out.Items, it)
	}
	return out, nil
}

// GetByID fetches a prescription with its items.
func (r *Repository) GetByID(ctx context.Context, db dbtx, id uuid.UUID) (Prescription, bool, error) {
	return r.getWhere(ctx, db, `id = $1`, id)
}

// GetByAppointment fetches the (at most one) prescription for an appointment.
func (r *Repository) GetByAppointment(ctx context.Context, db dbtx, appointmentID uuid.UUID) (Prescription, bool, error) {
	return r.getWhere(ctx, db, `appointment_id = $1`, appointmentID)
}

func (r *Repository) getWhere(ctx context.Context, db dbtx, where string, id uuid.UUID) (Prescription, bool, error) {
	q := fmt.Sprintf(`SELECT %s FROM prescriptions WHERE %s`, prescriptionColumns, where)
	out, err := scanPrescription(db.QueryRow(ctx, q, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Prescription{}, false, nil
		}
		return Prescription{}, false, fmt.Errorf("prescriptions: get: %w", err)
	}
	items, err := r.itemsFor(ctx, db, out.ID)
	if err != nil {
		return Prescription{}, false, err
	}
	out.Items = items
	return out, true, nil
}

func (r *Repository) itemsFor(ctx context.Context, db dbtx, prescriptionID uuid.UUID) ([]Item, error) {
	const q = `
		SELECT id, prescription_id, drug_name, strength, form, dosage, frequency, duration_days, quantity,
		       COALESCE(instructions, ''), is_generic, sort_order
		FROM prescription_items WHERE prescription_id = $1 ORDER BY sort_order`
	rows, err := db.Query(ctx, q, prescriptionID)
	if err != nil {
		return nil, fmt.Errorf("prescriptions: list items: %w", err)
	}
	defer rows.Close()

	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.PrescriptionID, &it.DrugName, &it.Strength, &it.Form, &it.Dosage,
			&it.Frequency, &it.DurationDays, &it.Quantity, &it.Instructions, &it.IsGeneric, &it.SortOrder); err != nil {
			return nil, fmt.Errorf("prescriptions: scan item: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("prescriptions: list items rows: %w", err)
	}
	return out, nil
}

// SetPDFObjectKey records where the generated PDF landed in object storage.
func (r *Repository) SetPDFObjectKey(ctx context.Context, db dbtx, id uuid.UUID, key string, expectedVersion int) error {
	const q = `UPDATE prescriptions SET pdf_object_key = $3, version = version + 1 WHERE id = $1 AND version = $2`
	tag, err := db.Exec(ctx, q, id, expectedVersion, key)
	if err != nil {
		return fmt.Errorf("prescriptions: set pdf key: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return database.ErrOptimisticLock
	}
	return nil
}

// SetFHIRReference stores the MedicationRequest id(s).
func (r *Repository) SetFHIRReference(ctx context.Context, db dbtx, id uuid.UUID, fhirRefID string, expectedVersion int) error {
	const q = `UPDATE prescriptions SET fhir_medication_request_id = $3, version = version + 1 WHERE id = $1 AND version = $2`
	tag, err := db.Exec(ctx, q, id, expectedVersion, fhirRefID)
	if err != nil {
		return fmt.Errorf("prescriptions: set fhir reference: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return database.ErrOptimisticLock
	}
	return nil
}

// UpdateStatus transitions a prescription's status, e.g. to "dispensed".
func (r *Repository) UpdateStatus(ctx context.Context, db dbtx, id uuid.UUID, status Status, expectedVersion int) error {
	const q = `UPDATE prescriptions SET status = $3, version = version + 1 WHERE id = $1 AND version = $2`
	tag, err := db.Exec(ctx, q, id, expectedVersion, string(status))
	if err != nil {
		return fmt.Errorf("prescriptions: update status: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return database.ErrOptimisticLock
	}
	return nil
}

// ListByPatient returns a patient's prescriptions, newest first.
func (r *Repository) ListByPatient(ctx context.Context, db dbtx, patientID uuid.UUID, page, perPage int) ([]Prescription, int64, error) {
	return r.list(ctx, db, `patient_id = $1`, patientID, page, perPage)
}

// ListByDoctor returns a doctor's issued prescriptions, newest first.
func (r *Repository) ListByDoctor(ctx context.Context, db dbtx, doctorID uuid.UUID, page, perPage int) ([]Prescription, int64, error) {
	return r.list(ctx, db, `doctor_id = $1`, doctorID, page, perPage)
}

func (r *Repository) list(ctx context.Context, db dbtx, where string, id uuid.UUID, page, perPage int) ([]Prescription, int64, error) {
	var total int64
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM prescriptions WHERE `+where, id).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("prescriptions: count: %w", err)
	}

	q := fmt.Sprintf(`SELECT %s FROM prescriptions WHERE %s ORDER BY issued_at DESC LIMIT $2 OFFSET $3`, prescriptionColumns, where)
	rows, err := db.Query(ctx, q, id, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("prescriptions: list: %w", err)
	}
	defer rows.Close()

	var out []Prescription
	for rows.Next() {
		p, err := scanPrescription(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("prescriptions: scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("prescriptions: list rows: %w", err)
	}
	return out, total, nil
}

// SearchDrugs performs a case-insensitive prefix search over the formulary
// by brand or generic name.
func (r *Repository) SearchDrugs(ctx context.Context, db dbtx, query string, limit int) ([]Drug, error) {
	const q = `
		SELECT id, name, generic_name, strength, form, COALESCE(manufacturer, ''), COALESCE(category, ''),
		       is_controlled, is_generic
		FROM drugs
		WHERE lower(name) LIKE lower($1) || '%' OR lower(generic_name) LIKE lower($1) || '%'
		ORDER BY name LIMIT $2`
	rows, err := db.Query(ctx, q, query, limit)
	if err != nil {
		return nil, fmt.Errorf("prescriptions: search drugs: %w", err)
	}
	defer rows.Close()

	var out []Drug
	for rows.Next() {
		var d Drug
		if err := rows.Scan(&d.ID, &d.Name, &d.GenericName, &d.Strength, &d.Form, &d.Manufacturer, &d.Category, &d.IsControlled, &d.IsGeneric); err != nil {
			return nil, fmt.Errorf("prescriptions: scan drug: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("prescriptions: search drugs rows: %w", err)
	}
	return out, nil
}

var (
	_ dbtx = database.Pool(nil)
	_ dbtx = pgx.Tx(nil)
)

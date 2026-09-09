//go:build integration

// These tests exist because of _shared/INTEGRATION-FIXES.md item 9: a
// producer and a consumer each declared their own private struct for
// doctor.approved, the producer never sent doctor_name or email, and
// encoding/json filled both with "". The approval notification went out
// addressed to nobody and nothing logged an error.
//
// Sharing the canonical events.* types makes that particular mismatch a
// compile error. What a compiler cannot check is the other half of the fix:
// that this service actually resolves the fields the canonical payloads
// deliberately leave out, instead of quietly rendering an empty string. That
// is what these tests pin down.
package notification_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"telemed/internal/domain/notification/notification"
	"telemed/internal/platform/events"
)

// fakeDirectory stands in for the gRPC call to user-service.
type fakeDirectory struct {
	users map[uuid.UUID]notification.Contact
	// err, when set, is returned for every lookup -- used to prove an
	// unreachable directory is a retry, not a silent empty recipient.
	err error
}

func (f *fakeDirectory) Contact(_ context.Context, userID uuid.UUID) (notification.Contact, error) {
	if f.err != nil {
		return notification.Contact{}, f.err
	}
	c, ok := f.users[userID]
	if !ok {
		return notification.Contact{}, notification.ErrContactNotFound
	}
	return c, nil
}

func testConsumer(pool *pgxpool.Pool, dir notification.Directory) (*notification.Consumer, *notification.Service, *notification.Repository) {
	svc, repo := testService(pool)
	c := notification.NewConsumer(svc, repo, dir, notification.NewLinks("https://app.test.lk"), zerolog.Nop())
	return c, svc, repo
}

func envelope(t *testing.T, subject events.Subject, payload any) events.Envelope {
	t.Helper()
	env, err := events.NewEnvelope(subject, "test", uuid.NewString(), payload)
	if err != nil {
		t.Fatalf("build envelope for %s: %v", subject, err)
	}
	return env
}

// bodyOf returns the rendered body of the one notification queued for user on
// the given channel and template, failing if there is not exactly one.
//
// It also asserts the recipient column is non-empty, which is the whole point:
// a queued notification with no address is the item-9 failure mode wearing a
// different hat.
func bodyOf(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, channel string, template notification.TemplateKey) string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT body, COALESCE(recipient, '') FROM notifications
		  WHERE user_id = $1 AND channel = $2 AND template_key = $3`, userID, channel, string(template))
	if err != nil {
		t.Fatalf("query notifications: %v", err)
	}
	defer rows.Close()

	var bodies, recipients []string
	for rows.Next() {
		var b, r string
		if err := rows.Scan(&b, &r); err != nil {
			t.Fatalf("scan notification: %v", err)
		}
		bodies = append(bodies, b)
		recipients = append(recipients, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate notifications: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("expected exactly 1 %s/%s notification for %s, got %d", channel, template, userID, len(bodies))
	}
	if recipients[0] == "" {
		t.Fatalf("%s notification was queued with an EMPTY recipient -- this is the item-9 failure mode", channel)
	}
	return bodies[0]
}

// TestDoctorApproved_CarriesNameAndEmail is the direct regression test for
// INTEGRATION-FIXES item 9.
func TestDoctorApproved_CarriesNameAndEmail(t *testing.T) {
	pool := setupDB(t)
	c, _, repo := testConsumer(pool, &fakeDirectory{})
	ctx := context.Background()

	doctorID, userID := uuid.New(), uuid.New()
	env := envelope(t, events.SubjectDoctorApproved, events.DoctorApproved{
		DoctorID: doctorID, UserID: userID,
		DoctorName: "Anushka Perera", Email: "anushka@example.lk",
		Specialty: "cardiology", FeeCents: 250000, Currency: "LKR",
		ApprovedAt: time.Now().UTC(),
	})

	if err := c.Handle(ctx, env); err != nil {
		t.Fatalf("handle doctor.approved: %v", err)
	}

	body := bodyOf(t, pool, userID, "email", notification.TemplateDoctorApproved)
	if !strings.Contains(body, "Anushka Perera") {
		t.Errorf("approval email does not name the doctor; body = %q", body)
	}

	// The same event must also have seeded the local projection every other
	// handler reads from.
	doc, err := repo.DoctorByID(ctx, repo.Pool(), doctorID)
	if err != nil {
		t.Fatalf("doctor.approved did not populate doctor_directory: %v", err)
	}
	if doc.FullName != "Anushka Perera" || doc.UserID != userID || doc.FeeCents != 250000 {
		t.Errorf("projected doctor = %+v, want name/user/fee from the event", doc)
	}
}

// TestDoctorUpdated_DoesNotBlankTheName pins the projection rule that matters
// most: doctor.updated carries specialty/fee/status but NOT the name or
// email, so applying it must leave those columns alone. Overwriting them with
// the payload's zero values would reintroduce the nameless notification by a
// different route.
func TestDoctorUpdated_DoesNotBlankTheName(t *testing.T) {
	pool := setupDB(t)
	c, _, repo := testConsumer(pool, &fakeDirectory{})
	ctx := context.Background()

	doctorID, userID := uuid.New(), uuid.New()
	if err := c.Handle(ctx, envelope(t, events.SubjectDoctorApproved, events.DoctorApproved{
		DoctorID: doctorID, UserID: userID, DoctorName: "Nimal Silva",
		Email: "nimal@example.lk", Specialty: "general", FeeCents: 150000, Currency: "LKR",
	})); err != nil {
		t.Fatalf("handle doctor.approved: %v", err)
	}

	if err := c.Handle(ctx, envelope(t, events.SubjectDoctorUpdated, events.DoctorUpdated{
		DoctorID: doctorID, Specialty: "dermatology", FeeCents: 300000,
		Currency: "LKR", Status: "approved", UpdatedAt: time.Now().UTC(),
	})); err != nil {
		t.Fatalf("handle doctor.updated: %v", err)
	}

	doc, err := repo.DoctorByID(ctx, repo.Pool(), doctorID)
	if err != nil {
		t.Fatalf("read projection: %v", err)
	}
	if doc.FullName != "Nimal Silva" {
		t.Errorf("doctor.updated blanked the name: got %q, want %q", doc.FullName, "Nimal Silva")
	}
	if doc.Email != "nimal@example.lk" {
		t.Errorf("doctor.updated blanked the email: got %q", doc.Email)
	}
	if doc.Specialty != "dermatology" || doc.FeeCents != 300000 {
		t.Errorf("doctor.updated did not apply what it DID carry: %+v", doc)
	}
}

// TestAppointmentConfirmed_QuotesTheBookedPriceNotTheCurrentOne is the reason
// this service consumes appointment.created at all.
//
// events.AppointmentConfirmed carries no money. The doctor's current list
// price is the wrong substitute: a patient who booked at LKR 2,000 must be
// told LKR 2,000 even if the doctor raised their fee in between. The quote
// recorded from appointment.created is the right one.
func TestAppointmentConfirmed_QuotesTheBookedPriceNotTheCurrentOne(t *testing.T) {
	pool := setupDB(t)
	patientID, doctorID, doctorUserID, appointmentID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	dir := &fakeDirectory{users: map[uuid.UUID]notification.Contact{
		patientID: {UserID: patientID, Name: "Kamal", Phone: "+94771234567", Email: "kamal@example.lk"},
	}}
	c, _, _ := testConsumer(pool, dir)
	ctx := context.Background()

	// The doctor's list price at approval: LKR 2,000.
	if err := c.Handle(ctx, envelope(t, events.SubjectDoctorApproved, events.DoctorApproved{
		DoctorID: doctorID, UserID: doctorUserID, DoctorName: "Ruwan Fernando",
		Email: "ruwan@example.lk", Specialty: "general", FeeCents: 200000, Currency: "LKR",
	})); err != nil {
		t.Fatalf("handle doctor.approved: %v", err)
	}

	startAt := time.Now().Add(48 * time.Hour).UTC()
	if err := c.Handle(ctx, envelope(t, events.SubjectAppointmentCreated, events.AppointmentCreated{
		AppointmentID: appointmentID, PatientID: patientID, DoctorID: doctorID,
		StartAt: startAt, Status: "pending_payment",
		AmountCents: 200000, Currency: "LKR", Specialty: "general",
	})); err != nil {
		t.Fatalf("handle appointment.created: %v", err)
	}

	// The doctor raises their fee to LKR 5,000 before the booking is paid.
	if err := c.Handle(ctx, envelope(t, events.SubjectDoctorUpdated, events.DoctorUpdated{
		DoctorID: doctorID, Specialty: "general", FeeCents: 500000,
		Currency: "LKR", Status: "approved",
	})); err != nil {
		t.Fatalf("handle doctor.updated: %v", err)
	}

	if err := c.Handle(ctx, envelope(t, events.SubjectAppointmentConfirmed, events.AppointmentConfirmed{
		AppointmentID: appointmentID, PatientID: patientID, DoctorID: doctorID,
		StartAt: startAt, PaymentID: uuid.New(), ConfirmedAt: time.Now().UTC(),
	})); err != nil {
		t.Fatalf("handle appointment.confirmed: %v", err)
	}

	body := bodyOf(t, pool, patientID, "sms", notification.TemplateBookingConfirmed)
	if !strings.Contains(body, "Rs. 2,000.00") {
		t.Errorf("confirmation must quote the booked price Rs. 2,000.00; body = %q", body)
	}
	if strings.Contains(body, "Rs. 5,000.00") {
		t.Errorf("confirmation quoted the doctor's CURRENT price instead of the booked one; body = %q", body)
	}
	if !strings.Contains(body, "Ruwan Fernando") {
		t.Errorf("confirmation does not name the doctor; body = %q", body)
	}
}

// TestAppointmentConfirmed_WithoutAQuoteIsRetriedNotGuessed. appointment.created
// causally precedes appointment.confirmed, so a missing quote means this
// consumer is ahead of the stream. Redelivery fixes that; inventing a number,
// or sending "Fee: ", does not.
func TestAppointmentConfirmed_WithoutAQuoteIsRetriedNotGuessed(t *testing.T) {
	pool := setupDB(t)
	patientID, doctorID := uuid.New(), uuid.New()

	dir := &fakeDirectory{users: map[uuid.UUID]notification.Contact{
		patientID: {UserID: patientID, Phone: "+94771234567"},
	}}
	c, _, _ := testConsumer(pool, dir)
	ctx := context.Background()

	if err := c.Handle(ctx, envelope(t, events.SubjectDoctorApproved, events.DoctorApproved{
		DoctorID: doctorID, UserID: uuid.New(), DoctorName: "Ruwan Fernando",
		Specialty: "general", FeeCents: 200000, Currency: "LKR",
	})); err != nil {
		t.Fatalf("handle doctor.approved: %v", err)
	}

	err := c.Handle(ctx, envelope(t, events.SubjectAppointmentConfirmed, events.AppointmentConfirmed{
		AppointmentID: uuid.New(), PatientID: patientID, DoctorID: doctorID,
		StartAt: time.Now().Add(time.Hour).UTC(),
	}))
	if err == nil {
		t.Fatal("a confirmation with no recorded quote must fail so JetStream redelivers it")
	}
}

// TestUnknownDoctorIsRetried: an event naming a doctor this service has never
// seen approved must not produce "Your appointment with Dr. ".
func TestUnknownDoctorIsRetried(t *testing.T) {
	pool := setupDB(t)
	patientID := uuid.New()
	dir := &fakeDirectory{users: map[uuid.UUID]notification.Contact{
		patientID: {UserID: patientID, Phone: "+94771234567"},
	}}
	c, _, _ := testConsumer(pool, dir)

	err := c.Handle(context.Background(), envelope(t, events.SubjectConsultationStarted, events.ConsultationStarted{
		ConsultationID: uuid.New(), AppointmentID: uuid.New(),
		PatientID: patientID, DoctorID: uuid.New(), RoomName: "room-1",
	}))
	if !errors.Is(err, notification.ErrDoctorNotProjected) {
		t.Fatalf("expected ErrDoctorNotProjected so the event is redelivered, got %v", err)
	}
}

// TestUnreachableDirectoryIsRetried distinguishes the two failure modes that
// look identical from the outside. A directory that is DOWN must retry; a
// user that does not EXIST must not.
func TestUnreachableDirectoryIsRetried(t *testing.T) {
	pool := setupDB(t)
	doctorID, patientID := uuid.New(), uuid.New()
	boom := errors.New("connection refused")
	dir := &fakeDirectory{err: boom}
	c, _, _ := testConsumer(pool, dir)
	ctx := context.Background()

	if err := c.Handle(ctx, envelope(t, events.SubjectDoctorApproved, events.DoctorApproved{
		DoctorID: doctorID, UserID: uuid.New(), DoctorName: "Ruwan Fernando",
		Specialty: "general", FeeCents: 200000, Currency: "LKR",
	})); err != nil {
		t.Fatalf("handle doctor.approved: %v", err)
	}

	err := c.Handle(ctx, envelope(t, events.SubjectPaymentFailed, events.PaymentFailed{
		PaymentID: uuid.New(), AppointmentID: uuid.New(), PatientID: patientID,
		AmountCents: 200000, Currency: "LKR", Reason: "card_declined",
	}))
	if !errors.Is(err, boom) {
		t.Fatalf("an unreachable directory must surface as a retryable error, got %v", err)
	}

	// A user that genuinely does not exist is the opposite: acknowledge and
	// move on, because redelivering forever will not conjure them up.
	c2, _, _ := testConsumer(pool, &fakeDirectory{users: map[uuid.UUID]notification.Contact{}})
	if err := c2.Handle(ctx, envelope(t, events.SubjectPaymentFailed, events.PaymentFailed{
		PaymentID: uuid.New(), AppointmentID: uuid.New(), PatientID: uuid.New(),
		AmountCents: 200000, Currency: "LKR", Reason: "card_declined",
	})); err != nil {
		t.Fatalf("an unknown recipient must be acknowledged, not retried forever: %v", err)
	}
}

// TestPayoutSent_AddressesTheUserBehindTheDoctor. events.PayoutSent names a
// DoctorID; notifications and preferences belong to the USER behind it. The
// mapping lives in the local projection.
func TestPayoutSent_AddressesTheUserBehindTheDoctor(t *testing.T) {
	pool := setupDB(t)
	doctorID, doctorUserID := uuid.New(), uuid.New()
	c, _, _ := testConsumer(pool, &fakeDirectory{})
	ctx := context.Background()

	if err := c.Handle(ctx, envelope(t, events.SubjectDoctorApproved, events.DoctorApproved{
		DoctorID: doctorID, UserID: doctorUserID, DoctorName: "Ruwan Fernando",
		Email: "ruwan@example.lk", Specialty: "general", FeeCents: 200000, Currency: "LKR",
	})); err != nil {
		t.Fatalf("handle doctor.approved: %v", err)
	}

	if err := c.Handle(ctx, envelope(t, events.SubjectPayoutSent, events.PayoutSent{
		PayoutID: uuid.New(), DoctorID: doctorID, AmountCents: 1750000, Currency: "LKR",
		PeriodStart: "2026-08-01", PeriodEnd: "2026-08-15", SentAt: time.Now().UTC(),
	})); err != nil {
		t.Fatalf("handle payout.sent: %v", err)
	}

	body := bodyOf(t, pool, doctorUserID, "email", notification.TemplatePayoutSent)
	if !strings.Contains(body, "Rs. 17,500.00") {
		t.Errorf("payout notice does not state the amount; body = %q", body)
	}
}

// TestAppointmentCancelled_DoesNotPersistThePatientsFreeText is security
// review F20(d).
//
// events.AppointmentCancelled.Reason carries up to 500 characters of
// patient-authored free text. It was interpolated into the appointment_cancelled
// template, which put it in notifications.body -- a row that lives in this
// service's database indefinitely, is copied into notification_dead_letters on
// failure, and on the sms channel is handed verbatim to Dialog or Twilio. Five
// hundred characters of free text on a medical cancellation reliably contains
// clinical content, and events/payloads.go's own comment says this is not
// where such text is supposed to end up.
//
// Put `Reason: p.Reason` back in onAppointmentCancelled and this test fails on
// every channel at once.
func TestAppointmentCancelled_DoesNotPersistThePatientsFreeText(t *testing.T) {
	pool := setupDB(t)
	patientID, doctorID, doctorUserID, appointmentID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	dir := &fakeDirectory{users: map[uuid.UUID]notification.Contact{
		patientID: {UserID: patientID, Name: "Kamal", Phone: "+94771234567", Email: "kamal@example.lk"},
	}}
	c, _, _ := testConsumer(pool, dir)
	ctx := context.Background()

	if err := c.Handle(ctx, envelope(t, events.SubjectDoctorApproved, events.DoctorApproved{
		DoctorID: doctorID, UserID: doctorUserID, DoctorName: "Ruwan Fernando",
		Email: "ruwan@example.lk", Specialty: "oncology", FeeCents: 200000, Currency: "LKR",
	})); err != nil {
		t.Fatalf("handle doctor.approved: %v", err)
	}

	// The kind of thing patients actually type into a cancellation box.
	const phi = "admitted to ward 7 last night for the chemo cycle, cannot make it"

	if err := c.Handle(ctx, envelope(t, events.SubjectAppointmentCancelled, events.AppointmentCancelled{
		AppointmentID: appointmentID, PatientID: patientID, DoctorID: doctorID,
		SlotID: uuid.New(), StartAt: time.Now().Add(24 * time.Hour).UTC(),
		CancelledBy: "patient", Reason: phi, RefundPolicy: "full", RefundPercent: 100,
		CancelledAt: time.Now().UTC(),
	})); err != nil {
		t.Fatalf("handle appointment.cancelled: %v", err)
	}

	// Not "the sms body does not contain it" -- nothing anywhere in the row
	// may. body, subject and the dedupe key are all persisted columns.
	rows, err := pool.Query(ctx,
		`SELECT channel, body, COALESCE(subject, '') FROM notifications
		  WHERE user_id = $1 AND template_key = $2`, patientID, string(notification.TemplateAppointmentCancelled))
	if err != nil {
		t.Fatalf("query notifications: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var channel, body, subject string
		if err := rows.Scan(&channel, &body, &subject); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if strings.Contains(body, phi) || strings.Contains(subject, phi) {
			t.Errorf("%s notification persisted the patient's free-text reason; body = %q", channel, body)
		}
		if strings.Contains(body, "chemo") || strings.Contains(body, "ward 7") {
			t.Errorf("%s notification persisted a fragment of the reason; body = %q", channel, body)
		}
		// The message still has to do its job.
		if !strings.Contains(body, "Ruwan Fernando") {
			t.Errorf("%s notification no longer names the doctor; body = %q", channel, body)
		}
		// ...and must not have been left with a dangling "Reason:" label by
		// dropping the value but keeping the template placeholder.
		if strings.Contains(strings.ToLower(body), "reason:") {
			t.Errorf("%s template still labels a reason it no longer carries; body = %q", channel, body)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen == 0 {
		t.Fatal("no cancellation notification was queued at all")
	}
}

// TestCancellationTemplates_DoNotReferenceReason pins the other half in the
// schema itself. Dropping the field in Go while leaving {{.Reason}} in the
// seeded templates would render an empty placeholder today and quietly start
// leaking again the moment someone re-populates TemplateData.Reason for an
// unrelated reason.
func TestCancellationTemplates_DoNotReferenceReason(t *testing.T) {
	pool := setupDB(t)

	rows, err := pool.Query(context.Background(),
		`SELECT channel, locale, body_template, COALESCE(subject_template, '')
		   FROM templates WHERE key = 'appointment_cancelled'`)
	if err != nil {
		t.Fatalf("query templates: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var channel, locale, body, subject string
		if err := rows.Scan(&channel, &locale, &body, &subject); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if strings.Contains(body, "{{.Reason}}") || strings.Contains(subject, "{{.Reason}}") {
			t.Errorf("appointment_cancelled/%s/%s still interpolates the patient's free text", channel, locale)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen == 0 {
		t.Fatal("no appointment_cancelled templates found; the seed migration did not run")
	}
}

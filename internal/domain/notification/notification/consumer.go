package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
)

// Consumer turns delivered NATS events into Notify calls (and, for the
// appointment and doctor lifecycles, updates to this service's local
// projections -- migrations/000002, 000004 and 000005 explain why those
// projections exist).
//
// Every payload below is a canonical type from
// internal/platform/events/payloads.go, imported by producer and consumer
// alike. This service used to declare a private struct per subject, listing
// the fields it wished the producer would send. encoding/json does not error
// on a field nobody sent -- it leaves the zero value -- so those wishes
// showed up in production as messages addressed to "" and fees rendered as
// "Rs. 0.00", with nothing logged. `doctor.approved` was the confirmed case
// (_shared/INTEGRATION-FIXES.md item 9): the doctor got an approval email
// with no name in it and no address to send it to.
//
// What the canonical payloads deliberately do NOT carry is contact details
// and denormalised copies of other services' data. Those are resolved here,
// once, at enqueue time:
//
//   - a patient's phone/email comes from Directory (gRPC to user-service).
//     It is PHI-adjacent and must not be broadcast to every consumer on the
//     platform, so it is fetched per person, deliberately.
//   - a doctor's display name, specialty and list price come from this
//     service's own doctor_directory projection, fed by doctor.approved and
//     doctor.updated -- events this service already receives, so no call is
//     needed.
//   - deep links are built from one configured base URL (see links.go),
//     because a URL scheme is this service's business, not five producers'.
//
// The lookups happen in the consumer, not on the delivery path: the resolved
// address is written to notifications.recipient and the dispatcher reads it
// from there. So the "no blocking cross-service call while delivering"
// property in docs/DESIGN.md still holds. What changed is that ENQUEUING can
// now fail and be redelivered, which is strictly better than enqueuing a
// notification addressed to nobody.
type Consumer struct {
	svc             *Service
	repo            *Repository
	dir             Directory
	links           Links
	opsEmail        string
	doctorPortalURL string
	log             zerolog.Logger
}

// lookupTimeout bounds a single directory lookup so one slow dependency
// cannot pin a JetStream delivery open indefinitely. Exceeding it is a
// transient error: the message is redelivered with backoff.
const lookupTimeout = 5 * time.Second

// NewConsumer builds the event consumer.
func NewConsumer(svc *Service, repo *Repository, dir Directory, links Links, log zerolog.Logger) *Consumer {
	return &Consumer{svc: svc, repo: repo, dir: dir, links: links, log: log}
}

// SetApplicationNotify configures ops inbox email and the doctor portal URL
// used in approval emails for public applications.
func (c *Consumer) SetApplicationNotify(opsEmail, doctorPortalURL string) {
	c.opsEmail = strings.TrimSpace(opsEmail)
	c.doctorPortalURL = strings.TrimRight(strings.TrimSpace(doctorPortalURL), "/")
}

// Subjects is the fixed subscription list for events.Subscriber.Subscribe.
func (c *Consumer) Subjects() []events.Subject {
	return []events.Subject{
		// appointment.created is consumed for one reason: it is the only
		// event that carries the price QUOTED at booking, which the
		// confirmation message has to state.
		events.SubjectAppointmentCreated,
		events.SubjectAppointmentConfirmed,
		events.SubjectAppointmentCancelled,
		events.SubjectAppointmentCompleted,
		events.SubjectAppointmentNoShow,
		events.SubjectAppointmentReminder,
		events.SubjectAppointmentRescheduleRequested,
		events.SubjectAppointmentRescheduled,
		events.SubjectDoctorApproved,
		events.SubjectDoctorRejected,
		events.SubjectDoctorApplicationSubmitted,
		events.SubjectDoctorApplicationApproved,
		events.SubjectDoctorApplicationRejected,
		// doctor.updated keeps the local doctor directory from going stale
		// when a doctor changes specialty, price or status.
		events.SubjectDoctorUpdated,
		events.SubjectPrescriptionIssued,
		events.SubjectPaymentSucceeded,
		events.SubjectPaymentFailed,
		events.SubjectPayoutSent,
		events.SubjectWaitlistSlotOffer,
		events.SubjectConsultationStarted,
		events.SubjectConsultationDoctorRunningLate,
		events.SubjectConsultationEarlyJoinOffered,
	}
}

// Handle dispatches one delivered envelope. It matches events.Handler:
// returning nil acknowledges the message; returning an error triggers
// JetStream redelivery with backoff. Every branch below is safe to run
// twice for the same envelope -- Notify enforces that via dedupe_key, and
// the projection writes are plain idempotent UPSERTs.
func (c *Consumer) Handle(ctx context.Context, env events.Envelope) error {
	switch env.Subject {
	case events.SubjectAppointmentCreated:
		return c.onAppointmentCreated(ctx, env)
	case events.SubjectAppointmentConfirmed:
		return c.onAppointmentConfirmed(ctx, env)
	case events.SubjectAppointmentCancelled:
		return c.onAppointmentCancelled(ctx, env)
	case events.SubjectAppointmentCompleted:
		return c.onAppointmentTerminal(ctx, env, "completed")
	case events.SubjectAppointmentNoShow:
		return c.onAppointmentTerminal(ctx, env, "no_show")
	case events.SubjectAppointmentReminder:
		return c.onAppointmentReminderDue(ctx, env)
	case events.SubjectAppointmentRescheduleRequested:
		return c.onAppointmentRescheduleRequested(ctx, env)
	case events.SubjectAppointmentRescheduled:
		return c.onAppointmentRescheduled(ctx, env)
	case events.SubjectDoctorApproved:
		return c.onDoctorApproved(ctx, env)
	case events.SubjectDoctorRejected:
		return c.onDoctorRejected(ctx, env)
	case events.SubjectDoctorApplicationSubmitted:
		return c.onDoctorApplicationSubmitted(ctx, env)
	case events.SubjectDoctorApplicationApproved:
		return c.onDoctorApplicationApproved(ctx, env)
	case events.SubjectDoctorApplicationRejected:
		return c.onDoctorApplicationRejected(ctx, env)
	case events.SubjectDoctorUpdated:
		return c.onDoctorUpdated(ctx, env)
	case events.SubjectPrescriptionIssued:
		return c.onPrescriptionIssued(ctx, env)
	case events.SubjectPaymentSucceeded:
		return c.onPaymentSucceeded(ctx, env)
	case events.SubjectPaymentFailed:
		return c.onPaymentFailed(ctx, env)
	case events.SubjectPayoutSent:
		return c.onPayoutSent(ctx, env)
	case events.SubjectWaitlistSlotOffer:
		return c.onWaitlistSlotOffered(ctx, env)
	case events.SubjectConsultationStarted:
		return c.onConsultationStarted(ctx, env)
	case events.SubjectConsultationDoctorRunningLate:
		return c.onDoctorRunningLate(ctx, env)
	case events.SubjectConsultationEarlyJoinOffered:
		return c.onEarlyJoinOffered(ctx, env)
	default:
		c.log.Warn().Str("subject", string(env.Subject)).Msg("consumer: subscribed to a subject with no handler")
		return nil
	}
}

// ---------------------------------------------------------------------------
// resolution helpers
// ---------------------------------------------------------------------------

// contact resolves a recipient, translating "no such user" into a decision
// rather than an error the caller has to re-derive.
//
// known is false only when the directory answered definitively that the user
// does not exist -- a stale id in a replayed event, which no amount of
// retrying will fix. An unreachable directory returns an error instead, so
// the message is redelivered.
func (c *Consumer) contact(ctx context.Context, userID uuid.UUID) (ct Contact, known bool, err error) {
	if userID == uuid.Nil {
		return Contact{}, false, nil
	}
	lookupCtx, cancel := deadlineFor(ctx, lookupTimeout)
	defer cancel()

	ct, err = c.dir.Contact(lookupCtx, userID)
	switch {
	case errors.Is(err, ErrContactNotFound):
		return Contact{}, false, nil
	case err != nil:
		return Contact{}, false, err
	}
	return ct, true, nil
}

// doctor reads the local doctor projection.
//
// A miss is an error, not an empty name. Two consumers racing on a
// freshly-approved doctor is the realistic cause, and redelivery fixes it;
// sending "Your appointment with Dr. is confirmed" does not.
func (c *Consumer) doctor(ctx context.Context, doctorID uuid.UUID) (Doctor, error) {
	return c.repo.DoctorByID(ctx, c.repo.Pool(), doctorID)
}

// skipUnknownRecipient logs and acknowledges an event whose recipient no
// longer exists. It is deliberately a Warn, not a silent return: "we chose
// not to send this" is an operational fact somebody should be able to grep
// for, and the user id is masked because it identifies a patient.
func (c *Consumer) skipUnknownRecipient(env events.Envelope, userID uuid.UUID) error {
	c.log.Warn().
		Str("subject", string(env.Subject)).
		Str("event_id", env.ID.String()).
		Str("user_id", logger.MaskID(userID.String())).
		Msg("consumer: recipient not found in user directory; acknowledging without sending")
	return nil
}

// ---------------------------------------------------------------------------
// appointment lifecycle
// ---------------------------------------------------------------------------

// onAppointmentCreated records the quote and produces no notification of its
// own. A booking that has not been paid for is not something to congratulate
// a patient about -- scheduling already told them the slot is held, and the
// confirmation follows payment.
func (c *Consumer) onAppointmentCreated(ctx context.Context, env events.Envelope) error {
	var p events.AppointmentCreated
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}
	// A zero quote is a producer bug, not a free consultation -- the
	// canonical payload says so explicitly. Record it anyway (the reminder
	// projection is still needed) but make the anomaly visible.
	if p.AmountCents == 0 {
		c.log.Error().
			Str("appointment_id", p.AppointmentID.String()).
			Msg("appointment.created carried amount_cents = 0; the booking confirmation will state a zero fee")
	}
	return c.repo.SeedReminderState(ctx, c.repo.Pool(), AppointmentReminderState{
		AppointmentID: p.AppointmentID,
		PatientID:     p.PatientID,
		DoctorID:      p.DoctorID,
		Specialty:     p.Specialty,
		StartsAt:      p.StartAt,
		AmountCents:   p.AmountCents,
		Currency:      p.Currency,
	})
}

func (c *Consumer) onAppointmentConfirmed(ctx context.Context, env events.Envelope) error {
	var p events.AppointmentConfirmed
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}

	doc, err := c.doctor(ctx, p.DoctorID)
	if err != nil {
		return err
	}
	patient, known, err := c.contact(ctx, p.PatientID)
	if err != nil {
		return err
	}
	if !known {
		return c.skipUnknownRecipient(env, p.PatientID)
	}

	// The fee stated to the patient is the one they were quoted at booking,
	// recorded by onAppointmentCreated -- never the doctor's current list
	// price, which may have moved since.
	cents, currency, haveQuote, err := c.repo.QuoteFor(ctx, c.repo.Pool(), p.AppointmentID)
	if err != nil {
		return err
	}
	if !haveQuote {
		// appointment.created causally precedes appointment.confirmed, so
		// this means we are ahead of the stream. Redelivery is the right
		// answer; inventing a number is not.
		return fmt.Errorf("consumer: no quote recorded for appointment %s yet (appointment.created not seen); retrying", p.AppointmentID)
	}

	joinLink := c.links.Join(p.AppointmentID)
	if err := c.repo.UpsertReminderState(ctx, c.repo.Pool(), AppointmentReminderState{
		AppointmentID: p.AppointmentID, PatientID: p.PatientID, DoctorID: p.DoctorID,
		DoctorName: doc.DisplayName(), Specialty: doc.Specialty,
		PatientPhone: patient.Phone, PatientEmail: patient.Email, JoinLink: joinLink,
		StartsAt: p.StartAt,
	}); err != nil {
		return err
	}

	_, err = c.svc.Notify(ctx, NotifyRequest{
		UserID:      p.PatientID,
		TemplateKey: TemplateBookingConfirmed,
		Data: TemplateData{
			DoctorName: doc.DisplayName(),
			DateTime:   formatDateTime(p.StartAt),
			FeeLKR:     formatMoney(cents, currency),
		},
		Phone: patient.Phone, Email: patient.Email, LocaleHint: patient.Locale,
		DedupeKeyBase: env.ID.String(), SourceEventID: &env.ID,
	})
	return err
}

func (c *Consumer) onAppointmentCancelled(ctx context.Context, env events.Envelope) error {
	var p events.AppointmentCancelled
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}

	if err := c.repo.SetReminderStateStatus(ctx, c.repo.Pool(), p.AppointmentID, "cancelled"); err != nil {
		return err
	}

	doc, err := c.doctor(ctx, p.DoctorID)
	if err != nil {
		return err
	}
	patient, known, err := c.contact(ctx, p.PatientID)
	if err != nil {
		return err
	}
	if !known {
		return c.skipUnknownRecipient(env, p.PatientID)
	}

	// SECURITY REVIEW F20(d): p.Reason is NOT passed through.
	//
	// events.AppointmentCancelled.Reason carries up to 500 characters of
	// patient-authored free text, and 500 characters of free text on a medical
	// cancellation reliably contains clinical content ("the lump turned out
	// to be...", "admitted for chemo"). Rendering it here would copy that text
	// into notifications.body in this service's database, into
	// notification_dead_letters on failure, and -- for the sms channel -- into
	// a third-party gateway's message log at Dialog or Twilio. None of those
	// are places the platform's data-protection story accounts for, and
	// events/payloads.go's own stated policy says so.
	//
	// The patient loses nothing: they either wrote the reason themselves, or
	// the cancellation came from the clinic and the reason belongs in the
	// appointment record, which scheduling-service already keeps in its own
	// column and shows in the app. The notification's job is to say it was
	// cancelled.
	//
	// The template no longer references {{.Reason}} either
	// (migrations/000006_cancellation_reason_out_of_body.up.sql) -- leaving
	// the placeholder in place would have rendered a dangling "Reason:" with
	// nothing after it.
	//
	// The real fix is one level up: Reason should not be on the event at all.
	// That is scheduling-service's call and the shared events package's, both
	// outside this repo. Dropping it here removes the persistence and the
	// third-party fan-out, which is the part this service owns.
	_, err = c.svc.Notify(ctx, NotifyRequest{
		UserID:      p.PatientID,
		TemplateKey: TemplateAppointmentCancelled,
		Data: TemplateData{
			DoctorName: doc.DisplayName(),
			DateTime:   formatDateTime(p.StartAt),
		},
		Phone: patient.Phone, Email: patient.Email, LocaleHint: patient.Locale,
		DedupeKeyBase: env.ID.String(), SourceEventID: &env.ID,
	})
	return err
}

func (c *Consumer) onAppointmentRescheduleRequested(ctx context.Context, env events.Envelope) error {
	var p events.AppointmentRescheduleRequested
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}
	doc, err := c.doctor(ctx, p.DoctorID)
	if err != nil {
		return err
	}
	patient, known, err := c.contact(ctx, p.PatientID)
	if err != nil {
		return err
	}
	if !known {
		return c.skipUnknownRecipient(env, p.PatientID)
	}
	_, err = c.svc.Notify(ctx, NotifyRequest{
		UserID:      p.PatientID,
		TemplateKey: TemplateRescheduleRequested,
		Data: TemplateData{
			DoctorName:       doc.DisplayName(),
			DateTime:         formatDateTime(p.OriginalStart),
			ProposedDateTime: formatDateTime(p.ProposedStart),
		},
		Phone: patient.Phone, Email: patient.Email, LocaleHint: patient.Locale,
		DedupeKeyBase: env.ID.String(), SourceEventID: &env.ID,
	})
	return err
}

func (c *Consumer) onAppointmentRescheduled(ctx context.Context, env events.Envelope) error {
	var p events.AppointmentRescheduled
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}
	if err := c.repo.UpdateReminderStartsAt(ctx, c.repo.Pool(), p.AppointmentID, p.ProposedStart); err != nil {
		return err
	}
	doc, err := c.doctor(ctx, p.DoctorID)
	if err != nil {
		return err
	}
	patient, known, err := c.contact(ctx, p.PatientID)
	if err != nil {
		return err
	}
	if !known {
		return c.skipUnknownRecipient(env, p.PatientID)
	}
	_, err = c.svc.Notify(ctx, NotifyRequest{
		UserID:      p.PatientID,
		TemplateKey: TemplateRescheduleConfirmed,
		Data: TemplateData{
			DoctorName: doc.DisplayName(),
			DateTime:   formatDateTime(p.ProposedStart),
		},
		Phone: patient.Phone, Email: patient.Email, LocaleHint: patient.Locale,
		DedupeKeyBase: env.ID.String(), SourceEventID: &env.ID,
	})
	return err
}

// appointment that completed or no-showed -- either way, no reminder is due
// for it again. This produces no notification of its own.
func (c *Consumer) onAppointmentTerminal(ctx context.Context, env events.Envelope, status string) error {
	var p events.AppointmentTerminal
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}
	return c.repo.SetReminderStateStatus(ctx, c.repo.Pool(), p.AppointmentID, status)
}

func (c *Consumer) onAppointmentReminderDue(ctx context.Context, env events.Envelope) error {
	var p events.AppointmentReminderDue
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}

	// LeadMinutes is the canonical form (60 or 1440). This service's own cron
	// (reminder.go) uses the same two kinds and the same deterministic dedupe
	// key, so whichever path fires first wins and the other is a no-op.
	key, kind := TemplateReminder1h, reminderKind1h
	if p.LeadMinutes >= 1440 {
		key, kind = TemplateReminder24h, reminderKind24h
	}

	// DoctorName is optional on this payload; fall back to the local
	// projection rather than sending a reminder with a blank name.
	doctorName := p.DoctorName
	if doctorName == "" {
		doc, err := c.doctor(ctx, p.DoctorID)
		if err != nil {
			return err
		}
		doctorName = doc.DisplayName()
	}

	patient, known, err := c.contact(ctx, p.PatientID)
	if err != nil {
		return err
	}
	if !known {
		return c.skipUnknownRecipient(env, p.PatientID)
	}

	_, err = c.svc.Notify(ctx, NotifyRequest{
		UserID:      p.PatientID,
		TemplateKey: key,
		Data: TemplateData{
			DoctorName: doctorName,
			DateTime:   formatDateTime(p.StartAt),
			JoinLink:   c.links.Join(p.AppointmentID),
		},
		Phone: patient.Phone, Email: patient.Email, LocaleHint: patient.Locale,
		DedupeKeyBase: reminderDedupeKey(kind, p.AppointmentID), SourceEventID: &env.ID,
	})
	if err != nil {
		return err
	}
	return c.markReminderSent(ctx, kind, p.AppointmentID)
}

// ---------------------------------------------------------------------------
// doctor verification and profile
// ---------------------------------------------------------------------------

// onDoctorApproved does two things: it seeds the local doctor projection every
// other handler reads from, and it congratulates the doctor.
//
// This is the subject INTEGRATION-FIXES item 9 caught. The canonical
// events.DoctorApproved carries DoctorName and Email, which the old private
// struct also declared but the producer never sent -- so the approval email
// went out addressed to nobody, at no address, and nothing logged an error.
// Producer and consumer now share the struct, so the producer cannot omit
// them without failing to compile.
func (c *Consumer) onDoctorApproved(ctx context.Context, env events.Envelope) error {
	var p events.DoctorApproved
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}

	if err := c.repo.UpsertDoctorFromApproval(ctx, c.repo.Pool(), Doctor{
		DoctorID: p.DoctorID, UserID: p.UserID, FullName: p.DoctorName, Email: p.Email,
		Specialty: p.Specialty, FeeCents: p.FeeCents, Currency: p.Currency,
	}); err != nil {
		return err
	}

	_, err := c.svc.Notify(ctx, NotifyRequest{
		UserID: p.UserID, TemplateKey: TemplateDoctorApproved,
		Data: TemplateData{DoctorName: p.DoctorName}, Email: p.Email,
		DedupeKeyBase: env.ID.String(), SourceEventID: &env.ID,
	})
	return err
}

func (c *Consumer) onDoctorRejected(ctx context.Context, env events.Envelope) error {
	var p events.DoctorRejected
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}
	_, err := c.svc.Notify(ctx, NotifyRequest{
		UserID: p.UserID, TemplateKey: TemplateDoctorRejected,
		Data: TemplateData{DoctorName: p.DoctorName, Reason: p.Reason}, Email: p.Email,
		DedupeKeyBase: env.ID.String(), SourceEventID: &env.ID,
	})
	return err
}

func (c *Consumer) onDoctorApplicationSubmitted(ctx context.Context, env events.Envelope) error {
	var p events.DoctorApplicationSubmitted
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}
	if c.opsEmail == "" {
		c.log.Info().Str("application_id", p.ApplicationID.String()).
			Msg("doctor application submitted; DOCTOR_APPLICATIONS_NOTIFY_EMAIL unset, skipping ops email")
		return nil
	}
	_, err := c.svc.Notify(ctx, NotifyRequest{
		UserID:      p.ApplicationID,
		TemplateKey: TemplateDoctorApplicationSubmitted,
		Data: TemplateData{
			DoctorName: p.FullName, ApplicantEmail: p.Email, Phone: p.Phone,
			SLMCNumber: p.SLMCNumber, Specialty: p.Specialty,
		},
		Email:         c.opsEmail,
		Channels:      []Channel{ChannelEmail},
		DedupeKeyBase: env.ID.String(),
		SourceEventID: &env.ID,
	})
	return err
}

func (c *Consumer) onDoctorApplicationApproved(ctx context.Context, env events.Envelope) error {
	var p events.DoctorApplicationApproved
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}
	portal := c.doctorPortalURL
	if portal == "" {
		portal = c.links.BaseURL
	}
	_, err := c.svc.Notify(ctx, NotifyRequest{
		UserID:        p.ApplicationID,
		TemplateKey:   approvalTemplate(p),
		Data:          TemplateData{DoctorName: p.FullName, PortalURL: portal},
		Email:         p.Email,
		Channels:      []Channel{ChannelEmail},
		DedupeKeyBase: env.ID.String(),
		SourceEventID: &env.ID,
	})
	return err
}

// approvalTemplate picks the instruction that matches the account that was
// actually created.
//
// The default is the OTP wording, not the password wording: an event written
// before this field existed decodes both booleans as false, and telling a
// doctor to use OTP when a password would also have worked is a smaller
// failure than telling them to use a password that does not exist.
func approvalTemplate(p events.DoctorApplicationApproved) TemplateKey {
	switch {
	case p.LoginReady && p.PasswordApplied:
		return TemplateDoctorApplicationApproved
	case p.LoginReady:
		return TemplateDoctorApplicationApprovedExisting
	default:
		return TemplateDoctorApplicationApprovedOTP
	}
}

func (c *Consumer) onDoctorApplicationRejected(ctx context.Context, env events.Envelope) error {
	var p events.DoctorApplicationRejected
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}
	_, err := c.svc.Notify(ctx, NotifyRequest{
		UserID:        p.ApplicationID,
		TemplateKey:   TemplateDoctorApplicationRejected,
		Data:          TemplateData{DoctorName: p.FullName, Reason: p.Reason},
		Email:         p.Email,
		Channels:      []Channel{ChannelEmail},
		DedupeKeyBase: env.ID.String(),
		SourceEventID: &env.ID,
	})
	return err
}

// onDoctorUpdated keeps the projection current. It sends nothing: a doctor
// changing their consultation fee is not news a patient should be pushed.
func (c *Consumer) onDoctorUpdated(ctx context.Context, env events.Envelope) error {
	var p events.DoctorUpdated
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}
	return c.repo.UpdateDoctorProfile(ctx, c.repo.Pool(), Doctor{
		DoctorID: p.DoctorID, Specialty: p.Specialty,
		FeeCents: p.FeeCents, Currency: p.Currency, Status: p.Status,
	})
}

// ---------------------------------------------------------------------------
// prescriptions
// ---------------------------------------------------------------------------

func (c *Consumer) onPrescriptionIssued(ctx context.Context, env events.Envelope) error {
	var p events.PrescriptionIssued
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}

	doc, err := c.doctor(ctx, p.DoctorID)
	if err != nil {
		return err
	}
	patient, known, err := c.contact(ctx, p.PatientID)
	if err != nil {
		return err
	}
	if !known {
		return c.skipUnknownRecipient(env, p.PatientID)
	}

	_, err = c.svc.Notify(ctx, NotifyRequest{
		UserID: p.PatientID, TemplateKey: TemplatePrescriptionReady,
		Data: TemplateData{
			DoctorName:  doc.DisplayName(),
			DownloadURL: c.links.Prescription(p.PrescriptionID),
		},
		Phone: patient.Phone, Email: patient.Email, LocaleHint: patient.Locale,
		DedupeKeyBase: env.ID.String(), SourceEventID: &env.ID,
	})
	return err
}

// ---------------------------------------------------------------------------
// payments
// ---------------------------------------------------------------------------

func (c *Consumer) onPaymentSucceeded(ctx context.Context, env events.Envelope) error {
	var p events.PaymentSucceeded
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}

	doc, err := c.doctor(ctx, p.DoctorID)
	if err != nil {
		return err
	}
	patient, known, err := c.contact(ctx, p.PatientID)
	if err != nil {
		return err
	}
	if !known {
		return c.skipUnknownRecipient(env, p.PatientID)
	}

	_, err = c.svc.Notify(ctx, NotifyRequest{
		UserID: p.PatientID, TemplateKey: TemplatePaymentReceipt,
		Data: TemplateData{
			DoctorName: doc.DisplayName(),
			// The amount CHARGED, straight off the payment event. Never the
			// doctor's list price and never the booking quote: a receipt
			// states what actually moved.
			AmountLKR:  formatMoney(p.AmountCents, p.Currency),
			ReceiptURL: c.links.Receipt(p.PaymentID),
		},
		Email: patient.Email, LocaleHint: patient.Locale,
		DedupeKeyBase: env.ID.String(), SourceEventID: &env.ID,
	})
	return err
}

func (c *Consumer) onPaymentFailed(ctx context.Context, env events.Envelope) error {
	var p events.PaymentFailed
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}
	// payment-service has not adopted the canonical payload yet and still
	// spells the reason `failure_reason`. Accept both so the patient is told
	// WHY their payment failed during the migration window. See
	// eventcompat.go; delete it once the producer moves.
	if from := applyPaymentFailedCompat(env, &p); from != "" {
		c.log.Warn().
			Str("subject", string(env.Subject)).
			Str("legacy_field", from).
			Msg("consumer: producer still uses a pre-canonical field spelling")
	}

	patient, known, err := c.contact(ctx, p.PatientID)
	if err != nil {
		return err
	}
	if !known {
		return c.skipUnknownRecipient(env, p.PatientID)
	}

	_, err = c.svc.Notify(ctx, NotifyRequest{
		UserID: p.PatientID, TemplateKey: TemplatePaymentFailed,
		Data:          TemplateData{AmountLKR: formatMoney(p.AmountCents, p.Currency), Reason: p.Reason},
		Phone:         patient.Phone,
		LocaleHint:    patient.Locale,
		DedupeKeyBase: env.ID.String(), SourceEventID: &env.ID,
	})
	return err
}

// onPayoutSent tells a doctor their money left. events.PayoutSent identifies
// the doctor by DoctorID; the notification (and the preferences row behind it)
// belongs to the USER behind that doctor, which the local projection knows.
func (c *Consumer) onPayoutSent(ctx context.Context, env events.Envelope) error {
	var p events.PayoutSent
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}

	doc, err := c.doctor(ctx, p.DoctorID)
	if err != nil {
		return err
	}

	_, err = c.svc.Notify(ctx, NotifyRequest{
		UserID: doc.UserID, TemplateKey: TemplatePayoutSent,
		Data:          TemplateData{DoctorName: doc.DisplayName(), AmountLKR: formatMoney(p.AmountCents, p.Currency)},
		Email:         doc.Email,
		DedupeKeyBase: env.ID.String(), SourceEventID: &env.ID,
	})
	return err
}

// ---------------------------------------------------------------------------
// waitlist / consultation
// ---------------------------------------------------------------------------

func (c *Consumer) onWaitlistSlotOffered(ctx context.Context, env events.Envelope) error {
	var p events.WaitlistSlotOffered
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}

	doc, err := c.doctor(ctx, p.DoctorID)
	if err != nil {
		return err
	}
	patient, known, err := c.contact(ctx, p.PatientID)
	if err != nil {
		return err
	}
	if !known {
		return c.skipUnknownRecipient(env, p.PatientID)
	}

	minutes := int(time.Until(p.ExpiresAt).Round(time.Minute) / time.Minute)
	if minutes < 0 {
		minutes = 0
	}
	_, err = c.svc.Notify(ctx, NotifyRequest{
		UserID: p.PatientID, TemplateKey: TemplateWaitlistAvailable,
		Data:          TemplateData{DoctorName: doc.DisplayName(), ExpiresInMinutes: minutes},
		Phone:         patient.Phone,
		LocaleHint:    patient.Locale,
		DedupeKeyBase: env.ID.String(), SourceEventID: &env.ID,
	})
	return err
}

func (c *Consumer) onConsultationStarted(ctx context.Context, env events.Envelope) error {
	var p events.ConsultationStarted
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}

	doc, err := c.doctor(ctx, p.DoctorID)
	if err != nil {
		return err
	}
	patient, known, err := c.contact(ctx, p.PatientID)
	if err != nil {
		return err
	}
	if !known {
		return c.skipUnknownRecipient(env, p.PatientID)
	}

	_, err = c.svc.Notify(ctx, NotifyRequest{
		UserID: p.PatientID, TemplateKey: TemplateConsultationStarting,
		Data:          TemplateData{DoctorName: doc.DisplayName(), JoinLink: c.links.Join(p.AppointmentID)},
		Phone:         patient.Phone,
		LocaleHint:    patient.Locale,
		DedupeKeyBase: env.ID.String(), SourceEventID: &env.ID,
	})
	return err
}

func (c *Consumer) onDoctorRunningLate(ctx context.Context, env events.Envelope) error {
	var p events.ConsultationDoctorRunningLate
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}

	doc, err := c.doctor(ctx, p.DoctorID)
	if err != nil {
		return err
	}
	patient, known, err := c.contact(ctx, p.NextPatientID)
	if err != nil {
		return err
	}
	if !known {
		return c.skipUnknownRecipient(env, p.NextPatientID)
	}

	_, err = c.svc.Notify(ctx, NotifyRequest{
		UserID:      p.NextPatientID,
		TemplateKey: TemplateDoctorRunningLate,
		Data:        TemplateData{DoctorName: doc.DisplayName()},
		Phone:       patient.Phone, Email: patient.Email, LocaleHint: patient.Locale,
		// Business key so a redelivered envelope and a second detect for the
		// same active→next pair cannot spam the waiting patient.
		DedupeKeyBase: "doctor_running_late:" + p.ActiveAppointmentID.String() + ":" + p.NextAppointmentID.String(),
		SourceEventID: &env.ID,
	})
	return err
}

func (c *Consumer) onEarlyJoinOffered(ctx context.Context, env events.Envelope) error {
	var p events.ConsultationEarlyJoinOffered
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("consumer: decode %s: %w", env.Subject, err)
	}

	doc, err := c.doctor(ctx, p.DoctorID)
	if err != nil {
		return err
	}
	patient, known, err := c.contact(ctx, p.NextPatientID)
	if err != nil {
		return err
	}
	if !known {
		return c.skipUnknownRecipient(env, p.NextPatientID)
	}

	_, err = c.svc.Notify(ctx, NotifyRequest{
		UserID:      p.NextPatientID,
		TemplateKey: TemplateEarlyJoinOffered,
		Data: TemplateData{
			DoctorName: doc.DisplayName(),
			JoinLink:   c.links.Join(p.NextAppointmentID),
		},
		Phone: patient.Phone, Email: patient.Email, LocaleHint: patient.Locale,
		DedupeKeyBase: "early_join_offered:" + p.SourceAppointmentID.String() + ":" + p.NextAppointmentID.String(),
		SourceEventID: &env.ID,
	})
	return err
}

// ---------------------------------------------------------------------------
// shared helpers (also used by reminder.go)
// ---------------------------------------------------------------------------

const (
	reminderKind1h  = "1h"
	reminderKind24h = "24h"
)

// reminderDedupeKey is deterministic in the appointment and reminder kind,
// not in any single event's ID: it is what makes the cron-driven path
// (reminder.go) and the event-driven path (onAppointmentReminderDue) above
// idempotent with respect to EACH OTHER, not just with respect to their own
// redelivery.
func reminderDedupeKey(kind string, appointmentID uuid.UUID) string {
	return fmt.Sprintf("reminder_%s:%s", kind, appointmentID)
}

func (c *Consumer) markReminderSent(ctx context.Context, kind string, appointmentID uuid.UUID) error {
	if kind == reminderKind24h {
		return c.repo.MarkReminder24hSent(ctx, appointmentID)
	}
	return c.repo.MarkReminder1hSent(ctx, appointmentID)
}

// colomboLocation is loaded once; a failure here would mean tzdata is
// missing from the runtime image, which Dockerfile already guards against
// by copying /usr/share/zoneinfo into the scratch image.
var colomboLocation = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Colombo")
	if err != nil {
		return time.UTC
	}
	return loc
}()

// formatDateTime renders an instant in Asia/Colombo using a plain,
// unambiguous format (day-month-year, 24h clock) rather than a
// locale-specific one: this service does not implement full date/time
// localisation for si/ta, and an unambiguous format beats a guessed idiom.
// See the build report.
func formatDateTime(t time.Time) string {
	return t.In(colomboLocation).Format("02 Jan 2006, 15:04")
}

// formatMoney renders integer cents as "Rs. 2,500.00" for LKR, or a generic
// "<CUR> 25.00" for anything else. Money is stored as cents platform-wide
// (AGENT-BRIEF §3 Database); this is the one place in this service that
// turns cents back into a human string, and it does so only for display,
// never for arithmetic.
func formatMoney(cents int64, currency string) string {
	negative := cents < 0
	if negative {
		cents = -cents
	}
	whole := cents / 100
	frac := cents % 100

	// Thousands separator, ASCII-only.
	digits := fmt.Sprintf("%d", whole)
	var grouped []byte
	for i, d := range []byte(digits) {
		if i != 0 && (len(digits)-i)%3 == 0 {
			grouped = append(grouped, ',')
		}
		grouped = append(grouped, d)
	}

	sign := ""
	if negative {
		sign = "-"
	}
	prefix := currency + " "
	if currency == "" || currency == "LKR" {
		prefix = "Rs. "
	}
	return fmt.Sprintf("%s%s%s.%02d", sign, prefix, grouped, frac)
}

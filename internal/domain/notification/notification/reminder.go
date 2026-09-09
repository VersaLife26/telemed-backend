package notification

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// ReminderCron scans the local appointment_reminder_state projection every
// tick and enqueues reminder_1h / reminder_24h notifications for anything
// newly due. It is one of two paths that can produce a reminder --
// consumer.go's onAppointmentReminderDue handles the same thing when
// scheduling-service publishes appointment.reminder_due itself -- and both
// converge on the identical deterministic dedupe_key
// ("reminder_<kind>:<appointment_id>:<channel>"), so a reminder is never
// sent twice no matter which path notices it first.
type ReminderCron struct {
	svc  *Service
	repo *Repository
	log  zerolog.Logger
}

// NewReminderCron builds the reminder cron.
func NewReminderCron(svc *Service, repo *Repository, log zerolog.Logger) *ReminderCron {
	return &ReminderCron{svc: svc, repo: repo, log: log}
}

// Run ticks every interval until ctx is cancelled. The brief specifies
// "every minute" for the 1-hour reminder window (55-65 minutes out); the
// same tick also sweeps the 24-hour window, since both are cheap indexed
// scans over the same small table.
func (rc *ReminderCron) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	rc.log.Info().Dur("interval", interval).Msg("reminder cron started")
	for {
		select {
		case <-ctx.Done():
			rc.log.Info().Msg("reminder cron stopped")
			return
		case <-ticker.C:
			rc.tick(ctx)
		}
	}
}

func (rc *ReminderCron) tick(ctx context.Context) {
	now := time.Now().UTC()

	if err := rc.sweep(ctx, reminderKind1h, now.Add(55*time.Minute), now.Add(65*time.Minute)); err != nil {
		rc.log.Error().Err(err).Msg("1h reminder sweep failed")
	}
	// A 20-minute window on the 24h reminder (23h50m-24h10m out) tolerates
	// this tick running every minute without either missing an appointment
	// between ticks or catching one twice -- the same reasoning as the
	// brief's 55-65 minute window for the 1h reminder, just wider because a
	// day-ahead reminder has no reason to be precise to the minute.
	if err := rc.sweep(ctx, reminderKind24h, now.Add(23*time.Hour+50*time.Minute), now.Add(24*time.Hour+10*time.Minute)); err != nil {
		rc.log.Error().Err(err).Msg("24h reminder sweep failed")
	}
}

func (rc *ReminderCron) sweep(ctx context.Context, kind string, windowStart, windowEnd time.Time) error {
	var due []AppointmentReminderState
	var err error
	templateKey := TemplateReminder1h
	if kind == reminderKind24h {
		due, err = rc.repo.FindDue24hReminders(ctx, windowStart, windowEnd)
		templateKey = TemplateReminder24h
	} else {
		due, err = rc.repo.FindDue1hReminders(ctx, windowStart, windowEnd)
	}
	if err != nil {
		return err
	}

	for i := range due {
		appt := &due[i]
		_, notifyErr := rc.svc.Notify(ctx, NotifyRequest{
			UserID:      appt.PatientID,
			TemplateKey: templateKey,
			Data: TemplateData{
				DoctorName: appt.DoctorName,
				DateTime:   formatDateTime(appt.StartsAt),
				JoinLink:   appt.JoinLink,
			},
			Phone: appt.PatientPhone, Email: appt.PatientEmail,
			// Deterministic, not tied to any inbound event: this send is
			// cron-triggered, not event-triggered. Combined with the
			// notifications.dedupe_key UNIQUE constraint, this is what
			// stops a second overlapping cron tick (or the event-driven
			// path above firing around the same moment) from double-
			// sending -- see AGENT-BRIEF's "the dedupe_key prevents a
			// double-send if the job overlaps itself".
			DedupeKeyBase: reminderDedupeKey(kind, appt.AppointmentID),
		})
		if notifyErr != nil {
			rc.log.Error().Err(notifyErr).
				Str("appointment_id", appt.AppointmentID.String()).
				Str("kind", kind).
				Msg("failed to enqueue reminder")
			continue
		}
		if err := rc.markSent(ctx, kind, appt.AppointmentID); err != nil {
			rc.log.Error().Err(err).
				Str("appointment_id", appt.AppointmentID.String()).
				Msg("failed to mark reminder sent on projection")
		}
	}
	return nil
}

func (rc *ReminderCron) markSent(ctx context.Context, kind string, appointmentID uuid.UUID) error {
	if kind == reminderKind24h {
		return rc.repo.MarkReminder24hSent(ctx, appointmentID)
	}
	return rc.repo.MarkReminder1hSent(ctx, appointmentID)
}

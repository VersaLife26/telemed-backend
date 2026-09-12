package notification

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/logger"
)

// ErrTokenUnregistered is the sentinel a push provider wraps (via Permanent)
// when the platform reports a device token as no longer valid. The dispatch
// loop watches for it via errors.Is to prune the token -- this is the "prune
// tokens FCM reports as unregistered" mechanism the brief calls out.
var ErrTokenUnregistered = errors.New("notification: device token unregistered")

// ServiceOptions tunes the retry/backoff and dispatch behaviour. Every field
// has a sane default so a service booted with a zero-value ServiceOptions
// still works.
type ServiceOptions struct {
	MaxAttempts   int           // give up and dead-letter after this many tries
	BackoffBase   time.Duration // delay before the 2nd attempt
	BackoffMax    time.Duration // ceiling on retry delay
	DefaultLocale Locale
	Now           func() time.Time // injectable clock for tests

	// SMSViaEmail routes the sms channel's messages to the recipient's EMAIL
	// address, for a deployment with no SMS rail (NOTIFICATION_SMS_PROVIDER=
	// email). The channel keeps its identity -- templates, preferences, quiet
	// hours and the notification rows all still say "sms", because what the
	// product means by that channel has not changed -- but the address it
	// resolves to, and the transport registered for it, are email.
	//
	// A user with a phone number and no email therefore has no reachable sms
	// channel, exactly as a user with no phone had none before.
	SMSViaEmail bool
}

func (o *ServiceOptions) setDefaults() {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 6
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = 30 * time.Second
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = 30 * time.Minute
	}
	if o.DefaultLocale == "" {
		o.DefaultLocale = LocaleEnglish
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Service is the notification business logic layer: rendering, dedupe,
// preference/quiet-hours enforcement, dispatch, retry classification and
// dead-lettering. It never runs raw SQL (that is Repository's job) and never
// sees an http.Request (that is Handler's job).
type Service struct {
	repo      *Repository
	providers ProviderRegistry
	log       zerolog.Logger
	opts      ServiceOptions
}

// NewService wires the business layer to its repository and provider
// registry.
func NewService(repo *Repository, providers ProviderRegistry, log zerolog.Logger, opts ServiceOptions) *Service {
	opts.setDefaults()
	return &Service{repo: repo, providers: providers, log: log, opts: opts}
}

// NotifyRequest is everything Notify needs to fan a business event out into
// one queued notification row per applicable channel.
type NotifyRequest struct {
	UserID      uuid.UUID
	TemplateKey TemplateKey
	Data        TemplateData

	// Phone/Email are the resolved contact details for sms/email channels.
	// This service does not look them up: the producing event (or the
	// direct-send caller) supplies them, keeping the delivery path free of
	// synchronous cross-service calls. Push resolves recipients from this
	// service's own device_tokens table at dispatch time; in_app needs none.
	Phone string
	Email string

	// DedupeKeyBase becomes "<base>:<channel>" per channel row. Pass the
	// triggering envelope.ID.String() for event-driven sends (idempotent
	// under at-least-once redelivery) or a deterministic business key (e.g.
	// "reminder_1h:<appointment_id>") for cron-driven sends.
	DedupeKeyBase string
	SourceEventID *uuid.UUID

	// LocaleHint overrides the user's stored preference locale when set and
	// valid; used by e.g. the OTP flow, which knows the language the user
	// picked at that exact moment (the login screen's language selector) and
	// may not have a preferences row yet.
	LocaleHint Locale

	// Channels overrides which channels to notify on. Empty means "every
	// channel that has an active template for TemplateKey".
	Channels []Channel

	// Immediate marks the notification 'sending' rather than 'queued' at
	// creation, keeping it invisible to the background dispatch poller. The
	// caller (the internal /notifications/send endpoint) must follow up with
	// DispatchOne itself. Used for latency-sensitive sends like otp_code.
	Immediate bool
}

// NotifyOutcome reports what happened to one channel's notification row.
type NotifyOutcome struct {
	NotificationID uuid.UUID
	Channel        Channel
	Status         Status
	// Created is false when dedupe_key already existed -- a redelivered
	// event produced no new row, which is success, not failure.
	Created bool
}

// Notify renders and enqueues one notification per applicable channel for
// req.TemplateKey. It is idempotent: calling it twice with the same
// DedupeKeyBase produces exactly one row per channel, enforced by the
// notifications.dedupe_key UNIQUE constraint, not by any in-process check.
func (s *Service) Notify(ctx context.Context, req NotifyRequest) ([]NotifyOutcome, error) {
	if req.UserID == uuid.Nil {
		return nil, fmt.Errorf("notification: user id is required")
	}
	if req.DedupeKeyBase == "" {
		return nil, fmt.Errorf("notification: dedupe key base is required")
	}

	prefs, found, err := s.repo.GetPreferences(ctx, s.repo.Pool(), req.UserID)
	if err != nil {
		return nil, err
	}
	if !found {
		prefs = DefaultPreferences(req.UserID)
	}

	locale := prefs.Locale
	if req.LocaleHint != "" && ValidLocale(req.LocaleHint) {
		locale = req.LocaleHint
	}
	if !ValidLocale(locale) {
		locale = s.opts.DefaultLocale
	}

	channels := req.Channels
	if len(channels) == 0 {
		channels, err = s.repo.ChannelsForKey(ctx, s.repo.Pool(), req.TemplateKey)
		if err != nil {
			return nil, err
		}
	}
	if len(channels) == 0 {
		return nil, fmt.Errorf("notification: no active templates for %s", req.TemplateKey)
	}

	loc, err := time.LoadLocation(prefs.Timezone)
	if err != nil {
		loc = time.UTC
	}
	now := s.opts.Now()

	var outcomes []NotifyOutcome
	var firstErr error
	for _, ch := range channels {
		outcome, err := s.notifyChannel(ctx, req, prefs, locale, loc, now, ch)
		if err != nil {
			s.log.Error().Err(err).
				Str("user_id", logger.MaskID(req.UserID.String())).
				Str("channel", string(ch)).
				Str("template", string(req.TemplateKey)).
				Msg("failed to enqueue notification channel")
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		outcomes = append(outcomes, outcome)
	}
	if len(outcomes) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return outcomes, nil
}

func (s *Service) notifyChannel(ctx context.Context, req NotifyRequest, prefs Preferences, locale Locale, loc *time.Location, now time.Time, ch Channel) (NotifyOutcome, error) {
	tmpl, err := s.repo.GetActiveTemplate(ctx, s.repo.Pool(), req.TemplateKey, ch, locale)
	if errors.Is(err, ErrNotFound) && locale != LocaleEnglish {
		tmpl, err = s.repo.GetActiveTemplate(ctx, s.repo.Pool(), req.TemplateKey, ch, LocaleEnglish)
		locale = LocaleEnglish
	}
	if err != nil {
		return NotifyOutcome{}, fmt.Errorf("no active template for %s/%s/%s: %w", req.TemplateKey, ch, locale, err)
	}

	rendered, err := RenderTemplate(tmpl, req.Data)
	if err != nil {
		return NotifyOutcome{}, err
	}

	recipient := ""
	switch ch {
	case ChannelSMS:
		// The sms channel addresses itself by email when this deployment has
		// no SMS rail. Falling back to the phone number when no email is
		// known would queue a row that the email transport cannot deliver and
		// that retries until it dead-letters; an empty recipient is treated
		// as "no recipient" by decideDispatch below, which is the honest
		// outcome and the same one an unknown phone already produced.
		if opts := s.opts; opts.SMSViaEmail {
			recipient = req.Email
		} else {
			recipient = req.Phone
		}
	case ChannelEmail:
		recipient = req.Email
	}

	status, scheduledFor := decideDispatch(now, loc, ch, tmpl.Urgency,
		prefs.Enabled(ch), recipient != "" || (ch != ChannelSMS && ch != ChannelEmail),
		prefs.QuietHoursStart, prefs.QuietHoursEnd, req.Immediate)

	n := Notification{
		UserID:        req.UserID,
		Channel:       ch,
		TemplateKey:   req.TemplateKey,
		Locale:        locale,
		Urgency:       tmpl.Urgency,
		Subject:       rendered.Subject,
		Body:          rendered.Body,
		Recipient:     recipient,
		Status:        status,
		DedupeKey:     req.DedupeKeyBase + ":" + string(ch),
		SourceEventID: req.SourceEventID,
		ScheduledFor:  scheduledFor,
	}

	id, created, err := s.repo.CreateIfNew(ctx, s.repo.Pool(), n)
	if err != nil {
		return NotifyOutcome{}, err
	}
	if !created {
		return NotifyOutcome{Channel: ch, Created: false}, nil
	}
	return NotifyOutcome{NotificationID: id, Channel: ch, Status: n.Status, Created: true}, nil
}

// decideDispatch is the pure business rule behind "what happens to this
// channel's notification row right now": preference suppression, missing-
// recipient suppression, quiet-hours deferral, and the immediate/queued
// split. It touches no database and no clock but the one passed in, which is
// what makes it exhaustively unit-testable (see decide_dispatch_test.go)
// without a Postgres instance -- the DB-backed paths (dedupe, preference
// persistence) are covered separately by the integration tests.
//
// hasRecipient is true for push/in_app unconditionally (they resolve their
// own recipient at dispatch time / need none) and reflects whether an
// address was supplied for sms/email.
func decideDispatch(now time.Time, loc *time.Location, ch Channel, urgency Urgency,
	prefEnabled, hasRecipient bool, quietStart, quietEnd *time.Duration, immediate bool,
) (Status, *time.Time) {
	if urgency != UrgencyCritical && !prefEnabled {
		return StatusSuppressed, nil
	}
	if !hasRecipient {
		return StatusSuppressed, nil
	}
	if until, hold := ShouldDefer(now, loc, ch, urgency, quietStart, quietEnd); hold {
		return StatusQueued, &until
	}
	if immediate {
		return StatusSending, nil
	}
	return StatusQueued, nil
}

// DispatchOne loads notification id and attempts delivery if it is still
// queued or sending. Called directly by the synchronous /notifications/send
// path right after Notify (for Immediate requests) and, with an
// already-loaded row, by the background dispatch loop.
func (s *Service) DispatchOne(ctx context.Context, id uuid.UUID) error {
	n, err := s.repo.GetByID(ctx, s.repo.Pool(), id, uuid.Nil)
	if err != nil {
		return err
	}
	return s.dispatch(ctx, n)
}

func (s *Service) dispatch(ctx context.Context, n Notification) error {
	if n.Status != StatusQueued && n.Status != StatusSending {
		return nil // already terminal (sent/delivered/failed/suppressed) -- nothing to do
	}

	provider, ok := s.providers.ProviderFor(n.Channel)
	if !ok {
		return s.recordOutcome(ctx, n, "unconfigured", Receipt{}, Permanent(fmt.Errorf("no provider configured for channel %s", n.Channel)))
	}

	if n.Channel == ChannelPush {
		return s.dispatchPush(ctx, n, provider)
	}

	msg := Message{
		Channel:        n.Channel,
		To:             n.Recipient,
		Subject:        n.Subject,
		Body:           n.Body,
		Locale:         n.Locale,
		TemplateKey:    n.TemplateKey,
		NotificationID: n.ID.String(),
	}
	receipt, sendErr := provider.Send(ctx, msg)
	return s.recordOutcome(ctx, n, provider.Name(), receipt, sendErr)
}

// dispatchPush fans one push notification out to every active device token
// the user has registered. It is delivered as soon as any single token
// accepts it; a token FCM/APNs reports unregistered is pruned immediately
// via ErrTokenUnregistered, which is how "prune tokens FCM reports as
// unregistered" actually happens rather than being a promise in a comment.
func (s *Service) dispatchPush(ctx context.Context, n Notification, provider NotificationProvider) error {
	tokens, err := s.repo.ListActiveDeviceTokens(ctx, s.repo.Pool(), n.UserID)
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return s.recordOutcome(ctx, n, provider.Name(), Receipt{}, Permanent(errors.New("no active device tokens registered")))
	}

	var lastErr error
	var receipt Receipt
	delivered := false

	for i := range tokens {
		tok := &tokens[i]
		msg := Message{
			Channel:        ChannelPush,
			DeviceToken:    tok.Token,
			Subject:        n.Subject,
			Body:           n.Body,
			Locale:         n.Locale,
			TemplateKey:    n.TemplateKey,
			NotificationID: n.ID.String(),
		}
		r, err := provider.Send(ctx, msg)
		if err != nil {
			if errors.Is(err, ErrTokenUnregistered) {
				if invErr := s.repo.InvalidateDeviceTokenByValue(ctx, s.repo.Pool(), tok.Token); invErr != nil {
					s.log.Warn().Err(invErr).Msg("failed to invalidate unregistered device token")
				}
			}
			lastErr = err
			continue
		}
		delivered = true
		receipt = r
		break // one accepted delivery is enough to mark the notification delivered
	}

	if delivered {
		return s.recordOutcome(ctx, n, provider.Name(), receipt, nil)
	}
	return s.recordOutcome(ctx, n, provider.Name(), Receipt{}, lastErr)
}

// recordOutcome applies a single dispatch attempt's result: success moves
// the notification to sent/delivered, a permanent failure or an
// attempts-exhausted transient failure dead-letters it, and any other
// transient failure schedules a backed-off retry.
func (s *Service) recordOutcome(ctx context.Context, n Notification, providerName string, receipt Receipt, sendErr error) error {
	attempt := n.Attempts + 1
	entry := DeliveryLogEntry{NotificationID: n.ID, Attempt: attempt, Provider: providerName}

	if sendErr == nil {
		status := StatusSent
		var deliveredAt *time.Time
		if receipt.Delivered {
			status = StatusDelivered
			t := s.opts.Now()
			deliveredAt = &t
		}
		sentAt := s.opts.Now()
		res := AttemptResult{
			Status: status, Provider: providerName, ProviderMessageID: receipt.ProviderMessageID,
			SentAt: &sentAt, DeliveredAt: deliveredAt,
		}
		entry.Status = "sent"
		if receipt.Delivered {
			entry.Status = "delivered"
		}
		if err := s.repo.InsertDeliveryLog(ctx, s.repo.Pool(), entry); err != nil {
			s.log.Warn().Err(err).Msg("failed to write delivery log")
		}
		return s.repo.RecordAttempt(ctx, s.repo.Pool(), n.ID, res)
	}

	class := Classify(sendErr)
	entry.Status = "failed"
	entry.ErrorClass = class
	if err := s.repo.InsertDeliveryLog(ctx, s.repo.Pool(), entry); err != nil {
		s.log.Warn().Err(err).Msg("failed to write delivery log")
	}

	errText := safeErrText(sendErr)

	if class == ErrorClassPermanent || attempt >= s.opts.MaxAttempts {
		reason := DeadLetterRetriesExhausted
		if class == ErrorClassPermanent {
			reason = DeadLetterPermanentError
		}
		res := AttemptResult{Status: StatusFailed, Provider: providerName, ErrorClass: class, LastError: errText}
		if err := s.repo.RecordAttempt(ctx, s.repo.Pool(), n.ID, res); err != nil {
			return err
		}
		return s.repo.InsertDeadLetter(ctx, s.repo.Pool(), n.ID, n.UserID, n.Channel, n.TemplateKey, attempt, errText, reason)
	}

	next := s.opts.Now().Add(backoffDelay(attempt, s.opts.BackoffBase, s.opts.BackoffMax))
	res := AttemptResult{
		Status: StatusQueued, Provider: providerName, ErrorClass: class,
		LastError: errText, NextAttemptAt: &next,
	}
	return s.repo.RecordAttempt(ctx, s.repo.Pool(), n.ID, res)
}

// backoffDelay doubles from base up to max: attempt 1 -> base, attempt 2 ->
// 2*base, ... capped. A permanent 503 storm backs off instead of hammering a
// recovering provider; a genuinely stuck notification still gets tried every
// BackoffMax until MaxAttempts gives up on it.
func backoffDelay(attempt int, base, maxDelay time.Duration) time.Duration {
	d := base * time.Duration(1<<min(attempt-1, 10))
	if d <= 0 || d > maxDelay {
		return maxDelay
	}
	return d
}

// safeErrText caps stored error text. Provider errors describe transport
// failures (status codes, timeouts), never message content, so this is a
// defensive cap rather than a redaction -- but a cap costs nothing and a
// misbehaving dependency echoing back a request body is not a risk worth
// taking on a healthcare platform.
func safeErrText(err error) string {
	return ScrubContact(err.Error())
}

// maxStoredErrLen caps what reaches notifications.last_error and the
// dead-letter table.
const maxStoredErrLen = 500

var (
	// phonePatterns match the three shapes a recipient number arrives in.
	// They are deliberately separate rather than one permissive alternation:
	// a pattern loose enough to catch "+94 77 123 4567" in one go also
	// swallows an SMTP status line like "550 5.1.1", which is the diagnostic
	// half of the message and the only reason to keep the string at all.
	phonePatterns = []*regexp.Regexp{
		// International, with separators: "+94771234567", "+94 77 123 4567".
		regexp.MustCompile(`\+\d[\d\s\-()]{6,}\d`),
		// Sri Lankan local format: "0771234567".
		regexp.MustCompile(`\b0\d{8,14}\b`),
		// A bare contiguous run long enough to be a subscriber number.
		regexp.MustCompile(`\b\d{9,15}\b`),
	}
	// emailPattern is deliberately loose. A false positive costs one redacted
	// token in an error message; a false negative persists a patient's email
	// address forever.
	emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
)

// ScrubContact removes recipient identifiers from text that is about to be
// stored or logged, and caps its length.
//
// safeErrText's own comment used to assert that provider errors "describe
// transport failures (status codes, timeouts), never message content". That is
// not true of any of the four backends:
//
//   - SMTP: classifySMTPError wraps the raw error, and a real 550 reads
//     "550 5.1.1 <patient@example.com>: Recipient address rejected".
//   - Twilio: error 21211 renders as "The 'To' number +94771234567 is not a
//     valid phone number".
//   - Dialog: the response carries a provider-authored Description.
//   - FCM: INVALID_ARGUMENT quotes the offending field value.
//
// Those strings land in notifications.last_error and
// notification_dead_letters.last_error, neither of which the retention job
// touches -- so a phone number written there outlives the message body by
// design. The cap alone was never the control the comment claimed.
func ScrubContact(s string) string {
	s = emailPattern.ReplaceAllString(s, "[email]")
	for _, re := range phonePatterns {
		s = re.ReplaceAllString(s, "[phone]")
	}
	if len(s) > maxStoredErrLen {
		s = s[:maxStoredErrLen]
	}
	return s
}

// RunDispatcher polls for due notifications and attempts delivery until ctx
// is cancelled. Call it in its own goroutine at boot, exactly like the
// outbox relay it is modelled on.
func (s *Service) RunDispatcher(ctx context.Context, interval time.Duration, batchSize int) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if batchSize <= 0 {
		batchSize = 50
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.log.Info().Dur("interval", interval).Int("batch", batchSize).Msg("notification dispatcher started")

	for {
		select {
		case <-ctx.Done():
			s.log.Info().Msg("notification dispatcher stopped")
			return
		case <-ticker.C:
			n, err := s.dispatchBatchOnce(ctx, batchSize)
			if err != nil {
				s.log.Error().Err(err).Msg("dispatch batch failed")
				continue
			}
			for err == nil && n == batchSize {
				n, err = s.dispatchBatchOnce(ctx, batchSize)
			}
		}
	}
}

func (s *Service) dispatchBatchOnce(ctx context.Context, batchSize int) (int, error) {
	batch, err := s.repo.ClaimDispatchBatch(ctx, batchSize)
	if err != nil {
		return 0, err
	}
	for i := range batch {
		if err := s.dispatch(ctx, batch[i]); err != nil {
			s.log.Error().Err(err).
				Str("notification_id", batch[i].ID.String()).
				Str("channel", string(batch[i].Channel)).
				Msg("dispatch attempt failed")
		}
	}
	return len(batch), nil
}

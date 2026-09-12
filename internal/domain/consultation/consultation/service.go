package consultation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/cache"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
)

// Sentinel errors the handler translates into the platform's httpx error
// taxonomy. Keeping them here, not in the handler, is what lets a future
// gRPC or CLI front-end reuse the same service without importing net/http.
var (
	ErrNotFound     = errors.New("consultation: not found")
	ErrForbidden    = errors.New("consultation: caller is not a party to this consultation")
	ErrInvalidState = errors.New("consultation: invalid state transition")
)

// store is the persistence contract the service depends on. Defining it here,
// next to its only consumer, keeps repository.go a thin SQL adapter and lets
// tests substitute an in-memory fake instead of standing up Postgres for
// every business rule -- Docker is contended on this machine, table-driven
// service tests are not optional.
type store interface {
	CreateConsultation(ctx context.Context, tx pgx.Tx, c *Consultation) error
	GetConsultation(ctx context.Context, q queryer, id uuid.UUID) (*Consultation, error)
	GetConsultationByAppointment(ctx context.Context, q queryer, appointmentID uuid.UUID) (*Consultation, error)
	GetConsultationByRoomName(ctx context.Context, q queryer, roomName string) (*Consultation, error)
	UpdateConsultation(ctx context.Context, tx pgx.Tx, c *Consultation) error
	UpdateConsultationScheduledAt(ctx context.Context, tx pgx.Tx, appointmentID uuid.UUID, startAt, endAt time.Time) error
	ListOverrunActive(ctx context.Context, q queryer, now time.Time, limit int) ([]*Consultation, error)
	FindNextUpcomingForDoctor(ctx context.Context, q queryer, doctorID uuid.UUID, afterScheduledAt time.Time) (*Consultation, error)
	ClaimRunningLateNotified(ctx context.Context, tx pgx.Tx, consultationID uuid.UUID, at time.Time) (bool, error)
	FindActiveForDoctor(ctx context.Context, q queryer, doctorID uuid.UUID) (*Consultation, error)
	FindLatestTerminalForDoctor(ctx context.Context, q queryer, doctorID uuid.UUID) (*Consultation, error)
	ClaimEarlyJoinOffered(ctx context.Context, tx pgx.Tx, consultationID uuid.UUID, at time.Time) (bool, error)
	SetEarlyJoinResponse(ctx context.Context, tx pgx.Tx, consultationID uuid.UUID, response string, at time.Time) (bool, error)

	UpsertParticipantJoin(ctx context.Context, tx pgx.Tx, consultationID uuid.UUID, identity string, role ParticipantRole, at time.Time) error
	MarkParticipantLeft(ctx context.Context, tx pgx.Tx, consultationID uuid.UUID, identity string, at time.Time) error
	ListParticipants(ctx context.Context, q queryer, consultationID uuid.UUID) ([]Participant, error)

	SaveConsent(ctx context.Context, tx pgx.Tx, c Consent) error
	RecordingConsentStatus(ctx context.Context, q queryer, consultationID uuid.UUID) (map[uuid.UUID]bool, error)

	RecordEvent(ctx context.Context, tx pgx.Tx, e Event) error
	RecentQualityEvents(ctx context.Context, q queryer, consultationID uuid.UUID, identity string, limit int) ([]Event, error)

	UpsertWaitingRoomEntry(ctx context.Context, tx pgx.Tx, e WaitingRoomEntry) error
	GetWaitingRoomEntry(ctx context.Context, q queryer, consultationID uuid.UUID) (*WaitingRoomEntry, error)
	MarkWaitingRoomAdmitted(ctx context.Context, tx pgx.Tx, consultationID uuid.UUID, at time.Time) error
	MarkWaitingRoomLeft(ctx context.Context, tx pgx.Tx, consultationID uuid.UUID, at time.Time, status WaitingRoomStatus) error
	CountWaitingAhead(ctx context.Context, q queryer, doctorID uuid.UUID, before time.Time) (int64, error)

	AverageDurationSeconds(ctx context.Context, q queryer, doctorID uuid.UUID, sampleSize int) (float64, bool, error)

	ClaimWebhookEvent(ctx context.Context, tx pgx.Tx, eventID, eventType string) (bool, error)

	ListStaleActive(ctx context.Context, q queryer, quietSince time.Time, limit int) ([]StaleCandidate, error)
}

// Options configures Service. Every duration has a sane default applied by
// NewService so a service booted with a bare-minimum .env still runs.
type Options struct {
	LiveKitURL string
	// SignalURL is the ABSOLUTE ws(s):// address of this deployment's
	// signalling socket, e.g. wss://rtc.example.lk/ws/consultation. Absolute
	// rather than a path because the browser connects to it directly: a Next
	// route handler cannot proxy a websocket upgrade.
	SignalURL               string
	TokenTTL                time.Duration
	EmptyRoomTimeoutSeconds uint32
	RecordingBucket         string

	// ICEServers is called per join rather than stored, so a deployment
	// minting short-lived TURN credentials returns fresh ones each time.
	// Mirrors signal.HubOptions.ICEServers.
	//
	// Before this was a function it was a stored slice that no module ever
	// set, so JoinResult.ICEServers was unconditionally empty in production
	// and every call was STUN-only without anything saying so.
	ICEServers func(ctx context.Context, identity string) []ICEServer

	// DefaultConsultationDuration is the wait estimate used only until a
	// doctor has enough completed consultations for a real rolling average.
	DefaultConsultationDuration time.Duration
	WaitingRoomAvgSampleSize    int

	// QualityDegradeThreshold is how many consecutive poor/lost samples from
	// the same participant trigger should_downgrade_video.
	QualityDegradeThreshold int
}

// Service holds every business rule for the consultation domain. Handlers
// call it; it never sees an http.Request, and it depends on VideoProvider and
// Cache only through their interfaces.
type Service struct {
	store  store
	video  VideoProvider
	cache  cache.Cache
	pool   database.Pool
	outbox *events.Outbox
	log    zerolog.Logger
	opts   Options
}

// NewService builds Service, filling in defaults for any zero-valued option.
func NewService(st store, video VideoProvider, c cache.Cache, pool database.Pool, outbox *events.Outbox, log zerolog.Logger, opts Options) *Service {
	if opts.TokenTTL <= 0 {
		opts.TokenTTL = 5 * time.Minute
	}
	if opts.QualityDegradeThreshold <= 0 {
		opts.QualityDegradeThreshold = 3
	}
	if opts.WaitingRoomAvgSampleSize <= 0 {
		opts.WaitingRoomAvgSampleSize = 10
	}
	if opts.DefaultConsultationDuration <= 0 {
		opts.DefaultConsultationDuration = 15 * time.Minute
	}
	if opts.ICEServers == nil {
		// Never nil, so Join can call it unconditionally. An empty list is
		// also what the handler contract promises: ice_servers is always an
		// array, never null.
		opts.ICEServers = func(context.Context, string) []ICEServer { return []ICEServer{} }
	}
	return &Service{store: st, video: video, cache: c, pool: pool, outbox: outbox, log: log, opts: opts}
}

// authorizeParty reports which role, if any, principal holds on c. Identity
// match is the actual authorization boundary -- not the role claim -- so a
// mis-set or missing role claim can never widen access; it can only narrow it
// by failing HasRole checks a caller relies on elsewhere.
func authorizeParty(p middleware.Principal, c *Consultation) (ParticipantRole, bool) {
	if p.UserID != uuid.Nil && p.UserID == c.PatientID {
		return RolePatient, true
	}
	if p.DoctorID != uuid.Nil && p.DoctorID == c.DoctorID {
		return RoleDoctor, true
	}
	return "", false
}

func waitingRoomKey(doctorID uuid.UUID) string { return "waiting_room:doctor:" + doctorID.String() }

// waitingRoomTTL bounds the sorted set behind a doctor's waiting room.
//
// Members are removed explicitly on admit, end and cancel, but a patient who
// joins and is never admitted leaves one behind forever: the stale sweeper
// only considers consultations with a started_at, and Admit is what sets it.
// Redis runs with maxmemory-policy noeviction on this deployment -- correct,
// because the other things in this Redis are OTP attempt counters and the
// suspension denylist, which must never be evicted -- so an unbounded key here
// eventually refuses writes for all of them.
//
// A day is far longer than any waiting room legitimately lives, so the TTL
// only ever collects abandoned sets. It is refreshed on every join, so a
// doctor with a continuously busy queue never loses one in use.
const waitingRoomTTL = 24 * time.Hour

func roomNameFor(appointmentID uuid.UUID) string { return "consultation-" + appointmentID.String() }

// partyIdentity is the canonical identity used for the LiveKit participant
// identity, consent attribution, and webhook correlation. It is deliberately
// NOT always principal.UserID: a patient's user id and consultations.patient_id
// are the same id space, but a doctor's user id (principal.UserID, their
// Keycloak account) and consultations.doctor_id (the doctor PROFILE id,
// doctors.id in the doctor service) are two different id spaces by design
// (see doctor-service's schema). Using the party's own consultation-scoped id
// here -- rather than whatever the caller's token happens to carry -- is what
// lets a LiveKit webhook's participant identity be compared directly against
// c.DoctorID/c.PatientID with no extra lookup, and what makes
// RecordingConsentStatus's map keyed by those same ids actually match.
func partyIdentity(role ParticipantRole, c *Consultation) uuid.UUID {
	if role == RoleDoctor {
		return c.DoctorID
	}
	return c.PatientID
}

// --- join / admit / end ----------------------------------------------------

// Join authorizes the caller, lazily ensures the LiveKit room exists, and
// mints a fresh token. A patient's first join transitions the consultation
// into the doctor's waiting queue; every later call (a token refresh, a
// reconnect after a dropped 3G connection) is a no-op on that state.
func (s *Service) Join(ctx context.Context, principal middleware.Principal, appointmentID uuid.UUID) (JoinResult, error) {
	c, err := s.store.GetConsultationByAppointment(ctx, s.pool, appointmentID)
	if err != nil {
		return JoinResult{}, err
	}

	role, ok := authorizeParty(principal, c)
	if !ok {
		return JoinResult{}, ErrForbidden
	}
	if c.Status.terminal() {
		return JoinResult{}, ErrInvalidState
	}

	if _, err := s.video.CreateRoom(ctx, RoomSpec{
		RoomName:            c.RoomName,
		EmptyTimeoutSeconds: s.opts.EmptyRoomTimeoutSeconds,
		MaxParticipants:     2,
	}); err != nil {
		return JoinResult{}, fmt.Errorf("consultation: ensure room %s: %w", c.RoomName, err)
	}

	identity := partyIdentity(role, c).String()
	token, err := s.video.GenerateToken(ctx, TokenSpec{
		RoomName:     c.RoomName,
		Identity:     identity,
		CanPublish:   true,
		CanSubscribe: true,
		TTL:          s.opts.TokenTTL,
	})
	if err != nil {
		return JoinResult{}, fmt.Errorf("consultation: generate token: %w", err)
	}

	now := time.Now().UTC()
	// Computed from the same clock reading the join is stamped with, so the
	// client is never told an expiry earlier than the token actually has.
	tokenExpiresAt := now.Add(s.opts.TokenTTL)
	enteredWaitingRoom := false
	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.store.UpsertParticipantJoin(ctx, tx, c.ID, identity, role, now); err != nil {
			return err
		}
		if err := s.store.RecordEvent(ctx, tx, Event{
			ConsultationID: c.ID, Type: EventJoined, ActorIdentity: identity,
			Metadata: map[string]any{"role": string(role)}, OccurredAt: now,
		}); err != nil {
			return err
		}

		if role == RolePatient && c.Status == StatusScheduled {
			enteredWaitingRoom = true
			c.Status = StatusWaiting
			if err := s.store.UpdateConsultation(ctx, tx, c); err != nil {
				return err
			}
			if err := s.store.UpsertWaitingRoomEntry(ctx, tx, WaitingRoomEntry{
				ConsultationID: c.ID, DoctorID: c.DoctorID, PatientID: c.PatientID, EnteredAt: now,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return JoinResult{}, err
	}

	if enteredWaitingRoom {
		key := waitingRoomKey(c.DoctorID)
		// UnixNano, not Unix: whole-second scores tie whenever two patients
		// join within the same second (routine at a busy clinic's start of
		// hour), and Redis breaks ties by member name, not arrival order.
		if err := s.cache.ZAdd(ctx, key, float64(now.UnixNano()), c.ID.String()); err != nil {
			// ADR-007: Postgres above is the truth. A Redis miss here only
			// costs a WaitingRoomStatus call the fast path; it never loses
			// the patient's place, which the CountWaitingAhead fallback covers.
			s.log.Warn().Err(err).Str("doctor_id", c.DoctorID.String()).
				Msg("waiting room redis mirror failed, postgres remains authoritative")
		} else if err := s.cache.Expire(ctx, key, waitingRoomTTL); err != nil {
			s.log.Warn().Err(err).Str("doctor_id", c.DoctorID.String()).
				Msg("waiting room ttl not set")
		}
	}

	return JoinResult{
		ConsultationID: c.ID,
		AppointmentID:  c.AppointmentID,
		Status:         c.Status,
		Role:           string(role),
		Token:          token,
		TokenExpiresAt: tokenExpiresAt,
		RoomName:       c.RoomName,
		Provider:       s.video.Name(),
		LiveKitURL:     s.opts.LiveKitURL,
		SignalURL:      s.opts.SignalURL,
		RecordingMode:  recordingModeFor(s.video.Name()),
		// Minted per join, and scoped to the identity that will present it:
		// a REST-style TURN credential is traceable only if it names someone.
		ICEServers: s.opts.ICEServers(ctx, identity),
	}, nil
}

// Admit is the doctor explicitly bringing a waiting patient into an active
// consultation. It is the only path that sets started_at and publishes
// consultation.started.
//
// # Why the status gate is the whole security control (F3)
//
// consultation.started is not just a notification. record-service consumes it
// and writes a row into treating_relationships, and that row is what grants a
// doctor read and download on the patient's entire medical vault -- every
// document, every prescription written by every other doctor, and everything
// the patient uploads years later.
//
// This method used to accept StatusScheduled. A consultation is scheduled from
// the moment the appointment is confirmed, by a consumer, with no human
// involved. So a doctor could advertise a free two-minute consultation, wait
// for a booking, call admit and then end, and hold permanent access to a
// patient who never opened the app. Nothing about that sequence required the
// patient to do anything except book.
//
// Requiring StatusWaiting closes it, because there is exactly one way to reach
// StatusWaiting: Join, called by a principal that authorizeParty matched
// against c.PatientID -- the patient's own token, on the patient's own device.
// The doctor cannot produce it, cannot forge it, and cannot wait it out. See
// docs/DESIGN.md, "What counts as a treating relationship".
func (s *Service) Admit(ctx context.Context, principal middleware.Principal, consultationID uuid.UUID) (*Consultation, error) {
	c, err := s.store.GetConsultation(ctx, s.pool, consultationID)
	if err != nil {
		return nil, err
	}
	if principal.DoctorID == uuid.Nil || principal.DoctorID != c.DoctorID {
		return nil, ErrForbidden
	}
	// StatusWaiting only. A scheduled consultation is one nobody has attended.
	if c.Status != StatusWaiting {
		return nil, ErrInvalidState
	}

	now := time.Now().UTC()
	c.Status = StatusActive
	if c.StartedAt == nil {
		c.StartedAt = &now
	}

	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.store.UpdateConsultation(ctx, tx, c); err != nil {
			return err
		}
		if err := s.store.MarkWaitingRoomAdmitted(ctx, tx, c.ID, now); err != nil {
			return err
		}
		if err := s.store.RecordEvent(ctx, tx, Event{
			ConsultationID: c.ID, Type: EventAdmitted, ActorIdentity: c.DoctorID.String(), OccurredAt: now,
		}); err != nil {
			return err
		}
		payload := events.ConsultationStarted{
			ConsultationID: c.ID, AppointmentID: c.AppointmentID,
			PatientID: c.PatientID, DoctorID: c.DoctorID,
			RoomName: c.RoomName, StartedAt: *c.StartedAt,
		}
		return s.outbox.Enqueue(ctx, tx, events.SubjectConsultationStarted, c.ID.String(), payload)
	})
	if err != nil {
		return nil, err
	}

	key := waitingRoomKey(c.DoctorID)
	if err := s.cache.ZRem(ctx, key, c.ID.String()); err != nil {
		s.log.Warn().Err(err).Msg("waiting room redis cleanup failed on admit")
	}

	// Consent may have been granted by both parties before the doctor ever
	// admitted the patient; catch that case here rather than only on the
	// consent call itself.
	if err := s.maybeStartRecording(ctx, c); err != nil {
		s.log.Error().Err(err).Str("consultation_id", c.ID.String()).Msg("failed to start recording after admit")
	}

	return c, nil
}

// End is the caller-initiated close of a consultation, from either the
// patient, the doctor, or platform support. It is idempotent: ending an
// already-terminal consultation returns the current state rather than an
// error, since a webhook-driven end (room_finished) racing a manual End tap
// must not surface as a client-visible failure.
func (s *Service) End(ctx context.Context, principal middleware.Principal, consultationID uuid.UUID, reason string) (*Consultation, error) {
	c, err := s.store.GetConsultation(ctx, s.pool, consultationID)
	if err != nil {
		return nil, err
	}
	if _, ok := authorizeParty(principal, c); !ok && !principal.IsAdmin() {
		return nil, ErrForbidden
	}
	return s.endInternal(ctx, c, reason, time.Time{})
}

// endInternal implements the state machine's two terminal paths:
//   - the consultation was active (started_at set)   -> ended
//   - the consultation never started (started_at nil) -> abandoned
//
// Both the HTTP End handler and the LiveKit room_finished webhook route
// through this single function so there is exactly one place that decides
// "ended" vs "abandoned".
// at is the instant the consultation actually ended. The two interactive
// callers pass the zero value and get time.Now(); the sweeper passes the last
// evidence of activity, because a webhook lost overnight must not be recorded
// as a nine-hour consultation in duration_seconds and in the doctor's rolling
// average.
func (s *Service) endInternal(ctx context.Context, c *Consultation, reason string, at time.Time) (*Consultation, error) {
	if c.Status.terminal() {
		return c, nil
	}

	now := at.UTC()
	if at.IsZero() {
		now = time.Now().UTC()
	}
	if c.StartedAt != nil {
		c.Status = StatusEnded
	} else {
		c.Status = StatusAbandoned
	}
	c.EndedAt = &now
	if reason != "" {
		c.EndReason = &reason
	}
	if c.StartedAt != nil {
		d := int(now.Sub(*c.StartedAt).Seconds())
		if d < 0 {
			d = 0
		}
		c.DurationSeconds = &d
	}

	eventType := EventEnded
	subject := events.SubjectConsultationEnded
	if c.Status == StatusAbandoned {
		eventType = EventAbandoned
	}

	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.store.UpdateConsultation(ctx, tx, c); err != nil {
			return err
		}
		if err := s.store.MarkWaitingRoomLeft(ctx, tx, c.ID, now, WaitingStatusLeft); err != nil {
			return err
		}
		if err := s.store.RecordEvent(ctx, tx, Event{
			ConsultationID: c.ID, Type: eventType, Metadata: map[string]any{"reason": reason}, OccurredAt: now,
		}); err != nil {
			return err
		}
		payload := events.ConsultationEnded{
			ConsultationID: c.ID, AppointmentID: c.AppointmentID,
			PatientID: c.PatientID, DoctorID: c.DoctorID,
			DurationSeconds: durationSeconds(c),
			EndReason:       endReasonFor(c),
			Recorded:        wasRecorded(c),
			EndedAt:         now,
		}
		return s.outbox.Enqueue(ctx, tx, subject, c.ID.String(), payload)
	})
	if err != nil {
		return nil, err
	}

	if err := s.video.EndRoom(ctx, c.RoomName); err != nil && !errors.Is(err, ErrRoomNotFound) {
		s.log.Warn().Err(err).Str("room", c.RoomName).Msg("end room on video provider failed")
	}
	key := waitingRoomKey(c.DoctorID)
	if err := s.cache.ZRem(ctx, key, c.ID.String()); err != nil {
		s.log.Warn().Err(err).Msg("waiting room redis cleanup failed on end")
	}

	return c, nil
}

// --- consent / recording ----------------------------------------------------

// ConsentInput is one consent decision submitted through POST .../consent.
type ConsentInput struct {
	Type      ConsentType
	Granted   bool
	IPAddress string
	UserAgent string
}

// SubmitConsent records a consent decision. Recording never starts from this
// call directly -- maybeStartRecording is the single gate, and it only opens
// once BOTH parties' most recent decision for consent_type=recording is
// granted=true. A lone consent, granted or not, cannot start a recording; that
// is enforced here, in the service layer, not left to the client UI to honor.
func (s *Service) SubmitConsent(ctx context.Context, principal middleware.Principal, consultationID uuid.UUID, in ConsentInput) (*Consultation, error) {
	c, err := s.store.GetConsultation(ctx, s.pool, consultationID)
	if err != nil {
		return nil, err
	}
	role, ok := authorizeParty(principal, c)
	if !ok {
		return nil, ErrForbidden
	}
	if c.Status.terminal() {
		return nil, ErrInvalidState
	}

	now := time.Now().UTC()
	identity := partyIdentity(role, c)
	consent := Consent{
		ConsultationID: c.ID, UserID: identity, Type: in.Type,
		Granted: in.Granted, GrantedAt: now, IPAddress: in.IPAddress, UserAgent: in.UserAgent,
	}

	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.store.SaveConsent(ctx, tx, consent); err != nil {
			return err
		}
		return s.store.RecordEvent(ctx, tx, Event{
			ConsultationID: c.ID, Type: EventConsentRecorded, ActorIdentity: identity.String(),
			Metadata: map[string]any{"consent_type": string(in.Type), "granted": in.Granted}, OccurredAt: now,
		})
	})
	if err != nil {
		return nil, err
	}

	if in.Type == ConsentRecording && in.Granted {
		if err := s.maybeStartRecording(ctx, c); err != nil {
			s.log.Error().Err(err).Str("consultation_id", c.ID.String()).Msg("failed to start recording after consent")
		}
	}
	return c, nil
}

// maybeStartRecording is the single gate on egress. It requires:
//  1. the consultation is currently active (no point recording a lobby),
//  2. no recording already started, and
//  3. the MOST RECENT recording consent from BOTH the patient and the doctor
//     is granted=true.
//
// Any one missing leaves recording_status untouched -- there is deliberately
// no error for "not yet both consented"; that is the expected steady state
// while the platform waits for the second party.
func (s *Service) maybeStartRecording(ctx context.Context, c *Consultation) error {
	if c.RecordingStatus != RecordingNone || c.Status != StatusActive {
		return nil
	}

	statuses, err := s.store.RecordingConsentStatus(ctx, s.pool, c.ID)
	if err != nil {
		return fmt.Errorf("consultation: recording consent status: %w", err)
	}
	if !statuses[c.PatientID] || !statuses[c.DoctorID] {
		return nil
	}

	egressID, err := s.video.StartRecording(ctx, RecordingSpec{
		RoomName:  c.RoomName,
		Bucket:    s.opts.RecordingBucket,
		OutputKey: c.AppointmentID.String() + ".mp4",
	})
	if errors.Is(err, ErrRecordingUnsupported) {
		// Both parties consented and this deployment has no server-side
		// recorder. RecordingStatus deliberately stays RecordingNone: it is
		// what wasRecorded() reads, and claiming otherwise would put
		// recorded=true on consultation.ended and tell record-service a
		// recording exists that nothing will ever produce.
		//
		// The consent is not wasted. It is what authorises the doctor's
		// client-side MediaRecorder, whose output lives and dies with that
		// browser tab -- a materially weaker guarantee than an SFU writing to
		// object storage, and the direct cost of not running one.
		s.log.Warn().Str("consultation_id", c.ID.String()).
			Msg("both parties consented to recording; this provider has no server-side media path")
		return database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			return s.store.RecordEvent(ctx, tx, Event{
				ConsultationID: c.ID, Type: EventRecordingUnavailable,
				Metadata:   map[string]any{"provider": s.video.Name()},
				OccurredAt: time.Now().UTC(),
			})
		})
	}
	if err != nil {
		return fmt.Errorf("consultation: start recording: %w", err)
	}

	c.RecordingStatus = RecordingInProgress
	c.EgressID = &egressID
	return database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.store.UpdateConsultation(ctx, tx, c); err != nil {
			return err
		}
		return s.store.RecordEvent(ctx, tx, Event{
			ConsultationID: c.ID, Type: EventRecordingStarted,
			Metadata: map[string]any{"egress_id": egressID}, OccurredAt: time.Now().UTC(),
		})
	})
}

// --- read paths --------------------------------------------------------

// Get returns the consultation and its participants.
//
// # Where the admin line is drawn (F4)
//
// The two parties read everything about their own consultation. The five
// platform admin roles read the ENVELOPE -- who, when, how long, how it ended,
// whether a recording exists -- and never the recording itself.
//
// That is the distinction the whole finding turns on. An administrator has real
// work here: reconciling a double-booking, deciding a refund dispute over a call
// that dropped at ninety seconds, telling a doctor's no-show from a patient's.
// Every one of those questions is answered by timestamps, statuses and
// durations. None of them is answered by watching the consultation.
//
// recording_url is the audiovisual record of a medical consultation. It is the
// most sensitive object this service can hand out -- the patient describing
// their symptoms aloud, the doctor's examination, the diagnosis -- and both
// parties had to consent, separately and explicitly, before it was ever made
// (see maybeStartRecording). A support-tier admin token consenting on their
// behalf afterwards is not a thing that should be possible, and the consent
// record makes that argument for us: the patient agreed to be recorded FOR THE
// CONSULTATION, not for later viewing by whoever holds a staff token.
//
// This route is auth: "authenticated" at the gateway with no role gate, so it
// carries no IP allowlist, no admin-origin check and no hash-chained audit --
// which is what makes the read the review classified as T1, reachable from any
// IP on the internet with any of the five admin roles.
//
// The redaction happens HERE, in the service, and not in the handler, so that a
// second front-end -- gRPC, a CLI, an export job -- cannot reintroduce it by
// forgetting. What a caller may see is a business rule.
//
// A staff member who genuinely needs the recording (a clinical-negligence
// investigation, a regulator's request) goes through admin-service, which has
// the IP allowlist and the append-only audit log that such a request should
// leave a trace in.
func (s *Service) Get(ctx context.Context, principal middleware.Principal, consultationID uuid.UUID) (*Consultation, []Participant, error) {
	c, err := s.store.GetConsultation(ctx, s.pool, consultationID)
	if err != nil {
		return nil, nil, err
	}
	_, isParty := authorizeParty(principal, c)
	if !isParty && !principal.IsAdmin() {
		return nil, nil, ErrForbidden
	}
	participants, err := s.store.ListParticipants(ctx, s.pool, consultationID)
	if err != nil {
		return nil, nil, err
	}
	if !isParty {
		// Copy rather than mutate: the store hands back a fresh row today, but
		// a cache in front of it tomorrow would make an in-place nil a
		// redaction that leaks into the next caller's read. recording_status
		// stays, so an admin can still see THAT a recording exists and ask for
		// it through the audited route.
		redacted := *c
		redacted.RecordingURL = nil
		redacted.EgressID = nil
		return &redacted, participants, nil
	}
	return c, participants, nil
}

// WaitingRoomStatus returns the caller's position in their doctor's queue and
// an estimated wait derived from that doctor's recent completed consultations.
func (s *Service) WaitingRoomStatus(ctx context.Context, principal middleware.Principal, consultationID uuid.UUID) (WaitingRoomStatusResult, error) {
	c, err := s.store.GetConsultation(ctx, s.pool, consultationID)
	if err != nil {
		return WaitingRoomStatusResult{}, err
	}
	if _, ok := authorizeParty(principal, c); !ok {
		return WaitingRoomStatusResult{}, ErrForbidden
	}

	entry, err := s.store.GetWaitingRoomEntry(ctx, s.pool, consultationID)
	switch {
	case errors.Is(err, ErrNotFound):
		return WaitingRoomStatusResult{Waiting: false}, nil
	case err != nil:
		return WaitingRoomStatusResult{}, err
	case entry.Status != WaitingStatusWaiting:
		return WaitingRoomStatusResult{Waiting: false}, nil
	}

	ahead, err := s.queuePosition(ctx, c.DoctorID, entry)
	if err != nil {
		return WaitingRoomStatusResult{}, err
	}
	avgSeconds := s.averageDurationSeconds(ctx, c.DoctorID)

	return WaitingRoomStatusResult{
		Waiting:              true,
		Position:             int(ahead) + 1,
		PatientsAhead:        int(ahead),
		EstimatedWaitSeconds: int(ahead) * avgSeconds,
	}, nil
}

// queuePosition prefers the Redis sorted set (O(log n), the fast path every
// waiting-room poll hits) and falls back to the Postgres mirror -- the ADR-007
// pattern applied to the waiting room instead of slot booking -- whenever
// Redis does not have the member, most notably right after a flush.
func (s *Service) queuePosition(ctx context.Context, doctorID uuid.UUID, entry *WaitingRoomEntry) (int64, error) {
	key := waitingRoomKey(doctorID)
	rank, err := s.cache.ZRank(ctx, key, entry.ConsultationID.String())
	if err == nil {
		return rank, nil
	}
	if !errors.Is(err, cache.ErrNotFound) {
		s.log.Warn().Err(err).Msg("waiting room redis rank failed, falling back to postgres")
	}
	return s.store.CountWaitingAhead(ctx, s.pool, doctorID, entry.EnteredAt)
}

func (s *Service) averageDurationSeconds(ctx context.Context, doctorID uuid.UUID) int {
	avg, ok, err := s.store.AverageDurationSeconds(ctx, s.pool, doctorID, s.opts.WaitingRoomAvgSampleSize)
	if err != nil {
		s.log.Warn().Err(err).Msg("average duration lookup failed, using configured default")
		return int(s.opts.DefaultConsultationDuration.Seconds())
	}
	if !ok || avg <= 0 {
		return int(s.opts.DefaultConsultationDuration.Seconds())
	}
	return int(avg)
}

// --- connection quality --------------------------------------------------

// QualityInput is one client-reported connection-quality sample.
type QualityInput struct {
	Quality       Quality
	PacketLossPct *float64
	BitrateKbps   *int
}

// ReportQuality records a sample on the append-only timeline and reports
// should_downgrade_video once the same participant has reported
// QualityDegradeThreshold consecutive poor/lost samples.
func (s *Service) ReportQuality(ctx context.Context, principal middleware.Principal, consultationID uuid.UUID, in QualityInput) (QualityReportResult, error) {
	if !in.Quality.valid() {
		return QualityReportResult{}, fmt.Errorf("%w: unknown quality value", ErrInvalidState)
	}
	c, err := s.store.GetConsultation(ctx, s.pool, consultationID)
	if err != nil {
		return QualityReportResult{}, err
	}
	role, ok := authorizeParty(principal, c)
	if !ok {
		return QualityReportResult{}, ErrForbidden
	}

	identity := partyIdentity(role, c).String()
	now := time.Now().UTC()
	meta := map[string]any{"quality": string(in.Quality)}
	if in.PacketLossPct != nil {
		meta["packet_loss_pct"] = *in.PacketLossPct
	}
	if in.BitrateKbps != nil {
		meta["bitrate_kbps"] = *in.BitrateKbps
	}

	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return s.store.RecordEvent(ctx, tx, Event{
			ConsultationID: c.ID, Type: EventQualitySample, ActorIdentity: identity, Metadata: meta, OccurredAt: now,
		})
	})
	if err != nil {
		return QualityReportResult{}, err
	}

	shouldDowngrade := false
	recent, err := s.store.RecentQualityEvents(ctx, s.pool, c.ID, identity, s.opts.QualityDegradeThreshold)
	if err != nil {
		s.log.Warn().Err(err).Msg("recent quality lookup failed")
	} else if len(recent) == s.opts.QualityDegradeThreshold && allPoor(recent) {
		shouldDowngrade = true
		degradeErr := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			return s.store.RecordEvent(ctx, tx, Event{
				ConsultationID: c.ID, Type: EventQualityDegraded, ActorIdentity: identity,
				Metadata: map[string]any{"consecutive_poor": s.opts.QualityDegradeThreshold}, OccurredAt: now,
			})
		})
		if degradeErr != nil {
			s.log.Warn().Err(degradeErr).Msg("failed to record quality_degraded event")
		}
	}

	return QualityReportResult{ShouldDowngradeVideo: shouldDowngrade}, nil
}

func allPoor(evts []Event) bool {
	for _, e := range evts {
		q, _ := e.Metadata["quality"].(string)
		if q != string(QualityPoor) && q != string(QualityLost) {
			return false
		}
	}
	return true
}

// --- webhook ---------------------------------------------------------------

// HandleWebhook verifies, dedupes, and dispatches one LiveKit webhook
// delivery. A duplicate delivery (LiveKit retries until it sees 2xx) is a
// silent no-op after the first successful claim, which is what makes retries
// safe.
func (s *Service) HandleWebhook(ctx context.Context, authHeader string, body []byte) error {
	evt, err := s.video.VerifyWebhook(ctx, authHeader, body)
	if err != nil {
		return err
	}

	var claimed bool
	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		claimed, err = s.store.ClaimWebhookEvent(ctx, tx, evt.ID, evt.Type)
		return err
	})
	if err != nil {
		return err
	}
	if !claimed {
		s.log.Debug().Str("event_id", evt.ID).Str("type", evt.Type).Msg("duplicate webhook, already processed")
		return nil
	}

	switch evt.Type {
	case WebhookRoomFinished:
		return s.handleRoomFinished(ctx, evt)
	case WebhookEgressEnded:
		return s.handleEgressEnded(ctx, evt)
	case WebhookParticipantJoined:
		return s.handleParticipantJoined(ctx, evt)
	case WebhookParticipantLeft:
		return s.handleParticipantLeft(ctx, evt)
	default:
		// room_started, egress_started/updated, track_*, ingress_* -- these
		// are informational for now and deliberately unhandled rather than
		// stubbed: there is no consultation-domain action for them yet.
		return nil
	}
}

func (s *Service) handleRoomFinished(ctx context.Context, evt WebhookEvent) error {
	c, err := s.store.GetConsultationByRoomName(ctx, s.pool, evt.RoomName)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.endInternal(ctx, c, "room_finished", time.Time{})
	return err
}

func (s *Service) handleEgressEnded(ctx context.Context, evt WebhookEvent) error {
	c, err := s.store.GetConsultationByRoomName(ctx, s.pool, evt.RoomName)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	eventType := EventRecordingFailed
	if evt.EgressStatus == EgressStatusComplete {
		c.RecordingStatus = RecordingCompleted
		if evt.RecordingURL != "" {
			url := evt.RecordingURL
			c.RecordingURL = &url
		}
		eventType = EventRecordingCompleted
	} else {
		c.RecordingStatus = RecordingFailed
	}

	return database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.store.UpdateConsultation(ctx, tx, c); err != nil {
			return err
		}
		return s.store.RecordEvent(ctx, tx, Event{
			ConsultationID: c.ID, Type: eventType,
			Metadata: map[string]any{"egress_id": evt.EgressID, "status": evt.EgressStatus}, OccurredAt: now,
		})
	})
}

// handleParticipantJoined/Left are a defensive dual-write: our own /join and
// /end calls are the primary source, but a client that is killed outright
// (a phone locked and OS-suspended mid-call, common on Sri Lankan 3G) never
// calls any API. LiveKit's own view of the room is the only way left_at ever
// gets set for that case.
func (s *Service) handleParticipantJoined(ctx context.Context, evt WebhookEvent) error {
	c, err := s.store.GetConsultationByRoomName(ctx, s.pool, evt.RoomName)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	role := RolePatient
	if evt.ParticipantIdentity == c.DoctorID.String() {
		role = RoleDoctor
	}
	return database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return s.store.UpsertParticipantJoin(ctx, tx, c.ID, evt.ParticipantIdentity, role, evt.OccurredAt)
	})
}

func (s *Service) handleParticipantLeft(ctx context.Context, evt WebhookEvent) error {
	c, err := s.store.GetConsultationByRoomName(ctx, s.pool, evt.RoomName)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.store.MarkParticipantLeft(ctx, tx, c.ID, evt.ParticipantIdentity, evt.OccurredAt); err != nil {
			return err
		}
		return s.store.RecordEvent(ctx, tx, Event{
			ConsultationID: c.ID, Type: EventLeft, ActorIdentity: evt.ParticipantIdentity, OccurredAt: evt.OccurredAt,
		})
	})
}

// --- event-driven lifecycle (consumer.go) -----------------------------

// CreateFromAppointment pre-creates the consultation row so the room name is
// stable and known before either party ever calls Join. It is idempotent on
// the UNIQUE(appointment_id) constraint: a redelivered event is a silent
// success, not a second row.
//
// The parameter is events.AppointmentConfirmed -- the canonical platform type,
// shared with scheduling-service, which publishes it. This service used to
// declare its own private copy with a `scheduled_at` field that the producer
// never sent; encoding/json filled it with the zero time and every
// consultation row was created with ScheduledAt = 0001-01-01. Sharing the type
// makes that class of mistake a compile error.
func (s *Service) CreateFromAppointment(ctx context.Context, in events.AppointmentConfirmed) error {
	endAt := in.EndAt
	if endAt.IsZero() || !endAt.After(in.StartAt) {
		d := s.opts.DefaultConsultationDuration
		if d <= 0 {
			d = 15 * time.Minute
		}
		endAt = in.StartAt.Add(d)
	}
	c := &Consultation{
		AppointmentID:  in.AppointmentID,
		PatientID:      in.PatientID,
		DoctorID:       in.DoctorID,
		RoomName:       roomNameFor(in.AppointmentID),
		Status:         StatusScheduled,
		ScheduledAt:    in.StartAt,
		ScheduledEndAt: endAt,
	}
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return s.store.CreateConsultation(ctx, tx, c)
	})
	if errors.Is(err, ErrDuplicateConsultation) {
		return nil
	}
	return err
}

// RescheduleFromAppointment moves consultations.scheduled_at / scheduled_end_at
// to the new visit window. Join URLs stay valid (same appointment id); only the
// clock on the row changes, and only while the consultation has not started.
func (s *Service) RescheduleFromAppointment(ctx context.Context, in events.AppointmentRescheduled) error {
	endAt := in.ProposedEnd
	if endAt.IsZero() || !endAt.After(in.ProposedStart) {
		d := s.opts.DefaultConsultationDuration
		if d <= 0 {
			d = 15 * time.Minute
		}
		endAt = in.ProposedStart.Add(d)
	}
	return database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return s.store.UpdateConsultationScheduledAt(ctx, tx, in.AppointmentID, in.ProposedStart, endAt)
	})
}

// TeardownForCancellation handles events.SubjectAppointmentCancelled. A
// consultation that has not started yet is abandoned and soft-deleted; one
// already active is left alone (a cancellation racing a live call is a
// business exception, not something to silently tear down mid-conversation)
// and only logged to the timeline for a human to look at.
func (s *Service) TeardownForCancellation(ctx context.Context, appointmentID uuid.UUID) error {
	c, err := s.store.GetConsultationByAppointment(ctx, s.pool, appointmentID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	if c.Status == StatusActive {
		return database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			return s.store.RecordEvent(ctx, tx, Event{
				ConsultationID: c.ID, Type: EventCancellationIgnoredActive, OccurredAt: now,
			})
		})
	}
	if c.Status.terminal() {
		return nil
	}

	c.Status = StatusAbandoned
	c.DeletedAt = &now
	reason := "appointment_cancelled"
	c.EndReason = &reason

	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.store.UpdateConsultation(ctx, tx, c); err != nil {
			return err
		}
		if err := s.store.MarkWaitingRoomLeft(ctx, tx, c.ID, now, WaitingStatusExpired); err != nil {
			return err
		}
		return s.store.RecordEvent(ctx, tx, Event{
			ConsultationID: c.ID, Type: EventCancelled, Metadata: map[string]any{"reason": reason}, OccurredAt: now,
		})
	})
	if err != nil {
		return err
	}

	if err := s.video.EndRoom(ctx, c.RoomName); err != nil && !errors.Is(err, ErrRoomNotFound) {
		s.log.Warn().Err(err).Str("room", c.RoomName).Msg("end room during cancellation teardown failed")
	}
	key := waitingRoomKey(c.DoctorID)
	if err := s.cache.ZRem(ctx, key, c.ID.String()); err != nil {
		s.log.Warn().Err(err).Msg("waiting room redis cleanup failed on cancellation")
	}
	return nil
}

// --- outbox payload projection -----------------------------------------
//
// The wire types themselves live in internal/platform/events/payloads.go and
// are shared with every consumer. What remains here is only the mapping from
// this service's internal Consultation state onto them.

// durationSeconds flattens the nullable internal field. A consultation that
// never started has no duration, and 0 is the honest answer for it: the
// canonical payload uses a plain int because "unset" and "zero seconds" mean
// the same thing to every consumer, and an omitted field would decode to 0
// anyway.
func durationSeconds(c *Consultation) int {
	if c.DurationSeconds == nil {
		return 0
	}
	return *c.DurationSeconds
}

// endReasonFor maps the terminal status onto the canonical vocabulary
// (completed | abandoned | failed | no_show). This is the field scheduling
// uses to choose between appointment.completed and appointment.no_show, so it
// must be one of those tokens and not the operator's free-text reason -- that
// stays on the consultation's own event timeline, which is queryable over the
// API and does not belong in a fan-out event.
func endReasonFor(c *Consultation) string {
	if c.Status == StatusAbandoned {
		return "abandoned"
	}
	return "completed"
}

// wasRecorded reports whether an egress actually captured anything. Pending
// and failed both mean "no recording exists to go looking for", which is what
// a consumer of this flag is really asking.
func wasRecorded(c *Consultation) bool {
	switch c.RecordingStatus {
	case RecordingInProgress, RecordingProcessing, RecordingCompleted:
		return true
	default:
		return false
	}
}

// --- signalling hooks -------------------------------------------------------
//
// These two exist because dropping the SFU dropped its webhooks with it. Under
// LiveKit, participant_joined/participant_left and room_finished arrived as
// signed HTTP callbacks; with a peer-to-peer provider there is no external
// system to send them, so the hub calls in directly.
//
// Both are best-effort and log rather than return: they are invoked from the
// signalling hub, on a path that is servicing a live websocket, and a database
// hiccup must not take a call down. Everything they write is bookkeeping the
// stale sweeper can reconstruct more crudely.

// NotePeerChange records a participant arriving at or leaving a room.
//
// Without it consultation_participants.left_at is never written by anything
// and every ended consultation reports all participants still joined, forever.
func (s *Service) NotePeerChange(ctx context.Context, roomName, identity string, joined bool) {
	c, err := s.store.GetConsultationByRoomName(ctx, s.pool, roomName)
	if errors.Is(err, ErrNotFound) {
		// A signalling room with no consultation behind it: the developer test
		// surface creates those deliberately.
		return
	}
	if err != nil {
		s.log.Warn().Err(err).Str("room", roomName).Msg("consultation: peer change lookup failed")
		return
	}

	now := time.Now().UTC()
	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if !joined {
			return s.store.MarkParticipantLeft(ctx, tx, c.ID, identity, now)
		}
		role := RolePatient
		if identity == c.DoctorID.String() {
			role = RoleDoctor
		}
		return s.store.UpsertParticipantJoin(ctx, tx, c.ID, identity, role, now)
	})
	if err != nil {
		s.log.Warn().Err(err).Str("consultation_id", c.ID.String()).
			Bool("joined", joined).Msg("consultation: could not record peer change")
	}
}

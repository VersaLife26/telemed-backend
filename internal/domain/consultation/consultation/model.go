// Package consultation owns the video consultation layer over LiveKit: room
// lifecycle, short-lived access tokens, the waiting room queue, consent-gated
// recording, and connection-quality tracking for Sri Lanka's 3G reality.
//
// Business logic in this package never imports the LiveKit SDK directly -- it
// depends only on the VideoProvider interface defined in port.go. In 2040
// LiveKit may not exist; Pion is the documented fallback, and swapping it in
// means writing one new file, not touching this one.
package consultation

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Status is the consultation's position in its lifecycle. Transitions are
// enforced in service.go, not by the database.
type Status string

const (
	StatusScheduled Status = "scheduled"
	StatusWaiting   Status = "waiting"
	StatusActive    Status = "active"
	StatusEnded     Status = "ended"
	StatusAbandoned Status = "abandoned"
	StatusFailed    Status = "failed"
)

// terminal reports whether no further transition is permitted.
func (s Status) terminal() bool {
	return s == StatusEnded || s == StatusAbandoned || s == StatusFailed
}

// RecordingStatus tracks the egress lifecycle independently of the
// consultation's own status, since a recording can still be processing after
// the call itself has ended.
type RecordingStatus string

const (
	RecordingNone       RecordingStatus = "none"
	RecordingPending    RecordingStatus = "pending"
	RecordingInProgress RecordingStatus = "recording"
	RecordingProcessing RecordingStatus = "processing"
	RecordingCompleted  RecordingStatus = "completed"
	RecordingFailed     RecordingStatus = "failed"
)

// ParticipantRole distinguishes the two legitimate parties on a call. The
// platform never issues a consultation token to anyone else.
type ParticipantRole string

const (
	RolePatient ParticipantRole = "patient"
	RoleDoctor  ParticipantRole = "doctor"
)

// ConsentType is one of the two consents this service tracks. "telemedicine"
// is the general consent-to-treat-remotely; "recording" specifically gates
// StartRecording and must be granted by both parties before egress starts.
type ConsentType string

const (
	ConsentRecording    ConsentType = "recording"
	ConsentTelemedicine ConsentType = "telemedicine"
)

// Quality mirrors the coarse signal LiveKit clients already compute
// client-side (ConnectionQuality) so the API vocabulary matches what a
// TypeScript web client already has on hand.
type Quality string

const (
	QualityExcellent Quality = "excellent"
	QualityGood      Quality = "good"
	QualityPoor      Quality = "poor"
	QualityLost      Quality = "lost"
)

func (q Quality) valid() bool {
	switch q {
	case QualityExcellent, QualityGood, QualityPoor, QualityLost:
		return true
	}
	return false
}

// Event types recorded on the append-only consultation_events timeline.
const (
	EventJoined             = "joined"
	EventLeft               = "left"
	EventAdmitted           = "admitted"
	EventQualitySample      = "quality_sample"
	EventQualityDegraded    = "quality_degraded"
	EventConsentRecorded    = "consent_recorded"
	EventRecordingStarted   = "recording_started"
	EventRecordingCompleted = "recording_completed"
	EventRecordingFailed    = "recording_failed"
	// EventRecordingUnavailable records that both parties consented and the
	// configured provider has no server-side recorder. Distinct from
	// recording_failed, which means one was attempted and did not work: this
	// is a capability the deployment does not have, and a clinician reading
	// the timeline is entitled to see which of the two happened.
	EventRecordingUnavailable      = "recording_unavailable"
	EventEnded                     = "ended"
	EventAbandoned                 = "abandoned"
	EventCancellationIgnoredActive = "cancellation_ignored_active"
	EventCancelled                 = "cancelled"
)

// Consultation is the aggregate root: one row per appointment, one LiveKit
// room, one recording (if consented), one timeline.
type Consultation struct {
	ID            uuid.UUID
	AppointmentID uuid.UUID
	PatientID     uuid.UUID
	DoctorID      uuid.UUID
	RoomName      string
	Status        Status
	ScheduledAt   time.Time
	// ScheduledEndAt is the booked slot end. Used to detect a live consult
	// that has run past its allotted time so the next patient can be told.
	ScheduledEndAt  time.Time
	StartedAt       *time.Time
	EndedAt         *time.Time
	DurationSeconds *int
	RecordingURL    *string
	RecordingStatus RecordingStatus
	EgressID        *string
	EndReason       *string
	// RunningLateNotifiedAt is set once the next patient has been told this
	// consult is running late. Null means not yet notified (or not overdue).
	RunningLateNotifiedAt *time.Time
	// EarlyJoinOfferedAt is set when the previous visit finished early and
	// this next patient was asked whether they can join now. Response is
	// accepted | declined; a null response is still pending. Neither field
	// rewrites scheduled_at.
	EarlyJoinOfferedAt   *time.Time
	EarlyJoinResponse    *string
	EarlyJoinRespondedAt *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
	DeletedAt            *time.Time
	Version              int
}

// Participant is one side of the call, joined at least once.
type Participant struct {
	ID             uuid.UUID
	ConsultationID uuid.UUID
	Identity       string
	Role           ParticipantRole
	JoinedAt       *time.Time
	LeftAt         *time.Time
	ReconnectCount int
}

// Consent is one append-only consent decision. The current effective consent
// for a (user, type) pair is the row with the latest GrantedAt.
type Consent struct {
	ID             uuid.UUID
	ConsultationID uuid.UUID
	UserID         uuid.UUID
	Type           ConsentType
	Granted        bool
	GrantedAt      time.Time
	IPAddress      string
	UserAgent      string
}

// Event is one entry on the append-only support/dispute timeline.
type Event struct {
	ID             int64
	ConsultationID uuid.UUID
	Type           string
	ActorIdentity  string
	Metadata       map[string]any
	OccurredAt     time.Time
}

// WaitingRoomStatus is one patient's mirrored entry in a doctor's queue.
type WaitingRoomStatus string

const (
	WaitingStatusWaiting  WaitingRoomStatus = "waiting"
	WaitingStatusAdmitted WaitingRoomStatus = "admitted"
	WaitingStatusLeft     WaitingRoomStatus = "left"
	WaitingStatusExpired  WaitingRoomStatus = "expired"
)

// WaitingRoomEntry is the durable Postgres mirror of one slot in the Redis
// sorted set keyed by doctor. If Redis is flushed, this row is how a patient
// does not silently lose their place in the queue.
type WaitingRoomEntry struct {
	ID             uuid.UUID
	ConsultationID uuid.UUID
	DoctorID       uuid.UUID
	PatientID      uuid.UUID
	EnteredAt      time.Time
	Status         WaitingRoomStatus
	AdmittedAt     *time.Time
	LeftAt         *time.Time
}

// ICEServer is a STUN/TURN hint returned to the client alongside the LiveKit
// token. LiveKit's own signaling negotiates ICE candidates for the media
// connection itself; these are additional hints for clients running their own
// coturn fallback path on constrained 3G links (see docs/DESIGN.md).
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// JoinResult is the response to POST /consultations/{appointment_id}/join.
//
// ConsultationID is load-bearing, not decoration. Join, ready-for-next and
// early-join are keyed by appointment id; /admit, /end, /consent,
// /waiting-room and /quality are keyed by the consultation's own id. A client
// arrives holding an appointment id (that is what scheduling gives it) and,
// until this field existed, left the join call still holding only an
// appointment id -- so it could not call any of them. Recording consent in
// particular could not be given through the API at all, and consent is what
// gates egress: the platform could offer a recording feature no patient was
// able to authorise.
//
// The remaining fields exist so a client can act without a second round trip:
// Status tells a patient whether they are in the waiting room or already live,
// Role tells a caller which side of the call it is on (a doctor renders an
// admit control, a patient renders a queue position), and TokenExpiresAt is
// when this LiveKit token stops working -- a client on a 3G link that
// reconnects after the TTL needs to know to re-join rather than retry a dead
// token forever.
type JoinResult struct {
	ConsultationID uuid.UUID `json:"consultation_id"`
	AppointmentID  uuid.UUID `json:"appointment_id"`
	Status         Status    `json:"status"`
	Role           string    `json:"role"` // patient | doctor
	ScheduledAt    time.Time `json:"scheduled_at"`

	Token          string    `json:"token"`
	TokenExpiresAt time.Time `json:"token_expires_at"`
	RoomName       string    `json:"room_name"`

	// Provider is VideoProvider.Name(): "inhouse" | "livekit" | "mock". The
	// client branches on this, NOT on which URL happens to be non-empty.
	Provider string `json:"provider"`

	// LiveKitURL is empty for every non-LiveKit provider.
	//
	// It is deliberately not reused to carry the signalling address. A client
	// that read livekit_url and handed it to livekit-client would open a
	// socket speaking an entirely different protocol and hang rather than
	// fail, and every log line and symbol named "livekit" would become a lie
	// while LiveKit is still the supported rollback.
	LiveKitURL string `json:"livekit_url,omitempty"`

	// SignalURL is the platform's own signalling websocket, absolute. The
	// client appends ?token=<Token>.
	SignalURL string `json:"signal_url,omitempty"`

	// RecordingMode tells the client where a recording would actually live:
	// "server" (an SFU writes to object storage), "client" (the browser
	// records locally and it dies with the tab), or "none".
	//
	// The consent dialog's wording depends on it. A patient consenting to a
	// recording is entitled to know whether it is being kept by the platform
	// or by the doctor's laptop.
	RecordingMode string `json:"recording_mode"`

	ICEServers []ICEServer `json:"ice_servers"`
}

// Recording modes reported by JoinResult.
const (
	RecordingModeServer = "server"
	RecordingModeClient = "client"
	RecordingModeNone   = "none"
)

// recordingModeFor maps a provider onto where its recordings live.
func recordingModeFor(provider string) string {
	switch provider {
	case "livekit":
		return RecordingModeServer
	case "inhouse":
		// peer.ts runs a MediaRecorder in the doctor's tab. That is a real
		// recording and a materially weaker guarantee than an SFU writing to
		// object storage -- a crash or a closed laptop loses it.
		return RecordingModeClient
	default:
		return RecordingModeNone
	}
}

// WaitingRoomStatusResult is the response to GET /consultations/{id}/waiting-room.
type WaitingRoomStatusResult struct {
	Waiting              bool `json:"waiting"`
	Position             int  `json:"position,omitempty"`
	PatientsAhead        int  `json:"patients_ahead,omitempty"`
	EstimatedWaitSeconds int  `json:"estimated_wait_seconds,omitempty"`
}

// QualityReportResult is the response to POST /consultations/{id}/quality.
type QualityReportResult struct {
	ShouldDowngradeVideo bool `json:"should_downgrade_video"`
}

// ChatMessage represents a single message sent during a consultation.
type ChatMessage struct {
	ID             uuid.UUID       `json:"id"`
	ConsultationID uuid.UUID       `json:"consultation_id"`
	SenderID       uuid.UUID       `json:"sender_id"`
	SenderRole     ParticipantRole `json:"sender_role"`
	SenderName     string          `json:"sender_name"`
	Content        string          `json:"content"`
	Metadata       json.RawMessage `json:"metadata"`
	CreatedAt      time.Time       `json:"created_at"`
}

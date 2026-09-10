package consultation

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors returned by VideoProvider implementations. Callers use
// errors.Is against these, never string matching, so LiveKitProvider and
// MockProvider can be told apart from a genuine downstream failure.
var (
	// ErrRoomNotFound is returned by EndRoom/ListParticipants/RemoveParticipant
	// when the room does not exist on the provider side, most often because a
	// LiveKit empty-room timeout already tore it down. Callers treat this as
	// "already gone", not a failure.
	ErrRoomNotFound = errors.New("consultation: room not found on video provider")

	// ErrWebhookUnverified is returned by VerifyWebhook when the signature,
	// checksum, or auth header does not check out.
	ErrWebhookUnverified = errors.New("consultation: webhook could not be verified")

	// ErrRecordingUnsupported is returned by StartRecording/StopRecording by a
	// provider with no server-side media path.
	//
	// It is NOT a failure. It is the provider stating a capability it does not
	// have, so callers can degrade deliberately -- and visibly -- instead of
	// logging an error every time both parties consent to a recording that was
	// never going to happen. A peer-to-peer call has no server in the media
	// path to record from; that is the trade made by not running an SFU.
	ErrRecordingUnsupported = errors.New("consultation: video provider cannot record server-side")
)

// RoomSpec describes the room CreateRoom should ensure exists. CreateRoom is
// idempotent: calling it against a name that already exists returns the
// existing room rather than erroring, which is what lets Join call it lazily
// on every request instead of requiring a separate provisioning job.
type RoomSpec struct {
	RoomName string
	// EmptyTimeoutSeconds is how long the room stays open with nobody in it
	// before the provider tears it down on its own.
	EmptyTimeoutSeconds uint32
	MaxParticipants     uint32
	Metadata            string
}

// Room is what CreateRoom returns once the provider has a room by that name.
type Room struct {
	Name      string
	SID       string
	CreatedAt time.Time
}

// TokenSpec describes the access token GenerateToken should mint. Tokens are
// short-lived and room-scoped by design (see docs/DESIGN.md): identity is
// always the platform user id, never a display name, so a token can never be
// replayed against a room it was not issued for.
type TokenSpec struct {
	RoomName     string
	Identity     string
	DisplayName  string
	CanPublish   bool
	CanSubscribe bool
	TTL          time.Duration
}

// ProviderParticipant is one entry in ListParticipants' result -- the
// provider's live view of who is actually connected right now, as opposed to
// the domain Participant type (model.go), which is our own joined_at/left_at
// bookkeeping derived from API calls and webhook deliveries.
type ProviderParticipant struct {
	Identity        string
	JoinedAt        time.Time
	IsPublisher     bool
	ConnectionState string
}

// RecordingSpec describes one StartRecording call. OutputKey is the object
// key inside Bucket, e.g. "{appointment_id}.mp4" -- the caller decides the
// naming convention, this port only carries it through.
type RecordingSpec struct {
	RoomName  string
	Bucket    string
	OutputKey string
}

// WebhookEvent is the provider-neutral shape VerifyWebhook returns. Handlers
// switch on Type using the constants below; RecordingURL and EgressStatus are
// only populated for egress_* events, ParticipantIdentity only for
// participant_*/track_* events.
//
// JSON tags matter here even though LiveKitProvider never unmarshals this
// struct (it builds one field by field from the protobuf WebhookEvent):
// MockProvider does unmarshal it directly, since its "webhook sender" -- tests
// and the local run script -- posts plain snake_case JSON. Without tags,
// encoding/json's case-insensitive fallback does not bridge "room_name" to
// RoomName (it ignores case, not underscores), and the field silently stays
// empty.
type WebhookEvent struct {
	ID                  string    `json:"id"`
	Type                string    `json:"type"`
	RoomName            string    `json:"room_name"`
	ParticipantIdentity string    `json:"participant_identity"`
	EgressID            string    `json:"egress_id"`
	EgressStatus        string    `json:"egress_status"`
	RecordingURL        string    `json:"recording_url"`
	OccurredAt          time.Time `json:"occurred_at"`
}

// Webhook event type constants. These match LiveKit's own event names so a
// RUNBOOK.md written against LiveKit's webhook docs stays directly usable;
// MockProvider reuses the same constants so tests do not need a second
// vocabulary.
const (
	WebhookRoomStarted       = "room_started"
	WebhookRoomFinished      = "room_finished"
	WebhookParticipantJoined = "participant_joined"
	WebhookParticipantLeft   = "participant_left"
	WebhookEgressStarted     = "egress_started"
	WebhookEgressUpdated     = "egress_updated"
	WebhookEgressEnded       = "egress_ended"

	EgressStatusComplete = "EGRESS_COMPLETE"
	EgressStatusFailed   = "EGRESS_FAILED"
	EgressStatusActive   = "EGRESS_ACTIVE"
)

// VideoProvider is the seam between this service's business logic and
// whichever SFU actually runs the call. Business logic (service.go) imports
// only this interface, never a provider SDK -- that is what makes swapping
// LiveKit for something else in 2040 a one-file change instead of a rewrite.
type VideoProvider interface {
	CreateRoom(ctx context.Context, spec RoomSpec) (Room, error)
	GenerateToken(ctx context.Context, spec TokenSpec) (string, error)
	EndRoom(ctx context.Context, roomName string) error
	ListParticipants(ctx context.Context, roomName string) ([]ProviderParticipant, error)
	RemoveParticipant(ctx context.Context, roomName, identity string) error
	StartRecording(ctx context.Context, spec RecordingSpec) (egressID string, err error)
	StopRecording(ctx context.Context, egressID string) error
	VerifyWebhook(ctx context.Context, authHeader string, body []byte) (WebhookEvent, error)
	Name() string
}

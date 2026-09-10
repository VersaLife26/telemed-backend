package consultation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"telemed/internal/domain/consultation/signal"
)

// InHouseProvider implements VideoProvider over the platform's own signalling
// stack: two browsers, one RTCPeerConnection, no server in the media path.
//
// WHAT IT DOES NOT REUSE, AND WHY
// MockProvider is also an in-process VideoProvider, and it is tempting to
// build on. It keeps a room map with participants derived from GenerateToken
// calls -- per-process fiction. Across two replicas that makes
// ListParticipants lie, and ListParticipants is the single thing standing
// between the stale sweeper and ending live consultations. So this type reads
// occupancy from Redis instead, and MockProvider stays what it is: the test
// double.
//
// WHERE THE INTERFACE DOES NOT FIT
// VideoProvider was shaped around an SFU. Three of its nine methods have no
// honest peer-to-peer meaning, and each is handled explicitly below rather
// than faked: CreateRoom (a room has no server-side existence until a socket
// arrives), StartRecording/StopRecording (nothing to record from), and
// VerifyWebhook (nobody to call back).
type InHouseProvider struct {
	// hub is this process's view. Used only for the single-process fallback
	// and for the local half of EndRoom/RemoveParticipant -- everything it is
	// asked for, the bus can also answer. Keeping it that way is what allows
	// the signalling stack to move to its own process later without rewriting
	// this file.
	hub *signal.Hub
	// bus is authoritative for occupancy when configured.
	bus *signal.Bus

	secret   []byte
	tokenTTL time.Duration
	// signalURL is the absolute ws(s):// address a browser connects to.
	signalURL string
	log       zerolog.Logger
}

// InHouseOptions configures the provider.
type InHouseOptions struct {
	Hub       *signal.Hub
	Bus       *signal.Bus
	Secret    []byte
	TokenTTL  time.Duration
	SignalURL string
	Log       zerolog.Logger
}

// DefaultRoomTokenTTL is how long a signalling room token stays valid.
//
// Deliberately hours, not the five minutes an SFU access token gets, and the
// difference is not cosmetic. A LiveKit token is spent ONCE, at
// room.connect(); the media session then outlives it. A signal room token is
// re-presented on EVERY websocket reconnect, and a mobile client reconnects
// whenever it changes cell. A five-minute token means a patient who loses
// signal at minute twelve gets a 401 on the upgrade and cannot rejoin without
// a whole new Join call.
const DefaultRoomTokenTTL = 2 * time.Hour

// NewInHouseProvider builds the provider. secret is required: without it the
// tokens it mints cannot be verified by the websocket handler.
func NewInHouseProvider(o InHouseOptions) (*InHouseProvider, error) {
	if o.Hub == nil {
		return nil, errors.New("consultation: in-house video provider needs a signalling hub")
	}
	if len(o.Secret) == 0 {
		return nil, errors.New("consultation: in-house video provider needs a signalling secret")
	}
	ttl := o.TokenTTL
	if ttl <= 0 {
		ttl = DefaultRoomTokenTTL
	}
	return &InHouseProvider{
		hub: o.Hub, bus: o.Bus,
		secret: o.Secret, tokenTTL: ttl,
		signalURL: o.SignalURL, log: o.Log,
	}, nil
}

var _ VideoProvider = (*InHouseProvider)(nil)

// Name is the VIDEO_PROVIDER value that selects this implementation.
func (p *InHouseProvider) Name() string { return "inhouse" }

// SignalURL is the absolute websocket address handed to the client.
func (p *InHouseProvider) SignalURL() string { return p.signalURL }

// CreateRoom is a no-op that reports success.
//
// A peer-to-peer room has no server-side existence: Hub.Join creates the map
// entry when the first socket arrives and Hub.Leave deletes it when the last
// one goes. Creating something here would create state on whichever replica
// happened to serve the HTTP request, which is frequently not the replica the
// websocket then lands on.
//
// service.Join calls this on every join -- including every token refresh --
// because LiveKit's version was idempotent. A pure function is strictly more
// idempotent than a stateful one, so that contract is not merely preserved, it
// is stronger.
//
// spec.EmptyTimeoutSeconds and spec.MaxParticipants are ignored, and that is
// worth saying out loud: the two-peer cap is signal.MaxPeersPerRoom, a
// compile-time constant enforced by a Lua CAS in Redis, not a per-room
// parameter; and there is no server-side timer to tear an empty room down,
// which is what the occupancy lease and the stale sweeper replace between
// them: the lease expires within about half a minute of the last socket
// going, and ListParticipants then reports the room gone.
func (p *InHouseProvider) CreateRoom(_ context.Context, spec RoomSpec) (Room, error) {
	if spec.RoomName == "" {
		return Room{}, errors.New("consultation: CreateRoom needs a room name")
	}
	return Room{
		Name:      spec.RoomName,
		SID:       "sig_" + spec.RoomName,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// GenerateToken mints the HMAC grant the signalling websocket verifies.
//
// TokenSpec's DisplayName, CanPublish and CanSubscribe are ignored. A
// signal.Token grants exactly one room to exactly one identity until an
// expiry, and nothing else -- there is no publish/subscribe asymmetry to
// express because there is no server in the media path to enforce one, and a
// display name asserted by the server is a string the relay would have to
// trust and never does.
func (p *InHouseProvider) GenerateToken(_ context.Context, spec TokenSpec) (string, error) {
	if spec.RoomName == "" || spec.Identity == "" {
		return "", errors.New("consultation: GenerateToken needs a room and an identity")
	}
	// Floored, never shortened. A caller passing LIVEKIT_TOKEN_TTL's five
	// minutes would otherwise mint a token that stops working at the first
	// mobile handover. See DefaultRoomTokenTTL.
	ttl := spec.TTL
	if ttl < p.tokenTTL {
		ttl = p.tokenTTL
	}
	return signal.MintToken(p.secret, spec.RoomName, spec.Identity, ttl), nil
}

// EndRoom evicts both peers, here and on whichever replica holds them.
//
// Always returns nil. There is no "room not found" that means anything: a room
// nobody is in is already in the state the caller asked for, and both call
// sites in service.go go on to treat ErrRoomNotFound as success anyway.
func (p *InHouseProvider) EndRoom(ctx context.Context, roomName string) error {
	p.hub.CloseRoom(ctx, roomName)
	return nil
}

// ListParticipants reports who is actually connected.
//
// THE MOST DANGEROUS METHOD IN THIS FILE.
//
// sweeper.sweepOne ends a consultation when this returns an empty list, and
// leaves it alone when it returns an error. So an implementation that reported
// "nobody here" on a Redis blip would end every live call on the platform at
// the next sweep. Every failure path below therefore returns an error, and
// only a definite, positive "the room's occupancy key does not exist" produces
// ErrRoomNotFound.
func (p *InHouseProvider) ListParticipants(ctx context.Context, roomName string) ([]ProviderParticipant, error) {
	if p.bus == nil {
		// Single process: the hub's own map is the whole truth. Correct for
		// one replica, and the only configuration where it is.
		return participantsFromPeers(p.hub.Peers(roomName)), nil
	}

	occupants, exists, err := p.bus.Occupants(ctx, roomName)
	if err != nil {
		// NOT an empty slice. See the method comment.
		return nil, fmt.Errorf("consultation: read room occupancy: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("consultation: room %q is empty: %w", roomName, ErrRoomNotFound)
	}

	seen := make(map[string]struct{}, len(occupants))
	out := make([]ProviderParticipant, 0, len(occupants))
	for identity, lease := range occupants {
		_, joinedAt := signal.ParseLeaseValue(lease)
		seen[identity] = struct{}{}
		out = append(out, ProviderParticipant{
			Identity:        identity,
			JoinedAt:        joinedAt,
			IsPublisher:     true,
			ConnectionState: "ACTIVE",
		})
	}

	// Union with the local view. Not belt-and-braces: Hub.Join has an explicit
	// "occupancy claim failed, admitting anyway" path, so a Redis hiccup at
	// join time leaves a peer that is genuinely connected and genuinely absent
	// from the hash. Without this, a sweep later would end their live call.
	for _, local := range p.hub.Peers(roomName) {
		if _, ok := seen[local.Identity]; ok {
			continue
		}
		out = append(out, ProviderParticipant{
			Identity:        local.Identity,
			JoinedAt:        local.JoinedAt,
			IsPublisher:     true,
			ConnectionState: "ACTIVE",
		})
	}
	return out, nil
}

func participantsFromPeers(peers []signal.PeerInfo) []ProviderParticipant {
	out := make([]ProviderParticipant, 0, len(peers))
	for _, p := range peers {
		out = append(out, ProviderParticipant{
			Identity:        p.Identity,
			JoinedAt:        p.JoinedAt,
			IsPublisher:     true,
			ConnectionState: "ACTIVE",
		})
	}
	return out
}

// RemoveParticipant evicts one identity, here and remotely.
//
// Returns nil whether or not a socket was closed in this process: the peer may
// be held on another replica, which the bus control frame reaches, and
// removing someone who is already gone is the state the caller wanted.
func (p *InHouseProvider) RemoveParticipant(ctx context.Context, roomName, identity string) error {
	p.hub.RemoveIdentity(ctx, roomName, identity)
	return nil
}

// StartRecording refuses, honestly.
//
// There is no server-side media path to record from and there cannot be one
// without an SFU. Returning ErrRecordingUnsupported lets maybeStartRecording
// distinguish "this provider cannot" from "this provider failed", and record
// the difference where a clinician can see it.
func (p *InHouseProvider) StartRecording(_ context.Context, _ RecordingSpec) (string, error) {
	return "", fmt.Errorf("consultation: inhouse: %w", ErrRecordingUnsupported)
}

// StopRecording refuses for the same reason. Reachable only for a
// consultation whose recording was started under a different provider.
func (p *InHouseProvider) StopRecording(_ context.Context, _ string) error {
	return fmt.Errorf("consultation: inhouse: %w", ErrRecordingUnsupported)
}

// VerifyWebhook always refuses.
//
// Nothing calls back: there is no external media server. The route stays
// mounted rather than being conditionally removed, so POST /webhooks/livekit
// answers 401 to everyone -- which closes the hole validateVideoConfig's own
// comment is about, an unauthenticated endpoint that can force-end a call and
// write recording_url, and is a clearer operational signal than a 404.
func (p *InHouseProvider) VerifyWebhook(_ context.Context, _ string, _ []byte) (WebhookEvent, error) {
	return WebhookEvent{}, fmt.Errorf(
		"consultation: the in-house video provider receives no webhooks: %w", ErrWebhookUnverified)
}

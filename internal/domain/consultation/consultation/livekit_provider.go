package consultation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	lkauth "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/twitchtv/twirp"
)

// LiveKitProvider is the production VideoProvider, backed by
// github.com/livekit/server-sdk-go/v2 v2.18.1.
//
// The source documentation's LiveKit snippet is stale against this version in
// three ways worth recording here, since the next person to touch this file
// will otherwise "fix" it back to the broken shape:
//
//  1. auth.AccessToken.AddGrant is deprecated in favour of SetVideoGrant. Both
//     still compile, but golangci-lint's staticcheck will flag AddGrant.
//  2. RoomServiceClient and EgressClient are NOT constructed with a bare
//     lksdk.RoomServiceClient{client: ...} struct literal -- the field is
//     unexported. Use lksdk.NewRoomServiceClient(url, key, secret) and
//     lksdk.NewEgressClient(url, key, secret).
//  3. There is no separate "LiveKit webhook auth" helper to hand-roll: the
//     verification (JWT-signed SHA-256 body checksum) lives in
//     github.com/livekit/protocol/webhook and takes an *http.Request, not a
//     bare byte slice -- see VerifyWebhook below for how that is bridged to
//     this port's (authHeader, body) shape.
type LiveKitProvider struct {
	rooms  *lksdk.RoomServiceClient
	egress *lksdk.EgressClient

	apiKey      string
	apiSecret   string
	keyProvider lkauth.KeyProvider

	tokenTTL     time.Duration
	emptyTimeout uint32

	recording RecordingConfig
}

// RecordingConfig carries the S3-compatible (MinIO) destination StartRecording
// writes to. LiveKit's Egress service talks S3 protocol directly; it does not
// go through our own Storage interface, since egress runs as a separate
// LiveKit component that needs its own credentials regardless.
type RecordingConfig struct {
	Endpoint       string // host:port, no scheme
	UseSSL         bool
	AccessKey      string
	SecretKey      string
	ForcePathStyle bool
}

var _ VideoProvider = (*LiveKitProvider)(nil)

// NewLiveKitProvider builds the provider. url is the LiveKit server's ws(s)://
// address; the SDK converts it to http(s):// internally for the twirp-based
// room/egress APIs.
func NewLiveKitProvider(url, apiKey, apiSecret string, tokenTTL time.Duration, emptyTimeoutSeconds uint32, recording RecordingConfig) *LiveKitProvider {
	return &LiveKitProvider{
		rooms:        lksdk.NewRoomServiceClient(url, apiKey, apiSecret),
		egress:       lksdk.NewEgressClient(url, apiKey, apiSecret),
		apiKey:       apiKey,
		apiSecret:    apiSecret,
		keyProvider:  lkauth.NewSimpleKeyProvider(apiKey, apiSecret),
		tokenTTL:     tokenTTL,
		emptyTimeout: emptyTimeoutSeconds,
		recording:    recording,
	}
}

func (p *LiveKitProvider) Name() string { return "livekit" }

func (p *LiveKitProvider) CreateRoom(ctx context.Context, spec RoomSpec) (Room, error) {
	et := spec.EmptyTimeoutSeconds
	if et == 0 {
		et = p.emptyTimeout
	}
	room, err := p.rooms.CreateRoom(ctx, &livekit.CreateRoomRequest{
		Name:            spec.RoomName,
		EmptyTimeout:    et,
		MaxParticipants: spec.MaxParticipants,
		Metadata:        spec.Metadata,
	})
	if err != nil {
		return Room{}, fmt.Errorf("consultation: livekit create room %s: %w", spec.RoomName, err)
	}
	return Room{
		Name:      room.Name,
		SID:       room.Sid,
		CreatedAt: time.Unix(room.CreationTime, 0).UTC(),
	}, nil
}

// GenerateToken mints a short-lived, room-scoped access token.
//
// This deliberately does not use the deprecated AddGrant/auth.VideoGrant
// snippet in the source docs verbatim -- SetCanPublish/SetCanSubscribe are
// used so the *bool distinction the SDK uses to tell "explicitly false" from
// "unset" is set correctly. An unset CanPublish grants publish by default
// (see auth.VideoGrant.GetCanPublish in protocol/auth/grants.go), which would
// silently hand a subscriber-only participant publish rights.
func (p *LiveKitProvider) GenerateToken(_ context.Context, spec TokenSpec) (string, error) {
	ttl := spec.TTL
	if ttl <= 0 {
		ttl = p.tokenTTL
	}

	grant := &lkauth.VideoGrant{
		RoomJoin: true,
		Room:     spec.RoomName,
	}
	grant.SetCanPublish(spec.CanPublish)
	grant.SetCanSubscribe(spec.CanSubscribe)

	at := lkauth.NewAccessToken(p.apiKey, p.apiSecret).
		SetIdentity(spec.Identity).
		SetValidFor(ttl).
		SetVideoGrant(grant)
	if spec.DisplayName != "" {
		at.SetName(spec.DisplayName)
	}

	token, err := at.ToJWT()
	if err != nil {
		return "", fmt.Errorf("consultation: livekit sign token: %w", err)
	}
	return token, nil
}

func (p *LiveKitProvider) EndRoom(ctx context.Context, roomName string) error {
	_, err := p.rooms.DeleteRoom(ctx, &livekit.DeleteRoomRequest{Room: roomName})
	if err != nil {
		return fmt.Errorf("consultation: livekit delete room %s: %w", roomName, translateNotFound(err))
	}
	return nil
}

func (p *LiveKitProvider) ListParticipants(ctx context.Context, roomName string) ([]ProviderParticipant, error) {
	resp, err := p.rooms.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: roomName})
	if err != nil {
		return nil, fmt.Errorf("consultation: livekit list participants %s: %w", roomName, translateNotFound(err))
	}
	out := make([]ProviderParticipant, 0, len(resp.Participants))
	for _, pi := range resp.Participants {
		out = append(out, ProviderParticipant{
			Identity:        pi.Identity,
			JoinedAt:        time.Unix(pi.JoinedAt, 0).UTC(),
			IsPublisher:     pi.IsPublisher,
			ConnectionState: pi.State.String(),
		})
	}
	return out, nil
}

func (p *LiveKitProvider) RemoveParticipant(ctx context.Context, roomName, identity string) error {
	_, err := p.rooms.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{Room: roomName, Identity: identity})
	if err != nil {
		return fmt.Errorf("consultation: livekit remove participant %s/%s: %w", roomName, identity, translateNotFound(err))
	}
	return nil
}

// StartRecording starts a room-composite (mixed audio+video) egress to the
// configured S3-compatible bucket. MinIO speaks the same S3 API LiveKit
// Egress already knows how to write to, so no custom uploader is needed.
func (p *LiveKitProvider) StartRecording(ctx context.Context, spec RecordingSpec) (string, error) {
	scheme := "http://"
	if p.recording.UseSSL {
		scheme = "https://"
	}
	// FileOutputs is the current field; the singular Output oneof it replaces
	// is deprecated in livekit_egress.proto. The wire result is identical for
	// a single output, which is all a consultation recording ever needs.
	info, err := p.egress.StartRoomCompositeEgress(ctx, &livekit.RoomCompositeEgressRequest{
		RoomName: spec.RoomName,
		FileOutputs: []*livekit.EncodedFileOutput{{
			FileType: livekit.EncodedFileType_MP4,
			Filepath: spec.OutputKey,
			Output: &livekit.EncodedFileOutput_S3{
				S3: &livekit.S3Upload{
					AccessKey:      p.recording.AccessKey,
					Secret:         p.recording.SecretKey,
					Bucket:         spec.Bucket,
					Endpoint:       scheme + p.recording.Endpoint,
					ForcePathStyle: p.recording.ForcePathStyle,
				},
			},
		}},
	})
	if err != nil {
		return "", fmt.Errorf("consultation: livekit start egress for room %s: %w", spec.RoomName, err)
	}
	return info.EgressId, nil
}

func (p *LiveKitProvider) StopRecording(ctx context.Context, egressID string) error {
	_, err := p.egress.StopEgress(ctx, &livekit.StopEgressRequest{EgressId: egressID})
	if err != nil {
		return fmt.Errorf("consultation: livekit stop egress %s: %w", egressID, err)
	}
	return nil
}

// VerifyWebhook bridges this port's (authHeader, body) shape to
// protocol/webhook's Receive, which is written against *http.Request. The
// request built here is never sent anywhere; it exists only so the vendored
// verification logic (JWT-signed SHA-256 body checksum, matched against our
// own API key/secret) runs unmodified rather than being hand-reimplemented.
func (p *LiveKitProvider) VerifyWebhook(_ context.Context, authHeader string, body []byte) (WebhookEvent, error) {
	req := &http.Request{
		Header: http.Header{"Authorization": []string{authHeader}},
		Body:   io.NopCloser(bytes.NewReader(body)),
	}
	evt, err := webhook.ReceiveWebhookEvent(req, p.keyProvider)
	if err != nil {
		return WebhookEvent{}, fmt.Errorf("consultation: %w: %w", ErrWebhookUnverified, err)
	}

	out := WebhookEvent{
		ID:         evt.Id,
		Type:       evt.Event,
		OccurredAt: time.Unix(evt.CreatedAt, 0).UTC(),
	}
	switch {
	case evt.Room != nil:
		out.RoomName = evt.Room.Name
	case evt.EgressInfo != nil:
		// EgressInfo carries its own room_name; the room_started/finished
		// events are the only ones that set the top-level Room field.
		out.RoomName = evt.EgressInfo.RoomName
	}
	if evt.Participant != nil {
		out.ParticipantIdentity = evt.Participant.Identity
	}
	if evt.EgressInfo != nil {
		out.EgressID = evt.EgressInfo.EgressId
		out.EgressStatus = evt.EgressInfo.Status.String()
		if len(evt.EgressInfo.FileResults) > 0 {
			out.RecordingURL = evt.EgressInfo.FileResults[0].Location
		}
	}
	return out, nil
}

// translateNotFound turns a twirp not_found error into ErrRoomNotFound so
// callers can errors.Is against a stable, provider-neutral sentinel instead
// of reaching into twirp.Error themselves.
func translateNotFound(err error) error {
	var twerr twirp.Error
	if errors.As(err, &twerr) && twerr.Code() == twirp.NotFound {
		return ErrRoomNotFound
	}
	return err
}

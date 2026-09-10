package consultation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"telemed/internal/domain/consultation/signal"
)

func newInHouse(t *testing.T) *InHouseProvider {
	t.Helper()
	hub := signal.NewHub(context.Background(), signal.HubOptions{Log: zerolog.Nop()})
	p, err := NewInHouseProvider(InHouseOptions{
		Hub:       hub,
		Secret:    []byte("test-signal-secret"),
		SignalURL: "wss://rtc.example.lk/ws/consultation",
		Log:       zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("NewInHouseProvider: %v", err)
	}
	return p
}

func TestInHouseProviderRefusesToBuildWithoutASecret(t *testing.T) {
	hub := signal.NewHub(context.Background(), signal.HubOptions{Log: zerolog.Nop()})

	// Without a secret it would mint tokens the websocket handler cannot
	// verify, and every join would fail at the upgrade with a 401 that names
	// nothing.
	if _, err := NewInHouseProvider(InHouseOptions{Hub: hub, Log: zerolog.Nop()}); err == nil {
		t.Error("expected an error with no signalling secret")
	}
	if _, err := NewInHouseProvider(InHouseOptions{Secret: []byte("s"), Log: zerolog.Nop()}); err == nil {
		t.Error("expected an error with no hub")
	}
}

// CreateRoom is called by service.Join on EVERY join, including every token
// refresh, because LiveKit's version was idempotent. A pure function is
// strictly more idempotent than a stateful one; this pins that.
func TestInHouseCreateRoomIsIdempotentAndPure(t *testing.T) {
	p := newInHouse(t)
	ctx := context.Background()

	first, err := p.CreateRoom(ctx, RoomSpec{RoomName: "consult-1"})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	second, err := p.CreateRoom(ctx, RoomSpec{RoomName: "consult-1"})
	if err != nil {
		t.Fatalf("CreateRoom again: %v", err)
	}
	if first.Name != "consult-1" || second.Name != "consult-1" {
		t.Errorf("room names = %q, %q", first.Name, second.Name)
	}
	if first.SID != second.SID {
		t.Errorf("SID is not stable across calls: %q vs %q", first.SID, second.SID)
	}

	if _, err := p.CreateRoom(ctx, RoomSpec{}); err == nil {
		t.Error("expected an error for an empty room name")
	}
}

// A five-minute LIVEKIT_TOKEN_TTL is fine for a token spent once at connect
// and catastrophic for one re-presented on every websocket reconnect: a
// patient who changes cell at minute twelve would be locked out of their own
// consultation.
func TestInHouseGenerateTokenFloorsShortTTLs(t *testing.T) {
	p := newInHouse(t)
	ctx := context.Background()

	short, err := p.GenerateToken(ctx, TokenSpec{
		RoomName: "consult-1", Identity: "patient-1", TTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	// Still valid well past the five minutes the caller asked for.
	grant, err := signal.VerifyToken([]byte("test-signal-secret"), short)
	if err != nil {
		t.Fatalf("the minted token does not verify: %v", err)
	}
	if !grant.Expires.After(time.Now().Add(time.Hour)) {
		t.Errorf("token expires at %s, want at least an hour out", grant.Expires)
	}
	if grant.Room != "consult-1" || grant.Identity != "patient-1" {
		t.Errorf("grant = %+v", grant)
	}
}

func TestInHouseGenerateTokenRejectsIncompleteSpecs(t *testing.T) {
	p := newInHouse(t)
	ctx := context.Background()
	for _, spec := range []TokenSpec{
		{Identity: "patient-1"},
		{RoomName: "consult-1"},
	} {
		if _, err := p.GenerateToken(ctx, spec); err == nil {
			t.Errorf("expected an error for %+v", spec)
		}
	}
}

// THE test for this file.
//
// sweeper.sweepOne ends a consultation when ListParticipants returns an empty
// list and leaves it alone when it returns an error. With no bus configured
// the hub is authoritative, so an empty room is genuinely empty -- but the
// moment a bus exists, "I could not reach Redis" must never be reported as
// "nobody is here", or one blip ends every live call on the platform.
func TestInHouseListParticipantsUsesTheLocalHubWithoutABus(t *testing.T) {
	p := newInHouse(t)
	ctx := context.Background()

	got, err := p.ListParticipants(ctx, "consult-1")
	if err != nil {
		t.Fatalf("ListParticipants on an empty room: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d participants in an empty room", len(got))
	}

	peer, _, err := p.hub.Join(ctx, "consult-1", "patient-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer p.hub.Leave(ctx, peer)

	got, err = p.ListParticipants(ctx, "consult-1")
	if err != nil {
		t.Fatalf("ListParticipants: %v", err)
	}
	if len(got) != 1 || got[0].Identity != "patient-1" {
		t.Fatalf("got %+v, want one participant identified as patient-1", got)
	}
	if got[0].JoinedAt.IsZero() {
		t.Error("JoinedAt is zero; the sweeper's wait estimates read this")
	}
	if !got[0].IsPublisher {
		t.Error("a peer-to-peer participant always publishes")
	}
}

// Recording has no server-side path and cannot have one without an SFU. The
// sentinel is what lets maybeStartRecording tell "cannot" from "failed" and
// record the difference where a clinician can see it.
func TestInHouseRecordingIsUnsupportedNotBroken(t *testing.T) {
	p := newInHouse(t)
	ctx := context.Background()

	if _, err := p.StartRecording(ctx, RecordingSpec{RoomName: "consult-1"}); !errors.Is(err, ErrRecordingUnsupported) {
		t.Errorf("StartRecording error = %v, want ErrRecordingUnsupported", err)
	}
	if err := p.StopRecording(ctx, "egress-1"); !errors.Is(err, ErrRecordingUnsupported) {
		t.Errorf("StopRecording error = %v, want ErrRecordingUnsupported", err)
	}
}

// POST /webhooks/livekit stays mounted and answers 401 to everyone. That route
// can force-end a call and write recording_url; leaving it verifiable by a
// provider that receives no webhooks would be an unauthenticated write to any
// consultation.
func TestInHouseVerifyWebhookAlwaysRefuses(t *testing.T) {
	p := newInHouse(t)
	_, err := p.VerifyWebhook(context.Background(), "Bearer anything", []byte(`{"event":"room_finished"}`))
	if !errors.Is(err, ErrWebhookUnverified) {
		t.Errorf("VerifyWebhook error = %v, want ErrWebhookUnverified", err)
	}
}

func TestInHouseEndRoomIsAlwaysSuccessful(t *testing.T) {
	p := newInHouse(t)
	ctx := context.Background()

	// Ending a room nobody is in is the state the caller asked for. Both call
	// sites in service.go treat ErrRoomNotFound as success anyway, so nil is
	// the quiet path they want.
	if err := p.EndRoom(ctx, "never-existed"); err != nil {
		t.Errorf("EndRoom on an absent room = %v, want nil", err)
	}
	if err := p.RemoveParticipant(ctx, "never-existed", "nobody"); err != nil {
		t.Errorf("RemoveParticipant on an absent room = %v, want nil", err)
	}
}

func TestInHouseEndRoomClosesLivePeers(t *testing.T) {
	p := newInHouse(t)
	ctx := context.Background()

	peer, _, err := p.hub.Join(ctx, "consult-1", "patient-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if err := p.EndRoom(ctx, "consult-1"); err != nil {
		t.Fatalf("EndRoom: %v", err)
	}
	select {
	case <-peer.Done():
	case <-time.After(time.Second):
		t.Error("EndRoom did not close the peer")
	}
}

func TestInHouseRemoveParticipantEvictsOnlyThatIdentity(t *testing.T) {
	p := newInHouse(t)
	ctx := context.Background()

	patient, _, err := p.hub.Join(ctx, "consult-1", "patient-1")
	if err != nil {
		t.Fatalf("join patient: %v", err)
	}
	doctor, _, err := p.hub.Join(ctx, "consult-1", "doctor-1")
	if err != nil {
		t.Fatalf("join doctor: %v", err)
	}

	if err := p.RemoveParticipant(ctx, "consult-1", "patient-1"); err != nil {
		t.Fatalf("RemoveParticipant: %v", err)
	}
	select {
	case <-patient.Done():
	case <-time.After(time.Second):
		t.Error("the removed participant was not evicted")
	}
	select {
	case <-doctor.Done():
		t.Error("removing one participant closed the other")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestInHouseNameMatchesTheConfigValue(t *testing.T) {
	// buildVideoProvider and validateVideoConfig switch on this string, and
	// JoinResult.provider carries it to the client.
	if got := newInHouse(t).Name(); got != "inhouse" {
		t.Errorf("Name() = %q, want inhouse", got)
	}
}

func TestRecordingModeFollowsTheProvider(t *testing.T) {
	// A patient consenting to a recording is entitled to know whether the
	// platform keeps it or the doctor's laptop does.
	for provider, want := range map[string]string{
		"livekit": RecordingModeServer,
		"inhouse": RecordingModeClient,
		"mock":    RecordingModeNone,
	} {
		if got := recordingModeFor(provider); got != want {
			t.Errorf("recordingModeFor(%q) = %q, want %q", provider, got, want)
		}
	}
}

// TestInHouseListParticipantsErrorsRatherThanReportingAnEmptyRoom is the
// single most important assertion about this provider.
//
// sweeper.sweepOne reads it like this:
//
//	err != nil                 -> provider unavailable, leave the call alone
//	len(participants) == 0     -> nobody is here, END THE CONSULTATION
//
// So an implementation that swallowed a Redis failure and returned an empty
// slice would end every live consultation on the platform at the next sweep,
// silently, with the database showing a clean shutdown. The bus must fail
// loudly or not at all.
func TestInHouseListParticipantsErrorsRatherThanReportingAnEmptyRoom(t *testing.T) {
	// A client pointed at a port nothing is listening on. Every command fails.
	rc := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 100 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { _ = rc.Close() })

	log := zerolog.Nop()
	bus := signal.NewBus(rc, log)
	hub := signal.NewHub(context.Background(), signal.HubOptions{Log: log, Bus: bus})
	p, err := NewInHouseProvider(InHouseOptions{
		Hub: hub, Bus: bus, Secret: []byte("s"), Log: log,
	})
	if err != nil {
		t.Fatalf("NewInHouseProvider: %v", err)
	}

	got, err := p.ListParticipants(context.Background(), "consult-1")
	if err == nil {
		t.Fatalf("ListParticipants returned (%v, nil) with Redis unreachable; "+
			"the sweeper reads an empty list as 'end this consultation'", got)
	}
	// And specifically not ErrRoomNotFound, which the sweeper also treats as
	// definitive.
	if errors.Is(err, ErrRoomNotFound) {
		t.Error("a Redis failure must not be reported as ErrRoomNotFound: " +
			"that is the sweeper's 'definitely gone' branch")
	}
}

package signal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

var testSecret = []byte("test-secret-not-used-anywhere-real")

func TestToken_RoundTrips(t *testing.T) {
	tok := MintToken(testSecret, "room-1", "peer-a", time.Minute)
	got, err := VerifyToken(testSecret, tok)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if got.Room != "room-1" || got.Identity != "peer-a" {
		t.Fatalf("got %+v, want room-1/peer-a", got)
	}
}

func TestToken_RejectsWrongKey(t *testing.T) {
	tok := MintToken(testSecret, "room-1", "peer-a", time.Minute)
	if _, err := VerifyToken([]byte("a different secret entirely"), tok); err == nil {
		t.Fatal("a token signed with another key verified")
	}
}

func TestToken_RejectsExpired(t *testing.T) {
	tok := MintToken(testSecret, "room-1", "peer-a", -time.Second)
	if _, err := VerifyToken(testSecret, tok); err == nil {
		t.Fatal("an expired token verified")
	}
}

// TestToken_RoomBoundaryCannotShift is the reason room and identity are
// base64-encoded before being joined with dots. Without that, a room named
// "a.b" and an identity "c" produce the same signing body as a room "a" and an
// identity "b.c", so a token for one verifies as a token for the other -- and
// the room name is the entire access grant here.
func TestToken_RoomBoundaryCannotShift(t *testing.T) {
	a := MintToken(testSecret, "a.b", "c", time.Minute)
	got, err := VerifyToken(testSecret, a)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if got.Room != "a.b" || got.Identity != "c" {
		t.Fatalf("room/identity boundary shifted: got %q/%q", got.Room, got.Identity)
	}
}

// newTestServer stands up the real websocket handler over httptest.
func newTestServer(t *testing.T) (*httptest.Server, *Hub) {
	t.Helper()
	hub := NewHub(context.Background(), HubOptions{
		Log: zerolog.Nop(),
		ICEServers: func(context.Context, string) []ICEServer {
			return []ICEServer{{URLs: []string{"stun:example:3478"}}}
		},
	})
	h := NewHandler(HandlerOptions{Hub: hub, Secret: testSecret, Log: zerolog.Nop()})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, hub
}

func dial(t *testing.T, srv *httptest.Server, room, identity string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") +
		"?token=" + MintToken(testSecret, room, identity, time.Minute)
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial %s: %v", identity, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readEnvelope(t *testing.T, conn *websocket.Conn) Envelope {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var env Envelope
	if err := conn.ReadJSON(&env); err != nil {
		t.Fatalf("read: %v", err)
	}
	return env
}

func readWelcome(t *testing.T, conn *websocket.Conn) Welcome {
	t.Helper()
	env := readEnvelope(t, conn)
	if env.Type != TypeWelcome {
		t.Fatalf("first frame is %q, want %q", env.Type, TypeWelcome)
	}
	var w Welcome
	if err := json.Unmarshal(env.Data, &w); err != nil {
		t.Fatalf("decode welcome: %v", err)
	}
	return w
}

func TestHandler_RejectsBadToken(t *testing.T) {
	srv, _ := newTestServer(t)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "?token=nonsense"
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		_ = conn.Close()
		t.Fatal("dialled with a bad token")
	}
	if resp != nil && resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 400 or 401", resp.StatusCode)
	}
}

// TestHub_ExactlyOnePeerIsPolite pins the perfect-negotiation assignment. Two
// impolite peers deadlock on every simultaneous offer; two polite ones deadlock
// on the first. The hub decides, so this is the property that must hold.
func TestHub_ExactlyOnePeerIsPolite(t *testing.T) {
	srv, _ := newTestServer(t)

	first := dial(t, srv, "room-polite", "peer-a")
	wFirst := readWelcome(t, first)

	second := dial(t, srv, "room-polite", "peer-b")
	wSecond := readWelcome(t, second)

	if wFirst.Polite == wSecond.Polite {
		t.Fatalf("both peers got polite=%v; exactly one must be polite", wFirst.Polite)
	}
	if !wFirst.Polite {
		t.Error("the peer already in the room should be the polite one")
	}
	if wFirst.PeerPresent {
		t.Error("the first peer to arrive should not see a peer present")
	}
	if !wSecond.PeerPresent {
		t.Error("the second peer should see the first one present")
	}
	if len(wSecond.ICEServers) == 0 {
		t.Error("welcome carried no ICE servers")
	}
}

func TestHub_RelaysOfferToTheFarPeer(t *testing.T) {
	srv, _ := newTestServer(t)

	first := dial(t, srv, "room-relay", "peer-a")
	readWelcome(t, first)
	second := dial(t, srv, "room-relay", "peer-b")
	wSecond := readWelcome(t, second)

	// The first peer is told someone arrived before it sees any relayed frame.
	if env := readEnvelope(t, first); env.Type != TypePeerJoined {
		t.Fatalf("first peer saw %q, want %q", env.Type, TypePeerJoined)
	}

	if err := second.WriteJSON(Envelope{Type: TypeOffer, Data: json.RawMessage(`{"sdp":"v=0"}`)}); err != nil {
		t.Fatalf("write offer: %v", err)
	}

	got := readEnvelope(t, first)
	if got.Type != TypeOffer {
		t.Fatalf("relayed frame is %q, want %q", got.Type, TypeOffer)
	}
	if string(got.Data) != `{"sdp":"v=0"}` {
		t.Fatalf("payload was altered in relay: %s", got.Data)
	}
	// From is the hub's statement about the sender, not the sender's claim.
	if got.From != wSecond.PeerID {
		t.Fatalf("From is %q, want the sending peer's id %q", got.From, wSecond.PeerID)
	}
}

func TestHub_RelaysFileShared(t *testing.T) {
	srv, _ := newTestServer(t)

	first := dial(t, srv, "room-file", "peer-a")
	readWelcome(t, first)
	second := dial(t, srv, "room-file", "peer-b")
	readWelcome(t, second)
	if env := readEnvelope(t, first); env.Type != TypePeerJoined {
		t.Fatalf("first peer saw %q, want %q", env.Type, TypePeerJoined)
	}

	if err := second.WriteJSON(Envelope{Type: TypeFileShared, Data: json.RawMessage(`{"name":"report.pdf"}`)}); err != nil {
		t.Fatalf("write file_shared: %v", err)
	}
	got := readEnvelope(t, first)
	if got.Type != TypeFileShared || string(got.Data) != `{"name":"report.pdf"}` {
		t.Fatalf("relayed %q %s, want file_shared with the payload unchanged", got.Type, got.Data)
	}
}

// TestHub_DoesNotRelayUnknownTypes is the allowlist doing its job: a client
// cannot invent a frame type and have the server deliver it verbatim into the
// other browser's message handler.
func TestHub_DoesNotRelayUnknownTypes(t *testing.T) {
	srv, _ := newTestServer(t)

	first := dial(t, srv, "room-allow", "peer-a")
	readWelcome(t, first)
	second := dial(t, srv, "room-allow", "peer-b")
	readWelcome(t, second)
	if env := readEnvelope(t, first); env.Type != TypePeerJoined {
		t.Fatalf("expected peer-joined, got %q", env.Type)
	}

	if err := second.WriteJSON(Envelope{Type: "execute-arbitrary-thing"}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The sender is told, rather than the frame being dropped in silence.
	if env := readEnvelope(t, second); env.Type != TypeError {
		t.Fatalf("sender saw %q, want %q", env.Type, TypeError)
	}

	// And the far peer never sees it. A ping/pong round trip proves the socket
	// is live and simply had nothing relayed to it, rather than being stuck.
	if err := first.WriteJSON(Envelope{Type: TypePing}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	if env := readEnvelope(t, first); env.Type != TypePong {
		t.Fatalf("far peer received %q; the unknown frame was relayed", env.Type)
	}
}

func TestHub_RefusesAThirdPeer(t *testing.T) {
	srv, _ := newTestServer(t)

	first := dial(t, srv, "room-full", "peer-a")
	readWelcome(t, first)
	second := dial(t, srv, "room-full", "peer-b")
	readWelcome(t, second)

	third := dial(t, srv, "room-full", "peer-c")
	env := readEnvelope(t, third)
	if env.Type != TypeError {
		t.Fatalf("third peer got %q, want %q", env.Type, TypeError)
	}
	var data errorData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("decode error frame: %v", err)
	}
	if data.Code != CodeRoomFull {
		t.Fatalf("error code %q, want %q", data.Code, CodeRoomFull)
	}
}

// TestHub_ReconnectUnderTheSameIdentityReplaces is what makes auto-reconnect
// work. A client whose socket dropped cannot tell the hub its old one is dead,
// so rejoining must evict rather than be refused as a third participant --
// otherwise every network blip locks a patient out of their own consultation.
func TestHub_ReconnectUnderTheSameIdentityReplaces(t *testing.T) {
	srv, hub := newTestServer(t)

	first := dial(t, srv, "room-reconnect", "peer-a")
	readWelcome(t, first)
	second := dial(t, srv, "room-reconnect", "peer-b")
	readWelcome(t, second)

	// peer-b comes back on a new socket without the old one having closed.
	again := dial(t, srv, "room-reconnect", "peer-b")
	w := readWelcome(t, again)
	if w.Room != "room-reconnect" {
		t.Fatalf("rejoined room %q", w.Room)
	}

	// The room still holds two peers, not three.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.PeerCount("room-reconnect") == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("room holds %d peers after a reconnect, want 2", hub.PeerCount("room-reconnect"))
}

func TestHub_PeerLeftReachesTheFarSide(t *testing.T) {
	srv, _ := newTestServer(t)

	first := dial(t, srv, "room-leave", "peer-a")
	readWelcome(t, first)
	second := dial(t, srv, "room-leave", "peer-b")
	readWelcome(t, second)
	if env := readEnvelope(t, first); env.Type != TypePeerJoined {
		t.Fatalf("expected peer-joined, got %q", env.Type)
	}

	_ = second.Close()

	if env := readEnvelope(t, first); env.Type != TypePeerLeft {
		t.Fatalf("far side saw %q, want %q", env.Type, TypePeerLeft)
	}
}

func TestHub_CloseRoomEvictsBoth(t *testing.T) {
	srv, hub := newTestServer(t)

	first := dial(t, srv, "room-close", "peer-a")
	readWelcome(t, first)

	hub.CloseRoom(context.Background(), "room-close")

	if env := readEnvelope(t, first); env.Type != TypeRoomClosed {
		t.Fatalf("peer saw %q, want %q", env.Type, TypeRoomClosed)
	}
	if n := hub.PeerCount("room-close"); n != 0 {
		t.Fatalf("room holds %d peers after close", n)
	}
}

func TestICEConfig_TURNRESTCredentialIsMinted(t *testing.T) {
	cfg := ICEConfig{
		STUNURLs:   []string{"stun:stun.example:3478"},
		TURNURLs:   []string{"turn:turn.example:3478"},
		TURNSecret: "shared-secret",
		TURNTTL:    time.Hour,
	}
	servers, err := cfg.Servers(context.Background(), "peer-a")
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("got %d ICE servers, want 2", len(servers))
	}
	turn := servers[1]
	if !strings.HasSuffix(turn.Username, ":peer-a") {
		t.Errorf("username %q does not encode the identity", turn.Username)
	}
	if turn.Credential == "" {
		t.Error("no credential minted")
	}

	// A static pair is used only when no REST secret is set.
	static := ICEConfig{TURNURLs: []string{"turn:turn.example:3478"}, TURNUsername: "u", TURNCredential: "p"}
	staticServers, err := static.Servers(context.Background(), "peer-a")
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if got := staticServers[0]; got.Username != "u" || got.Credential != "p" {
		t.Errorf("static credentials not used: %+v", got)
	}
}

// TestICEConfig_NoTURNYieldsSTUNOnly documents the default deployment shape,
// and the gap it leaves: calls behind symmetric or carrier-grade NAT will not
// connect until TURN is configured.
func TestICEConfig_NoTURNYieldsSTUNOnly(t *testing.T) {
	cfg := ICEConfig{STUNURLs: []string{"stun:stun.example:3478"}}
	servers, err := cfg.Servers(context.Background(), "peer-a")
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(servers) != 1 || len(servers[0].URLs) != 1 {
		t.Fatalf("got %+v, want exactly one STUN entry", servers)
	}
}

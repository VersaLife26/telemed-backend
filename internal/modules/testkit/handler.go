package testkit

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/domain/consultation/signal"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/testkit"
)

type handler struct {
	outbox *testkit.Outbox
	hub    *signal.Hub
	secret []byte
	ice    signal.ICEProvider
	env    string
	ver    string
}

// statusResponse tells the test page what it is connected to and what is
// actually available, so a developer debugging a call that will not connect
// can see "turn_configured: false" rather than guessing.
type statusResponse struct {
	TestMode        bool   `json:"test_mode"`
	Env             string `json:"env"`
	Version         string `json:"version"`
	SignalPath      string `json:"signal_path"`
	OutboxEnabled   bool   `json:"outbox_enabled"`
	STUNConfigured  bool   `json:"stun_configured"`
	TURNConfigured  bool   `json:"turn_configured"`
	MaxPeersPerRoom int    `json:"max_peers_per_room"`
}

func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	stun, turn := h.iceAvailability(r.Context())
	httpx.OK(w, r, statusResponse{
		TestMode:      true,
		Env:           h.env,
		Version:       h.ver,
		SignalPath:    signalPath,
		OutboxEnabled: h.outbox != nil,
		// PROBED, not read from config. "TURN_URLS is set" and "a browser
		// would actually be handed a TURN server" are different claims, and
		// only the second one tells a developer why a call across two networks
		// is not connecting.
		STUNConfigured:  stun,
		TURNConfigured:  turn,
		MaxPeersPerRoom: signal.MaxPeersPerRoom,
	})
}

// --- outbox ------------------------------------------------------------

func (h *handler) listOutbox(w http.ResponseWriter, r *http.Request) {
	kind := testkit.Kind(r.URL.Query().Get("kind"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	httpx.OK(w, r, map[string]any{
		"messages": h.outbox.List(kind, limit),
	})
}

// latestOutbox answers the one question the test page asks constantly: "what
// code was just sent to this number". Polling the full list and filtering
// client-side would work, but it puts every OTP for every number on the wire
// to answer a question about one.
func (h *handler) latestOutbox(w http.ResponseWriter, r *http.Request) {
	kind := testkit.Kind(r.URL.Query().Get("kind"))
	if kind == "" {
		kind = testkit.KindSMS
	}
	to := r.URL.Query().Get("to")
	if to == "" {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "to is required"))
		return
	}
	if kind == testkit.KindSMS {
		// The OTP send path normalises before dialling out, so a raw
		// "0771234567" typed into the test page would never match the stored
		// "+94771234567".
		to = httpx.NormalizePhone(to)
	}

	msg, ok := h.outbox.Latest(kind, to)
	if !ok {
		httpx.Error(w, r, httpx.NewError(http.StatusNotFound, httpx.CodeNotFound,
			"nothing captured for that recipient yet"))
		return
	}
	httpx.OK(w, r, msg)
}

func (h *handler) clearOutbox(w http.ResponseWriter, r *http.Request) {
	h.outbox.Clear()
	httpx.NoContent(w, r)
}

// --- signalling rooms ---------------------------------------------------

type createRoomRequest struct {
	// Room is optional. Left empty a random one is generated, which is what
	// the "start a call" button does; supplied, it is how the second device
	// joins the first one's room.
	Room string `json:"room"`
	// Identity distinguishes the two peers. Two tabs that send the same
	// identity are treated as the same participant reconnecting, and the
	// second one evicts the first -- which looks exactly like a broken server
	// unless you know it is happening, hence the generated default.
	Identity string `json:"identity"`
}

type createRoomResponse struct {
	Room       string             `json:"room"`
	Identity   string             `json:"identity"`
	Token      string             `json:"token"`
	SignalPath string             `json:"signal_path"`
	ICEServers []signal.ICEServer `json:"ice_servers"`
	ExpiresIn  int                `json:"expires_in_seconds"`
}

func (h *handler) createRoom(w http.ResponseWriter, r *http.Request) {
	var req createRoomRequest
	// An empty body is a valid request here (generate everything), so a decode
	// failure is only reported when there was something to decode.
	if r.ContentLength > 0 {
		if err := httpx.DecodeJSON(w, r, &req); err != nil {
			return
		}
	}

	room := normaliseRoom(req.Room)
	if room == "" {
		room = "test-" + uuid.NewString()[:8]
	}
	identity := normaliseRoom(req.Identity)
	if identity == "" {
		identity = "peer-" + uuid.NewString()[:8]
	}

	httpx.OK(w, r, createRoomResponse{
		Room:       room,
		Identity:   identity,
		Token:      signal.MintToken(h.secret, room, identity, roomTokenTTL),
		SignalPath: signalPath,
		ICEServers: iceOrEmpty(r.Context(), h.ice, identity),
		ExpiresIn:  int(roomTokenTTL.Seconds()),
	})
}

func (h *handler) roomStatus(w http.ResponseWriter, r *http.Request) {
	room := normaliseRoom(chi.URLParam(r, "room"))
	httpx.OK(w, r, map[string]any{
		"room": room,
		// Local to this process. With more than one replica the far peer may
		// be held elsewhere and not counted here, which is why this is
		// reported as a diagnostic and not used to decide anything.
		"local_peers": h.hub.PeerCount(room),
	})
}

func (h *handler) closeRoom(w http.ResponseWriter, r *http.Request) {
	room := normaliseRoom(chi.URLParam(r, "room"))
	h.hub.CloseRoom(r.Context(), room)
	httpx.NoContent(w, r)
}

func (h *handler) iceServers(w http.ResponseWriter, r *http.Request) {
	identity := r.URL.Query().Get("identity")
	if identity == "" {
		identity = "testkit"
	}
	servers, err := h.ice.Servers(r.Context(), identity)
	if err != nil {
		// Reported rather than hidden: on the test surface, "TURN could not be
		// minted" is exactly what the developer is here to find out.
		httpx.OK(w, r, map[string]any{"ice_servers": servers, "error": err.Error()})
		return
	}
	httpx.OK(w, r, map[string]any{"ice_servers": servers})
}

// iceAvailability reports what a browser would actually receive right now.
func (h *handler) iceAvailability(ctx context.Context) (stun, turn bool) {
	servers, err := h.ice.Servers(ctx, "testkit")
	if err != nil {
		return false, false
	}
	for _, s := range servers {
		for _, u := range s.URLs {
			switch {
			case strings.HasPrefix(u, "stun:"), strings.HasPrefix(u, "stuns:"):
				stun = true
			case strings.HasPrefix(u, "turn:"), strings.HasPrefix(u, "turns:"):
				turn = true
			}
		}
	}
	return stun, turn
}

// iceOrEmpty mints ICE servers, degrading rather than failing room creation: a
// room with no TURN still works on a LAN, which is most of what the test
// console is used for. Servers returns whatever it managed alongside the
// error, so the degraded list is the right thing to hand back either way.
func iceOrEmpty(ctx context.Context, p signal.ICEProvider, identity string) []signal.ICEServer {
	servers, _ := p.Servers(ctx, identity)
	return servers
}

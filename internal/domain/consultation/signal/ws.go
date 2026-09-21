package signal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

// Socket timing.
const (
	// pongWait is how long a peer may go silent before the hub gives up on it.
	// Long enough to survive a mobile handover, short enough that a peer whose
	// phone went into a tunnel is declared gone while the other end still
	// cares.
	pongWait = 60 * time.Second
	// pingPeriod must be comfortably less than pongWait, or the hub times out
	// peers that were about to answer. It is also what keeps a load balancer
	// or mobile NAT from reaping an idle socket mid-consultation: a signalling
	// channel is legitimately silent for minutes once the call is up.
	pingPeriod = 25 * time.Second
	// writeWait bounds a single frame write.
	writeWait = 10 * time.Second
	// readLimit caps one client frame. An SDP offer with a dozen codecs runs
	// to a few kilobytes; 64 KiB is generous for that and still refuses a peer
	// trying to use the relay as a file transfer into the other browser.
	readLimit = 64 * 1024
)

// Handler upgrades a request to a signalling socket and runs it.
type Handler struct {
	hub    *Hub
	secret []byte
	log    zerolog.Logger

	// allowedOrigins is checked on upgrade. WebSocket is deliberately outside
	// the same-origin policy -- the browser will happily open one from any
	// page and CORS does not apply -- so this is the only origin control there
	// is. Empty means "any", which is right for the test surface and wrong for
	// a consultation.
	allowedOrigins []string

	upgrader websocket.Upgrader
}

// HandlerOptions configures a Handler.
type HandlerOptions struct {
	Hub            *Hub
	Secret         []byte
	Log            zerolog.Logger
	AllowedOrigins []string
}

// NewHandler builds the websocket entry point.
func NewHandler(o HandlerOptions) *Handler {
	h := &Handler{
		hub:            o.Hub,
		secret:         o.Secret,
		log:            o.Log,
		allowedOrigins: o.AllowedOrigins,
	}
	h.upgrader = websocket.Upgrader{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
		CheckOrigin:      h.checkOrigin,
	}
	return h
}

func (h *Handler) checkOrigin(r *http.Request) bool {
	if len(h.allowedOrigins) == 0 {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Origin header means no browser, and a non-browser caller was
		// never subject to this control in the first place -- it already had
		// to present a valid room token to get here.
		return true
	}
	return slices.Contains(h.allowedOrigins, origin)
}

// ServeHTTP verifies the room token, upgrades, and runs the read and write
// loops until either end goes away.
//
// The token travels in the query string rather than an Authorization header
// because the browser WebSocket API cannot set headers on the opening
// handshake. That makes the token a URL, which means it can land in an access
// log, so tokens minted for this are short-lived and grant exactly one room.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok, err := VerifyToken(h.secret, r.URL.Query().Get("token"))
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, ErrTokenMalformed) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}

	// The socket outlives the request: the server's request timeout would
	// otherwise cancel this context roughly 30 seconds into every call, and
	// the hub work keyed off it -- occupancy claims, bus publishes -- would
	// start failing while the socket itself stayed open and looked healthy.
	ctx := context.WithoutCancel(r.Context())

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written a response by this point.
		h.log.Debug().Err(err).Msg("signal: upgrade failed")
		return
	}

	peer, welcome, err := h.hub.Join(ctx, tok.Room, tok.Identity)
	if err != nil {
		// Sent as a frame rather than refused at the HTTP layer: the upgrade
		// already succeeded, and a client that sees a socket open and then
		// close with no explanation cannot tell "room full" from "server
		// down", which are opposite things to do next.
		_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
		_ = conn.WriteMessage(websocket.TextMessage, errorFrame(CodeRoomFull, err.Error()))
		_ = conn.Close()
		return
	}

	log := h.log.With().Str("room", tok.Room).Str("peer", peer.ID).Logger()
	log.Debug().Msg("signal: peer joined")

	go h.writeLoop(ctx, conn, peer)
	h.readLoop(ctx, conn, peer, welcome, log)

	h.hub.Leave(ctx, peer)
	log.Debug().Msg("signal: peer left")
}

// readLoop delivers the welcome frame, then pumps client frames into the hub
// until the socket closes. It runs on the request goroutine so ServeHTTP does
// not return -- and the connection is not recycled -- while the call is live.
func (h *Handler) readLoop(ctx context.Context, conn *websocket.Conn, peer *Peer, welcome Welcome, log zerolog.Logger) {
	defer func() { _ = conn.Close() }()

	conn.SetReadLimit(readLimit)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	data, err := json.Marshal(welcome)
	if err != nil {
		log.Error().Err(err).Msg("signal: marshal welcome")
		return
	}
	raw, err := json.Marshal(Envelope{Type: TypeWelcome, Data: data})
	if err != nil {
		log.Error().Err(err).Msg("signal: marshal welcome envelope")
		return
	}
	peer.send(raw)

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Debug().Err(err).Msg("signal: read")
			}
			return
		}

		var env Envelope
		if err := json.Unmarshal(message, &env); err != nil {
			peer.send(errorFrame(CodeMalformed, "frame is not valid JSON"))
			continue
		}

		if env.Type == TypePing {
			peer.send(mustFrame(Envelope{Type: TypePong}))
			continue
		}
		if !h.hub.Relay(ctx, peer, env) {
			peer.send(errorFrame(CodeUnknownType, "this server does not relay frames of type "+env.Type))
		}
	}
}

// writeLoop drains the peer's queue onto the socket and keeps it alive.
//
// One goroutine owns every write, including the pings: a websocket connection
// tolerates exactly one concurrent writer, and a ping sent from a timer while
// the queue drains is the classic way to corrupt a stream.
func (h *Handler) writeLoop(ctx context.Context, conn *websocket.Conn, peer *Peer) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case raw, ok := <-peer.Out():
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
				_ = conn.Close()
				return
			}
		case <-ticker.C:
			// The same tick refreshes the occupancy lease. Coupling them is
			// deliberate: the lease says "this peer is still here" and the
			// ping is the only thing that actually establishes that. A
			// separate timer would let the two disagree, and the direction
			// they disagree in decides whether the stale sweeper ends a live
			// call or refuses to end a dead one. See occupancyTTL.
			h.hub.Touch(ctx, peer)

			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				_ = conn.Close()
				return
			}
		case <-peer.Done():
			// CloseRoom (and similar) queues a last frame then signals Done.
			// Both channels are then ready; picking Done first used to drop
			// TypeRoomClosed and the client only saw a normal close. Drain
			// anything already queued so they see why the socket is going
			// away, then close cleanly.
			for {
				select {
				case raw, ok := <-peer.Out():
					if !ok {
						writeNormalClose(conn)
						return
					}
					_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
					if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
						_ = conn.Close()
						return
					}
				default:
					writeNormalClose(conn)
					return
				}
			}
		}
	}
}

func writeNormalClose(conn *websocket.Conn) {
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	_ = conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	_ = conn.Close()
}

func mustFrame(env Envelope) []byte {
	raw, _ := json.Marshal(env)
	return raw
}

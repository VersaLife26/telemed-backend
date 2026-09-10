// Package testkit assembles the developer test surface into a modular.Module.
//
// WHAT THIS IS FOR
// Two things that are otherwise impossible to exercise without standing up
// third-party infrastructure:
//
//  1. The platform's own WebRTC stack. A signalling room can be created and
//     joined here with no appointment, no consultation row and no login, which
//     is what lets two browser tabs (or a laptop and a phone) place a real
//     peer-to-peer call against the real signalling server.
//  2. The outbox. Every OTP and every email is captured rather than delivered
//     while test mode is on, and read back through here -- so a developer sees
//     the six digits without an SMS account and the rendered email body
//     without an SMTP server.
//
// WHY IT IS A MODULE AND NOT A DEBUG HANDLER SOMEWHERE
// Because it is mounted, or not, by exactly one decision in one place
// (config.Base.TestModeEnabled), the same way a domain is. A debug handler
// bolted onto an existing module is one `if` away from being reachable in an
// environment that should not have it, and the `if` is somewhere nobody reads.
//
// THE SECURITY POSITION, STATED PLAINLY
// This surface is unauthenticated and it hands back plaintext OTP codes for
// any phone number that has been sent one. That is a complete account takeover
// of every account on the platform, so the only thing standing between it and
// disaster is that it is never mounted in production. TestModeEnabled makes
// the production environment override the flag rather than merely default it,
// because a default is a thing a copied .env file quietly loses.
package testkit

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"telemed/internal/domain/consultation/signal"
	"telemed/internal/platform/modular"
	"telemed/internal/platform/testkit"
)

// Domain is the name this module registers under, and the upstream name the
// edge route table dispatches to.
const Domain = "testkit"

// Options are the pieces the composer hands this module.
type Options struct {
	// Outbox is the shared capture buffer the user and notification domains
	// write into.
	Outbox *testkit.Outbox
	// SignalSecret signs room tokens. It is the same secret the signalling
	// handler verifies with.
	SignalSecret []byte
	// ICE is the STUN/TURN configuration handed to browsers.
	ICE signal.ICEProvider
	// AllowedOrigins restricts the signalling socket's Origin header. Empty
	// means any, which is what the test surface wants: the page is opened
	// from a laptop, a phone, and whatever port `next dev` picked today.
	AllowedOrigins []string
	// Hub is the consultation domain's signalling hub, when that domain is
	// loaded in this process.
	//
	// Handed over rather than built here so /test/rooms exercises the REAL
	// hub. Two hubs in one process would each be correct about their own
	// rooms and useless for testing the one that carries consultations --
	// and the test surface exists precisely to exercise that stack.
	//
	// Nil when the consultation domain is not enabled, in which case testkit
	// stands up its own so the WebRTC page still works.
	Hub *signal.Hub
	// Env is the environment name, echoed back so the test page can show
	// which deployment it is talking to.
	Env string
	// Version is the build version, echoed for the same reason.
	Version string
}

// New assembles the test surface.
func New(ctx context.Context, deps modular.Deps, o Options) (*modular.Module, error) {
	if len(o.SignalSecret) == 0 {
		return nil, fmt.Errorf("testkit: signal secret is empty")
	}

	log := deps.Log.With().Str("domain", Domain).Logger()

	hub := o.Hub
	var bus *signal.Bus
	if hub == nil {
		// The consultation domain is not in this process. Stand up a private
		// hub so the WebRTC page still works on its own.
		if rc, ok := deps.RedisClient(); ok {
			bus = signal.NewBus(rc, log)
		}
		hub = signal.NewHub(ctx, signal.HubOptions{
			Log: log,
			Bus: bus,
			// Rebuilt per join so a REST-minted TURN credential is fresh
			// rather than one that expired while the process was up.
			ICEServers: func(ctx context.Context, identity string) []signal.ICEServer {
				servers, err := o.ICE.Servers(ctx, identity)
				if err != nil {
					log.Warn().Err(err).Msg("testkit: could not mint ICE servers; the client falls back to STUN")
				}
				return servers
			},
		})
		log.Info().Msg("testkit: consultation domain not loaded; using a private signalling hub")
	}

	ws := signal.NewHandler(signal.HandlerOptions{
		Hub:            hub,
		Secret:         o.SignalSecret,
		Log:            log,
		AllowedOrigins: o.AllowedOrigins,
	})

	h := &handler{
		outbox: o.Outbox,
		hub:    hub,
		secret: o.SignalSecret,
		ice:    o.ICE,
		env:    o.Env,
		ver:    o.Version,
	}

	m := &modular.Module{
		Name: Domain,
		API: func(r chi.Router) {
			r.Route("/test", func(r chi.Router) {
				r.Get("/status", h.status)

				r.Get("/outbox", h.listOutbox)
				r.Delete("/outbox", h.clearOutbox)
				r.Get("/outbox/latest", h.latestOutbox)

				r.Post("/rooms", h.createRoom)
				r.Get("/rooms/{room}", h.roomStatus)
				r.Delete("/rooms/{room}", h.closeRoom)

				r.Get("/ice-servers", h.iceServers)
			})
		},
	}

	// The signalling socket is deliberately NOT under API.
	//
	// A websocket hijacks the connection, and everything the shared router
	// wraps a request in assumes it will not: the 30-second request timeout
	// would cancel the context a few seconds into every call, the compression
	// middleware wraps the ResponseWriter, and the edge's per-domain in-flight
	// semaphore would be held for the socket's entire lifetime -- two
	// consultations would consume two slots of a budget sized for requests
	// measured in milliseconds. It is mounted raw instead; see
	// server.Options.RawHandlers.
	m.Raw = map[string]http.Handler{signalPath: ws}

	m.Closers = append(m.Closers, func() {
		if bus != nil {
			_ = bus.Close()
		}
	})

	// Probed once at boot rather than read off a config struct: with a hosted
	// TURN provider "configured" and "actually mints a credential" are
	// different things, and the second is the one worth logging.
	iceServers, iceErr := o.ICE.Servers(ctx, "boot-probe")
	log.Warn().
		Bool("outbox", o.Outbox != nil).
		Int("ice_servers", len(iceServers)).
		Msg("TEST MODE IS ON: /api/v1/test/* is mounted and requires no authentication")

	if iceErr != nil {
		log.Error().Err(iceErr).Msg("could not mint ICE servers at boot")
	}
	if !hasTURN(iceServers) {
		log.Warn().Msg(
			"no TURN relay available: peer-to-peer calls will fail behind symmetric or carrier-grade NAT " +
				"(most Sri Lankan mobile networks) -- configure TURN before this carries real consultations)")
	}

	return m, nil
}

// signalPath is where the raw websocket handler is mounted.
const signalPath = "/test/ws/signal"

// SignalPath is exported so the composer can log it and the route-table
// contract test can assert it is not in the table.
func SignalPath() string { return signalPath }

// roomTokenTTL bounds a minted test room token. Short, because the token is
// carried in a URL query string (the browser WebSocket API cannot set headers)
// and therefore lands in access logs.
const roomTokenTTL = 30 * time.Minute

// normaliseRoom bounds a caller-supplied room name. Room names become Redis
// keys and pub/sub channel names, so a name with a newline or an unbounded
// length is a caller shaping this process's keyspace.
func normaliseRoom(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > 64 {
		raw = raw[:64]
	}
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// hasTURN reports whether any minted server is actually a relay. A list of
// STUN servers is not a fallback for TURN; it is the configuration that fails
// for exactly the users TURN exists for.
func hasTURN(servers []signal.ICEServer) bool {
	for _, s := range servers {
		for _, u := range s.URLs {
			if strings.HasPrefix(u, "turn:") || strings.HasPrefix(u, "turns:") {
				return true
			}
		}
	}
	return false
}

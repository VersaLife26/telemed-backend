package signal

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

// Bus bridges one logical room across processes.
//
// WHY THIS IS NEEDED AT ALL
// A signalling room is two long-lived sockets, and nothing routes them to the
// same replica. Behind two pods, a doctor and a patient joining the same
// consultation land on different processes roughly half the time, each sees an
// empty room, and neither call ever connects. That failure is invisible in
// development, where there is one process, and total in production.
//
// WHAT IT CARRIES
// Two things, and they are separate concerns that happen to share a Redis:
// the relay (a pub/sub channel per room, so a frame published by one process
// reaches peers held by another) and occupancy (a hash per room, so the
// two-peer cap is global rather than per-process). Occupancy is claimed with a
// Lua script because "count, then decide, then insert" across two round trips
// admits a third peer whenever two join at once.
type Bus struct {
	client redis.UniversalClient
	log    zerolog.Logger
	ttl    time.Duration

	hub *Hub

	mu     sync.Mutex
	sub    *redis.PubSub
	closed bool
}

// busFrame is what travels on a room's pub/sub channel.
type busFrame struct {
	// Origin is the publishing process's instance id. A process ignores its
	// own frames: it already delivered them locally, and delivering them twice
	// would duplicate every ICE candidate.
	Origin string `json:"o"`
	// Except is the peer id the frame must not be delivered back to.
	Except string          `json:"x"`
	Raw    json.RawMessage `json:"m,omitempty"`

	// Op turns a frame into a control instruction rather than something to
	// relay. Empty is the ordinary relay path.
	//
	// Control frames exist because closing a room used to be half-remote: the
	// room-closed ENVELOPE reached the far peer, so its client hung up, but
	// the process holding that peer still had it in its room map while the
	// occupancy hash had already been deleted underneath it. Convergence
	// depended on the far client behaving.
	Op string `json:"op,omitempty"`
	// Identity is the target of opKick.
	Identity string `json:"id,omitempty"`
}

const (
	opClose = "close"
	opKick  = "kick"
)

// occupancyTTL bounds how long a claim outlives the process that made it.
//
// This is a LEASE, not a claim, and the difference matters. It used to be four
// hours and was set only on join, never refreshed -- which made the hash
// useless as a liveness signal in both directions: a kill -9'd process left a
// phantom participant for four hours, and any call LONGER than four hours had
// its occupancy expire while both parties were still talking.
//
// Two minutes, refreshed from the websocket's own 25-second ping tick (see
// Hub.Touch), makes the hash accurate to within about half a minute. That is
// what lets ListParticipants be trusted, and ListParticipants is the only
// thing standing between the stale sweeper and ending live consultations.
//
// Changing this without changing the ping period, or vice versa, breaks calls.
// The TTL must be several ping periods so one dropped refresh is survivable.
const occupancyTTL = 2 * time.Minute

// claimScript inserts identity into the room hash if the room has room for it,
// atomically. Returns 1 on success, 0 when the room is full.
//
// An identity already in the hash is always re-admitted regardless of
// occupancy: that is a reconnect, not a third participant, and refusing it
// would mean a dropped socket locks its own owner out.
var claimScript = redis.NewScript(`
local existing = redis.call('HGET', KEYS[1], ARGV[1])
if existing == false and redis.call('HLEN', KEYS[1]) >= tonumber(ARGV[3]) then
  return 0
end
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[4])
return 1
`)

// NewBus builds a Bus over an existing Redis client.
func NewBus(client redis.UniversalClient, log zerolog.Logger) *Bus {
	return &Bus{client: client, log: log, ttl: occupancyTTL}
}

// attach binds the bus to the hub it delivers into and starts the receive
// loop. Called by NewHub.
func (b *Bus) attach(ctx context.Context, h *Hub) {
	b.hub = h
	b.mu.Lock()
	// Subscribed with no channels: rooms are added and removed as they come
	// and go, so a process only carries traffic for rooms it actually holds a
	// peer in.
	b.sub = b.client.Subscribe(ctx)
	sub := b.sub
	b.mu.Unlock()

	go b.receive(sub)
}

func (b *Bus) receive(sub *redis.PubSub) {
	for msg := range sub.Channel() {
		var frame busFrame
		if err := json.Unmarshal([]byte(msg.Payload), &frame); err != nil {
			b.log.Warn().Err(err).Str("channel", msg.Channel).Msg("signal: bad bus frame")
			continue
		}
		if frame.Origin == b.hub.instance {
			continue
		}
		roomName := roomFromChannel(msg.Channel)
		switch frame.Op {
		case opClose:
			b.hub.closeLocal(roomName)
		case opKick:
			b.hub.removeLocal(roomName, frame.Identity)
		default:
			b.hub.deliverLocal(roomName, frame.Except, frame.Raw)
		}
	}
}

// Close stops the receive loop.
func (b *Bus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.sub == nil {
		return nil
	}
	b.closed = true
	return b.sub.Close()
}

func (b *Bus) publish(ctx context.Context, roomName, exceptID string, raw []byte) {
	payload, err := json.Marshal(busFrame{Origin: b.hub.instance, Except: exceptID, Raw: raw})
	if err != nil {
		b.log.Error().Err(err).Msg("signal: marshal bus frame")
		return
	}
	if err := b.client.Publish(ctx, relayChannel(roomName), payload).Err(); err != nil {
		// Not fatal: the local peer was already delivered to directly, so a
		// Redis blip degrades a two-process room rather than breaking a
		// one-process one.
		b.log.Warn().Err(err).Str("room", roomName).Msg("signal: publish to bus")
	}
}

// publishControl sends an instruction rather than something to relay.
func (b *Bus) publishControl(ctx context.Context, roomName, op, identity string) {
	payload, err := json.Marshal(busFrame{Origin: b.hub.instance, Op: op, Identity: identity})
	if err != nil {
		b.log.Error().Err(err).Msg("signal: marshal control frame")
		return
	}
	if err := b.client.Publish(ctx, relayChannel(roomName), payload).Err(); err != nil {
		b.log.Warn().Err(err).Str("room", roomName).Str("op", op).
			Msg("signal: publish control frame")
	}
}

func (b *Bus) subscribe(ctx context.Context, roomName string) {
	b.mu.Lock()
	sub := b.sub
	b.mu.Unlock()
	if sub == nil {
		return
	}
	if err := sub.Subscribe(ctx, relayChannel(roomName)); err != nil {
		b.log.Warn().Err(err).Str("room", roomName).Msg("signal: subscribe to room channel")
	}
}

func (b *Bus) unsubscribe(ctx context.Context, roomName string) {
	b.mu.Lock()
	sub := b.sub
	b.mu.Unlock()
	if sub == nil {
		return
	}
	if err := sub.Unsubscribe(ctx, relayChannel(roomName)); err != nil {
		b.log.Warn().Err(err).Str("room", roomName).Msg("signal: unsubscribe from room channel")
	}
}

func (b *Bus) claim(ctx context.Context, roomName, identity, instance string) (bool, error) {
	n, err := claimScript.Run(ctx, b.client,
		[]string{occupancyKey(roomName)},
		identity, instance, MaxPeersPerRoom, b.ttl.Milliseconds(),
	).Int64()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// touch refreshes an existing lease. It reuses claimScript rather than a bare
// PEXPIRE so a peer whose entry has already expired is re-inserted -- subject
// to the same occupancy cap -- instead of continuing to talk while invisible
// to every other process.
func (b *Bus) touch(ctx context.Context, roomName, identity, value string) error {
	_, err := claimScript.Run(ctx, b.client,
		[]string{occupancyKey(roomName)},
		identity, value, MaxPeersPerRoom, b.ttl.Milliseconds(),
	).Int64()
	return err
}

// Occupants returns the room's occupancy hash: identity -> lease value.
//
// exists is false when the key is absent, which is a genuinely different
// answer from "present but empty" -- HDEL of the last field deletes the key,
// so an absent key means nobody is in the room, while an error means we do not
// know. Callers that end consultations on an empty result depend on that
// distinction absolutely.
func (b *Bus) Occupants(ctx context.Context, roomName string) (map[string]string, bool, error) {
	members, err := b.client.HGetAll(ctx, occupancyKey(roomName)).Result()
	if err != nil {
		return nil, false, err
	}
	return members, len(members) > 0, nil
}

func (b *Bus) release(ctx context.Context, roomName, identity string) {
	if err := b.client.HDel(ctx, occupancyKey(roomName), identity).Err(); err != nil {
		b.log.Warn().Err(err).Str("room", roomName).Msg("signal: release occupancy")
	}
}

func (b *Bus) releaseRoom(ctx context.Context, roomName string) {
	if err := b.client.Del(ctx, occupancyKey(roomName)).Err(); err != nil {
		b.log.Warn().Err(err).Str("room", roomName).Msg("signal: clear occupancy")
	}
}

// remotePresent reports whether the room's occupancy hash names anyone other
// than identity. It answers "is the far side already here" for a peer that may
// be on a different process, which decides whether the arriving client offers
// immediately or waits for a peer-joined frame.
func (b *Bus) remotePresent(ctx context.Context, roomName, identity string) bool {
	members, err := b.client.HKeys(ctx, occupancyKey(roomName)).Result()
	if err != nil {
		b.log.Warn().Err(err).Str("room", roomName).Msg("signal: read occupancy")
		return false
	}
	for _, m := range members {
		if m != identity {
			return true
		}
	}
	return false
}

const relayPrefix = "signal:relay:"

func relayChannel(roomName string) string { return relayPrefix + roomName }
func occupancyKey(roomName string) string { return "signal:room:" + roomName }

func roomFromChannel(channel string) string {
	if len(channel) > len(relayPrefix) && channel[:len(relayPrefix)] == relayPrefix {
		return channel[len(relayPrefix):]
	}
	return channel
}

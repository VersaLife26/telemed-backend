package signal

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// MaxPeersPerRoom is the two-party cap. See the package comment for why this
// is a hard limit rather than a default.
const MaxPeersPerRoom = 2

// outboxDepth is how many frames may queue for one peer before the hub gives
// up on it. Signalling frames are small and bursty -- a handshake is a handful
// of them, then a trickle of ICE -- so a peer that is 64 frames behind is not
// slow, it is gone, and holding the room open for it delays the other side's
// reconnect.
const outboxDepth = 64

// ErrRoomFull is returned by Join when a third participant tries to enter.
var ErrRoomFull = errors.New("signal: room already has two participants")

// Hub owns the live signalling rooms in this process.
type Hub struct {
	mu    sync.Mutex
	rooms map[string]*room

	log      zerolog.Logger
	bus      *Bus
	instance string
	ice      func(ctx context.Context, identity string) []ICEServer

	onPeerChange func(ctx context.Context, roomName, identity string, joined bool)
}

type room struct {
	name  string
	peers map[string]*Peer
}

// Peer is one connected client's handle on the hub. The websocket layer owns
// the socket; this owns the queue of frames destined for it.
type Peer struct {
	ID       string
	Identity string
	Room     string
	// JoinedAt is when the hub admitted this peer. Reported by Peers and, via
	// the occupancy lease, by a VideoProvider's ListParticipants.
	JoinedAt time.Time

	out    chan []byte
	closed chan struct{}
	once   sync.Once
}

// PeerInfo is a snapshot of one locally-held peer.
//
// LOCAL. It describes this process's room map and nothing else, so it is a
// diagnostic and a single-process fallback, never the authoritative answer to
// "who is in this room" when a Bus is configured. Bus.Occupants is that.
type PeerInfo struct {
	ID       string
	Identity string
	JoinedAt time.Time
}

// Out is the channel the socket's write loop drains.
func (p *Peer) Out() <-chan []byte { return p.out }

// Done is closed when the hub has finished with this peer, either because it
// left or because the room was closed underneath it.
func (p *Peer) Done() <-chan struct{} { return p.closed }

func (p *Peer) close() {
	p.once.Do(func() { close(p.closed) })
}

// send queues raw for this peer, dropping it and closing the peer if the queue
// is already full. Never blocks: it is called while another peer's read loop is
// waiting on it, and one stalled client must not wedge the other.
func (p *Peer) send(raw []byte) {
	select {
	case <-p.closed:
	case p.out <- raw:
	default:
		p.close()
	}
}

// HubOptions configures a Hub.
type HubOptions struct {
	Log zerolog.Logger
	// Bus bridges rooms whose two peers landed on different processes. Nil
	// means single-process: rooms are memory-local, which is correct for one
	// replica and silently wrong for more than one. See Bus.
	Bus *Bus
	// ICEServers is called per join rather than stored, so a deployment that
	// mints short-lived TURN credentials returns fresh ones each time.
	//
	// identity is passed so a REST-style TURN credential can be scoped to the
	// peer it was minted for, which is what makes a leaked one traceable.
	ICEServers func(ctx context.Context, identity string) []ICEServer

	// OnPeerChange is called when a peer joins or leaves a room.
	//
	// It exists because dropping the SFU dropped its participant webhooks with
	// it. Without this, consultation_participants.left_at is never written by
	// anything and every ended consultation shows all participants still
	// joined, forever.
	//
	// Called off the hot path, without the hub lock held. Must not block.
	OnPeerChange func(ctx context.Context, roomName, identity string, joined bool)
}

// NewHub builds an empty hub.
//
// ctx bounds the bus subscription, so cancelling it stops the receive loop
// along with the rest of the process rather than leaving a goroutine holding a
// Redis connection open past shutdown.
func NewHub(ctx context.Context, o HubOptions) *Hub {
	h := &Hub{
		rooms:        map[string]*room{},
		log:          o.Log,
		bus:          o.Bus,
		instance:     uuid.NewString(),
		ice:          o.ICEServers,
		onPeerChange: o.OnPeerChange,
	}
	if h.ice == nil {
		h.ice = func(context.Context, string) []ICEServer { return nil }
	}
	if h.bus != nil {
		h.bus.attach(ctx, h)
	}
	return h
}

// Join admits identity to a room and returns its Peer plus the welcome frame
// the caller must deliver first.
//
// Rejoining under an identity already in the room REPLACES the old peer rather
// than being refused, and that is what makes reconnection work at all: a
// client whose socket dropped has no way to tell the hub its old one is dead,
// and the hub's own read loop may not have noticed yet. Without replacement,
// every network blip would lock a patient out of their own consultation for as
// long as the dead socket took to time out.
func (h *Hub) Join(ctx context.Context, roomName, identity string) (*Peer, Welcome, error) {
	p := &Peer{
		ID:       uuid.NewString(),
		Identity: identity,
		Room:     roomName,
		JoinedAt: time.Now().UTC(),
		out:      make(chan []byte, outboxDepth),
		closed:   make(chan struct{}),
	}

	h.mu.Lock()
	r, ok := h.rooms[roomName]
	if !ok {
		r = &room{name: roomName, peers: map[string]*Peer{}}
		h.rooms[roomName] = r
	}

	var evicted *Peer
	for _, existing := range r.peers {
		if existing.Identity == identity {
			evicted = existing
			delete(r.peers, existing.ID)
			break
		}
	}
	if len(r.peers) >= MaxPeersPerRoom {
		h.mu.Unlock()
		return nil, Welcome{}, ErrRoomFull
	}

	// The peer already present is the polite one, so the arriving peer -- the
	// one with something to say -- wins a glare. An empty room means this peer
	// is the one that will be waiting, so it takes the polite role itself.
	peerPresent := len(r.peers) > 0
	r.peers[p.ID] = p
	fresh := len(r.peers) == 1
	h.mu.Unlock()

	if evicted != nil {
		evicted.close()
	}

	// Claiming occupancy on the bus can fail (another process already holds
	// both slots), so it happens after the local bookkeeping and rolls it back
	// on refusal rather than leaving a phantom peer behind.
	if h.bus != nil {
		claimed, err := h.bus.claim(ctx, roomName, identity, h.leaseValue(p))
		if err != nil {
			h.log.Warn().Err(err).Str("room", roomName).Msg("signal: occupancy claim failed, admitting anyway")
		} else if !claimed {
			h.Leave(ctx, p)
			return nil, Welcome{}, ErrRoomFull
		}
		if fresh {
			h.bus.subscribe(ctx, roomName)
		}
		peerPresent = peerPresent || h.bus.remotePresent(ctx, roomName, identity)
	}

	h.broadcast(ctx, roomName, p.ID, Envelope{Type: TypePeerJoined, From: p.ID})
	h.notifyPeerChange(ctx, roomName, identity, true)

	return p, Welcome{
		PeerID:      p.ID,
		Room:        roomName,
		Polite:      !peerPresent,
		PeerPresent: peerPresent,
		ICEServers:  h.ice(ctx, identity),
	}, nil
}

// leaseValue is what a peer writes into the occupancy hash: which process
// holds it, and when it joined. The join time travels here rather than being
// looked up because the process asking (ListParticipants) is frequently not
// the process holding the peer.
func (h *Hub) leaseValue(p *Peer) string {
	return h.instance + "|" + strconv.FormatInt(p.JoinedAt.Unix(), 10)
}

// ParseLeaseValue splits a value written by leaseValue. A value it does not
// recognise yields a zero time rather than an error: the identity is the part
// that matters, and a caller must not lose a participant because its join time
// was unreadable.
func ParseLeaseValue(v string) (instance string, joinedAt time.Time) {
	i := strings.IndexByte(v, '|')
	if i < 0 {
		return v, time.Time{}
	}
	instance = v[:i]
	if unix, err := strconv.ParseInt(v[i+1:], 10, 64); err == nil {
		joinedAt = time.Unix(unix, 0).UTC()
	}
	return instance, joinedAt
}

// Touch refreshes a peer's occupancy lease. Called from the socket's ping
// tick; see occupancyTTL for why the two periods are coupled.
func (h *Hub) Touch(ctx context.Context, p *Peer) {
	if h.bus == nil || p == nil {
		return
	}
	if err := h.bus.touch(ctx, p.Room, p.Identity, h.leaseValue(p)); err != nil {
		// Not fatal on its own: the lease has room for several missed
		// refreshes. Sustained failure is what matters, and that shows up as
		// the room going quiet.
		h.log.Warn().Err(err).Str("room", p.Room).Msg("signal: occupancy lease refresh failed")
	}
}

// Peers snapshots the peers this process holds in a room.
func (h *Hub) Peers(roomName string) []PeerInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	if !ok {
		return nil
	}
	out := make([]PeerInfo, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, PeerInfo{ID: p.ID, Identity: p.Identity, JoinedAt: p.JoinedAt})
	}
	return out
}

func (h *Hub) notifyPeerChange(ctx context.Context, roomName, identity string, joined bool) {
	if h.onPeerChange != nil {
		h.onPeerChange(ctx, roomName, identity, joined)
	}
}

// Leave removes p from its room and tells the far side it has gone.
func (h *Hub) Leave(ctx context.Context, p *Peer) {
	h.mu.Lock()
	r, ok := h.rooms[p.Room]
	if !ok {
		h.mu.Unlock()
		p.close()
		return
	}
	// Guarded: a reconnect under the same identity has already replaced this
	// peer in the map, and deleting by room+identity would evict the LIVE
	// socket when the dead one's read loop finally unblocks.
	if current, present := r.peers[p.ID]; !present || current != p {
		h.mu.Unlock()
		p.close()
		return
	}
	delete(r.peers, p.ID)
	empty := len(r.peers) == 0
	if empty {
		delete(h.rooms, p.Room)
	}
	h.mu.Unlock()

	p.close()

	if h.bus != nil {
		h.bus.release(ctx, p.Room, p.Identity)
		if empty {
			h.bus.unsubscribe(ctx, p.Room)
		}
	}
	h.broadcast(ctx, p.Room, p.ID, Envelope{Type: TypePeerLeft, From: p.ID})
	h.notifyPeerChange(ctx, p.Room, p.Identity, false)
}

// closeLocal evicts this process's peers from a room without touching the bus.
// It is the local half of CloseRoom, shared with the control frame a remote
// CloseRoom publishes.
func (h *Hub) closeLocal(roomName string) {
	h.mu.Lock()
	r, ok := h.rooms[roomName]
	if ok {
		delete(h.rooms, roomName)
	}
	h.mu.Unlock()
	if !ok {
		return
	}
	for _, p := range r.peers {
		p.close()
	}
}

// removeLocal evicts one identity from a room in this process.
func (h *Hub) removeLocal(roomName, identity string) bool {
	h.mu.Lock()
	r, ok := h.rooms[roomName]
	if !ok {
		h.mu.Unlock()
		return false
	}
	var gone []*Peer
	for id, p := range r.peers {
		if p.Identity == identity {
			gone = append(gone, p)
			delete(r.peers, id)
		}
	}
	empty := len(r.peers) == 0
	if empty {
		delete(h.rooms, roomName)
	}
	h.mu.Unlock()

	// Outside the lock: close() runs the peer's once and unblocks its write
	// loop, which may take other locks.
	for _, p := range gone {
		p.close()
	}
	return len(gone) > 0
}

// RemoveIdentity evicts one participant from a room, in this process and in
// whichever other process holds them.
//
// Returns true when a socket in THIS process was closed. A false return is not
// a failure: the peer may be held elsewhere, and the control frame published
// below is what reaches it.
func (h *Hub) RemoveIdentity(ctx context.Context, roomName, identity string) bool {
	closed := h.removeLocal(roomName, identity)
	if h.bus != nil {
		h.bus.release(ctx, roomName, identity)
		h.bus.publishControl(ctx, roomName, opKick, identity)
	}
	h.notifyPeerChange(ctx, roomName, identity, false)
	return closed
}

// Relay forwards one client frame to the far peer.
//
// Returns false when the frame's type is not on the relay allowlist, which the
// caller reports back to the sender as an error rather than dropping silently:
// a client sending a type the server does not carry has a bug, and a silent
// drop turns it into an unexplained hang.
func (h *Hub) Relay(ctx context.Context, p *Peer, env Envelope) bool {
	if !relayable[env.Type] {
		return false
	}
	// Stamped, not trusted: whatever the client put in From is discarded.
	env.From = p.ID
	h.broadcast(ctx, p.Room, p.ID, env)
	return true
}

// PeerCount reports how many peers this process holds for a room. It counts
// local sockets only and is for diagnostics, not for admission control --
// Join's occupancy claim is what actually bounds a room.
func (h *Hub) PeerCount(roomName string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.rooms[roomName]; ok {
		return len(r.peers)
	}
	return 0
}

// CloseRoom evicts every peer in a room. It is how the consultation domain
// ends a call from the server side.
func (h *Hub) CloseRoom(ctx context.Context, roomName string) {
	// The envelope tells the far CLIENT to hang up; the control frame tells
	// the far PROCESS to drop the peer. Both are needed: without the control
	// frame the remote process keeps the peer in its room map after the
	// occupancy hash has been deleted underneath it, and convergence depends
	// on the client behaving.
	h.broadcast(ctx, roomName, "", Envelope{Type: TypeRoomClosed})

	h.closeLocal(roomName)

	if h.bus != nil {
		h.bus.publishControl(ctx, roomName, opClose, "")
		h.bus.releaseRoom(ctx, roomName)
		h.bus.unsubscribe(ctx, roomName)
	}
}

// broadcast delivers env to every peer in a room except exceptID, locally and
// (when configured) on the bus.
func (h *Hub) broadcast(ctx context.Context, roomName, exceptID string, env Envelope) {
	raw, err := json.Marshal(env)
	if err != nil {
		h.log.Error().Err(err).Str("type", env.Type).Msg("signal: marshal frame")
		return
	}
	h.deliverLocal(roomName, exceptID, raw)
	if h.bus != nil {
		h.bus.publish(ctx, roomName, exceptID, raw)
	}
}

// deliverLocal fans raw out to this process's peers in a room.
func (h *Hub) deliverLocal(roomName, exceptID string, raw []byte) {
	h.mu.Lock()
	r, ok := h.rooms[roomName]
	if !ok {
		h.mu.Unlock()
		return
	}
	targets := make([]*Peer, 0, len(r.peers))
	for id, p := range r.peers {
		if id == exceptID {
			continue
		}
		targets = append(targets, p)
	}
	h.mu.Unlock()

	// Outside the lock: send() is non-blocking but may close a peer, and
	// closing under h.mu invites a lock-ordering problem the day a close hook
	// wants to touch the room map.
	for _, p := range targets {
		p.send(raw)
	}
}

// errorFrame builds a TypeError frame ready to hand to Peer.send.
func errorFrame(code, msg string) []byte {
	data, _ := json.Marshal(errorData{Code: code, Message: msg})
	raw, _ := json.Marshal(Envelope{Type: TypeError, Data: data})
	return raw
}

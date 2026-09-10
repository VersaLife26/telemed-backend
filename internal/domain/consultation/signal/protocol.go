// Package signal is the signalling half of the platform's own WebRTC stack.
//
// WHAT IT IS AND IS NOT
// It carries SDP offers, SDP answers and ICE candidates between exactly two
// peers, and nothing else. It never parses an SDP, never terminates media and
// never sees a video frame -- once the two browsers have exchanged candidates
// the media path is directly between them (or via TURN), and this server has
// no further part in the call. That is the whole difference from an SFU like
// LiveKit, and it is why a room here costs a map entry and two goroutines
// rather than a transcoding pipeline.
//
// WHY TWO PEERS AND NOT N
// A consultation is a doctor and a patient. Mesh WebRTC degrades quadratically
// with participants, so "we could allow three" is not a small extension -- it
// is the point at which an SFU becomes the right answer again. The cap is
// enforced rather than assumed, and a third joiner is refused rather than
// silently degrading the other two.
package signal

import (
	"encoding/json"
	"fmt"
)

// Envelope is every frame on the wire, in both directions.
//
// Data is json.RawMessage rather than a typed union on purpose: the hub is a
// relay, and an SDP or ICE candidate it decoded would be an SDP or ICE
// candidate it could get wrong. Only Type is inspected -- to decide whether a
// frame is relayable at all -- and the payload is forwarded byte for byte.
type Envelope struct {
	Type string `json:"type"`
	// From is the sending peer's id, stamped by the hub on relay. A client
	// that sets it is overwritten: it is the hub's statement about who sent
	// something, not the sender's claim about themselves.
	From string          `json:"from,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Frame types a client may send.
const (
	// TypeOffer, TypeAnswer and TypeICE are the WebRTC handshake proper.
	TypeOffer  = "offer"
	TypeAnswer = "answer"
	TypeICE    = "ice"
	// TypeBye is a clean hangup: the sender is leaving on purpose, as opposed
	// to a socket that dropped. The difference matters to the far side, which
	// should retry an unexpected drop and must not retry a hangup.
	TypeBye = "bye"
	// TypeRecordingState tells the far side that this peer started or stopped
	// a local MediaRecorder. Recording is client-side in a peer-to-peer call
	// (there is no server in the media path to record it), so the only way the
	// patient learns the doctor pressed record is a relayed message like this.
	// It is relayed, never trusted: it is a courtesy indicator, and the
	// consent gate that actually authorises recording lives in the
	// consultation domain, not here.
	TypeRecordingState = "recording-state"
	// TypeQuality carries a periodic getStats summary to the far side so each
	// end can show "their connection is poor" rather than only its own.
	TypeQuality = "quality"
	// TypePing keeps intermediaries from idling the socket out. The hub
	// answers with TypePong and does not relay it.
	TypePing = "ping"
)

// Frame types only the hub sends.
const (
	TypeWelcome    = "welcome"
	TypePeerJoined = "peer-joined"
	TypePeerLeft   = "peer-left"
	TypeRoomClosed = "room-closed"
	TypePong       = "pong"
	TypeError      = "error"
)

// relayable is the allowlist of frame types the hub forwards to the far peer.
//
// An allowlist rather than a denylist, and it is the security boundary of this
// package: anything not named here is dropped, so a client cannot invent a
// frame type and have the hub relay it verbatim into the other browser's
// message handler. A new client feature adds an entry here deliberately.
var relayable = map[string]bool{
	TypeOffer:          true,
	TypeAnswer:         true,
	TypeICE:            true,
	TypeBye:            true,
	TypeRecordingState: true,
	TypeQuality:        true,
}

// Welcome is the first frame the hub sends a peer that has joined.
type Welcome struct {
	PeerID string `json:"peer_id"`
	Room   string `json:"room"`
	// Polite decides who yields when both ends offer at the same moment --
	// the "perfect negotiation" pattern from the WebRTC spec. Exactly one peer
	// in a room is polite, and the hub assigns it rather than letting the
	// clients agree, because two clients that both believe they are impolite
	// deadlock on every glare and two that both believe they are polite
	// deadlock on the first one.
	//
	// The peer already in the room is the polite one: it is idle and waiting,
	// so yielding costs it nothing, while the peer that just arrived is the
	// one with something to say.
	Polite bool `json:"polite"`
	// PeerPresent says whether the far side was already in the room at the
	// moment of joining. A client uses it to decide whether to offer now or
	// wait for TypePeerJoined.
	PeerPresent bool        `json:"peer_present"`
	ICEServers  []ICEServer `json:"ice_servers"`
}

// ICEServer is one entry of an RTCConfiguration.iceServers array, shaped
// exactly as the browser expects it so the client can pass it through
// untouched.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// UnmarshalJSON accepts "urls" as either a string or an array of strings.
//
// The WebRTC dictionary allows both, and hosted TURN services use both -- a
// STUN entry commonly arrives as a bare string while a TURN entry arrives as
// an array. Decoding into []string alone fails on the first shape with
// "cannot unmarshal string into Go struct field", which would surface as
// "TURN unavailable" rather than as a parsing bug.
func (s *ICEServer) UnmarshalJSON(raw []byte) error {
	var wire struct {
		URLs       json.RawMessage `json:"urls"`
		Username   string          `json:"username"`
		Credential string          `json:"credential"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	s.Username, s.Credential = wire.Username, wire.Credential
	if len(wire.URLs) == 0 {
		s.URLs = nil
		return nil
	}
	if err := json.Unmarshal(wire.URLs, &s.URLs); err == nil {
		return nil
	}
	var one string
	if err := json.Unmarshal(wire.URLs, &one); err != nil {
		return fmt.Errorf("signal: ice server urls is neither a string nor an array: %w", err)
	}
	s.URLs = []string{one}
	return nil
}

// errorData is the payload of a TypeError frame.
type errorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes carried by TypeError.
const (
	CodeRoomFull    = "ROOM_FULL"
	CodeUnknownType = "UNKNOWN_TYPE"
	CodeMalformed   = "MALFORMED"
	CodeNoPeer      = "NO_PEER"
)

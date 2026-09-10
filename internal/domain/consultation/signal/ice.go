package signal

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // G505: the TURN REST API spec that coturn implements mandates HMAC-SHA1; see turnRESTCredential.
	"encoding/base64"
	"strconv"
	"time"
)

// ICEConfig is where a deployment's STUN and TURN servers are configured.
//
// STUN ALONE IS NOT ENOUGH, AND THIS IS THE PLACE THAT SAYS SO
// STUN only tells a peer its own public address. That is sufficient when at
// least one end has a NAT that reuses a port across destinations. It is NOT
// sufficient behind symmetric NAT or carrier-grade NAT, where the mapping
// differs per destination and the address STUN reported is useless to the far
// side -- those calls need a TURN relay or they do not connect at all.
//
// That matters more here than it would elsewhere: most of this platform's
// users reach it over Sri Lankan mobile networks, which are heavily CGNAT'd.
// A deployment with TURNURLs empty will work perfectly in development, on one
// LAN, and on the /test page, and will then fail for a real and non-trivial
// share of actual consultations. Shipping TURN is not an optimisation to do
// later; it is the difference between a call connecting and not.
type ICEConfig struct {
	STUNURLs []string
	TURNURLs []string

	// TURNUsername and TURNCredential are the static long-term credential. Fine
	// for a private TURN nobody else can reach; on a public one they are a
	// shared password with no expiry, which is why TURNSecret exists.
	TURNUsername   string
	TURNCredential string

	// TURNSecret enables coturn's REST scheme (its `use-auth-secret` mode):
	// instead of a fixed password, each client is handed a username that
	// encodes an expiry and a credential that is an HMAC of it under this
	// secret. coturn verifies the HMAC itself and needs no per-user state, and
	// a credential that leaks stops working on its own. Set, it takes
	// precedence over the static pair.
	TURNSecret string
	TURNTTL    time.Duration
}

// ICEProvider mints the RTCConfiguration.iceServers array for one peer.
//
// An interface because the two ways to get TURN credentials have genuinely
// different shapes: coturn's REST mode is a local HMAC and cannot fail, while
// a hosted service is a network call that can. Everything downstream takes the
// interface so switching between them is configuration.
type ICEProvider interface {
	Servers(ctx context.Context, identity string) ([]ICEServer, error)
}

// DefaultTURNTTL is how long a minted TURN credential stays valid. It must
// outlast the longest consultation, because a credential that expires
// mid-call takes the relay -- and therefore the call -- down with it.
const DefaultTURNTTL = 12 * time.Hour

// Servers returns the RTCConfiguration.iceServers array for one peer.
//
// identity is folded into a REST credential so a leaked one is traceable and
// scoped; it is not a secret and coturn does not check it against anything.
// It never returns an error: every mode here is local arithmetic. The error
// in the signature belongs to ICEProvider, whose other implementation makes a
// network call.
func (c ICEConfig) Servers(_ context.Context, identity string) ([]ICEServer, error) {
	var out []ICEServer
	if len(c.STUNURLs) > 0 {
		out = append(out, ICEServer{URLs: c.STUNURLs})
	}
	if len(c.TURNURLs) == 0 {
		return out, nil
	}

	turn := ICEServer{URLs: c.TURNURLs}
	switch {
	case c.TURNSecret != "":
		ttl := c.TURNTTL
		if ttl <= 0 {
			ttl = DefaultTURNTTL
		}
		username := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10) + ":" + identity
		turn.Username = username
		turn.Credential = turnRESTCredential(c.TURNSecret, username)
	default:
		turn.Username = c.TURNUsername
		turn.Credential = c.TURNCredential
	}
	return append(out, turn), nil
}

var _ ICEProvider = ICEConfig{}

// turnRESTCredential is base64(HMAC-SHA1(secret, username)), which is what
// coturn's use-auth-secret mode specifies. SHA-1 is not a choice here -- it is
// the algorithm baked into the TURN REST API draft and into coturn's
// verification, so anything else simply fails to authenticate. It is a MAC
// under a secret key rather than a collision-resistance claim, which is the
// use SHA-1 remains sound for.
func turnRESTCredential(secret, username string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

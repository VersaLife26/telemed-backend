package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// cloudflareTURNEndpoint is where credentials are minted. The response is
// already an iceServers array, so nothing here has to know how Cloudflare
// spells a TURN URL.
const cloudflareTURNEndpoint = "https://rtc.live.cloudflare.com/v1/turn/keys/%s/credentials/generate-ice-servers"

// CloudflareTURN mints ICE servers from Cloudflare Realtime TURN.
//
// WHY THIS IS NOT ICEConfig WITH DIFFERENT VALUES
// coturn's REST mode is a local HMAC: the server verifies a credential it
// never issued, so minting one is arithmetic and cannot fail. Cloudflare does
// not implement that scheme. Credentials come from an authenticated HTTP call,
// which means minting can fail, can be slow, and must be cached.
//
// WHY THE CACHE IS NOT AN OPTIMISATION
// Hub.ICEServers is called on EVERY join, and a join includes every signalling
// reconnect -- so an uncached implementation puts a cross-internet API call on
// the recovery path of a call that is already failing, at the exact moment the
// network is least reliable. The cache is what keeps a reconnect local.
type CloudflareTURN struct {
	keyID    string
	apiToken string
	ttl      time.Duration
	// stun is served alongside whatever Cloudflare returns. Cloudflare's own
	// response includes a STUN entry, so this is only a fallback for the
	// degraded path below.
	stun   []string
	client *http.Client
	log    zerolog.Logger

	mu     sync.Mutex
	cached []ICEServer
	until  time.Time
	now    func() time.Time
}

// NewCloudflareTURN builds the provider. ttl is how long Cloudflare is asked
// to make each credential valid; it must outlast the longest consultation,
// because a credential that expires mid-call takes the relay with it.
func NewCloudflareTURN(keyID, apiToken string, ttl time.Duration, stun []string, log zerolog.Logger) *CloudflareTURN {
	if ttl <= 0 {
		ttl = DefaultTURNTTL
	}
	return &CloudflareTURN{
		keyID:    keyID,
		apiToken: apiToken,
		ttl:      ttl,
		stun:     stun,
		// Short, and deliberately so: this call sits between a user pressing
		// "join" and their camera turning on.
		client: &http.Client{Timeout: 5 * time.Second},
		log:    log,
		now:    time.Now,
	}
}

var _ ICEProvider = (*CloudflareTURN)(nil)

type cloudflareICEResponse struct {
	ICEServers json.RawMessage `json:"iceServers"`
}

// Servers returns ICE servers, from cache when it is warm.
//
// identity is not sent: Cloudflare scopes a credential by TTL rather than by
// user, so there is nothing to scope it to and nothing gained by telling a
// third party who is on the call.
func (c *CloudflareTURN) Servers(ctx context.Context, _ string) ([]ICEServer, error) {
	if servers, ok := c.fromCache(); ok {
		return servers, nil
	}

	servers, err := c.fetch(ctx)
	if err != nil {
		// Degrade rather than fail, in a specific order.
		//
		// A slightly stale TURN credential may well still work -- Cloudflare
		// issues them with a long TTL and we refresh at half of it -- whereas
		// returning STUN only definitely fails for the CGNAT users who are the
		// reason TURN is here at all. So a warm-but-expired cache beats
		// nothing, and nothing beats failing the join outright.
		if stale, ok := c.stale(); ok {
			c.log.Warn().Err(err).Msg("signal: cloudflare turn unavailable; using the previous credential")
			return stale, nil
		}
		c.log.Error().Err(err).Msg("signal: cloudflare turn unavailable and no cached credential; " +
			"this call has STUN only and will not connect from behind carrier-grade NAT")
		return c.stunOnly(), err
	}

	c.store(servers)
	return servers, nil
}

func (c *CloudflareTURN) fetch(ctx context.Context) ([]ICEServer, error) {
	body, err := json.Marshal(map[string]any{"ttl": int(c.ttl.Seconds())})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf(cloudflareTURNEndpoint, c.keyID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("signal: cloudflare turn request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("signal: cloudflare turn returned %s", resp.Status)
	}

	var out cloudflareICEResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("signal: decode cloudflare turn response: %w", err)
	}

	// Cloudflare's iceServers entry spells "urls" as either a string or an
	// array depending on the entry, which is legal in the WebRTC dictionary
	// and awkward in Go. ICEServer handles both on the way in.
	var servers []ICEServer
	if err := json.Unmarshal(out.ICEServers, &servers); err != nil {
		// A single object rather than an array.
		var one ICEServer
		if errOne := json.Unmarshal(out.ICEServers, &one); errOne != nil {
			return nil, fmt.Errorf("signal: cloudflare turn iceServers shape: %w", err)
		}
		servers = []ICEServer{one}
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("signal: cloudflare turn returned no ice servers")
	}
	return servers, nil
}

func (c *CloudflareTURN) fromCache() ([]ICEServer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached == nil || c.now().After(c.until) {
		return nil, false
	}
	return c.cached, true
}

func (c *CloudflareTURN) stale() ([]ICEServer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cached, c.cached != nil
}

func (c *CloudflareTURN) store(servers []ICEServer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = servers
	// Half the credential's life. Refreshing at the very end would mean every
	// caller between the last mint and the next one races the expiry.
	c.until = c.now().Add(c.ttl / 2)
}

func (c *CloudflareTURN) stunOnly() []ICEServer {
	if len(c.stun) == 0 {
		return nil
	}
	return []ICEServer{{URLs: c.stun}}
}

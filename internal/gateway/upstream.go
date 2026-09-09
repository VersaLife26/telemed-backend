package gateway

import (
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/rs/zerolog"
)

// Upstream bundles everything the gateway needs to talk to one backend
// service: where it lives, a connection-pooled client tuned for gateway
// fan-out, and the circuit breaker guarding it.
type Upstream struct {
	Name    string
	BaseURL *url.URL
	Client  *http.Client
	Breaker *Breaker

	// Handler is set when this "upstream" is a domain in this process rather
	// than a service across the network. When it is set, BaseURL and Client
	// are nil and nothing dials. See inprocess.go.
	Handler http.Handler

	// sem bounds this domain's share of the process's in-flight work. Nil
	// means unbounded, which is right for a real network upstream: the thing
	// being protected there is the other process, and its own load shedding
	// does that.
	sem chan struct{}
}

// NewUpstream builds an Upstream with a transport whose connection pool is
// sized for a gateway (which fans many client requests into a handful of
// backend hosts) rather than the http.DefaultTransport pool, which is sized
// for a client calling many different hosts a few times each.
//
// MaxIdleConnsPerHost defaults to 2 in net/http. Left at that default, a
// gateway under load reopens a TCP+TLS handshake per request the moment
// concurrency exceeds 2, which is the single most common cause of gateway
// tail latency reported in production incident writeups. We raise it to 128.
func NewUpstream(name, baseURL string, breaker *Breaker, maxRetries int, retryBackoff time.Duration, onRetry func(), log zerolog.Logger) (*Upstream, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		Proxy: nil, // internal mesh call: never honour HTTP_PROXY env vars here
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   128,
		MaxConnsPerHost:       0, // unbounded; load shedding caps total in-flight instead
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 0, // bounded by the per-route context timeout instead
	}

	rt := newRetryTransport(transport, maxRetries, retryBackoff, name, log)
	rt.onRetry = onRetry

	return &Upstream{
		Name:    name,
		BaseURL: u,
		Client:  &http.Client{Transport: rt},
		Breaker: breaker,
	}, nil
}

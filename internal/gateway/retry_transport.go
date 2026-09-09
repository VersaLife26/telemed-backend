package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// idempotentMethods is the exact set the brief allows retries on. POST is
// deliberately absent: a POST may have charged a card or booked a slot, and
// retrying it blind on a timeout risks doing that twice.
var idempotentMethods = map[string]bool{
	http.MethodGet:    true,
	http.MethodHead:   true,
	http.MethodPut:    true,
	http.MethodDelete: true,
}

// retryTransport wraps an http.RoundTripper and retries idempotent requests
// that failed with a connection-level error -- never an HTTP response, not
// even a 5xx, and never a non-idempotent method.
type retryTransport struct {
	base       http.RoundTripper
	maxRetries int
	backoff    time.Duration
	log        zerolog.Logger
	upstream   string
	// onRetry, when set, is called once per retry attempt (not the initial
	// try) so callers can drive a Prometheus counter without this file
	// depending on the metrics package.
	onRetry func()
}

func newRetryTransport(base http.RoundTripper, maxRetries int, backoff time.Duration, upstream string, log zerolog.Logger) *retryTransport {
	return &retryTransport{base: base, maxRetries: maxRetries, backoff: backoff, upstream: upstream, log: log}
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !idempotentMethods[req.Method] {
		return t.base.RoundTrip(req)
	}

	// Buffer the body (GET/HEAD/DELETE rarely have one; PUT might) so it can
	// be replayed on retry. httputil.ReverseProxy always gives us a body we
	// own, so this never races the original caller.
	var bodyBytes []byte
	if req.Body != nil && req.Body != http.NoBody {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		bodyBytes = b
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	var lastErr error
	for attempt := 0; attempt <= t.maxRetries; attempt++ {
		if attempt > 0 {
			if bodyBytes != nil {
				req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}
			select {
			case <-time.After(t.backoff * time.Duration(attempt)):
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			t.log.Warn().Str("upstream", t.upstream).Str("method", req.Method).
				Int("attempt", attempt).Msg("retrying after connection error")
			if t.onRetry != nil {
				t.onRetry()
			}
		}

		resp, err := t.base.RoundTrip(req)
		if err == nil {
			return resp, nil
		}
		if !isRetryableConnError(err) {
			return resp, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// isRetryableConnError reports whether err represents a connection-level
// failure (dial refused, reset, DNS failure, i/o timeout on the socket) as
// opposed to a context cancellation/deadline the caller controls, which must
// never be retried -- retrying past a caller's own timeout just wastes the
// upstream's time.
func isRetryableConnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return false
}

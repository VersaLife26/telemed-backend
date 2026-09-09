package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// deadUpstreamURL returns the address of an httptest server that has already
// been closed, so any dial against it fails with a real connection-refused
// error -- the same failure mode a client sees when a backend pod is gone.
func deadUpstreamURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

func TestRetryTransport_POSTIsNeverRetried(t *testing.T) {
	var calls int32
	base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return nil, &net.OpError{Op: "dial", Err: errSimulatedOutage}
	})
	rt := newRetryTransport(base, 5, time.Millisecond, "test-upstream", zerolog.Nop())

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.invalid/x", strings.NewReader(`{"a":1}`))
	resp, err := rt.RoundTrip(req)
	closeBody(resp)
	if err == nil {
		t.Fatal("expected the connection error to propagate")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("POST was attempted %d times, want exactly 1 (never retried)", got)
	}
}

func TestRetryTransport_GETRetriedOnConnectionError(t *testing.T) {
	target := deadUpstreamURL(t) // connection refused for every attempt

	transport := &http.Transport{}
	rt := newRetryTransport(transport, 3, 5*time.Millisecond, "test-upstream", zerolog.Nop())

	var retries int32
	rt.onRetry = func() { atomic.AddInt32(&retries, 1) }

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target+"/x", http.NoBody)
	resp, err := rt.RoundTrip(req)
	closeBody(resp)
	if err == nil {
		t.Fatal("expected a connection error against a closed server")
	}
	if got := atomic.LoadInt32(&retries); got != 3 {
		t.Fatalf("retries = %d, want 3 (maxRetries)", got)
	}
}

func TestRetryTransport_SuccessOnSecondAttempt(t *testing.T) {
	var calls int32
	base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return nil, &net.OpError{Op: "dial", Err: errSimulatedOutage}
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
	})
	rt := newRetryTransport(base, 3, time.Millisecond, "test-upstream", zerolog.Nop())

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.invalid/x", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer closeBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("calls = %d, want 2 (one failure then one success)", got)
	}
}

func TestRetryTransport_HTTPErrorStatusIsNotRetried(t *testing.T) {
	// A 5xx response is not a connection error -- RoundTrip returns err=nil,
	// so it must be returned as-is on the first attempt, never retried, even
	// though GET is an idempotent method.
	var calls int32
	base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader("boom")), Header: http.Header{}}, nil
	})
	rt := newRetryTransport(base, 3, time.Millisecond, "test-upstream", zerolog.Nop())

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.invalid/x", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer closeBody(resp)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1 (a 5xx is not a connection error and must not be retried)", got)
	}
}

func TestRetryTransport_ContextCancelIsNotRetried(t *testing.T) {
	var calls int32
	base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return nil, context.DeadlineExceeded
	})
	rt := newRetryTransport(base, 3, time.Millisecond, "test-upstream", zerolog.Nop())

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.invalid/x", http.NoBody)
	resp, err := rt.RoundTrip(req)
	closeBody(resp)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1 (a caller-side deadline must not be retried)", got)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// closeBody releases a RoundTrip response. A transport test that leaves bodies
// open leaks a connection per call in exactly the way the production code is
// linted against, so the tests hold themselves to the same rule. nil-safe,
// because RoundTrip returns (nil, err) on a connection failure and several of
// these tests are asserting precisely that.
func closeBody(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

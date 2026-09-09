package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLoadShed_AnOversizedCapDoesNotWrapIntoATinyOne is the runtime half of the
// load-shed overflow fix.
//
// The in-flight counter is an int32 and MAX_IN_FLIGHT parses to an int. The
// call site used to bridge the two with a bare int32(...) conversion, which
// keeps only the low 32 bits. 2^32+1 has low bits 0x00000001, so an operator
// who typed a number meaning "effectively unlimited" got a cap of ONE: every
// concurrent request after the first is answered 503 by the gateway itself,
// with no upstream in trouble and nothing in the logs to say why. That is a
// self-inflicted outage produced by a configuration value, which is why the
// conversion is now a clamp.
//
// Removing the clamp in LoadShed makes this test fail: only one request ever
// reaches the handler and the rest are shed.
func TestLoadShed_AnOversizedCapDoesNotWrapIntoATinyOne(t *testing.T) {
	// Computed via int64 so the file still compiles on a 32-bit target.
	oversized := int(int64(1)<<32 + 1)

	release := make(chan struct{})
	var reached atomic.Int32

	h := LoadShed(oversized, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	const concurrent = 8
	codes := make(chan int, concurrent)
	var wg sync.WaitGroup
	for range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/doctors", http.NoBody)
			h.ServeHTTP(rec, req)
			codes <- rec.Code
		}()
	}

	// Every request must be sitting inside the handler at the same time. If
	// the cap wrapped to 1 this never happens -- the others are already shed.
	deadline := time.Now().Add(5 * time.Second)
	for reached.Load() < concurrent {
		if time.Now().After(deadline) {
			close(release)
			wg.Wait()
			t.Fatalf("only %d of %d concurrent requests reached the handler: "+
				"MAX_IN_FLIGHT=%d wrapped into a tiny cap instead of being clamped",
				reached.Load(), concurrent, oversized)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	close(codes)

	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("got status %d, want 200: nothing should be shed under an effectively unlimited cap", code)
		}
	}
}

// TestLoadShed_StillShedsAtAConfiguredCap makes sure the clamp did not simply
// disable the control it was protecting: a real, in-range cap must still bite.
func TestLoadShed_StillShedsAtAConfiguredCap(t *testing.T) {
	release := make(chan struct{})
	var reached atomic.Int32

	h := LoadShed(1, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/doctors", http.NoBody)
		h.ServeHTTP(rec, req)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for reached.Load() < 1 {
		if time.Now().After(deadline) {
			close(release)
			wg.Wait()
			t.Fatal("the first request never reached the handler")
		}
		time.Sleep(time.Millisecond)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/doctors", http.NoBody)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("second concurrent request got %d, want 503 at MAX_IN_FLIGHT=1", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q", got, "1")
	}

	close(release)
	wg.Wait()
}

package payment

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"telemed/internal/platform/cache"
)

// outageCache is a cache.Cache whose Incr always fails, standing in for a
// Redis outage.
type outageCache struct{ cache.Cache }

func (outageCache) Incr(context.Context, string, time.Duration) (int64, error) {
	return 0, errors.New("redis: connection refused")
}

// TestWebhookRoutesShedDuringARedisOutage pins the WIRING half of F24.
//
// The middleware-level test proves RateLimitConfig.FailClosed works. This one
// proves /webhooks/* actually sets it -- the mechanism existing and the
// endpoint not using it is precisely the shape of the original finding.
//
// It asserts on the router built by Routes() rather than on a config literal,
// so removing FailClosed from the call site fails here even though the
// mechanism it removed is still correct.
func TestWebhookRoutesShedDuringARedisOutage(t *testing.T) {
	routes := NewWebhookHandler(nil, zerolog.Nop()).Routes(outageCache{})

	for _, path := range []string{"/stripe", "/payhere", "/dialog", "/mock"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, http.NoBody)
			rec := httptest.NewRecorder()
			routes.ServeHTTP(rec, req)

			// 503 from the limiter, before any signature check, body read or
			// database write. Providers all retry on 503, so a shed callback
			// is delayed rather than lost.
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("POST %s during a Redis outage = %d, want %d; the limiter is the only throttle on this surface",
					path, rec.Code, http.StatusServiceUnavailable)
			}
		})
	}
}

package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// labelValues returns every distinct "route" label value the request counter
// currently holds.
func labelValues(t *testing.T, m *Metrics) []string {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out []string
	for _, f := range families {
		if !strings.HasSuffix(f.GetName(), "http_requests_total") {
			continue
		}
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				if l.GetName() == "route" {
					out = append(out, l.GetValue())
				}
			}
		}
	}
	return out
}

// A 404 must not let an anonymous caller choose a Prometheus label value.
//
// chi only fills the route pattern in during routing, so a request that
// matched nothing has an empty pattern. The fallback used to be r.URL.Path,
// which on this platform means /metrics -- served unauthenticated on every
// service listener -- published document and prescription UUIDs, permanently,
// to anyone who asked. It is also an unbounded label set, so the same requests
// are a memory-exhaustion DoS with no credential.
func TestMetricsRouteLabel_UnmatchedRequestCarriesNoIdentifier(t *testing.T) {
	m := NewMetrics("telemed-test")

	r := chi.NewRouter()
	r.Use(m.Middleware)
	r.Get("/api/v1/records/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	probes := []string{
		"/api/v1/records/7d2f4b1a-0000-4a3b-9c1d-2f3e4a5b6c7d/download",
		"/api/v1/verify/prescriptions/9f8e7d6c-1111-4222-8333-444455556666",
		"/nope/a1", "/nope/a2", "/nope/a3",
	}
	for _, p := range probes {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, p, http.NoBody)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}

	// One legitimate, routed request, so the test also proves the useful
	// label survives.
	r.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/records/7d2f4b1a-0000-4a3b-9c1d-2f3e4a5b6c7d", http.NoBody))

	got := labelValues(t, m)

	for _, v := range got {
		if strings.Contains(v, "7d2f4b1a") || strings.Contains(v, "9f8e7d6c") {
			t.Fatalf("a resource UUID reached an unauthenticated Prometheus label: route=%q", v)
		}
	}

	distinct := map[string]struct{}{}
	for _, v := range got {
		distinct[v] = struct{}{}
	}
	if _, ok := distinct["/api/v1/records/{id}"]; !ok {
		t.Fatalf("the matched route pattern is missing; got %v", got)
	}
	// 5 unmatched probes across 3 distinct paths must collapse to one label.
	if len(distinct) != 2 {
		t.Fatalf("route label set is not bounded: %d distinct values %v; unmatched requests must share one constant", len(distinct), got)
	}
	if _, ok := distinct["unmatched"]; !ok {
		t.Fatalf("expected the constant \"unmatched\" label; got %v", got)
	}
}

func TestSafePathIsUsedForUnmatchedLogLines(t *testing.T) {
	// Guards the log-side half of the same hole; see logger.SafePath.
	cases := map[string]string{
		"/api/v1/records/7d2f4b1a-0000-4a3b-9c1d-2f3e4a5b6c7d": "/api/v1/records/-",
		"/api/v1/doctors/pending":                              "/api/v1/doctors/pending",
	}
	for in, want := range cases {
		if got := safePathForTest(in); got != want {
			t.Fatalf("SafePath(%q) = %q, want %q", in, got, want)
		}
	}
}

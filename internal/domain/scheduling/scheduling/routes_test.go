package scheduling_test

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/go-chi/chi/v5"

	"telemed/internal/domain/scheduling/scheduling"
)

// TestRouteRegistration mounts every route group the way cmd/server does.
//
// chi panics at registration time on a conflicting pattern, so without this the
// first sign of a clash is a crash-looping pod. Mounting them here also pins the
// public surface: adding or renaming a route fails this test, which is the point.
func TestRouteRegistration(t *testing.T) {
	h := scheduling.NewHandler(scheduling.NewService(scheduling.Options{}), "")

	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		// The real router wraps these in OptionalAuth / RequireAuth /
		// IPAllowlist. Those are middleware, not routing, so they are omitted
		// here -- this test is about the routing table.
		r.Group(h.PublicRoutes)
		r.Group(h.PatientRoutes)
		r.Route("/admin", h.AdminRoutes)
	})

	var got []string
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got = append(got, method+" "+route)
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	sort.Strings(got)

	want := []string{
		"DELETE /api/v1/doctors/me/holidays/{holidayID}",
		"DELETE /api/v1/waitlist/{waitlistID}",
		"GET /api/v1/admin/appointments",
		"GET /api/v1/appointments/",
		"GET /api/v1/appointments/{appointmentID}",
		"GET /api/v1/doctors/me/holidays/",
		"GET /api/v1/doctors/{doctorID}/slots",
		"GET /api/v1/waitlist/",
		"POST /api/v1/admin/appointments/{appointmentID}/force-cancel",
		"POST /api/v1/admin/holidays",
		"POST /api/v1/admin/slots/{slotID}/block",
		"POST /api/v1/appointments/",
		"POST /api/v1/appointments/{appointmentID}/complete",
		"POST /api/v1/appointments/{appointmentID}/no-show",
		"POST /api/v1/doctors/me/holidays/",
		"POST /api/v1/waitlist/",
		"PUT /api/v1/appointments/{appointmentID}/cancel",
	}
	if len(got) != len(want) {
		t.Fatalf("registered %d routes, want %d:\ngot  %v\nwant %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("route %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestPublicRouteNeedsNoToken guards the one deliberate hole in the auth wall.
func TestPublicRouteNeedsNoToken(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)

	r := chi.NewRouter()
	r.Route("/api/v1", scheduling.NewHandler(svc, "").PublicRoutes)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/doctors/6f9619ff-8b86-d011-b42d-00c04fc964ff/slots", http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous availability lookup got %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestDoctorHolidayRoutesDoNotShadowSlots pins the one routing hazard the leave
// endpoints introduce.
//
// /doctors/me/holidays and /doctors/{doctorID}/slots share a prefix, and "me"
// is a legal value for {doctorID} as far as the pattern is concerned. chi
// resolves this by preferring the static segment, so the two coexist -- but
// that is a property of chi's trie, not something the code says out loud, and
// a future refactor to /doctors/{doctorID}/holidays would silently route a
// doctor's leave through the public slot handler.
func TestDoctorHolidayRoutesDoNotShadowSlots(t *testing.T) {
	h := scheduling.NewHandler(scheduling.NewService(scheduling.Options{}), "")

	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Group(h.PublicRoutes)
		r.Group(h.PatientRoutes)
	})

	cases := []struct {
		method, path, wantPattern string
	}{
		// Without the trailing slash -- which is how every client writes it.
		// chi still resolves it to the holidays subtree rather than letting
		// "me" fall through to {doctorID}.
		{http.MethodGet, "/api/v1/doctors/me/holidays", "/api/v1/doctors/me/holidays"},
		{http.MethodPost, "/api/v1/doctors/me/holidays", "/api/v1/doctors/me/holidays"},
		{http.MethodDelete, "/api/v1/doctors/me/holidays/6f9619ff-8b86-d011-b42d-00c04fc964ff",
			"/api/v1/doctors/me/holidays/{holidayID}"},
		{http.MethodGet, "/api/v1/doctors/6f9619ff-8b86-d011-b42d-00c04fc964ff/slots",
			"/api/v1/doctors/{doctorID}/slots"},
	}
	for _, tc := range cases {
		rctx := chi.NewRouteContext()
		if !r.Match(rctx, tc.method, tc.path) {
			t.Errorf("%s %s did not match any route", tc.method, tc.path)
			continue
		}
		if got := rctx.RoutePattern(); got != tc.wantPattern {
			t.Errorf("%s %s routed to %q, want %q", tc.method, tc.path, got, tc.wantPattern)
		}
	}
}

//go:build integration

package doctor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/platform/middleware"
)

// internalRouter reproduces the chain main.go mounts around InternalRoutes, so
// these tests exercise the same auth and role checks a deployment runs rather
// than calling the handlers directly.
func internalRouter(t *testing.T, env *exposureEnv) chi.Router {
	t.Helper()
	h := NewHandler(env.svc, env.signer.authenticator(t))
	r := chi.NewRouter()
	r.Route("/api/v1/admin/doctors", func(r chi.Router) {
		r.Use(middleware.RequireAuth(env.signer.authenticator(t)))
		r.Use(middleware.RequireRole(
			append([]middleware.Role{middleware.RoleService}, middleware.AdminRoles...)...))
		r.Mount("/", h.AdminScheduleRoutes())
	})
	return r
}

func doRequest(t *testing.T, r chi.Router, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestIntegration_AdminSetsAnotherDoctorsAvailability is the workflow this
// surface exists for: a doctor phones the clinic with their hours and an
// administrator enters them. Before it, the only writer was
// PUT /doctors/me/availability, which takes the doctor from the caller's own
// token and so cannot express "staff editing someone else's schedule".
func TestIntegration_AdminSetsAnotherDoctorsAvailability(t *testing.T) {
	env := newExposureEnv(t)
	r := internalRouter(t, env)

	doctorID, _ := env.registerDoctor(t, "SLMC8101", "Dr. Phones-It-In")
	adminToken := env.signer.token(t, uuid.New(), "admin")

	body := `{"working_hours":[
		{"day_of_week":4,"start_time":"09:00","end_time":"13:00","is_available":true}
	],"slot_duration_minutes":20,"buffer_minutes":0,"max_per_day":10}`

	rec := doRequest(t, r, http.MethodPut,
		"/api/v1/admin/doctors/"+doctorID.String()+"/availability", body, adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PUT availability = %d, body %s", rec.Code, rec.Body.String())
	}

	// Read it back through the admin GET, which is the only way staff can see
	// the schedule they are about to overwrite.
	rec = doRequest(t, r, http.MethodGet,
		"/api/v1/admin/doctors/"+doctorID.String()+"/availability", "", adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET availability = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "09:00") {
		t.Errorf("saved hours not returned: %s", rec.Body.String())
	}
}

// TestIntegration_AdminScheduleEditRejectsAnUnknownDoctor pins the guard.
// GetAvailability returns an EMPTY set for an id that does not exist rather
// than an error, so without the explicit lookup a typo in the path would
// report a successful save of a schedule belonging to nobody -- and staff
// would believe the doctor was bookable.
func TestIntegration_AdminScheduleEditRejectsAnUnknownDoctor(t *testing.T) {
	env := newExposureEnv(t)
	r := internalRouter(t, env)
	adminToken := env.signer.token(t, uuid.New(), "admin")

	body := `{"working_hours":[
		{"day_of_week":1,"start_time":"09:00","end_time":"12:00","is_available":true}
	],"slot_duration_minutes":30,"max_per_day":5}`

	rec := doRequest(t, r, http.MethodPut,
		"/api/v1/admin/doctors/"+uuid.New().String()+"/availability", body, adminToken)
	if rec.Code == http.StatusOK {
		t.Fatalf("saving a schedule for a doctor that does not exist returned 200: %s", rec.Body.String())
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown doctor = %d, want 404", rec.Code)
	}
}

// TestIntegration_AdminScheduleEditKeepsAZeroBuffer pins the pointer.
//
// buffer_minutes is a *int all the way to the wire because 0 -- back-to-back
// consultations -- is a real preference and not the absence of one. An
// administrator entering it on a doctor's behalf means exactly what the doctor
// means, so the admin DTO must not be the place the distinction is lost.
func TestIntegration_AdminScheduleEditKeepsAZeroBuffer(t *testing.T) {
	env := newExposureEnv(t)
	r := internalRouter(t, env)

	doctorID, _ := env.registerDoctor(t, "SLMC8102", "Dr. Back To Back")
	adminToken := env.signer.token(t, uuid.New(), "admin")

	body := `{"working_hours":[
		{"day_of_week":2,"start_time":"08:00","end_time":"12:00","is_available":true}
	],"slot_duration_minutes":15,"buffer_minutes":0,"max_per_day":0}`

	if rec := doRequest(t, r, http.MethodPut,
		"/api/v1/admin/doctors/"+doctorID.String()+"/availability", body, adminToken); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, body %s", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, r, http.MethodGet,
		"/api/v1/admin/doctors/"+doctorID.String()+"/schedule-settings", "", adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET schedule-settings = %d, body %s", rec.Code, rec.Body.String())
	}

	var env0 struct {
		Data struct {
			BufferMinutes *int `json:"buffer_minutes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env0); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if env0.Data.BufferMinutes == nil {
		t.Fatal("buffer_minutes came back null: a deliberate 0 was collapsed into 'unset', " +
			"and this doctor now gets the default gap forever")
	}
	if *env0.Data.BufferMinutes != 0 {
		t.Errorf("buffer_minutes = %d, want 0", *env0.Data.BufferMinutes)
	}
}

// TestIntegration_ScheduleEditingIsClosedToNonStaff states the boundary. These
// routes take the doctor from the PATH, so reaching them with a doctor's or a
// patient's token would be one person editing another's working hours.
func TestIntegration_ScheduleEditingIsClosedToNonStaff(t *testing.T) {
	env := newExposureEnv(t)
	r := internalRouter(t, env)

	victimID, _ := env.registerDoctor(t, "SLMC8103", "Dr. Victim")
	_, attackerToken := env.registerDoctor(t, "SLMC8104", "Dr. Attacker")

	body := `{"working_hours":[],"slot_duration_minutes":30,"max_per_day":1}`
	rec := doRequest(t, r, http.MethodPut,
		"/api/v1/admin/doctors/"+victimID.String()+"/availability", body, attackerToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a doctor token editing another doctor's schedule = %d, want 403", rec.Code)
	}

	if rec := doRequest(t, r, http.MethodGet,
		"/api/v1/admin/doctors/"+victimID.String()+"/availability", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated read = %d, want 401", rec.Code)
	}
}

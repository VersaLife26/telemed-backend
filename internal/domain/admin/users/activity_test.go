package users

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestRoutesMountsActivity(t *testing.T) {
	t.Parallel()

	found := false
	err := chi.Walk(NewHandler(nil).Routes(), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet && strings.Contains(route, "activity") {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	if !found {
		t.Fatal("GET /{id}/activity is not mounted; the console 404s as NOT_FOUND")
	}
}

func TestRegistrationSummary(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"doctor":  "Registered as a doctor",
		"patient": "Registered as a patient",
		"":        "Account registered",
	}
	for role, want := range cases {
		if got := registrationSummary(role); got != want {
			t.Errorf("registrationSummary(%q) = %q, want %q", role, got, want)
		}
	}
}

func TestAuditSummary(t *testing.T) {
	t.Parallel()
	if got := auditSummary("user.suspend_requested", " no-show "); got != "Suspension requested: no-show" {
		t.Errorf("got %q", got)
	}
	if got := auditSummary("user.reinstate_requested", ""); got != "Reinstatement requested" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 250)
	got := auditSummary("user.suspend_requested", long)
	if !strings.HasPrefix(got, "Suspension requested: ") {
		t.Errorf("got %q", got)
	}
	if len(got) != len("Suspension requested: ")+200 {
		t.Errorf("reason was not capped to 200 runes/bytes: len=%d", len(got))
	}
}

func TestAppointmentSummary(t *testing.T) {
	t.Parallel()
	if got := appointmentSummary("confirmed", "gp"); got != "Appointment confirmed (gp)" {
		t.Errorf("got %q", got)
	}
	if got := appointmentSummary("no_show", ""); got != "Appointment marked no-show" {
		t.Errorf("got %q", got)
	}
}

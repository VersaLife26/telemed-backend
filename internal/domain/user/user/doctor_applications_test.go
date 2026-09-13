package user

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type staticToken struct{}

func (staticToken) Token(context.Context) (string, error) { return "mesh-token", nil }

func TestHTTPDoctorApplications_ApplicationByEmail(t *testing.T) {
	id := uuid.New()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if got := r.Header.Get("Authorization"); got != "Bearer mesh-token" {
			t.Errorf("Authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"application_id": id.String(),
				"status":         "approved",
				"display_name":   "Kavindu Sanjana",
				"email":          "kavindusanjana@gmail.com",
				"phone":          "+94772568596",
				"password_hash":  "$2a$12$example",
			},
		})
	}))
	t.Cleanup(srv.Close)

	c := NewHTTPDoctorApplications(srv.URL, staticToken{})
	app, err := c.ApplicationByEmail(context.Background(), "kavindusanjana@gmail.com")
	if err != nil {
		t.Fatalf("ApplicationByEmail: %v", err)
	}
	if app.ID != id || app.Status != "approved" || app.PasswordHash == "" {
		t.Fatalf("app = %+v", app)
	}
	if !strings.Contains(gotPath, "/applications/by-email/") || !strings.Contains(gotPath, "kavindusanjana") {
		t.Fatalf("path = %s", gotPath)
	}
}

func TestHTTPDoctorApplications_ApplicationByIDNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := NewHTTPDoctorApplications(srv.URL, staticToken{})
	_, err := c.ApplicationByID(context.Background(), uuid.New())
	if !errors.Is(err, ErrDoctorApplicationNotFound) {
		t.Fatalf("got %v, want ErrDoctorApplicationNotFound", err)
	}
}

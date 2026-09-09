package consultation

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
)

// The recording-consent row is the legal record of who agreed to be filmed,
// and consultation_consents.ip_address is the only thing in it that says
// "from where". The handler used to resolve that address with a private copy
// of clientIP that trusted X-Forwarded-For from ANY peer and took the
// left-most value -- so the person consenting chose what the record said.
//
// The platform ships middleware.RealIP + ClientIPFrom, which walk XFF from the
// right and ignore it entirely when the TCP peer is not a trusted proxy. This
// pins that the handler uses it.
func TestConsentIPCannotBeChosenByTheCaller(t *testing.T) {
	st := newFakeStore()
	video := NewMockProvider("test-webhook-token")
	svc := NewService(st, video, newFakeCache(), fakePool{},
		events.NewOutbox("test-consultation-service"), zerolog.Nop(), Options{})

	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()
	patientJoins(t, svc, patientID, c)
	_, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorID), c.ID)
	require.NoError(t, err)

	h := NewHandler(svc, zerolog.Nop())
	r := chi.NewRouter()
	// RealIP with the default trusted set (private ranges only), exactly as
	// platform/server mounts it.
	r.Use(middleware.RealIP(nil))
	// Only the consent route, mounted without RequireAuth: the principal is
	// injected below. What is under test is where the IP comes from, not auth.
	r.Post("/consultations/{id}/consent", h.consent)

	body, err := json.Marshal(map[string]any{"consent_type": "recording", "granted": true})
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(
		middleware.WithPrincipal(ctx, middleware.Principal{UserID: patientID, Roles: []middleware.Role{middleware.RolePatient}}),
		http.MethodPost, "/consultations/"+c.ID.String()+"/consent", bytes.NewReader(body))
	// A public, untrusted peer claiming to be somebody else.
	req.RemoteAddr = "203.0.113.9:44321"
	req.Header.Set("X-Forwarded-For", "8.8.8.8, 1.1.1.1")
	req.Header.Set("X-Real-IP", "9.9.9.9")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	st.mu.Lock()
	defer st.mu.Unlock()
	require.NotEmpty(t, st.consents)
	got := st.consents[len(st.consents)-1].IPAddress
	require.Equal(t, "203.0.113.9", got,
		"the consent record's ip_address was chosen by the caller's headers (%q); it must come from the TCP peer when that peer is not a trusted proxy", got)
}

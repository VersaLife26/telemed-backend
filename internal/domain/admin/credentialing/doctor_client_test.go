package credentialing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type stubTokens struct{}

func (stubTokens) Token(context.Context) (string, error) { return "mesh-token", nil }

// TestVerifyApplication_StatusMapping is what keeps an admin from being told
// to retry something that cannot succeed.
//
// Approving now provisions the doctor's login, so this call fails in two ways
// that mean opposite things: a clashing account (409, permanent) and a
// user-service outage (503, retryable). Both used to arrive as the same opaque
// error and surfaced as a 500.
func TestVerifyApplication_StatusMapping(t *testing.T) {
	t.Parallel()
	const detail = "the email or phone on it already belongs to another account"

	cases := []struct {
		name    string
		status  int
		body    string
		want    error
		wantMsg string
	}{
		{
			name:   "approved",
			status: http.StatusOK,
			body:   `{"data":{}}`,
		},
		{
			name:   "no application: legacy doctor.registered row",
			status: http.StatusNotFound,
			body:   `{"code":"NOT_FOUND","message":"not found"}`,
			want:   ErrApplicationNotFound,
		},
		{
			name:    "permanent account clash",
			status:  http.StatusConflict,
			body:    `{"code":"CONFLICT","message":"` + detail + `"}`,
			want:    ErrLoginConflict,
			wantMsg: detail,
		},
		{
			name:    "user-service down",
			status:  http.StatusServiceUnavailable,
			body:    `{"code":"UNAVAILABLE","message":"retry the approval"}`,
			want:    ErrLoginUnavailable,
			wantMsg: "retry the approval",
		},
		{
			name:   "anything else stays generic",
			status: http.StatusInternalServerError,
			body:   `{"code":"INTERNAL","message":"boom"}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer mesh-token" {
					t.Errorf("Authorization = %q, want the mesh token", got)
				}
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			err := NewHTTPApplicationVerifier(srv.URL, stubTokens{}).
				VerifyApplication(context.Background(), uuid.New(), true, "")

			switch {
			case c.want != nil:
				if !errors.Is(err, c.want) {
					t.Fatalf("got %v, want %v", err, c.want)
				}
				if c.wantMsg != "" && !strings.Contains(err.Error(), c.wantMsg) {
					t.Errorf("error %q does not carry the upstream sentence %q", err, c.wantMsg)
				}
			case c.status == http.StatusOK:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			default:
				if err == nil {
					t.Fatal("expected an error")
				}
				if errors.Is(err, ErrLoginConflict) || errors.Is(err, ErrLoginUnavailable) || errors.Is(err, ErrApplicationNotFound) {
					t.Errorf("a %d must not be classified as a known provisioning outcome: %v", c.status, err)
				}
			}
		})
	}
}

func TestUpstreamMessage(t *testing.T) {
	t.Parallel()
	if got := upstreamMessage([]byte(`{"code":"CONFLICT","message":"  clash  "}`)); got != "clash" {
		t.Errorf("got %q, want the trimmed message field", got)
	}
	// A body that is not the error envelope still has to reach the operator.
	if got := upstreamMessage([]byte("plain text failure\n")); got != "plain text failure" {
		t.Errorf("got %q, want the raw body", got)
	}
	if got := upstreamMessage([]byte(`{"code":"CONFLICT"}`)); got != `{"code":"CONFLICT"}` {
		t.Errorf("got %q, want the raw body when there is no message", got)
	}
}

func TestProvisionDetail(t *testing.T) {
	t.Parallel()
	err := errors.New("credentialing: approved, but no doctor login could be created: reconcile the accounts first")
	if got := provisionDetail(err); got != "reconcile the accounts first" {
		t.Errorf("got %q, want just the upstream sentence", got)
	}
	// Nothing to strip: return it whole rather than an empty string.
	if got := provisionDetail(errors.New("bare")); got != "bare" {
		t.Errorf("got %q, want the message unchanged", got)
	}
}

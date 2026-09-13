package credentialing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ApplicationVerifier asks doctor-service to approve/reject a public application.
type ApplicationVerifier interface {
	VerifyApplication(ctx context.Context, applicationID uuid.UUID, approve bool, reason string) error
}

// ErrApplicationNotFound means doctor-service has no application with that id
// (legacy doctor.registered rows still use the event-only approve path).
var ErrApplicationNotFound = errors.New("credentialing: doctor application not found")

// ErrLoginConflict is doctor-service reporting that the application was
// approved but its identity cannot be given a login, and never will be
// without someone reconciling the accounts first.
//
// It exists so the admin console can say that instead of a bare 500. The
// upstream sentence is carried along, because it is the one that tells the
// operator which account is in the way.
var ErrLoginConflict = errors.New("credentialing: approved, but no doctor login could be created")

// ErrLoginUnavailable is the retryable twin: user-service was down or broken
// when the approval ran. Approving again is the fix.
var ErrLoginUnavailable = errors.New("credentialing: approved, but the doctor login could not be created yet")

// TokenSource mints a mesh Bearer token.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// HTTPApplicationVerifier calls doctor-service internal verify.
type HTTPApplicationVerifier struct {
	baseURL string
	tokens  TokenSource
	client  *http.Client
}

func NewHTTPApplicationVerifier(baseURL string, tokens TokenSource) *HTTPApplicationVerifier {
	return &HTTPApplicationVerifier{
		baseURL: strings.TrimRight(baseURL, "/"),
		tokens:  tokens,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *HTTPApplicationVerifier) VerifyApplication(ctx context.Context, applicationID uuid.UUID, approve bool, reason string) error {
	action := "reject"
	if approve {
		action = "approve"
	}
	body, err := json.Marshal(map[string]string{"action": action, "reason": reason})
	if err != nil {
		return err
	}
	tok, err := c.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("credentialing: mesh token: %w", err)
	}
	url := c.baseURL + "/api/v1/internal/doctors/applications/" + applicationID.String() + "/verify"
	// The target is the operator-configured address of another domain on this
	// platform, never anything derived from a request, so there is no
	// user-controlled taint here for G704 to follow.
	//nolint:gosec // G704: mesh address from config, not from a request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	// The target is the operator-configured address of another domain on this
	// platform, never anything derived from a request, so there is no
	// user-controlled taint here for G704 to follow.
	//nolint:gosec // G704: mesh address from config, not from a request
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("credentialing: doctor-service verify: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrApplicationNotFound
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrLoginConflict, upstreamMessage(raw))
	case http.StatusServiceUnavailable:
		return fmt.Errorf("%w: %s", ErrLoginUnavailable, upstreamMessage(raw))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("credentialing: doctor-service verify returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// upstreamMessage pulls the human sentence out of doctor-service's error
// envelope. It falls back to the raw body: a message that reads oddly beats
// swallowing the only clue about why an approval failed.
func upstreamMessage(raw []byte) string {
	var env struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && strings.TrimSpace(env.Message) != "" {
		return strings.TrimSpace(env.Message)
	}
	return strings.TrimSpace(string(raw))
}

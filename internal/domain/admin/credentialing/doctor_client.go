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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("credentialing: doctor-service verify: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return ErrApplicationNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("credentialing: doctor-service verify returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

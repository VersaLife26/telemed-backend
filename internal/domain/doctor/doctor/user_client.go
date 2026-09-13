package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TokenSource mints a mesh Bearer token.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// HTTPAccountProvisioner calls user-service to create the doctor login.
type HTTPAccountProvisioner struct {
	baseURL string
	tokens  TokenSource
	client  *http.Client
}

// NewHTTPAccountProvisioner talks to user-service over the mesh.
func NewHTTPAccountProvisioner(baseURL string, tokens TokenSource) *HTTPAccountProvisioner {
	return &HTTPAccountProvisioner{
		baseURL: strings.TrimRight(baseURL, "/"),
		tokens:  tokens,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

type provisionEnvelope struct {
	Data struct {
		UserID          string `json:"user_id"`
		PasswordApplied bool   `json:"password_applied"`
	} `json:"data"`
}

func (c *HTTPAccountProvisioner) ProvisionDoctor(ctx context.Context, in DoctorAccount) (ProvisionResult, error) {
	body, err := json.Marshal(map[string]string{
		"email":         in.Email,
		"phone":         in.Phone,
		"name":          in.Name,
		"password_hash": in.PasswordHash,
	})
	if err != nil {
		return ProvisionResult{}, err
	}
	tok, err := c.tokens.Token(ctx)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("doctor: mesh token: %w", err)
	}
	path := c.baseURL + "/api/v1/internal/users/doctors"
	//nolint:gosec // G704: mesh address from config, not from a request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return ProvisionResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	//nolint:gosec // G704: mesh address from config, not from a request
	resp, err := c.client.Do(req)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("doctor: user-service provision: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("doctor: read user-service response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := strings.TrimSpace(string(raw))
		if permanentProvisionStatus(resp.StatusCode) {
			return ProvisionResult{}, fmt.Errorf("%w: user-service returned %d: %s", ErrAccountConflict, resp.StatusCode, detail)
		}
		return ProvisionResult{}, fmt.Errorf("doctor: user-service returned %d: %s", resp.StatusCode, detail)
	}
	var env provisionEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return ProvisionResult{}, fmt.Errorf("doctor: decode user-service envelope: %w", err)
	}
	id, err := uuid.Parse(env.Data.UserID)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("doctor: user-service user_id: %w", err)
	}
	return ProvisionResult{UserID: id, PasswordApplied: env.Data.PasswordApplied}, nil
}

// permanentProvisionStatus separates "this will never work" from "this might
// work in a minute", so the admin console can tell an operator to fix the
// duplicate account instead of telling them to press Approve again forever.
//
// Every 4xx user-service raises here is a fact about the data -- a conflicting
// account (409), a suspended one (403), an unusable email or phone (400) --
// none of which a retry changes. The exceptions are the three that explicitly
// mean "later": request timeout, too early, and rate limited.
func permanentProvisionStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	default:
		return code >= 400 && code < 500
	}
}
